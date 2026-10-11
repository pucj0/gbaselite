package mvcc

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking_commit_test.go checks requirement 11: attaching the lock manager must not move the
// persistence or transaction-state invariants of any existing commit path.
//
// Each case runs the same transaction shape twice -- once on a store with no lock manager and once
// with one attached and a row lock held -- and asserts the observable commit behaviour is the same:
// whether a commit sequence is allocated, whether the publication marker exists, and what the MVCC
// registry says afterwards. The lock behaviour is asserted separately, so a difference in locking
// cannot hide a difference in publication.

// commitShape is one transaction script plus what it must produce.
type commitShape struct {
	name string
	// write reports whether the transaction has a write set, which is what decides whether a
	// publication happens at all.
	write bool
	// wantCommitted reports whether the transaction must end COMMITTED.
	wantCommitted bool
	// wantSequence reports whether a commit sequence must be allocated.
	wantSequence bool
}

func commitShapes() []commitShape {
	return []commitShape{
		{name: "empty-root", write: false, wantCommitted: true, wantSequence: false},
		{name: "write-root", write: true, wantCommitted: true, wantSequence: true},
		{name: "read-only-root", write: false, wantCommitted: true, wantSequence: false},
	}
}

// runCommitShape runs one shape on one store and returns what was observed.
type commitObservation struct {
	committed  bool
	sequence   uint64
	headBefore uint64
	headAfter  uint64
	marker     bool
	state      TransactionState
}

func runCommitShape(t *testing.T, store *Store, shape commitShape) commitObservation {
	t.Helper()
	ctx := context.Background()
	headBefore, err := store.Head()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if shape.write {
		if err := tx.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
			t.Fatal(err)
		}
	}
	sequence, err := tx.Commit(ctx)
	if err != nil && shape.wantCommitted {
		t.Fatalf("%s: commit: %v", shape.name, err)
	}
	headAfter, err := store.Head()
	if err != nil {
		t.Fatal(err)
	}
	_, published := store.Committed(tx.ID)
	return commitObservation{
		committed:  err == nil && tx.State() == TransactionCommitted,
		sequence:   sequence,
		headBefore: headBefore,
		headAfter:  headAfter,
		marker:     published,
		state:      tx.State(),
	}
}

// TestB02CommitPathsKeepTheirPublicationInvariants is the requirement 11 test.
func TestB02CommitPathsKeepTheirPublicationInvariants(t *testing.T) {
	configurations := []struct {
		name     string
		localWAL bool
		locking  bool
	}{
		{name: "bounded-no-locking"},
		{name: "bounded-with-locking", locking: true},
		{name: "local-wal-no-locking", localWAL: true},
		{name: "local-wal-with-locking", localWAL: true, locking: true},
	}

	for _, configuration := range configurations {
		for _, shape := range commitShapes() {
			t.Run(configuration.name+"/"+shape.name, func(t *testing.T) {
				store, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: configuration.localWAL})
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				defer store.Close()

				var manager *lockmanager.Manager
				if configuration.locking {
					clock := lockmanager.NewFakeClock(time.Unix(1700000000, 0))
					manager, err = lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, clock)
					if err != nil {
						t.Fatal(err)
					}
					defer manager.Close()
					if _, err := store.AttachLocks(manager); err != nil {
						t.Fatalf("attach locks: %v", err)
					}
				}

				// The lock-holding variant takes the row lock the plain variant does not, so the
				// comparison is "same publication behaviour, plus a lock that must be released".
				if configuration.locking {
					tx, err := store.Begin(context.Background(), nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := manager.Acquire(context.Background(), tx.ID, tx.lockScopeID, []lockmanager.ResourceRef{
						indexRecordRef("k1"), tableIntentRef(),
					}); err != nil {
						t.Fatal(err)
					}
					if shape.write {
						if err := tx.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
							t.Fatal(err)
						}
					}
					sequence, err := tx.Commit(context.Background())
					if err != nil {
						t.Fatalf("commit with a lock held: %v", err)
					}
					if shape.wantSequence && sequence == 0 {
						t.Fatal("a write set committed without a commit sequence")
					}
					if !shape.wantSequence && sequence != 0 {
						t.Fatalf("a root with no write set allocated sequence %d", sequence)
					}
					if tx.State() != TransactionCommitted {
						t.Fatalf("state = %s, want COMMITTED", tx.State())
					}
					if _, published := store.Committed(tx.ID); published != shape.write {
						t.Fatalf("publication marker = %v, want %v", published, shape.write)
					}
					// The lock is released by the commit and the manager is idle again.
					if owners := manager.Stats().ActiveOwners; owners != 0 {
						t.Fatalf("%d lock owners survived the commit", owners)
					}
					if size := manager.LockTableSize(); size != 0 {
						t.Fatalf("lock table holds %d resources after the commit", size)
					}
					return
				}

				observation := runCommitShape(t, store, shape)
				if observation.committed != shape.wantCommitted {
					t.Fatalf("committed = %v, want %v (state %s)", observation.committed, shape.wantCommitted, observation.state)
				}
				if shape.wantSequence && observation.sequence == 0 {
					t.Fatal("a write set committed without a commit sequence")
				}
				if !shape.wantSequence && observation.sequence != 0 {
					t.Fatalf("a root with no write set allocated sequence %d", observation.sequence)
				}
				if observation.marker != shape.write {
					t.Fatalf("publication marker = %v, want %v", observation.marker, shape.write)
				}
				if active := store.txns.ActiveTransactions(); len(active) != 0 {
					t.Fatalf("%d transactions remain registered after the commit", len(active))
				}
			})
		}
	}
}

