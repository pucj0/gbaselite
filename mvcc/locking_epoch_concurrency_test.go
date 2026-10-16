package mvcc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gbaselite/lockmanager"
)

// TestP7R2_ScenarioA_DelayedLeaderLossCannotDisruptNewTerm covers FIX-02 Scenario A:
// A Leader Loss event from Term 10 pauses before acquiring epochMu.
// In the meantime, Term 11 is elected, syncs lock epoch, and new transactions acquire locks and wait.
// When Term 10's delayed step-down resumes, it is rejected by monotonicity under epochMu,
// and must NOT invalidate Term 11's locks, waiters, wait edges, or dirty registry entries.
func TestP7R2_ScenarioA_DelayedLeaderLossCannotDisruptNewTerm(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	// 1. Initialize store in Term 10
	f.store.ResetLockEpoch(10)

	// 2. Set up deterministic hook to pause Term 10 ForceResetLockEpoch before acquiring epochMu
	inHook := make(chan struct{})
	allowStepDown := make(chan struct{})
	var hookOnce sync.Once

	forceResetLockEpochHook = func(stage string, term uint64) {
		if stage == "before_acquire_epoch_mu" && term == 10 {
			hookOnce.Do(func() {
				close(inHook)
				<-allowStepDown
			})
		}
	}
	defer func() { forceResetLockEpochHook = nil }()

	stepDownResult := make(chan int, 1)
	go func() {
		stepDownResult <- f.store.ForceResetLockEpoch(10)
	}()

	// Wait until Term 10 step-down is paused before epochMu
	<-inHook

	// 3. New leader elected in Term 11! Sync epoch to 11
	if acted := f.store.SyncLockEpoch(11); !acted {
		t.Fatal("SyncLockEpoch(11) did not act")
	}

	propTerm11 := &barrierInterleavingProposer{
		Store: f.store,
		term:  11,
	}

	// Root A starts in Term 11, acquires Record X, and publishes dirty version
	txA, err := f.store.Begin(ctx, propTerm11)
	if err != nil {
		t.Fatalf("txA begin in term 11 failed: %v", err)
	}
	defer txA.Rollback()

	if err := txA.Put(rawSpace, rawKey, []byte("valA_term11")); err != nil {
		t.Fatalf("txA Put failed: %v", err)
	}
	versionsA, err := txA.CollectDirtyVersions()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.DirtyRegistry().Publish(txA.ID, txA.Generation(), txA.Term(), versionsA); err != nil {
		t.Fatal(err)
	}

	// Verify A holds ModeX
	if mode, ok := f.manager.HeldMode(txA.ID, sqlShapedRecord()); !ok || mode != lockmanager.ModeX {
		t.Fatalf("txA does not hold ModeX: %v, %v", mode, ok)
	}

	// Root B starts in Term 11 and requests the same record, blocking behind A
	txB, err := f.store.Begin(ctx, propTerm11)
	if err != nil {
		t.Fatalf("txB begin in term 11 failed: %v", err)
	}
	defer txB.Rollback()

	bDone := make(chan error, 1)
	go func() {
		bDone <- txB.Put(rawSpace, rawKey, []byte("valB_term11"))
	}()
	f.awaitPending(txB.ID, 1)

	// Verify wait edge txB -> txA exists
	edges := f.manager.WaitEdges()
	if len(edges) == 0 || edges[0].FromRootID != txB.ID || edges[0].ToRootID != txA.ID {
		t.Fatalf("unexpected wait edges in term 11: %v", edges)
	}

	// 4. Resume the delayed Term 10 Leader Loss event
	close(allowStepDown)
	affected := <-stepDownResult
	if affected != 0 {
		t.Fatalf("stale Term 10 ForceResetLockEpoch affected %d owners, want 0", affected)
	}

	// 5. Assert Term 11 state was NOT disrupted:
	// A's lock must still be held
	if mode, ok := f.manager.HeldMode(txA.ID, sqlShapedRecord()); !ok || mode != lockmanager.ModeX {
		t.Fatalf("txA lost ModeX lock after delayed term 10 step-down: %v, %v", mode, ok)
	}

	// B must still be waiting behind A, NOT invalidated
	select {
	case err := <-bDone:
		t.Fatalf("txB unexpectedly unblocked by delayed term 10 step-down: %v", err)
	default:
	}
	if pending := f.manager.PendingRequests(txB.ID); pending != 1 {
		t.Fatalf("txB pending requests = %d, want 1", pending)
	}

	// Wait edge must still exist
	edgesAfter := f.manager.WaitEdges()
	if len(edgesAfter) == 0 || edgesAfter[0].FromRootID != txB.ID || edgesAfter[0].ToRootID != txA.ID {
		t.Fatalf("wait edge destroyed by delayed term 10 step-down: %v", edgesAfter)
	}

	// Dirty registry must still have txA's dirty version
	snap, err := f.store.DirtyRegistry().Sample("reader")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := snap.Get(rawSpace, rawKey); !ok || string(d.Value) != "valA_term11" {
		t.Fatalf("dirty registry lost Term 11 version: %v, %v", d, ok)
	}

	// 6. Old Term 10 proposer must not be able to acquire new locks
	propTerm10 := &barrierInterleavingProposer{
		Store: f.store,
		term:  10,
	}
	txOld, err := f.store.Begin(ctx, propTerm10)
	if err == nil {
		defer txOld.Rollback()
		if err := txOld.Put(rawSpace, []byte("k_other"), []byte("val")); err == nil {
			t.Fatal("old term 10 transaction unexpectedly succeeded in acquiring lock under term 11")
		}
	}

	// 7. Tx A rolls back: Tx B unblocks, acquires lock, and finishes
	if err := txA.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("txB Put failed after txA rollback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("txB never unblocked after txA rollback")
	}

	if _, err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.requireIdle()
}

