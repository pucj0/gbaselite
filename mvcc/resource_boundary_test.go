package mvcc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// Extreme numeric and resource boundaries.
//
// Two rules are pinned here:
//
//   - Commit sequence allocation fails closed. The last legal sequence is
//     math.MaxUint64; past it no new version is allocated, because wrapping would
//     hand out sequence 0, which is not a version, and reusing a sequence would
//     overwrite committed history.
//   - The write-set budget keeps its exact semantics: an exact fit succeeds, one
//     byte more fails, and 0 means an unlimited total with a still-bounded memory
//     buffer that spills to the staging database.

// storeMeta reads the durable sequence accounting without going through the
// public API, so a test can prove nothing moved.
func storeMeta(t *testing.T, s *Store) (head, applied, allocated uint64) {
	t.Helper()
	if err := s.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		head = number(meta.Get([]byte("head")))
		applied = number(meta.Get([]byte("applied")))
		allocated = number(meta.Get([]byte("allocated")))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return head, applied, allocated
}

// requireNoVersionZero proves the store never wrote a record at sequence 0, which
// the version layout rejects and which could shadow committed history.
func requireNoVersionZero(t *testing.T, s *Store) {
	t.Helper()
	if err := s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(commitsBucket).Get(sequence(0)) != nil {
			t.Fatal("a publication marker exists for sequence 0")
		}
		if tx.Bucket(commitsBucket).Get([]byte{0}) != nil {
			t.Fatal("a transaction ID entry exists for the zero sequence")
		}
		return tx.Bucket(dataBucket).ForEach(func(k, _ []byte) error {
			rows := tx.Bucket(dataBucket).Bucket(k)
			if rows == nil {
				return nil // flat-layout directory entry
			}
			if rows.Get(sequence(0)) != nil {
				t.Fatalf("key %q holds a version-0 record", k)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// exhaustionPath commits one transaction through one local commit path.
type exhaustionPath struct {
	name     string
	localWAL bool
	commit   func(t *testing.T, s *Store) string
}

func newExhaustionStore(t *testing.T, localWAL bool) *Store {
	t.Helper()
	s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: localWAL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedExhaustionStore commits one row and returns the snapshot that reads it.
func seedExhaustionStore(t *testing.T, s *Store) uint64 {
	t.Helper()
	tx := beginBaseline(t, s)
	putVisible(t, tx, "existing", "committed")
	mustCommit(t, tx)
	return mustHead(t, s)
}

func smallWriteSet(t *testing.T, s *Store) []Op {
	t.Helper()
	return captureConflictOps(t, s, putConflict("k", "value"))
}

var exhaustionPaths = []exhaustionPath{
	{
		name: "local-bounded",
		commit: func(t *testing.T, s *Store) string {
			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			putVisible(t, tx, "k", "value")
			if _, err := tx.Commit(context.Background()); !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("bounded commit error = %v, want ErrSequenceExhausted", err)
			}
			return tx.ID
		},
	},
	{
		name: "local-group",
		commit: func(t *testing.T, s *Store) string {
			id := randomID()
			request := &localCommitRequest{ctx: context.Background(), generation: s.generation.Load(), snapshot: mustHead(t, s), id: id, ops: smallWriteSet(t, s), done: make(chan struct{})}
			s.commitLocalGroup([]*localCommitRequest{request})
			if !errors.Is(request.err, ErrSequenceExhausted) {
				t.Fatalf("group commit error = %v, want ErrSequenceExhausted", request.err)
			}
			if request.index != 0 {
				t.Fatalf("group commit reported index %d", request.index)
			}
			return id
		},
	},
	{
		name: "local-streaming",
		commit: func(t *testing.T, s *Store) string {
			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			putVisible(t, tx, "k", "value")
			padCancelStream(t, tx, "exhaust")
			if _, err := tx.Commit(context.Background()); !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("streaming commit error = %v, want ErrSequenceExhausted", err)
			}
			return tx.ID
		},
	},
	{
		name:     "local-wal",
		localWAL: true,
		commit: func(t *testing.T, s *Store) string {
			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			putVisible(t, tx, "k", "value")
			if _, err := tx.Commit(context.Background()); !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("wal commit error = %v, want ErrSequenceExhausted", err)
			}
			return tx.ID
		},
	},
	{
		name:     "local-wal-streaming",
		localWAL: true,
		commit: func(t *testing.T, s *Store) string {
			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			putVisible(t, tx, "k", "value")
			padCancelStream(t, tx, "walexhaust")
			if _, err := tx.Commit(context.Background()); !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("wal streaming commit error = %v, want ErrSequenceExhausted", err)
			}
			return tx.ID
		},
	},
	{
		name: "replicated-client",
		commit: func(t *testing.T, s *Store) string {
			id := randomID()
			// Every command consumes a sequence index, so exhaustion closes the whole
			// proposal path: staging fails before a commit can even be proposed.
			if _, err := s.Propose(context.Background(), Command{Kind: "stage", ID: id, Ops: smallWriteSet(t, s)}); !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("exhausted stage error = %v, want ErrSequenceExhausted", err)
			}
			if _, err := s.Propose(context.Background(), Command{Kind: "commit", ID: id, Snapshot: mustHead(t, s)}); !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("exhausted commit error = %v, want ErrSequenceExhausted", err)
			}
			return id
		},
	},
}

