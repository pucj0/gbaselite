package mvcc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"gbaselite/storageengine"
	bolt "go.etcd.io/bbolt"
)

// ErrWriteSetLimit reports an explicitly configured logical write-set budget.
var ErrWriteSetLimit = storageengine.ErrWriteSetLimit

type Tx struct {
	generation  uint64
	term        uint64
	store       *Store
	proposer    Proposer
	parent      *Tx
	Snapshot    uint64
	ID          string
	stage       *bolt.DB
	path        string
	closed      atomic.Bool
	stagedBytes int64
	buffered    map[string][]byte
	bufferBytes int64

	// mu guards the mutable lifecycle fields below: state, commitTS, hasCommitTS and
	// abortReason. The owning goroutine writes them in applyTransition, while State and
	// Info are diagnostics accessors that a caller may reach from another goroutine, so
	// those four fields are the one part of a transaction that is safe to read
	// concurrently. startedAt sits in the same block but is written once at construction
	// and then immutable, so Info reads it without the lock. The rest of Tx keeps the
	// documented single-goroutine contract.
	mu          sync.Mutex
	startedAt   time.Time
	state       TransactionState
	commitTS    uint64
	hasCommitTS bool
	abortReason string

	// counters is shared with the registry entry, so diagnostics can read it without
	// locking the commit path.
	counters *transactionCounters

	// lockOwnerID is the root lock owner every grant of this transaction belongs to. A child
	// inherits its parent's, so a grant taken by a savepoint layer is owned by the root.
	lockOwnerID string
	// lockScopeID is this transaction's own lock scope: a statement scope for a root, a child
	// scope for a child. It is a request origin only and never owns a grant.
	lockScopeID uint64
	// lockDeadline bounds how long a request from this transaction may wait. Zero means the
	// manager's session default.
	lockDeadline time.Time

	// writeBaseline records, per logical key, the version this transaction observed under the
	// covering lock. It is what reconciles a locked current read with the commit rule: a write to
	// a key with a recorded baseline is validated against the version that was actually observed,
	// while every other key keeps the transaction's readTS comparison.
	//
	// It is bounded by the lock budget, because a baseline is only recorded for a key this
	// transaction successfully locked, and it is released with the transaction.
	writeBaseline map[string]uint64
	// baselineRejections counts observations dropped because the baseline bound was reached. It
	// is a counter rather than another collection, so a transaction that read more locked keys
	// than the bound costs a fixed amount of state.
	baselineRejections uint64

	// guardedKeys tracks keys that had an explicit optimistic Guard dependency declared.
	// An optimistic guard on a key must never be relaxed by a subsequent write baseline on the same key.
	guardedKeys map[string]bool

	stageReaders int
	stageDrained chan struct{}
}

func (t *Tx) acquireStageLease() (*bolt.DB, func(), error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed.Load() {
		return nil, nil, ErrClosed
	}
	if t.stage == nil {
		return nil, nil, nil
	}
	t.stageReaders++
	stage := t.stage
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.stageReaders--
			if t.closed.Load() && t.stageReaders == 0 && t.stageDrained != nil {
				close(t.stageDrained)
				t.stageDrained = nil
			}
		})
	}
	return stage, release, nil
}

func randomID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}
func (s *Store) Begin(ctx context.Context, p Proposer) (*Tx, error) {
	if p == nil {
		p = s
	}
	if err := p.Barrier(ctx); err != nil {
		return nil, err
	}
	s.views.Lock()
	defer s.views.Unlock()
	snapshot, err := s.Head()
	if err != nil {
		return nil, err
	}
	// Registering while Store.views is held ties the new root transaction to the
	// generation it actually observed: Store.Restore bumps the generation and
	// purges the registry under the same lock.
	return s.newTx(ctx, p, snapshot, nil)
}

// BeginLazy starts a root transaction without pre-pinning a snapshot revision.
// The snapshot view will be pinned on the first consistent read (MySQL RR Lazy Snapshot).
func (s *Store) BeginLazy(ctx context.Context, p Proposer) (*Tx, error) {
	if p == nil {
		p = s
	}
	if err := p.Barrier(ctx); err != nil {
		return nil, err
	}
	s.views.Lock()
	defer s.views.Unlock()
	snapshot, err := s.Head()
	if err != nil {
		return nil, err
	}
	return s.newTxInternal(ctx, p, snapshot, nil, true)
}

