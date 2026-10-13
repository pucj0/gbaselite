package mvcc

import (
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

type termProposer struct {
	*Store
	term uint64
}

func (p *termProposer) Term() uint64                      { return p.term }
func (p *termProposer) Barrier(ctx context.Context) error { return nil }

func TestT054SyncLockEpochRaftTermChange(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	const termN uint64 = 10
	f.store.ResetLockEpoch(termN)

	propN := &termProposer{Store: f.store, term: termN}

	// 1. Term=N, register Root A, Root B
	txA, err := f.store.Begin(ctx, propN)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	txB, err := f.store.Begin(ctx, propN)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()

	// 2. A holds Record X
	if err := txA.Put(rawSpace, rawKey, []byte("valA")); err != nil {
		t.Fatalf("A Put failed: %v", err)
	}

	// 3. B requests the same resource and enters wait
	outcomeB := make(chan error, 1)
	go func() {
		outcomeB <- txB.Put(rawSpace, rawKey, []byte("valB"))
	}()
	f.awaitWait("B waiting on rawKey")

	if f.manager.PendingRequests(txB.ID) != 1 {
		t.Fatalf("expected B to have 1 pending request, got %d", f.manager.PendingRequests(txB.ID))
	}
	if len(f.manager.WaitEdges()) != 1 {
		t.Fatalf("expected 1 wait edge, got %d", len(f.manager.WaitEdges()))
	}

	// 4 & 5. Raft Term N -> N+1, Store generation unchanged. Call SyncLockEpoch(N+1).
	const termNext uint64 = 11
	synced := f.store.SyncLockEpoch(termNext)
	if !synced {
		t.Fatal("SyncLockEpoch(N+1) returned false, expected true when Term changed!")
	}

	// 6. Verify A's old grant is immediately invalidated
	// Reading under A's old grant must fail with ErrLockInvalidated
	heldMode, held := f.manager.HeldMode(txA.ID, xRef(rawSpace, rawKey))
	if held {
		t.Fatalf("A still holds %v on rawKey after term reset!", heldMode)
	}

	// 7. Verify B receives ErrLockInvalidated
	select {
	case err := <-outcomeB:
		if !errors.Is(err, storageengine.ErrLockInvalidated) && !errors.Is(err, lockmanager.ErrLockInvalidated) {
			t.Fatalf("expected ErrLockInvalidated for B, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("B was not woken with ErrLockInvalidated after SyncLockEpoch")
	}

	// 8. Verify Owner, Pending, WaitEdges, Grant, Ledger all cleaned up
	stats := f.manager.Stats()
	if stats.ActiveOwners != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 || f.manager.LockTableSize() != 0 {
		t.Fatalf("lock manager not cleaned up: stats=%+v, size=%d", stats, f.manager.LockTableSize())
	}

	// 9. Root in new Term N+1 can acquire the resource
	propNext := &termProposer{Store: f.store, term: termNext}
	txC, err := f.store.Begin(ctx, propNext)
	if err != nil {
		t.Fatal(err)
	}
	defer txC.Rollback()

	if err := txC.Put(rawSpace, rawKey, []byte("valC")); err != nil {
		t.Fatalf("txC Put in new term failed: %v", err)
	}

	// 10. Repeated sync with same generation and same Term must be idempotent
	repeatedSync := f.store.SyncLockEpoch(termNext)
	if repeatedSync {
		t.Fatal("repeated SyncLockEpoch with same generation and term returned true, expected false (must be idempotent)!")
	}
	// txC still holds its lock!
	if mode, held := f.manager.HeldMode(txC.ID, xRef(rawSpace, rawKey)); !held || mode != lockmanager.ModeX {
		t.Fatalf("txC lost its lock after idempotent SyncLockEpoch: mode=%v held=%v", mode, held)
	}
}

// TestT054ConcurrentSyncLockEpochTOCTOUEliminated verifies that concurrent calls to SyncLockEpoch
// for the same term transition execute ResetEpoch at most once, and a newly registered Root
// in the new term is not wiped out by a redundant concurrent reset.
func TestT054ConcurrentSyncLockEpochTOCTOUEliminated(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	const termN uint64 = 5
	f.store.ResetLockEpoch(termN)

	// Two concurrent SyncLockEpoch(6) requests coordinated with a start barrier
	const termNext uint64 = 6
	start := make(chan struct{})
	res1 := make(chan bool, 1)
	res2 := make(chan bool, 1)

	go func() {
		<-start
		res1 <- f.store.SyncLockEpoch(termNext)
	}()
	go func() {
		<-start
		res2 <- f.store.SyncLockEpoch(termNext)
	}()

	close(start)
	r1 := <-res1
	r2 := <-res2

	// Exactly one must return true (performed transition), and the other must return false (idempotent no-op)
	if (r1 && r2) || (!r1 && !r2) {
		t.Fatalf("expected exactly one sync to return true, got r1=%v r2=%v", r1, r2)
	}

	// Register a new Root in termNext and verify it holds a lock safely
	propNext := &termProposer{Store: f.store, term: termNext}
	tx, err := f.store.Begin(ctx, propNext)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	if err := tx.Put(rawSpace, rawKey, []byte("valNew")); err != nil {
		t.Fatal(err)
	}
	if mode, held := f.manager.HeldMode(tx.ID, xRef(rawSpace, rawKey)); !held || mode != lockmanager.ModeX {
		t.Fatalf("new transaction lost lock: mode=%v held=%v", mode, held)
	}
}

// TestT054StaleTermSyncCannotRevertEpoch verifies that a SyncLockEpoch request with a stale term
// cannot roll back the epoch or invalidate newer locks.
func TestT054StaleTermSyncCannotRevertEpoch(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	const currentTerm uint64 = 20
	f.store.ResetLockEpoch(currentTerm)

	prop := &termProposer{Store: f.store, term: currentTerm}
	tx, err := f.store.Begin(ctx, prop)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	if err := tx.Put(rawSpace, rawKey, []byte("val20")); err != nil {
		t.Fatal(err)
	}

	// Stale term 19 must be rejected and must not invalidate tx's lock
	if synced := f.store.SyncLockEpoch(19); synced {
		t.Fatal("SyncLockEpoch with stale term 19 returned true, expected false")
	}

	if mode, held := f.manager.HeldMode(tx.ID, xRef(rawSpace, rawKey)); !held || mode != lockmanager.ModeX {
		t.Fatalf("tx in term 20 lost its lock after stale term sync: mode=%v held=%v", mode, held)
	}
}