// The last legal sequence is usable, and exactly one commit can take it.
func TestLastLegalSequenceIsUsable(t *testing.T) {
	s := newExhaustionStore(t, false)
	head := seedExhaustionStore(t, s)

	s.localSeq = math.MaxUint64 - 1
	tx := beginCancelTx(t, s, nil)
	defer tx.Rollback()
	putVisible(t, tx, "last", "value")
	sequence, err := tx.Commit(context.Background())
	if err != nil {
		t.Fatalf("commit at the last legal sequence failed: %v", err)
	}
	if sequence != math.MaxUint64 {
		t.Fatalf("sequence = %d, want %d", sequence, uint64(math.MaxUint64))
	}
	if published, committed := s.Committed(tx.ID); !committed || published != math.MaxUint64 {
		t.Fatalf("Committed = (%d, %v), want (%d, true)", published, committed, uint64(math.MaxUint64))
	}
	// The last version is a normal, visible commit.
	requireCancelValue(t, s, "last", "value")
	requireCancelValue(t, s, "existing", "committed")
	if after := mustHead(t, s); after != math.MaxUint64 {
		t.Fatalf("head = %d, want %d", after, uint64(math.MaxUint64))
	}
	if s.localSeq != math.MaxUint64 {
		t.Fatalf("localSeq = %d after the last sequence", s.localSeq)
	}

	// The next write transaction fails closed and changes nothing.
	next := beginCancelTx(t, s, nil)
	defer next.Rollback()
	putVisible(t, next, "beyond", "value")
	if _, err := next.Commit(context.Background()); !errors.Is(err, ErrSequenceExhausted) {
		t.Fatalf("commit past the last sequence = %v, want ErrSequenceExhausted", err)
	}
	if s.localSeq != math.MaxUint64 {
		t.Fatalf("sequence wrapped to %d", s.localSeq)
	}
	requireCancelValue(t, s, "last", "value")
	requireInvisible(t, s, "beyond")
	requireNoVersionZero(t, s)
	if head != 1 {
		t.Fatalf("seed head = %d, want 1", head)
	}
}

