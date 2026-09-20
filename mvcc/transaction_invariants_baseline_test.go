package mvcc

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// B01 baseline: the transaction invariants every later refactor must preserve.
//
// These are characterization tests. They pin today's behaviour; they do not ask
// for new behaviour, and they do not need the Transaction Manager to exist.
// They are the executable half of specs/001-b01-mvcc-transaction-manager/spec.md
// §6 (INV-001..INV-008) plus the parent/child and publication boundaries of §5.
//
// Every test here synchronizes on channels or a protocol gate; none of them uses
// a sleep to decide ordering.
const baselineSpace = "invariant"

func baselineStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func beginBaseline(t *testing.T, s *Store) *Tx {
	t.Helper()
	tx, err := s.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

func mustPut(t *testing.T, tx *Tx, key, value string) {
	t.Helper()
	if err := tx.Put(baselineSpace, []byte(key), []byte(value)); err != nil {
		t.Fatal(err)
	}
}

func mustCommit(t *testing.T, tx *Tx) uint64 {
	t.Helper()
	sequence, err := tx.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sequence
}

func mustHead(t *testing.T, s *Store) uint64 {
	t.Helper()
	head, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func requireValue(t *testing.T, tx *Tx, key, want string) {
	t.Helper()
	value, ok, err := tx.Get(baselineSpace, []byte(key))
	if err != nil || !ok || string(value) != want {
		t.Fatalf("Get(%q) = %q, %v, %v; want %q", key, value, ok, err, want)
	}
}

// requireMissing asserts the read contract: existence is reported by ok, never by
// the value. A caller's own tombstone comes back as a zero-length slice with
// ok=false, so the length check (not a nil check) is the correct assertion.
func requireMissing(t *testing.T, tx *Tx, key string) {
	t.Helper()
	value, ok, err := tx.Get(baselineSpace, []byte(key))
	if err != nil || ok || len(value) != 0 {
		t.Fatalf("Get(%q) = %q, %v, %v; want missing", key, value, ok, err)
	}
}

// INV-001: ReadTS is fixed from Begin to termination, and a transaction never
// observes a commit that landed after its own snapshot.
func TestBaselineSnapshotIsFixedWithinTransaction(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	mustPut(t, seed, "k", "v1")
	mustCommit(t, seed)

	reader := beginBaseline(t, s)
	readTS := reader.Snapshot
	requireValue(t, reader, "k", "v1")

	writer := beginBaseline(t, s)
	mustPut(t, writer, "k", "v2")
	mustCommit(t, writer)

	if reader.Snapshot != readTS {
		t.Fatalf("read timestamp moved inside the transaction: %d -> %d", readTS, reader.Snapshot)
	}
	requireValue(t, reader, "k", "v1")

	fresh := beginBaseline(t, s)
	if fresh.Snapshot < readTS {
		t.Fatalf("new snapshot %d went backwards from %d", fresh.Snapshot, readTS)
	}
	requireValue(t, fresh, "k", "v2")
}

// INV-003: read-your-writes, including replacement and tombstone, and the same
// writes stay invisible to a concurrent transaction.
func TestBaselineReadYourWrites(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	requireMissing(t, tx, "k")

	mustPut(t, tx, "k", "own")
	requireValue(t, tx, "k", "own")
	mustPut(t, tx, "k", "replaced")
	requireValue(t, tx, "k", "replaced")

	if err := tx.Delete(baselineSpace, []byte("k")); err != nil {
		t.Fatal(err)
	}
	requireMissing(t, tx, "k")

	other := beginBaseline(t, s)
	requireMissing(t, other, "k")
}

// INV-004: no dirty reads. An uncommitted write set is invisible through a
// snapshot transaction, through the newest-revision read, and after the writer
// commits it stays invisible to a snapshot taken before that commit.
func TestBaselineNoDirtyRead(t *testing.T) {
	s := baselineStore(t)
	writer := beginBaseline(t, s)
	mustPut(t, writer, "k", "uncommitted")

	other := beginBaseline(t, s)
	before := mustHead(t, s)
	requireMissing(t, other, "k")
	if _, _, ok, err := s.Get(^uint64(0), baselineSpace, []byte("k")); err != nil || ok {
		t.Fatalf("newest-revision read saw uncommitted data: ok=%v err=%v", ok, err)
	}

	sequence := mustCommit(t, writer)
	if sequence <= before {
		t.Fatalf("write commit did not advance past head %d: %d", before, sequence)
	}
	if head := mustHead(t, s); head != sequence {
		t.Fatalf("head %d does not equal the commit sequence %d", head, sequence)
	}
	requireMissing(t, other, "k")

	fresh := beginBaseline(t, s)
	requireValue(t, fresh, "k", "uncommitted")
}

// INV-006 (first half): a child reads its ancestors' writes, its own write wins
// over the parent's, and the parent does not see the child before the merge.
func TestBaselineChildReadsParentWrites(t *testing.T) {
	s := baselineStore(t)
	parent := beginBaseline(t, s)
	mustPut(t, parent, "k", "parent")

	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Rollback() })
	requireValue(t, child, "k", "parent")

	grandchild, err := child.Child()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { grandchild.Rollback() })
	requireValue(t, grandchild, "k", "parent")

	mustPut(t, child, "k", "child")
	requireValue(t, child, "k", "child")
	requireValue(t, parent, "k", "parent")
}

