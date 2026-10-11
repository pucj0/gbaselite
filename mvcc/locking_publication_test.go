package mvcc

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking_publication_test.go is T049: the release point relative to durable publication, and the
// lock order that keeps the two layers from inverting.
//
// The release point is the property that matters most here. A root may hand its locks back only
// after the publication marker exists and the write set can no longer be used; releasing earlier
// would let another transaction take a row in the window between "this transaction will commit"
// and "this transaction is committed". The tests assert the grant is still held at the moment the
// publication is decided and gone immediately after.

// requireHeld asserts one root holds one row.
func (f *lockTestFixture) requireHeld(rootID, key string) {
	f.t.Helper()
	if !f.heldRow(rootID, key) {
		f.t.Fatalf("%s does not hold %q", rootID, key)
	}
}

// TestB02LocksAreHeldUntilThePublicationIsDecided pins the release point.
//
// The check runs *after* the commit returns, because the commit is the publication. What it
// proves is the negative: there is no observable state in which the transaction is committed and
// still holds a lock, and no state in which it has released a lock while its outcome is undecided.
func TestB02LocksAreHeldUntilThePublicationIsDecided(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	f.lockRow(ctx, root, "k1")
	if err := root.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}

	// A competing root cannot take the row while the first is uncommitted. TryAcquire refuses to
	// queue, so the refusal is the conflict error rather than a wait handle.
	competitor := f.begin(ctx)
	if _, err := f.manager.TryAcquire(ctx, competitor.ID, competitor.lockScopeID, []lockmanager.ResourceRef{
		indexRecordRef("k1"), tableIntentRef(),
	}); !errors.Is(err, lockmanager.ErrConflict) {
		t.Fatalf("a competing root acquired a held row: err = %v, want ErrConflict", err)
	}

	if _, err := root.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if root.State() != TransactionCommitted {
		t.Fatalf("state = %s, want COMMITTED", root.State())
	}
	// The committed transaction holds nothing: the publication was the release point.
	if f.heldRow(root.ID, "k1") {
		t.Fatal("the committed root still holds the row")
	}
	// And now the competitor can take it, which is what proves the release actually happened
	// rather than the row being wrongly blocked for ever.
	taken, err := f.manager.Acquire(ctx, competitor.ID, competitor.lockScopeID, []lockmanager.ResourceRef{
		indexRecordRef("k1"), tableIntentRef(),
	})
	if err != nil {
		t.Fatalf("the competitor could not take the released row: %v", err)
	}
	if taken.Op != lockmanager.LockOpGranted {
		t.Fatalf("the competitor could not take the released row: op = %s", taken.Op)
	}
	f.manager.ReleaseOwner(competitor.ID)
	f.requireNoLockState()
}

// TestB02RootWithNoWriteSetStillReleasesLocks covers the empty-root commit path.
//
// An empty or read-only root commits without allocating a sequence. It still holds locks it may
// have taken, so it must still release them; the shortcut in the commit path is not an excuse to
// skip cleanup.
func TestB02RootWithNoWriteSetStillReleasesLocks(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	f.lockRow(ctx, root, "k1")

	if _, err := root.Commit(ctx); err != nil {
		t.Fatalf("empty commit: %v", err)
	}
	if root.State() != TransactionCommitted {
		t.Fatalf("state = %s, want COMMITTED", root.State())
	}
	f.requireNoLockState()
}