// Exhaustion fails closed on every local path, without wrapping, without a marker
// and without touching committed data.
func TestSequenceExhaustionFailsClosedEveryPath(t *testing.T) {
	for _, path := range exhaustionPaths {
		t.Run(path.name, func(t *testing.T) {
			s := newExhaustionStore(t, path.localWAL)
			seedExhaustionStore(t, s)
			head, applied, allocated := storeMeta(t, s)

			s.localSeq = math.MaxUint64
			id := path.commit(t, s)

			if s.localSeq != math.MaxUint64 {
				t.Fatalf("sequence wrapped to %d", s.localSeq)
			}
			if _, committed := s.Committed(id); committed {
				t.Fatal("an exhausted commit published a marker")
			}
			afterHead, afterApplied, afterAllocated := storeMeta(t, s)
			if afterHead != head || afterApplied != applied || afterAllocated != allocated {
				t.Fatalf("metadata moved: head %d->%d applied %d->%d allocated %d->%d",
					head, afterHead, applied, afterApplied, allocated, afterAllocated)
			}
			// Exhaustion is a refused write, not a broken store: reads keep working.
			if err := s.AvailabilityError(); err != nil {
				t.Fatalf("exhaustion poisoned the store: %v", err)
			}
			requireCancelValue(t, s, "existing", "committed")
			requireInvisible(t, s, "k")
			requireNoVersionZero(t, s)

			// A read-only transaction still starts and reads at the current head.
			reader := beginBaseline(t, s)
			requireCancelValue(t, s, "existing", "committed")
			if _, err := reader.Commit(context.Background()); err != nil {
				t.Fatalf("read-only commit failed after exhaustion: %v", err)
			}
		})
	}
}

// A replicated apply carries its index from the log, so the same rule is enforced
// where the sequence arrives: sequence 0 is not a version.
func TestReplicatedApplyRejectsZeroSequence(t *testing.T) {
	s := newExhaustionStore(t, false)
	seedExhaustionStore(t, s)

	id := randomID()
	// Apply ignores an index that is not above the applied high-water mark, so the
	// staged command must use a fresh one.
	_, applied, _ := storeMeta(t, s)
	if _, err := s.Apply(applied+1, Command{Kind: "stage", ID: id, Ops: smallWriteSet(t, s)}); err != nil {
		t.Fatal(err)
	}
	// Baseline after staging: the failing commit must move nothing at all.
	head, applied, allocated := storeMeta(t, s)
	result, err := s.Apply(0, Command{Kind: "commit", ID: id, Snapshot: head})
	if err != nil {
		t.Fatalf("apply error = %v, want the failure reported through the result", err)
	}
	if !errors.Is(result.Err(), ErrSequenceExhausted) {
		t.Fatalf("result error = %v, want ErrSequenceExhausted", result.Err())
	}
	if _, committed := s.Committed(id); committed {
		t.Fatal("a zero-sequence commit published a marker")
	}
	if afterHead, _, _ := storeMeta(t, s); afterHead != head {
		t.Fatalf("head moved to %d", afterHead)
	}
	if _, afterApplied, afterAllocated := storeMeta(t, s); afterApplied != applied || afterAllocated != allocated {
		t.Fatalf("sequence metadata moved: applied %d allocated %d", afterApplied, afterAllocated)
	}
	requireCancelValue(t, s, "existing", "committed")
	requireInvisible(t, s, "k")
	requireNoVersionZero(t, s)
}