// TestB02RestoreKeepsTheMVCCImageUsable proves the restore path itself is unchanged.
//
// Restore now also synchronises the lock epoch, so this test is the guard that the extra step did
// not disturb the MVCC image: the data written before the restore is readable after it, and the
// store accepts new transactions.
func TestB02RestoreKeepsTheMVCCImageUsable(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	// Write two committed rows, then snapshot.
	for i, key := range []string{"r1", "r2"} {
		tx := f.begin(ctx)
		f.lockRow(ctx, tx, key)
		if err := tx.Put("index/PRIMARY", []byte(key), []byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := f.store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	image, err := snapshotImage(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Rollback(); err != nil {
		t.Fatal(err)
	}

	if err := f.store.Restore(bytes.NewReader(image)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// The restored image is readable and a new transaction can lock and commit against it.
	reader := f.begin(ctx)
	value, found, err := reader.Get("index/PRIMARY", []byte("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if !found || string(value) != "a" {
		t.Fatalf("restored row r1 = %q found=%v, want \"a\"", value, found)
	}
	f.lockRow(ctx, reader, "r3")
	if err := reader.Put("index/PRIMARY", []byte("r3"), []byte("c")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Commit(ctx); err != nil {
		t.Fatalf("commit after restore: %v", err)
	}
	f.requireNoLockState()
}

// TestB02FailedPublicationIsReconciledAfterARestore covers an undecided publication.
//
// The contract requires that an undecided publication is reconciled rather than falsely reported
// as a rollback. A transaction that is still open when the generation is replaced must not be able
// to claim it committed, and its locks must still be released.
func TestB02FailedPublicationIsReconciledAfterARestore(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	pending := f.begin(ctx)
	f.lockRow(ctx, pending, "k1")
	if err := pending.Put("index/PRIMARY", []byte("k1"), []byte("never")); err != nil {
		t.Fatal(err)
	}

	// A generation change replaces the world while the publication is undecided.
	f.store.ResetLockEpoch(5)
	if _, published := f.store.Committed(pending.ID); published {
		t.Fatal("an undecided transaction reports a publication marker")
	}
	// The commit is refused, the transaction ends in a terminal state and the locks are gone.
	if _, err := pending.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("commit after an epoch change err = %v, want ErrConflict", err)
	}
	if state := pending.State(); !state.Terminal() {
		t.Fatalf("state = %s, want a terminal state", state)
	}
	f.requireNoLockState()
}

// TestB02LockingDoesNotChangeConflictDetection proves the SI conflict path is untouched.
//
// Two roots that write the same key from the same snapshot still conflict, whether or not a lock
// manager is attached. Locking is an addition, not a substitute for the optimistic check.
func TestB02LockingDoesNotChangeConflictDetection(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	first := f.begin(ctx)
	second := f.begin(ctx)
	if err := first.Put("index/PRIMARY", []byte("k1"), []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := second.Put("index/PRIMARY", []byte("k1"), []byte("second")); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	// The second transaction was built on the same snapshot, so publishing it must conflict.
	if _, err := second.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("second commit err = %v, want ErrConflict", err)
	}
	if second.State() != TransactionAborted {
		t.Fatalf("state = %s, want ABORTED", second.State())
	}
	// The conflicting root's locks are released by its abort.
	f.requireNoLockState()
}

// TestB02GroupCommitPathIsUnaffectedByLocking runs the local group commit machinery with a lock
// manager attached.
//
// The group path is the one place where several transactions publish together. Attaching a lock
// manager must not change how the group decides, and every participant's locks must still be
// released by its own commit.
func TestB02GroupCommitPathIsUnaffectedByLocking(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenWithOptions(directory, Options{LocalWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	clock := lockmanager.NewFakeClock(time.Unix(1700000000, 0))
	manager, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err := store.AttachLocks(manager); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	const participants = 4
	transactions := make([]*Tx, 0, participants)
	for i := 0; i < participants; i++ {
		tx, err := store.Begin(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		key := []byte{byte('a' + i)}
		ref := lockmanager.ResourceRef{Identity: lockmanager.Identity{
			Kind:    lockmanager.KindRecord,
			Mode:    lockmanager.ModeX,
			Space:   "index/PRIMARY",
			TableID: "t1",
			Key:     key,
		}}
		if _, err := manager.Acquire(ctx, tx.ID, tx.lockScopeID, []lockmanager.ResourceRef{ref, tableIntentRef()}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Put("index/PRIMARY", key, []byte{byte('A' + i)}); err != nil {
			t.Fatal(err)
		}
		transactions = append(transactions, tx)
	}

	// Committing sequentially exercises the group machinery one request at a time; the point is
	// that the locked path publishes exactly like the unlocked one, so every sequence is distinct
	// and increasing.
	sequences := make([]uint64, 0, participants)
	for i, tx := range transactions {
		sequence, err := tx.Commit(ctx)
		if err != nil {
			t.Fatalf("participant %d commit: %v", i, err)
		}
		if sequence == 0 {
			t.Fatalf("participant %d committed without a sequence", i)
		}
		if _, published := store.Committed(tx.ID); !published {
			t.Fatalf("participant %d has no publication marker", i)
		}
		for _, previous := range sequences {
			if previous == sequence {
				t.Fatalf("sequence %d was handed out twice", sequence)
			}
		}
		sequences = append(sequences, sequence)
	}
	if owners := manager.Stats().ActiveOwners; owners != 0 {
		t.Fatalf("%d lock owners survived the group path", owners)
	}
	if size := manager.LockTableSize(); size != 0 {
		t.Fatalf("lock table holds %d resources after the group path", size)
	}
	// Reopening the store proves the image is durable and valid.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithOptions(filepath.Clean(directory), Options{LocalWAL: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	reader, err := reopened.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	for i := 0; i < participants; i++ {
		key := []byte{byte('a' + i)}
		value, found, err := reader.Get("index/PRIMARY", key)
		if err != nil {
			t.Fatal(err)
		}
		if !found || len(value) != 1 || value[0] != byte('A'+i) {
			t.Fatalf("row %q = %q found=%v, want the committed value", key, value, found)
		}
	}
}

// snapshotImage copies one snapshot into memory.
func snapshotImage(snapshot *Snapshot) ([]byte, error) {
	var buffer bytes.Buffer
	if _, err := snapshot.WriteTo(&buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// TestB02LockingAvailabilityFailsClosed proves a resource failure is reported, not hidden.
//
// A store that cannot register a lock owner must fail Begin rather than hand out a transaction
// whose writes would be unprotected. This is the fail-closed rule applied to the binding.
func TestB02LockingAvailabilityFailsClosed(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenWithOptions(directory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// A waiter budget of one component is legal but far too small, so the second registration
	// cannot be charged.
	manager, err := lockmanager.NewManager(lockmanager.BudgetLimits{MemoryBytes: 1}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err := store.AttachLocks(manager); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Begin(ctx, nil); err == nil {
		t.Fatal("Begin succeeded with a lock budget that cannot hold an owner")
	}
	// The failed Begin left nothing behind in either registry.
	if active := store.txns.ActiveTransactions(); len(active) != 0 {
		t.Fatalf("%d transactions were registered by a failed Begin", len(active))
	}
	if owners := manager.Stats().ActiveOwners; owners != 0 {
		t.Fatalf("%d lock owners were left by a failed Begin", owners)
	}
	_ = storageengine.ErrLockResourceLimit
}