func (s *Store) newTx(ctx context.Context, p Proposer, snapshot uint64, parent *Tx) (*Tx, error) {
	return s.newTxInternal(ctx, p, snapshot, parent, false)
}

func (s *Store) newTxInternal(ctx context.Context, p Proposer, snapshot uint64, parent *Tx, lazy bool) (*Tx, error) {
	id := randomID()
	path := filepath.Join(filepath.Dir(s.path), "transactions", id+".tmp")
	var term uint64
	if source, ok := p.(interface{ Term() uint64 }); ok {
		term = source.Term()
	}
	generation := s.generation.Load()
	startedAt := time.Now()
	var counters *transactionCounters
	if parent == nil {
		// Only a root transaction owns history retention: it pins its read
		// timestamp so compaction cannot drop versions it may still need
		// (FR-007 / INV-007).
		registered, err := s.txns.RegisterRoot(RootRegistration{
			ID:         id,
			ReadTS:     snapshot,
			Generation: generation,
			StartedAt:  startedAt,
			Lazy:       lazy,
		})
		if err != nil {
			return nil, err
		}
		counters = registered
	} else {
		// The child inherits the parent's read timestamp from the registry, so a
		// savepoint layer can never pin a second snapshot.
		registered, err := s.txns.RegisterChild(ChildRegistration{ID: id, ParentID: parent.ID, Generation: generation, StartedAt: startedAt})
		if err != nil {
			return nil, err
		}
		counters = registered
	}
	tx := &Tx{generation: generation, term: term, store: s, proposer: p, parent: parent, Snapshot: snapshot, ID: id, path: path, startedAt: startedAt, state: TransactionActive, counters: counters}
	// The lock owner and the transaction's own scope are created here, while the transaction is
	// still being built and before any caller can use it. A failure leaves no half-registered
	// transaction behind: the registry entry is removed and the owner is released.
	if err := s.beginLockOwner(tx); err != nil {
		s.txns.Unregister(id)
		return nil, err
	}
	if err := s.beginTransactionScope(tx); err != nil {
		tx.closeTransactionLocks()
		s.txns.Unregister(id)
		return nil, err
	}
	return tx, nil
}
func (t *Tx) ensureStage() error {
	if t.stage != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0700); err != nil {
		return err
	}
	db, err := bolt.Open(t.path, 0600, &bolt.Options{Timeout: time.Second, NoSync: true, NoFreelistSync: true})
	if err != nil {
		return err
	}
	if err = db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucket([]byte("writes")); return err }); err != nil {
		db.Close()
		os.Remove(t.path)
		return err
	}
	t.stage = db
	return nil
}
func (t *Tx) Child() (*Tx, error) { return t.ChildContext(context.Background()) }