// The replicated path refuses a sequence that was already used, so it can never
// overwrite committed history, and it reports the refusal as a conflict just like a
// local path reports a stale write.
func TestReplicatedApplyRejectsUsedSequence(t *testing.T) {
	s := newExhaustionStore(t, false)
	head := seedExhaustionStore(t, s)
	before := physicalVersionCount(t, s, visibilitySpace, "k")

	first := randomID()
	_, applied, _ := storeMeta(t, s)
	stageIndex, commitIndex := applied+1, applied+2
	if _, err := s.Apply(stageIndex, Command{Kind: "stage", ID: first, Ops: smallWriteSet(t, s)}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Apply(commitIndex, Command{Kind: "commit", ID: first, Snapshot: head})
	if err != nil || result.Err() != nil {
		t.Fatalf("first commit = %+v, %v", result, err)
	}
	if result.Sequence != commitIndex {
		t.Fatalf("first sequence = %d, want %d", result.Sequence, commitIndex)
	}
	installed := physicalVersionCount(t, s, visibilitySpace, "k")
	if installed <= before {
		t.Fatalf("the first commit installed nothing (%d -> %d)", before, installed)
	}
	committedHead, _, _ := storeMeta(t, s)

	// A different transaction reusing the same sequence must conflict, not
	// overwrite the version already published at that sequence.
	second := randomID()
	if _, err := s.Apply(commitIndex+1, Command{Kind: "stage", ID: second, Ops: captureConflictOps(t, s, putConflict("k", "overwrite"))}); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Apply(commitIndex, Command{Kind: "commit", ID: second, Snapshot: head})
	if err != nil {
		t.Fatalf("stale apply error = %v", err)
	}
	if !errors.Is(stale.Err(), ErrConflict) {
		t.Fatalf("stale sequence result = %v, want ErrConflict", stale.Err())
	}
	if _, committed := s.Committed(second); committed {
		t.Fatal("the stale commit published a marker")
	}
	if got := physicalVersionCount(t, s, visibilitySpace, "k"); got != installed {
		t.Fatalf("version count changed from %d to %d", installed, got)
	}
	if afterHead, _, _ := storeMeta(t, s); afterHead != committedHead {
		t.Fatalf("head moved to %d", afterHead)
	}
	requireCancelValue(t, s, "k", "value")
}

// M-8: the replicated apply path carries its index from the log, so the last legal
// revision can arrive from the log rather than from the local allocator. The sequence
// model pinned here is:
//
//   - sequence 0 is never a version (rejected where the index arrives, see
//     TestReplicatedApplyRejectsZeroSequence);
//   - MaxUint64 is the last legal revision, and a replicated apply may use it;
//   - once the high-water mark is MaxUint64, every later *local* allocation must fail
//     closed instead of wrapping to 0.
//
// The high-water mark is reached through the real Apply entry point rather than by
// setting localSeq in the test, so this covers the hand-off from the replicated path to
// the local allocator.
func TestReplicatedMaxSequenceThenLocalAllocationFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := newExhaustionStore(t, false)
	head := seedExhaustionStore(t, s)

	// A replicated transaction staged at the next index and committed at MaxUint64 is a
	// legal commit: the last revision is usable.
	id := randomID()
	_, applied, _ := storeMeta(t, s)
	if _, err := s.Apply(applied+1, Command{Kind: "stage", ID: id, Ops: smallWriteSet(t, s)}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Apply(math.MaxUint64, Command{Kind: "commit", ID: id, Snapshot: head})
	if err != nil {
		t.Fatalf("replicated apply at MaxUint64 = %v, want success", err)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("replicated apply at MaxUint64 reported %v", err)
	}
	if result.Sequence != math.MaxUint64 {
		t.Fatalf("replicated sequence = %d, want %d", result.Sequence, uint64(math.MaxUint64))
	}
	if published, committed := s.Committed(id); !committed || published != math.MaxUint64 {
		t.Fatalf("Committed = (%d, %v), want (%d, true)", published, committed, uint64(math.MaxUint64))
	}
	lastHead, lastApplied, lastAllocated := storeMeta(t, s)
	if lastHead != math.MaxUint64 || lastApplied != math.MaxUint64 || lastAllocated != math.MaxUint64 {
		t.Fatalf("metadata = head %d applied %d allocated %d, want MaxUint64 for all three",
			lastHead, lastApplied, lastAllocated)
	}
	if s.localSeq != math.MaxUint64 {
		t.Fatalf("localSeq = %d after a MaxUint64 apply, want %d", s.localSeq, uint64(math.MaxUint64))
	}
	requireCancelValue(t, s, "k", "value")
	requireCancelValue(t, s, "existing", "committed")

	// Every local allocation is now refused: no wrap, no version 0, no metadata move.
	writer := beginCancelTx(t, s, nil)
	defer writer.Rollback()
	putVisible(t, writer, "after", "value")
	if _, err := writer.Commit(ctx); !errors.Is(err, ErrSequenceExhausted) {
		t.Fatalf("local commit after a MaxUint64 apply = %v, want ErrSequenceExhausted", err)
	}
	if s.localSeq != math.MaxUint64 {
		t.Fatalf("the local allocator wrapped to %d", s.localSeq)
	}
	if _, committed := s.Committed(writer.ID); committed {
		t.Fatal("the refused local commit published a marker")
	}
	info := writer.Info()
	if info.State != TransactionAborted || info.HasCommitTS {
		t.Fatalf("refused local commit lifecycle = %+v, want ABORTED without a commit revision", info)
	}
	if afterHead, afterApplied, afterAllocated := storeMeta(t, s); afterHead != lastHead || afterApplied != lastApplied || afterAllocated != lastAllocated {
		t.Fatalf("a refused local commit moved metadata: head %d->%d applied %d->%d allocated %d->%d",
			lastHead, afterHead, lastApplied, afterApplied, lastAllocated, afterAllocated)
	}
	// Exhaustion is a refused write, not a broken store.
	if err := s.AvailabilityError(); err != nil {
		t.Fatalf("exhaustion poisoned the store: %v", err)
	}
	requireCancelValue(t, s, "k", "value")
	requireCancelValue(t, s, "existing", "committed")
	requireInvisible(t, s, "after")
	requireNoVersionZero(t, s)

	// A replicated apply may still use an index at or below the high-water mark: the
	// store stays readable and idempotent for replay, it just cannot allocate a new one.
	reader := beginBaseline(t, s)
	if reader.Snapshot != math.MaxUint64 {
		t.Fatalf("a new snapshot = %d, want the last revision %d", reader.Snapshot, uint64(math.MaxUint64))
	}
	requireCancelValue(t, s, "existing", "committed")
	if _, err := reader.Commit(ctx); err != nil {
		t.Fatalf("read-only commit after exhaustion: %v", err)
	}
}

// --- Write-set budget -------------------------------------------------------

// exactPutCost is the staged cost of one write, the unit the budget uses.
func exactPutCost(t *testing.T, rowKey, value string) int64 {
	t.Helper()
	encoded, err := key(visibilitySpace, []byte(rowKey))
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(encoded) + len(encodeStagedOp(putOp(rowKey, value))))
}