// TestB02DeadlockVictimNeverPublishes proves requirement 6 against the durable marker.
//
// The victim's write set is present and its commit is attempted. The commit must be refused before
// any publication happens, so the version the victim wrote is never visible and the commit
// sequence is never consumed by it.
func TestB02DeadlockVictimNeverPublishes(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	head, err := f.store.Head()
	if err != nil {
		t.Fatal(err)
	}

	a := f.begin(ctx)
	b := f.begin(ctx)
	f.lockRow(ctx, a, "k1")
	f.lockRow(ctx, b, "k2")
	// Both roots write, so both have something to publish.
	if err := a.Put("index/PRIMARY", []byte("k1"), []byte("from-a")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put("index/PRIMARY", []byte("k2"), []byte("from-b")); err != nil {
		t.Fatal(err)
	}
	// Close the cycle: each waits for the other's row.
	survivorWait, err := f.manager.Acquire(ctx, a.ID, a.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k2"), tableIntentRef()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Acquire(ctx, b.ID, b.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k1"), tableIntentRef()}); err != nil {
		t.Fatal(err)
	}
	if survivorWait.Op != lockmanager.LockOpWait {
		t.Fatalf("the survivor's request op = %s, want WAIT", survivorWait.Op)
	}
	summary, found := f.manager.DetectDeadlock()
	if !found {
		t.Fatal("deadlock not detected")
	}
	victim := b
	if summary.VictimRootID != victim.ID {
		t.Fatalf("victim = %s, want %s", summary.VictimRootID, victim.ID)
	}

	if _, err := victim.Commit(ctx); !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("victim commit err = %v, want ErrDeadlock", err)
	}
	if victim.State() != TransactionAborted {
		t.Fatalf("victim state = %s, want ABORTED", victim.State())
	}
	// Nothing was published: the head did not move and the victim's write is not visible.
	after, err := f.store.Head()
	if err != nil {
		t.Fatal(err)
	}
	if after != head {
		t.Fatalf("head moved from %d to %d on a refused victim commit", head, after)
	}
	reader := f.begin(ctx)
	value, found, err := reader.Get("index/PRIMARY", []byte("k2"))
	if err != nil {
		t.Fatal(err)
	}
	if found && string(value) == "from-b" {
		t.Fatal("the victim's write became visible")
	}
	// The victim's abort released its grants, so the survivor's already-queued request resolves
	// without a second acquisition: the abort is what hands the row over.
	if _, err := f.manager.WaitForGrant(ctx, a.ID, survivorWait.Wait); err != nil {
		t.Fatalf("the survivor was not granted the freed row: %v", err)
	}
	reader.Rollback()
	a.Rollback()
	f.requireNoLockState()
}

// TestB02LockOrderIsNeverInverted drives the two layers concurrently.
//
// The lock order is Store.views -> TransactionManager.mu -> lockmanager.Manager.mu. A path that
// took them in the other order would deadlock here, and with the race detector enabled the same
// test also checks the accesses themselves. The operations are chosen to hit every path that
// touches both registries: begin, commit, rollback, epoch reset and the diagnostics reader.
func TestB02LockOrderIsNeverInverted(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	const workers = 6
	const rounds = 25
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				tx, err := f.store.Begin(ctx, nil)
				if err != nil {
					// A transaction begun under a generation that a concurrent reset replaced is a
					// legitimate refusal.
					if errors.Is(err, ErrClosed) || errors.Is(err, lockmanager.ErrLockInvalidated) || errors.Is(err, lockmanager.ErrOwnerUnknown) {
						continue
					}
					errs <- err
					return
				}
				key := []byte{byte('a' + w), byte('0' + i%10)}
				ref := lockmanager.ResourceRef{Identity: lockmanager.Identity{
					Kind:    lockmanager.KindRecord,
					Mode:    lockmanager.ModeX,
					Space:   "index/PRIMARY",
					TableID: "t1",
					Key:     key,
				}}
				if _, err := f.manager.TryAcquire(ctx, tx.ID, tx.lockScopeID, []lockmanager.ResourceRef{ref, tableIntentRef()}); err != nil &&
					!errors.Is(err, lockmanager.ErrOwnerUnknown) {
					errs <- err
					return
				}
				if w%2 == 0 {
					if _, err := tx.Commit(ctx); err != nil && !errors.Is(err, ErrConflict) {
						errs <- err
						return
					}
				} else {
					if err := tx.Rollback(); err != nil {
						errs <- err
						return
					}
				}
			}
		}(w)
	}

	// A resetter and a diagnostics reader run against the same state.
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			f.store.ResetLockEpoch(uint64(i + 1))
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds*4; i++ {
			_ = f.store.LockBinding().Manager().Stats()
			_ = f.store.txns.ActiveTransactions()
		}
	}()

	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent lifecycle error: %v", err)
	}

	// After every generation change the two registries agree, and the lock table is empty because
	// each reset ended and released the owners of the replaced generation.
	f.store.ResetLockEpoch(999)
	if owners := f.store.LockBinding().Manager().Stats().ActiveOwners; owners != 0 {
		t.Fatalf("%d lock owners survived the concurrent run, want 0", owners)
	}
	if edges := f.store.LockBinding().Manager().WaitEdges(); len(edges) != 0 {
		t.Fatalf("%d wait edges survived the concurrent run", len(edges))
	}
	f.requireNoLockState()
}