// INV-006 (second half): an unmerged child is invisible to the parent through
// both point reads and scans, and becomes visible exactly on merge.
func TestBaselineParentCannotSeeUnmergedChild(t *testing.T) {
	s := baselineStore(t)
	parent := beginBaseline(t, s)
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	defer child.Rollback()
	mustPut(t, child, "k", "child")

	requireMissing(t, parent, "k")
	observed := 0
	if err := parent.Scan(context.Background(), baselineSpace, func(_, _ []byte) error { observed++; return nil }); err != nil {
		t.Fatal(err)
	}
	if observed != 0 {
		t.Fatalf("parent scan observed %d unmerged child rows", observed)
	}
	if _, err := child.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireValue(t, parent, "k", "child")
}

// INV-008 / D5: the durable publication marker is the commit point. While the
// write set is staged but unpublished, no reader may observe it and the head
// must not move. The gate parks the commit proposal, so the assertion is made at
// an exact protocol boundary rather than after an arbitrary delay.
func TestBaselinePublicationMarkerIsCommitPoint(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	mustPut(t, seed, "seed", "1")
	mustCommit(t, seed)
	before := mustHead(t, s)

	gate := newPublicationGate(s, "commit")
	t.Cleanup(gate.releaseNow)
	writer, err := s.Begin(context.Background(), gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Rollback() })
	mustPut(t, writer, "k", "pending")

	done := make(chan commitOutcome, 1)
	go func() {
		sequence, commitErr := writer.Commit(context.Background())
		done <- commitOutcome{sequence: sequence, err: commitErr}
	}()
	awaitSignal(t, gate.parked, "the commit proposal")

	if pending, err := s.PendingBytes(); err != nil || pending == 0 {
		t.Fatalf("expected a durable staged write set at the commit boundary: pending=%d err=%v", pending, err)
	}
	if head := mustHead(t, s); head != before {
		t.Fatalf("head moved to %d before the publication marker (%d)", head, before)
	}
	observer := beginBaseline(t, s)
	requireMissing(t, observer, "k")
	if _, _, ok, err := s.Get(^uint64(0), baselineSpace, []byte("k")); err != nil || ok {
		t.Fatalf("newest revision saw an unpublished version: ok=%v err=%v", ok, err)
	}

	gate.releaseNow()
	outcome := awaitCommit(t, done)
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.sequence <= before {
		t.Fatalf("published sequence %d did not advance past head %d", outcome.sequence, before)
	}
	if head := mustHead(t, s); head != outcome.sequence {
		t.Fatalf("head %d does not equal the published sequence %d", head, outcome.sequence)
	}
	reader := beginBaseline(t, s)
	requireValue(t, reader, "k", "pending")
	requireMissing(t, observer, "k")
}