func TestWriteSetLimitExactAndPlusOne(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	exact := exactPutCost(t, "k", "value")

	s.writeSetLimit = exact
	putVisible(t, tx, "k", "value")
	if tx.stagedBytes != exact {
		t.Fatalf("stagedBytes = %d, want the exact budget %d", tx.stagedBytes, exact)
	}
	// One byte past the budget fails closed and leaves nothing behind.
	if err := tx.Put(visibilitySpace, []byte("other"), []byte("x")); !errors.Is(err, ErrWriteSetLimit) {
		t.Fatalf("limit+1 error = %v, want ErrWriteSetLimit", err)
	}
	requireInvisible(t, s, "other")
	if tx.stagedBytes != exact {
		t.Fatalf("a rejected write changed stagedBytes to %d", tx.stagedBytes)
	}
	// A shrinking overwrite frees budget, so restoring the original value fits again
	// exactly at the budget: only the replacement delta is accounted.
	if err := tx.Put(visibilitySpace, []byte("k"), []byte("v")); err != nil {
		t.Fatalf("shrinking overwrite failed: %v", err)
	}
	if tx.stagedBytes >= exact {
		t.Fatalf("shrinking overwrite did not free budget: %d", tx.stagedBytes)
	}
	if err := tx.Put(visibilitySpace, []byte("k"), []byte("value")); err != nil {
		t.Fatalf("regrowing to the exact budget failed: %v", err)
	}
	if tx.stagedBytes != exact {
		t.Fatalf("stagedBytes = %d, want the exact budget %d", tx.stagedBytes, exact)
	}
	// A new key still does not fit.
	if err := tx.Put(visibilitySpace, []byte("other"), []byte("x")); !errors.Is(err, ErrWriteSetLimit) {
		t.Fatalf("second key error = %v, want ErrWriteSetLimit", err)
	}
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	requireCancelValue(t, s, "k", "value")
	requireInvisible(t, s, "other")
}