// ChildContext creates a child transaction.
//
// A child inherits the root's lock owner, so every grant it takes is attributed to the root and
// survives the child's merge. No grant is ever owned by a child, which is why a child commit
// cannot release the root's record X even in principle.
func (t *Tx) ChildContext(ctx context.Context) (*Tx, error) {
	if t.closed.Load() || t.generation != t.store.generation.Load() {
		if t.generation != t.store.generation.Load() {
			return nil, ErrClosed
		}
		if t.deadlockAborted() {
			return nil, storageengine.ErrDeadlock
		}
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t.store.newTx(ctx, t.proposer, t.Snapshot, t)
}
func (t *Tx) readOp(space string, rowKey []byte) (Op, bool, error) {
	k, err := key(space, rowKey)
	if err != nil {
		return Op{}, false, err
	}
	t.mu.Lock()
	if t.stage == nil && len(t.buffered) == 0 {
		t.mu.Unlock()
		return Op{}, false, nil
	}
	t.mu.Unlock()

	v, err := t.stagedValue(k)
	if err != nil || v == nil {
		return Op{}, false, err
	}
	op, err := decodeStagedOp(k, v)
	return op, true, err
}

// checkOpen reports whether this transaction may still be used.
func (t *Tx) checkOpen() error {
	if t.generation != t.store.generation.Load() {
		return ErrClosed
	}
	if t.closed.Load() {
		if t.deadlockAborted() {
			return storageengine.ErrDeadlock
		}
		return ErrClosed
	}
	return nil
}

// DeadlockAborted reports whether this transaction was aborted as a deadlock victim.
func (t *Tx) DeadlockAborted() bool {
	return t.deadlockAborted()
}

// Get records one point-read observation. Only counters change: the keys a
// transaction read are never retained, so a large SELECT cannot grow a read set.
func (t *Tx) Get(space string, rowKey []byte) ([]byte, bool, error) {
	if err := t.checkOpen(); err != nil {
		return nil, false, err
	}
	value, ok, err := t.read(space, rowKey)
	observation := transactionObservation{}
	if err == nil && ok {
		observation = transactionObservation{rows: 1, bytes: uint64(len(rowKey) + len(value))}
	}
	t.observePointRead(observation)
	return value, ok, err
}

// read resolves a read without recording an observation, so an ancestor lookup
// stays part of the observation that started it.
func (t *Tx) read(space string, rowKey []byte) ([]byte, bool, error) {
	if err := t.checkOpen(); err != nil {
		return nil, false, err
	}
	op, found, err := t.readOp(space, rowKey)
	if err != nil {
		return nil, false, err
	}
	if found && !op.Check {
		return op.Value, !op.Delete, nil
	}
	if t.parent != nil {
		return t.parent.read(space, rowKey)
	}
	v, _, ok, err := t.store.Get(t.Snapshot, space, rowKey)
	if t.generation != t.store.generation.Load() {
		return nil, false, ErrConflict
	}
	return v, ok, err
}

// ReadAt resolves a point read at the specified snapshot revision, merging this transaction's
// own staged writes and its ancestors' staged writes.
func (t *Tx) ReadAt(revision uint64, space string, rowKey []byte) ([]byte, bool, error) {
	return t.readAt(revision, space, rowKey)
}

func (t *Tx) readAt(revision uint64, space string, rowKey []byte) ([]byte, bool, error) {
	if err := t.checkOpen(); err != nil {
		return nil, false, err
	}
	op, found, err := t.readOp(space, rowKey)
	if err != nil {
		return nil, false, err
	}
	if found && !op.Check {
		return op.Value, !op.Delete, nil
	}
	if t.parent != nil {
		return t.parent.readAt(revision, space, rowKey)
	}
	v, _, ok, err := t.store.Get(revision, space, rowKey)
	if t.generation != t.store.generation.Load() {
		return nil, false, ErrConflict
	}
	return v, ok, err
}
func (t *Tx) Put(space string, rowKey, value []byte) error {
	return t.write(Op{Space: space, Key: rowKey, Value: value})
}
func (t *Tx) Delete(space string, rowKey []byte) error {
	return t.write(Op{Space: space, Key: rowKey, Delete: true})
}
func (t *Tx) write(op Op) error {
	if t.closed.Load() || t.generation != t.store.generation.Load() {
		return ErrClosed
	}
	if err := validateCommand(Command{Ops: []Op{op}}); err != nil {
		return err
	}
	// Every raw write entry point participates in the same lock domain as SQL: a Put or Delete
	// takes the exclusive record lock of its exact key before anything is staged, so neither layer
	// can bypass the other and no bbolt write transaction is open while a caller waits. A guard is
	// an optimistic dependency and takes no lock.
	if err := t.AcquireRawWriteLock(op); err != nil {
		return err
	}
	k, _ := key(op.Space, op.Key)
	return t.bufferWrite(op, k, encodeStagedOp(op))
}

// Scan records one range-read observation for the whole call, never per row.
func (t *Tx) Scan(ctx context.Context, space string, yield func([]byte, []byte) error) (scanErr error) {
	defer func() {
		if t.generation != t.store.generation.Load() {
			scanErr = ErrConflict
		}
	}()
	if err := t.checkOpen(); err != nil {
		return err
	}
	var observation transactionObservation
	wrapped := func(k, v []byte) error {
		observation.add(k, v)
		return yield(k, v)
	}
	err := t.scan(ctx, space, wrapped)
	// Rows observed before a consumer error or a cancellation were still observed.
	t.observeRangeRead(observation)
	return err
}

// scan performs the layered read without recording an observation, so an ancestor
// lookup stays part of the observation that started it.
func (t *Tx) scan(ctx context.Context, space string, yield func([]byte, []byte) error) error {
	if err := t.checkOpen(); err != nil {
		return err
	}
	if err := t.flushWrites(); err != nil {
		return err
	}
	visit := func(k, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Most scan layers have no writes. Avoid building a namespace key for
		// every row; recheck each callback so writes made by a consumer still apply.
		if t.stage == nil && len(t.buffered) == 0 {
			return yield(k, v)
		}
		op, found, err := t.readOp(space, k)
		if err != nil {
			return err
		}
		if found && !op.Check {
			if op.Delete || op.Check {
				return nil
			}
			v = op.Value
		}
		return yield(k, v)
	}
	var err error
	if t.parent != nil {
		err = t.parent.scan(ctx, space, visit)
	} else {
		err = t.store.Scan(ctx, t.Snapshot, space, visit)
	}
	if err != nil {
		return err
	}
	if t.stage == nil {
		return nil
	}
	prefix, _ := key(space, nil)
	return t.stage.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("writes")).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			op, err := decodeStagedOp(k, v)
			if err != nil {
				return err
			}
			if op.Delete || op.Check {
				continue
			}
			var exists bool
			if t.parent != nil {
				_, exists, err = t.parent.read(space, op.Key)
			} else {
				_, _, exists, err = t.store.Get(t.Snapshot, space, op.Key)
			}
			if err != nil {
				return err
			}
			if !exists {
				if err := yield(bytes.Clone(op.Key), op.Value); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func (t *Tx) WalkWrites(ctx context.Context, yield func(Op) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.stage == nil {
		keys := make([]string, 0, len(t.buffered))
		for k := range t.buffered {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			op, err := decodeStagedOp([]byte(k), t.buffered[k])
			if err != nil {
				return err
			}
			if err := yield(op); err != nil {
				return err
			}
		}
		return nil
	}
	if err := t.flushWrites(); err != nil {
		return err
	}
	return t.stage.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("writes")).ForEach(func(k, v []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			op, err := decodeStagedOp(k, v)
			if err != nil {
				return err
			}
			return yield(op)
		})
	})
}

