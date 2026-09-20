package mvcc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"gbaselite/storageengine"
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
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
	closed      bool
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
	return s.newTx(p, snapshot, nil)
}
func (s *Store) newTx(p Proposer, snapshot uint64, parent *Tx) (*Tx, error) {
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
		registered, err := s.txns.RegisterRoot(RootRegistration{ID: id, ReadTS: snapshot, Generation: generation, StartedAt: startedAt})
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
	return &Tx{generation: generation, term: term, store: s, proposer: p, parent: parent, Snapshot: snapshot, ID: id, path: path, startedAt: startedAt, state: TransactionActive, counters: counters}, nil
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
func (t *Tx) Child() (*Tx, error) {
	if t.closed || t.generation != t.store.generation.Load() {
		return nil, ErrClosed
	}
	return t.store.newTx(t.proposer, t.Snapshot, t)
}
func (t *Tx) readOp(space string, rowKey []byte) (Op, bool, error) {
	if t.stage == nil && len(t.buffered) == 0 {
		return Op{}, false, validateKey(space, rowKey)
	}
	k, err := key(space, rowKey)
	if err != nil {
		return Op{}, false, err
	}
	v, err := t.stagedValue(k)
	if err != nil || v == nil {
		return Op{}, false, err
	}
	op, err := decodeStagedOp(k, v)
	return op, true, err
}

// checkOpen reports whether this transaction may still be used.
func (t *Tx) checkOpen() error {
	if t.closed || t.generation != t.store.generation.Load() {
		return ErrClosed
	}
	return nil
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
func (t *Tx) Put(space string, rowKey, value []byte) error {
	return t.write(Op{Space: space, Key: rowKey, Value: value})
}
func (t *Tx) Delete(space string, rowKey []byte) error {
	return t.write(Op{Space: space, Key: rowKey, Delete: true})
}
func (t *Tx) write(op Op) error {
	if t.closed || t.generation != t.store.generation.Load() {
		return ErrClosed
	}
	if err := validateCommand(Command{Ops: []Op{op}}); err != nil {
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
	if t.closed {
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
	if t.closed {
		return nil
	}
	return t.finish(TransactionAborted, TransitionOptions{})
}

// finishIncomplete is the safety net for a commit path that exits without deciding
// an outcome. It cannot revise a decided result, because finish is a no-op once the
// transaction is closed, and it stays publication-aware: if the marker already
// exists the transaction is committed whatever happened to the caller.
func (t *Tx) finishIncomplete() {
	if t.closed {
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
	if t.closed {
		return nil
	}
	t.applyTransition(state, options)
	err := t.cleanup()
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
// separate from the lifecycle state: a successful commit and an abort release the
// same resources the same way, and cleanup never changes a decided outcome.
func (t *Tx) cleanup() error {
	if t.closed {
		return nil
	}
	t.closed = true
	t.buffered = nil
	t.bufferBytes = 0
	var err, removeErr error
	if t.stage != nil {
		err = t.stage.Close()
		removeErr = os.Remove(t.path)
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

func (t *Tx) Guard(space string, rowKey []byte) error {
	return t.write(Op{Space: space, Key: rowKey, Check: true})
}