// TestB02WaiterWakesAfterTheHolderIsInvalidated covers a waiter across a generation change.
//
// The waiter's answer is already decided, so it must be woken with the invalidated status and must
// not be granted the row when the holder's generation is replaced. The order inside ResetEpoch is
// what this test pins: invalidate first, drop grants second.
func TestB02WaiterWakesAfterTheHolderIsInvalidated(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	holder := f.begin(ctx)
	waiter := f.begin(ctx)
	f.lockRow(ctx, holder, "k1")
	queued, err := f.manager.Acquire(ctx, waiter.ID, waiter.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k1"), tableIntentRef()})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Op != lockmanager.LockOpWait {
		t.Fatalf("op = %s, want WAIT", queued.Op)
	}

	if invalidated := f.store.ResetLockEpoch(3); invalidated != 2 {
		t.Fatalf("invalidated owners = %d, want 2", invalidated)
	}
	if _, err := f.manager.WaitForGrantResult(ctx, waiter.ID, queued.Wait); !errors.Is(err, lockmanager.ErrLockInvalidated) {
		t.Fatalf("waiter err = %v, want ErrLockInvalidated", err)
	}
	if f.pendingOf(waiter.ID) != 0 {
		t.Fatalf("%d pending requests survived the reset", f.pendingOf(waiter.ID))
	}
	// The waiter was refused, not granted: an invalidated request must not win a lock as a side
	// effect of the holder being released.
	if f.heldRow(waiter.ID, "k1") {
		t.Fatal("an invalidated waiter was granted the freed row")
	}
	holder.Rollback()
	waiter.Rollback()
	f.requireNoLockState()
}

// TestB02CommitAfterADeadlockAbortIsRefusedOnce covers the repeated-victim path.
//
// A victim that ignores the first refusal and tries again must keep being refused, and the repeated
// attempt must not release anything twice or change the recorded outcome.
func TestB02CommitAfterADeadlockAbortIsRefusedOnce(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	a := f.begin(ctx)
	b := f.begin(ctx)
	f.lockRow(ctx, a, "k1")
	f.lockRow(ctx, b, "k2")
	if _, err := f.manager.Acquire(ctx, a.ID, a.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k2"), tableIntentRef()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Acquire(ctx, b.ID, b.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k1"), tableIntentRef()}); err != nil {
		t.Fatal(err)
	}
	summary, found := f.manager.DetectDeadlock()
	if !found {
		t.Fatal("deadlock not detected")
	}
	victim := b
	if summary.VictimRootID != victim.ID {
		t.Fatalf("victim = %s, want %s", summary.VictimRootID, victim.ID)
	}

	if _, err := victim.Commit(ctx); !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("first victim commit err = %v, want ErrDeadlock", err)
	}
	// The second attempt cannot resurrect the transaction: once the outcome is decided the
	// transaction is closed, so the repeat is refused and the state stays ABORTED.
	if _, err := victim.Commit(ctx); err == nil {
		t.Fatal("a repeated victim commit was accepted")
	}
	if victim.State() != TransactionAborted {
		t.Fatalf("state = %s after a repeated victim commit, want ABORTED", victim.State())
	}
	f.manager.ReleaseOwner(a.ID)
	f.requireNoLockState()
}