func TestWriteSetLimitZeroIsUnlimitedTotalWithBoundedMemory(t *testing.T) {
	directory := t.TempDir()
	s, err := OpenWithOptions(directory, Options{WriteSetLimitBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx := beginBaseline(t, s)
	value := bytes.Repeat([]byte{'v'}, 4096)
	const writes = 96 // ~384 KiB of logical write set, three times the memory buffer
	for i := 0; i < writes; i++ {
		if err := tx.Put(visibilitySpace, []byte(fmt.Sprintf("k%04d", i)), value); err != nil {
			t.Fatalf("unlimited budget rejected write %d: %v", i, err)
		}
		if tx.bufferBytes > writeBufferBytes {
			t.Fatalf("memory buffer exceeded its bound at write %d: %d", i, tx.bufferBytes)
		}
	}
	if tx.stage == nil {
		t.Fatal("an unlimited write set did not spill to the staging database")
	}
	if tx.stagedBytes <= int64(2*writeBufferBytes) {
		t.Fatalf("stagedBytes = %d, want the total to exceed the memory bound", tx.stagedBytes)
	}
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("unlimited commit failed: %v", err)
	}
	// The whole write set is durable.
	reader := beginBaseline(t, s)
	for i := 0; i < writes; i++ {
		rowKey := fmt.Sprintf("k%04d", i)
		if value, ok, err := reader.Get(visibilitySpace, []byte(rowKey)); err != nil || !ok || len(value) != 4096 {
			t.Fatalf("row %s = (%d bytes, %v, %v)", rowKey, len(value), ok, err)
		}
	}
}

// The largest operation the public API accepts stays inside the memory buffer: the
// value budget plus the largest legal key is well below the buffer budget, so the
// direct-to-stage fallback is not reachable through Put. The fallback itself is
// pinned separately below so the branch cannot rot.
func TestLargestLegalOperationStaysInTheMemoryBuffer(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	large := bytes.Repeat([]byte{'L'}, MaxValueBytes)
	if err := tx.Put(visibilitySpace, []byte("large"), large); err != nil {
		t.Fatalf("largest legal operation failed: %v", err)
	}
	if tx.stage != nil {
		t.Fatal("the largest legal operation opened the staging database")
	}
	if tx.bufferBytes == 0 || tx.bufferBytes > writeBufferBytes {
		t.Fatalf("bufferBytes = %d, want within (0, %d]", tx.bufferBytes, writeBufferBytes)
	}
	if want := exactPutCost(t, "large", string(large)); tx.stagedBytes != want {
		t.Fatalf("stagedBytes = %d, want %d", tx.stagedBytes, want)
	}
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	value, _, ok, err := s.Get(^uint64(0), visibilitySpace, []byte("large"))
	if err != nil || !ok || !bytes.Equal(value, large) {
		t.Fatalf("large row read back as %d bytes, ok=%v err=%v", len(value), ok, err)
	}
}

// An operation whose cost exceeds the memory budget is written straight to the
// staging database instead of enlarging the buffer. The public API cannot reach
// this branch under the current key and value limits, so it is exercised directly.
func TestOperationAboveTheMemoryBudgetGoesStraightToStage(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	op := Op{Space: visibilitySpace, Key: []byte("huge"), Value: make([]byte, writeBufferBytes)}
	encoded := encodeStagedOp(op)
	encodedKey, err := key(op.Space, op.Key)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(encodedKey)+len(encoded)) <= writeBufferBytes {
		t.Fatal("the fixture does not exceed the memory budget")
	}
	if err := tx.bufferWrite(op, encodedKey, encoded); err != nil {
		t.Fatalf("oversized operation failed: %v", err)
	}
	if tx.stage == nil {
		t.Fatal("an operation above the memory budget did not open the staging database")
	}
	if tx.bufferBytes != 0 {
		t.Fatalf("bufferBytes = %d, want the operation to bypass the buffer", tx.bufferBytes)
	}
	if tx.stagedBytes != int64(len(encodedKey)+len(encoded)) {
		t.Fatalf("stagedBytes = %d, want the operation's full cost", tx.stagedBytes)
	}
	// The synthetic operation is larger than the value budget, which is exactly why
	// the branch is unreachable through Put: the largest legal value plus the largest
	// legal key still stays below the memory budget. The placement invariant above is
	// therefore pinned directly rather than through the public API.
	if err := validateCommand(Command{ID: "x", Ops: []Op{op}}); err == nil {
		t.Fatal("the synthetic operation unexpectedly passes the value budget")
	}
}

