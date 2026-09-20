package mvcc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// B01 baseline: the transaction invariants every later refactor must preserve.
//
// These are characterization tests. They pin today's behaviour; they do not ask
// for new behaviour, and they do not need the Transaction Manager to exist.
// They are the executable half of specs/001-b01-mvcc-transaction-manager/spec.md
// §6 (INV-001..INV-008) plus the parent/child and publication boundaries of §5.
//
// A case that the spec has since decided is no longer a characterization: it is
// named as the contract it now is, and a change to it has to change the spec first
// (for example TestDependencyOnlyCommitPublishesRevision).
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

// installedDataVersions counts the data versions installed at one revision, in both
// physical layouts, so a test can prove that a publication installed none.
func installedDataVersions(t *testing.T, s *Store, revision uint64) int {
	t.Helper()
	installed := 0
	if err := s.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(dataBucket).ForEach(func(k, _ []byte) error {
			if rows := tx.Bucket(dataBucket).Bucket(k); rows != nil && rows.Get(sequence(revision)) != nil {
				installed++
			}
			return nil
		}); err != nil {
			return err
		}
		flat := tx.Bucket(flatVersionsBucket)
		if flat == nil {
			return nil
		}
		return flat.ForEach(func(k, _ []byte) error {
			_, version, err := splitFlatVersionKey(k)
			if err != nil {
				return err
			}
			if number(version) == revision {
				installed++
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return installed
}

// The decided contract for a dependency-only root transaction: Guard/GuardRange and no
// Put/Delete (spec.md FR-004, FR-005, §7 "事务结束" and §9 Clarifications).
//
// Guard and GuardRange are explicit commit-time validation dependencies, not ordinary
// read observations, so they make the transaction publishable even though they install
// no value. Such a root therefore moves ACTIVE -> COMMITTING -> COMMITTED, records a
// durable commit revision and advances the head, while installing zero data versions.
// HasCommitTS reports "this root has a durable commit revision"; it never promises that
// the transaction installed data. This is a contract test, not a characterization: the
// case is decided, so a change to it must change the spec first.
func TestDependencyOnlyCommitPublishesRevision(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	mustPut(t, seed, "k", "v")
	mustCommit(t, seed)
	before := mustHead(t, s)

	// A second transaction shares the pre-revision snapshot and guards the same key, so
	// the revision this test publishes is observed from before it exists.
	concurrent := beginBaseline(t, s)
	if err := concurrent.Guard(baselineSpace, []byte("absent")); err != nil {
		t.Fatal(err)
	}

	guarded := beginBaseline(t, s)
	if err := guarded.Guard(baselineSpace, []byte("absent")); err != nil {
		t.Fatal(err)
	}
	if err := guarded.GuardRange(baselineSpace, KeyRange{Lower: []byte("a"), Upper: []byte("z")}); err != nil {
		t.Fatal(err)
	}
	revision, err := guarded.Commit(context.Background())
	if err != nil {
		t.Fatalf("dependency-only commit: %v", err)
	}

	// Head advances, the revision is the published one, and the transaction is a
	// COMMITTED root with a durable commit revision.
	after := mustHead(t, s)
	if after <= before {
		t.Fatalf("dependency-only commit did not advance the head: %d -> %d", before, after)
	}
	if revision != after {
		t.Fatalf("dependency-only commit revision %d does not equal the published head %d", revision, after)
	}
	info := guarded.Info()
	if info.State != TransactionCommitted {
		t.Fatalf("state = %s, want COMMITTED", info.State)
	}
	if !info.HasCommitTS || info.CommitTS != revision {
		t.Fatalf("lifecycle = %+v, want a commit revision of %d", info, revision)
	}
	if published, committed := s.Committed(guarded.ID); !committed || published != revision {
		t.Fatalf("publication marker = (%d, %v), want (%d, true)", published, committed, revision)
	}
	// The dependency set is publishable work, but it is not a data mutation: a guard is
	// a dependency rather than a write, so it contributes no write count and no write
	// bytes, and only the dependency counters move. HasCommitTS therefore cannot be read
	// as "this transaction installed data".
	if info.Writes != 0 || info.WriteBytes != 0 {
		t.Fatalf("dependency-only write set = (%d writes, %d bytes), want none", info.Writes, info.WriteBytes)
	}
	if info.PointDependencies != 1 || info.RangeDependencies != 1 {
		t.Fatalf("dependency counters = %+v, want one point and one range dependency", info)
	}

	// The revision exists; no data version does.
	if installed := installedDataVersions(t, s, revision); installed != 0 {
		t.Fatalf("dependency-only commit installed %d data versions at revision %d", installed, revision)
	}

	// An ordinary reader at that revision sees exactly the committed data: a dependency
	// revision never fabricates a row for the key it guarded.
	reader := beginBaseline(t, s)
	if reader.Snapshot != after {
		t.Fatalf("fresh reader snapshot = %d, want the published revision %d", reader.Snapshot, after)
	}
	requireValue(t, reader, "k", "v")
	requireMissing(t, reader, "absent")

	// A dependency-only publication is a revision rather than a data change, so it does
	// not by itself invalidate a guard that was taken before it.
	if _, err := concurrent.Commit(context.Background()); err != nil {
		t.Fatalf("guard taken before a dependency-only revision failed: %v", err)
	}

	// Dependency validation still runs: a guard-only transaction whose guarded key was
	// written after its snapshot must fail on commit.
	contended := beginBaseline(t, s)
	if err := contended.Guard(baselineSpace, []byte("contended")); err != nil {
		t.Fatal(err)
	}
	writer := beginBaseline(t, s)
	mustPut(t, writer, "contended", "value")
	mustCommit(t, writer)
	if _, err := contended.Commit(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("dependency-only commit over a newer write = %v, want ErrConflict", err)
	}
	if info := contended.Info(); info.State != TransactionAborted || info.AbortReason != "conflict" {
		t.Fatalf("conflicted dependency-only outcome = %+v", info)
	}
	if info := contended.Info(); info.HasCommitTS {
		t.Fatalf("a conflicted dependency-only commit kept a commit revision: %+v", info)
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