// TestP7R2_ScenarioB_NormalLeaderLossAndRepeatedEvents covers FIX-02 Scenario B:
// Verifies that a normal Leader Loss event in Term 10 invalidates all locks and waiters,
// and repeated step-down events are idempotent and do not disrupt subsequent terms.
func TestP7R2_ScenarioB_NormalLeaderLossAndRepeatedEvents(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	// Initialize in Term 10
	f.store.ResetLockEpoch(10)

	propTerm10 := &barrierInterleavingProposer{
		Store: f.store,
		term:  10,
	}

	txA, err := f.store.Begin(ctx, propTerm10)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()
	if err := txA.Put(rawSpace, rawKey, []byte("valA")); err != nil {
		t.Fatal(err)
	}

	txB, err := f.store.Begin(ctx, propTerm10)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()
	bDone := make(chan error, 1)
	go func() {
		bDone <- txB.Put(rawSpace, rawKey, []byte("valB"))
	}()
	f.awaitPending(txB.ID, 1)

	// Normal Leader Loss in Term 10
	affected := f.store.ForceResetLockEpoch(10)
	if affected == 0 {
		t.Fatal("ForceResetLockEpoch(10) affected 0 owners, want > 0")
	}

	// Waiter txB must be woken with ErrLockInvalidated
	select {
	case err := <-bDone:
		if !errors.Is(err, lockmanager.ErrLockInvalidated) {
			t.Fatalf("expected ErrLockInvalidated, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not woken with ErrLockInvalidated")
	}

	// Repeated ForceResetLockEpoch(10) must be a safe no-op
	if aff := f.store.ForceResetLockEpoch(10); aff != 0 {
		t.Fatalf("repeated ForceResetLockEpoch affected %d owners, want 0", aff)
	}

	f.requireIdle()
}

// TestP7R2_ScenarioC_RestoreGenerationProtectsAgainstDelayedForceReset covers FIX-02 Scenario C:
// Verifies that when a store restores and increments its generation,
// a late ForceReset from an older generation cannot roll back the generation or disrupt locks.
func TestP7R2_ScenarioC_RestoreGenerationProtectsAgainstDelayedForceReset(t *testing.T) {
	f := newRawLockFixture(t)

	// Term 0, initial generation
	txA := f.begin()
	defer txA.Rollback()
	if err := txA.Put(rawSpace, rawKey, []byte("valA")); err != nil {
		t.Fatal(err)
	}

	// Simulate generation increment (like Restore does)
	f.store.generation.Add(1)
	f.store.ResetLockEpoch(0)

	// Now a late ForceReset from generation 0 attempts to reset
	// Calling DirtyRegistry.ForceReset with old generation 0 must be rejected!
	if reset := f.store.DirtyRegistry().ForceReset(0, 0); reset {
		t.Fatal("DirtyRegistry.ForceReset accepted older generation 0, want false")
	}
}