// The accounting guard fails closed at the int64 boundary and never lets a
// shrinking overwrite be rejected.
func TestStagedBytesAccountingOverflowFailsClosed(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	putVisible(t, tx, "k", "value")
	before := tx.stagedBytes

	tx.stagedBytes = math.MaxInt64
	if err := tx.Put(visibilitySpace, []byte("grow"), []byte("x")); !errors.Is(err, ErrWriteSetLimit) {
		t.Fatalf("overflow error = %v, want ErrWriteSetLimit", err)
	}
	requireInvisible(t, s, "grow")
	if tx.stagedBytes != math.MaxInt64 {
		t.Fatalf("a rejected overflow changed stagedBytes to %d", tx.stagedBytes)
	}

	// One byte below the ceiling with a delta of more than one still overflows.
	tx.stagedBytes = math.MaxInt64 - 1
	if err := tx.Put(visibilitySpace, []byte("grow2"), []byte("xy")); !errors.Is(err, ErrWriteSetLimit) {
		t.Fatalf("near-ceiling overflow error = %v, want ErrWriteSetLimit", err)
	}

	// Shrinking is always allowed: the delta is negative, so it cannot overflow.
	shrunkCost := exactPutCost(t, "k", "v")
	delta := shrunkCost - before
	if delta >= 0 {
		t.Fatalf("the fixture does not shrink the write set: %d -> %d", before, shrunkCost)
	}
	tx.stagedBytes = math.MaxInt64
	if err := tx.Put(visibilitySpace, []byte("k"), []byte("v")); err != nil {
		t.Fatalf("shrinking overwrite at the ceiling failed: %v", err)
	}
	if tx.stagedBytes != math.MaxInt64+delta {
		t.Fatalf("stagedBytes = %d, want %d", tx.stagedBytes, int64(math.MaxInt64+delta))
	}

	tx.stagedBytes = before
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
}

// The configured resource parameters are part of the contract and must not drift.
func TestResourceParametersUnchanged(t *testing.T) {
	if MaxChunkBytes != 64<<10 {
		t.Fatalf("MaxChunkBytes = %d", MaxChunkBytes)
	}
	if MaxValueBytes != 48<<10 {
		t.Fatalf("MaxValueBytes = %d", MaxValueBytes)
	}
	if writeBufferBytes != 128<<10 {
		t.Fatalf("writeBufferBytes = %d", writeBufferBytes)
	}
	if localInstallBytes != 256<<10 {
		t.Fatalf("localInstallBytes = %d", localInstallBytes)
	}

	s := baselineStore(t)
	if err := validateCommand(Command{ID: "x", Ops: []Op{{Space: visibilitySpace, Key: []byte("k"), Value: make([]byte, MaxValueBytes+1)}}}); err == nil {
		t.Fatal("a value above MaxValueBytes was accepted")
	}
	// Fill one chunk past MaxChunkBytes with several bounded operations.
	var ops []Op
	for size := 0; size <= MaxChunkBytes; size += 512 {
		ops = append(ops, Op{Space: visibilitySpace, Key: []byte(fmt.Sprintf("k%05d", size)), Value: make([]byte, 512)})
	}
	if err := validateCommand(Command{ID: "x", Ops: ops}); err == nil {
		t.Fatal("a chunk above MaxChunkBytes was accepted")
	}
	if err := validateCommand(Command{ID: "x", Ops: []Op{{Space: visibilitySpace, Key: []byte("k"), Value: make([]byte, MaxValueBytes)}}}); err != nil {
		t.Fatalf("a value exactly at MaxValueBytes was rejected: %v", err)
	}
	_ = s
}