// Commit ends a root transaction at its durable publication, or merges a child into
// its parent. The terminal state is decided by the outcome, never by cleanup:
// see finish.
func (t *Tx) Commit(ctx context.Context) (uint64, error) {
	if t.generation != t.store.generation.Load() {
		t.finish(TransactionAborted, TransitionOptions{Reason: "generation change"})
		return 0, ErrConflict
	}
	if t.closed.Load() {
		if t.deadlockAborted() {
			return 0, storageengine.ErrDeadlock
		}
		return 0, ErrClosed
	}
	if t.parent != nil {
		return t.commitChild(ctx)
	}
	return t.commitRoot(ctx)
}

// commitChild merges into the parent. A child commit is not a database commit: it
// installs no version, allocates no sequence and leaves the head alone, so a
// successful merge ends the child as MERGED -- never COMMITTED, and never ABORTED
// after a merge that succeeded.
func (t *Tx) commitChild(ctx context.Context) (uint64, error) {
	defer t.finishIncomplete()
	// A chosen deadlock victim cannot merge: the merge would publish its writes into a root that
	// is about to be aborted, and the contract forbids a victim from continuing to write.
	if err := t.canStillCommit(); err != nil {
		t.finish(TransactionAborted, TransitionOptions{Reason: abortReason(err)})
		return t.Snapshot, err
	}
	if err := t.mergeParent(ctx); err != nil {
		// The merge failed, so the child's writes were discarded. mergeParent has
		// already aborted the parent when it could not complete.
		t.finish(TransactionAborted, TransitionOptions{Reason: "merge failed"})
		return t.Snapshot, err
	}
	t.finish(TransactionMerged, TransitionOptions{})
	return t.Snapshot, nil
}