// US6#1: cancelling before publication must leave nothing visible, must not move
// the head, and must release the staged write set.
func TestBaselineCancelBeforePublicationNeverBecomesVisible(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	mustPut(t, seed, "k", "committed")
	mustCommit(t, seed)
	before := mustHead(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gate := newPublicationGate(s, "commit")
	t.Cleanup(gate.releaseNow)
	writer, err := s.Begin(ctx, gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Rollback() })
	mustPut(t, writer, "pending", "canceled")

	done := make(chan commitOutcome, 1)
	go func() {
		sequence, commitErr := writer.Commit(ctx)
		done <- commitOutcome{sequence: sequence, err: commitErr}
	}()
	awaitSignal(t, gate.parked, "the commit proposal")
	cancel()

	outcome := awaitCommit(t, done)
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("commit error = %v, want context.Canceled", outcome.err)
	}
	if outcome.sequence != 0 {
		t.Fatalf("canceled commit reported sequence %d", outcome.sequence)
	}
	if head := mustHead(t, s); head != before {
		t.Fatalf("canceled commit moved head %d -> %d", before, head)
	}
	observer := beginBaseline(t, s)
	requireMissing(t, observer, "pending")
	requireValue(t, observer, "k", "committed")
	if pending, err := s.PendingBytes(); err != nil || pending != 0 {
		t.Fatalf("canceled commit retained a staged write set: pending=%d err=%v", pending, err)
	}
}

// INV-005: same logical key after the snapshot conflicts; disjoint keys from the
// same snapshot still commit. The last block pins that ordinary reads never
// become commit dependencies, so write skew stays allowed under SI (US4#8).
func TestBaselineConflictMatrixStaysSnapshotIsolation(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	mustPut(t, seed, "shared", "v0")
	mustCommit(t, seed)

	first := beginBaseline(t, s)
	second := beginBaseline(t, s)
	mustPut(t, first, "shared", "first")
	mustPut(t, second, "shared", "second")
	mustCommit(t, first)
	if _, err := second.Commit(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("second writer error = %v, want ErrConflict", err)
	}
	reader := beginBaseline(t, s)
	requireValue(t, reader, "shared", "first")

	left := beginBaseline(t, s)
	right := beginBaseline(t, s)
	mustPut(t, left, "left", "a")
	mustPut(t, right, "right", "b")
	mustCommit(t, left)
	mustCommit(t, right)
	after := beginBaseline(t, s)
	requireValue(t, after, "left", "a")
	requireValue(t, after, "right", "b")

	// Write skew: both transactions read the other's key and write their own.
	// Without Guard/GuardRange this must remain allowed -- SI, not Serializable.
	skewA := beginBaseline(t, s)
	skewB := beginBaseline(t, s)
	if _, _, err := skewA.Get(baselineSpace, []byte("right")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skewB.Get(baselineSpace, []byte("left")); err != nil {
		t.Fatal(err)
	}
	mustPut(t, skewA, "skew-a", "1")
	mustPut(t, skewB, "skew-b", "1")
	mustCommit(t, skewA)
	mustCommit(t, skewB)
}

// Guard/GuardRange are the only commit-time dependencies: a point guard detects
// a same-key change and a bounded range guard detects inside its bounds only.
// Both durability paths must agree (SC-006).
func TestBaselineGuardAndRangeGuardDetectDependencies(t *testing.T) {
	for _, wal := range []bool{false, true} {
		t.Run(fmt.Sprintf("wal=%v", wal), func(t *testing.T) {
			newStore := func(t *testing.T) *Store {
				t.Helper()
				s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: wal})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				return s
			}

			t.Run("point", func(t *testing.T) {
				s := newStore(t)
				guard := beginBaseline(t, s)
				if err := guard.Guard(baselineSpace, []byte("missing")); err != nil {
					t.Fatal(err)
				}
				writer := beginBaseline(t, s)
				mustPut(t, writer, "missing", "v")
				mustCommit(t, writer)
				if _, err := guard.Commit(context.Background()); !errors.Is(err, ErrConflict) {
					t.Fatalf("point guard error = %v, want ErrConflict", err)
				}
			})

			bounds := KeyRange{Lower: []byte("b"), Upper: []byte("d"), LowerInclusive: true, UpperInclusive: true}
			t.Run("range-inside", func(t *testing.T) {
				s := newStore(t)
				guard := beginBaseline(t, s)
				if err := guard.GuardRange(baselineSpace, bounds); err != nil {
					t.Fatal(err)
				}
				writer := beginBaseline(t, s)
				mustPut(t, writer, "c", "phantom")
				mustCommit(t, writer)
				if _, err := guard.Commit(context.Background()); !errors.Is(err, ErrConflict) {
					t.Fatalf("range guard error = %v, want ErrConflict", err)
				}
			})

			t.Run("range-outside", func(t *testing.T) {
				s := newStore(t)
				guard := beginBaseline(t, s)
				if err := guard.GuardRange(baselineSpace, bounds); err != nil {
					t.Fatal(err)
				}
				writer := beginBaseline(t, s)
				mustPut(t, writer, "z", "outside")
				mustCommit(t, writer)
				if _, err := guard.Commit(context.Background()); err != nil {
					t.Fatalf("range guard rejected an out-of-range change: %v", err)
				}
			})
		})
	}
}

// spec.md §7: an empty transaction and a read-only transaction commit
// successfully without allocating a version or moving the head.
func TestBaselineEmptyAndReadOnlyCommitDoNotAdvanceHead(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	mustPut(t, seed, "k", "v")
	mustCommit(t, seed)
	before := mustHead(t, s)

	empty := beginBaseline(t, s)
	if _, err := empty.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if head := mustHead(t, s); head != before {
		t.Fatalf("empty commit moved head %d -> %d", before, head)
	}

	readOnly := beginBaseline(t, s)
	requireValue(t, readOnly, "k", "v")
	if _, err := readOnly.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if head := mustHead(t, s); head != before {
		t.Fatalf("read-only commit moved head %d -> %d", before, head)
	}
}

// Characterization, not an endorsement. Today a guard-only transaction is
// treated as a write transaction: it allocates a version and moves the head even
// though it installs no row version. spec.md §7 names only "empty" and
// "read-only" commits, so B01 has to decide this case explicitly; this test
// makes the current behaviour visible and fails loudly if it changes.
func TestBaselineGuardOnlyCommitCurrentlyAdvancesHead(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	mustPut(t, seed, "k", "v")
	mustCommit(t, seed)
	before := mustHead(t, s)

	guarded := beginBaseline(t, s)
	if err := guarded.Guard(baselineSpace, []byte("absent")); err != nil {
		t.Fatal(err)
	}
	sequence, err := guarded.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	after := mustHead(t, s)
	if after == before {
		t.Fatalf("guard-only commit no longer advances the head (%d -> %d); B01 must decide this case and update this characterization", before, after)
	}
	if sequence != after {
		t.Fatalf("guard-only commit sequence %d does not equal the published head %d", sequence, after)
	}
}

// FR-008 boundaries that B01 must not disturb: the configured write-set limit is
// inclusive, limit+1 fails, and a failing write never becomes visible.
func TestBaselineWriteSetLimitBoundaryIsInclusive(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	mustPut(t, tx, "a", "value")

	// One more byte than the budget must fail closed.
	s.writeSetLimit = tx.stagedBytes
	if err := tx.Put(baselineSpace, []byte("b"), []byte("x")); !errors.Is(err, ErrWriteSetLimit) {
		t.Fatalf("limit+1 error = %v, want ErrWriteSetLimit", err)
	}
	requireMissing(t, tx, "b")
	requireValue(t, tx, "a", "value")

	// Landing exactly on the budget must still succeed. The staged cost of one
	// write is len(encodedKey)+len(encodedOp), the same accounting the store uses.
	tx2 := beginBaseline(t, s)
	encodedKey, err := key(baselineSpace, []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	encodedOp := encodeStagedOp(Op{Space: baselineSpace, Key: []byte("b"), Value: []byte("y")})
	s.writeSetLimit = int64(len(encodedKey) + len(encodedOp))
	mustPut(t, tx2, "b", "y")
	if tx2.stagedBytes != s.writeSetLimit {
		t.Fatalf("staged bytes %d do not sit exactly on the limit %d", tx2.stagedBytes, s.writeSetLimit)
	}
	if _, err := tx2.Commit(context.Background()); err != nil {
		t.Fatalf("exact-limit commit failed: %v", err)
	}
}