// commitRoot publishes a root transaction.
//
// The state moves to COMMITTING only when there is something to publish, and only a
// confirmed durable publication records COMMITTED with the sequence that published
// it. A root with no write set commits without a commit sequence and without moving
// the head, exactly as before.
func (t *Tx) commitRoot(ctx context.Context) (uint64, error) {
	defer t.finishIncomplete()
	// The victim check is the first thing a commit does, before the empty-root shortcut and before
	// the COMMITTING transition. A victim may therefore not publish even an empty transaction:
	// "cannot continue to commit" is about the outcome, not about whether there was work to do.
	if err := t.canStillCommit(); err != nil {
		t.finish(TransactionAborted, TransitionOptions{Reason: abortReason(err)})
		return 0, err
	}
	if !t.hasWriteSet() {
		// Read-only or empty: no sequence is allocated, no version is installed and
		// the publication marker is untouched.
		t.finish(TransactionCommitted, TransitionOptions{})
		return t.Snapshot, nil
	}
	// The durable commit is in flight from here on. COMMITTING is the only state a
	// root may leave before it becomes COMMITTED or ABORTED, and it is recorded while
	// the registry entry still exists.
	t.applyTransition(TransactionCommitting, TransitionOptions{})
	sequence, published, err := t.publish(ctx)
	switch {
	case published:
		// The publication marker is the commit point: the sequence that published it
		// is the commit sequence, and it is recorded only now.
		t.finish(TransactionCommitted, TransitionOptions{CommitTS: sequence, HasCommitTS: true})
		return sequence, nil
	case err == nil:
		// Nothing needed publishing after all: commit without a commit sequence.
		t.finish(TransactionCommitted, TransitionOptions{})
		return sequence, nil
	default:
		// The conflict marker is what makes the aggregate conflict count mean
		// something: a conflict-driven abort is still an abort, and it is counted as
		// both. It classifies the outcome only; it changes no visibility, no
		// validation and no commit decision.
		t.finish(TransactionAborted, TransitionOptions{Reason: abortReason(err), Conflict: errors.Is(err, ErrConflict)})
		return sequence, err
	}
}

// publish performs the durable commit attempt and reports whether a publication
// marker exists afterwards. Callers reach it only with a write set, so a nil error
// means the transaction is durably committed. The returned sequence is the commit
// sequence exactly when published is true.
func (t *Tx) publish(ctx context.Context) (uint64, bool, error) {
	if sequence, handled, err := t.tryLocalCommit(ctx); handled {
		return sequence, err == nil, err
	}
	completed := false
	defer func() {
		if !completed {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = t.proposer.Propose(cleanup, Command{Kind: "abort", ID: t.ID})
		}
	}()
	var batch []Op
	size := 0
	count := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		r, err := t.proposer.Propose(ctx, Command{Term: t.term, Kind: "stage", ID: t.ID, Snapshot: t.Snapshot, Ops: batch})
		if err == nil {
			err = r.Err()
		}
		batch = nil
		size = 0
		return err
	}
	err := t.WalkWrites(ctx, func(op Op) error {
		n := len(op.Space) + len(op.Key) + len(op.Value) + 64
		if size+n > MaxChunkBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, op)
		size += n
		count++
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return 0, false, err
	}
	if count == 0 {
		completed = true
		return t.Snapshot, false, nil
	}
	result, err := t.proposer.Propose(ctx, Command{Term: t.term, Kind: "commit", ID: t.ID, Snapshot: t.Snapshot})
	if err != nil {
		return 0, false, err
	}
	completed = result.Err() == nil
	return result.Sequence, completed, result.Err()
}

// hasWriteSet reports whether the transaction has any dependency to publish. Being
// empty here is exactly the read-only or empty transaction, so no walk of the write
// set is needed to decide it.
func (t *Tx) hasWriteSet() bool {
	return t.stage != nil || len(t.buffered) != 0 || t.stagedBytes != 0
}

// Rollback aborts the transaction: it records ABORTED, releases resources and
// removes the registry entry. A transaction that already reached a terminal state is
// left exactly as it is, so a double rollback stays idempotent and a successful
// commit can never be rewritten as an abort.
func (t *Tx) Rollback() error {
	if t.closed.Load() {
		return nil
	}
	return t.finish(TransactionAborted, TransitionOptions{})
}

// finishIncomplete is the safety net for a commit path that exits without deciding
// an outcome. It cannot revise a decided result, because finish is a no-op once the
// transaction is closed, and it stays publication-aware: if the marker already
// exists the transaction is committed whatever happened to the caller.
func (t *Tx) finishIncomplete() {
	if t.closed.Load() {
		return
	}
	if sequence, committed := t.store.Committed(t.ID); committed {
		t.finish(TransactionCommitted, TransitionOptions{CommitTS: sequence, HasCommitTS: true})
		return
	}
	t.finish(TransactionAborted, TransitionOptions{Reason: "commit incomplete"})
}

// finish records the terminal lifecycle state, releases the transaction's resources
// and removes the registry entry, in that order: the semantic outcome is recorded
// while the entry still exists, and the entry is dropped only afterwards. It is
// idempotent, which is what keeps a double rollback harmless and forbids a later
// cleanup from rewriting COMMITTED or MERGED into ABORTED.
func (t *Tx) finish(state TransactionState, options TransitionOptions) error {
	if t.closed.Load() {
		return nil
	}
	if state == TransactionAborted && options.Reason == "" {
		if reason := t.AbortRequestReason(); reason != "" {
			options.Reason = string(reason)
		}
	}
	// The order is the contract's and each step depends on the previous one:
	//
	//  1. the semantic outcome is recorded while the registry entry still exists;
	//  2. the write set becomes unusable, so no later step can still publish from it;
	//  3. the lock participation ends: a child withdraws its scope, a root releases its owner;
	//  4. the registry entry is dropped.
	//
	// Releasing locks after step 2 is what makes "a root release happens only after the write set
	// can no longer be used" true for the success and the failure path alike, and doing it after
	// step 1 is what forbids a release from rewriting COMMITTED into ABORTED.
	t.applyTransition(state, options)
	err := t.cleanup()
	t.closeTransactionLocks()
	if t.generation == t.store.generation.Load() {
		t.store.txns.Unregister(t.ID)
	}
	return err
}

// applyTransition records one lifecycle move on the registry entry while it still
// exists and on this transaction's own snapshot, so the two can never disagree. A
// stale or already purged transaction only updates its own snapshot. It is the only
// writer of the lifecycle fields, and the manager is the authority for the
// registered copy: state, commit sequence, abort category and statistics.
//
// The manager call happens before the transaction's own fields are published, so no
// lock is ever held across the two, and a concurrent diagnostics snapshot sees
// either the previous or the new lifecycle values but never a partial move.
func (t *Tx) applyTransition(state TransactionState, options TransitionOptions) {
	if t.generation == t.store.generation.Load() {
		_ = t.store.txns.Transition(t.ID, state, options)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = state
	if options.HasCommitTS {
		t.commitTS, t.hasCommitTS = options.CommitTS, true
	}
	if state == TransactionAborted {
		t.abortReason = options.Reason
	}
}

// cleanup releases the transaction's resources exactly once. It is deliberately
var stageCleanupWaitHook func()

// separate from the lifecycle state: a successful commit and an abort release the
// same resources the same way, and cleanup never changes a decided outcome.
func (t *Tx) cleanup() error {
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		return nil
	}
	t.closed.Store(true)
	t.buffered = nil
	t.bufferBytes = 0
	stage := t.stage
	t.stage = nil
	path := t.path

	var waitCh chan struct{}
	if t.stageReaders > 0 {
		waitCh = make(chan struct{})
		t.stageDrained = waitCh
		if stageCleanupWaitHook != nil {
			stageCleanupWaitHook()
		}
	}
	t.mu.Unlock()

	if waitCh != nil {
		<-waitCh
	}

	if t.parent == nil && t.store != nil && t.store.dirtyRegistry != nil {
		t.store.dirtyRegistry.Withdraw(t.ID)
	}
	var err, removeErr error
	if stage != nil {
		err = stage.Close()
		removeErr = os.Remove(path)
	}
	return errors.Join(err, removeErr)
}

// abortReason maps a terminal commit error to the short diagnostics category the
// contract allows. It is a class, never a key, value or SQL payload.
func abortReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrSequenceExhausted):
		return "sequence exhausted"
	case errors.Is(err, ErrWriteSetLimit):
		return "write set limit"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, ErrClosed):
		return "closed"
	default:
		return "commit failed"
	}
}

// Info returns this transaction's diagnostics snapshot, including the terminal
// outcome it ended in. A finished transaction's registry entry is removed, so this
// is the snapshot that keeps its state, commit sequence, abort category and counters
// observable. It is safe to call from another goroutine while the owning goroutine
// commits.
func (t *Tx) Info() TransactionInfo {
	t.mu.Lock()
	state, commitTS, hasCommitTS, abortReason := t.state, t.commitTS, t.hasCommitTS, t.abortReason
	t.mu.Unlock()
	info := TransactionInfo{
		ID:          t.ID,
		StartTS:     t.Snapshot,
		ReadTS:      t.Snapshot,
		CommitTS:    commitTS,
		HasCommitTS: hasCommitTS,
		State:       state,
		Generation:  t.generation,
		StartedAt:   t.startedAt,
		AbortReason: abortReason,
	}
	if t.parent != nil {
		info.ParentID = t.parent.ID
	}
	t.counters.applyTo(&info)
	return info
}

// State reports the transaction's lifecycle state, including the terminal state it
// ended in. It is safe to call from another goroutine while the owning goroutine
// commits.
func (t *Tx) State() TransactionState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

const maxGuardedKeys = 16384

func (t *Tx) Guard(space string, rowKey []byte) error {
	k, err := key(space, rowKey)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		return ErrClosed
	}
	if t.guardedKeys == nil {
		t.guardedKeys = make(map[string]bool, 4)
	}
	sk := string(k)
	isNew := !t.guardedKeys[sk]
	if isNew {
		if len(t.guardedKeys) >= maxGuardedKeys {
			t.mu.Unlock()
			return fmt.Errorf("%w: maximum optimistic guarded keys (%d) exceeded", ErrWriteSetLimit, maxGuardedKeys)
		}
		t.guardedKeys[sk] = true
	}
	t.mu.Unlock()

	err = t.write(Op{Space: space, Key: rowKey, Check: true})
	if err != nil {
		if isNew {
			t.mu.Lock()
			delete(t.guardedKeys, sk)
			t.mu.Unlock()
		}
		return err
	}
	return nil
}

// Closed reports whether the transaction is closed.
func (t *Tx) Closed() bool {
	return t.closed.Load()
}

func (t *Tx) Generation() uint64 {
	return t.generation
}

func (t *Tx) Term() uint64 {
	return t.term
}

// CollectDirtyVersions extracts the uncommitted writes of t and its ancestor layers.
func (t *Tx) CollectDirtyVersions() []DirtyVersion {
	t.mu.Lock()
	defer t.mu.Unlock()

	var chain []*Tx
	for curr := t; curr != nil; curr = curr.parent {
		chain = append(chain, curr)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	collected := make(map[string]DirtyVersion)

	for _, tx := range chain {
		if tx.stage == nil {
			for k, v := range tx.buffered {
				op, err := decodeStagedOp([]byte(k), v)
				if err != nil || op.Check {
					continue
				}
				compKey := op.Space + "\x00" + string(op.Key)
				collected[compKey] = DirtyVersion{
					Space:   op.Space,
					Key:     bytes.Clone(op.Key),
					Value:   bytes.Clone(op.Value),
					Deleted: op.Delete,
				}
			}
		} else {
			_ = tx.stage.View(func(btx *bolt.Tx) error {
				b := btx.Bucket([]byte("writes"))
				if b == nil {
					return nil
				}
				return b.ForEach(func(k, v []byte) error {
					op, err := decodeStagedOp(k, v)
					if err != nil || op.Check {
						return nil
					}
					compKey := op.Space + "\x00" + string(op.Key)
					collected[compKey] = DirtyVersion{
						Space:   op.Space,
						Key:     bytes.Clone(op.Key),
						Value:   bytes.Clone(op.Value),
						Deleted: op.Delete,
					}
					return nil
				})
			})
			for k, v := range tx.buffered {
				op, err := decodeStagedOp([]byte(k), v)
				if err != nil || op.Check {
					continue
				}
				compKey := op.Space + "\x00" + string(op.Key)
				collected[compKey] = DirtyVersion{
					Space:   op.Space,
					Key:     bytes.Clone(op.Key),
					Value:   bytes.Clone(op.Value),
					Deleted: op.Delete,
				}
			}
		}
	}

	res := make([]DirtyVersion, 0, len(collected))
	for _, ver := range collected {
		res = append(res, ver)
	}
	return res
}
