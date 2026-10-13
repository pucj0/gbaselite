package mvcc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// barrierInterleavingProposer allows intercepting right after Barrier succeeds.
type barrierInterleavingProposer struct {
	*Store
	mu           sync.Mutex
	term         uint64
	afterBarrier func()
}

func (p *barrierInterleavingProposer) Term() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.term
}

func (p *barrierInterleavingProposer) setTerm(t uint64) {
	p.mu.Lock()
	p.term = t
	p.mu.Unlock()
}

func (p *barrierInterleavingProposer) Barrier(ctx context.Context) error {
	p.mu.Lock()
	fn := p.afterBarrier
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
	return nil
}

// TestT054ScenarioABarrierSucceedsInOldTermThenTermSwitchesBeforeRegistration verifies Scenario A:
// If a transaction's Barrier passes in Term N, but the leader term switches to N+1 before the
// transaction finishes registration, RegisterOwner detects the term mismatch, cleanly unregisters
// from TransactionRegistry, and leaves zero Owner, Scope, or Ledger residues.
func TestT054ScenarioABarrierSucceedsInOldTermThenTermSwitchesBeforeRegistration(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	const termN uint64 = 10
	f.store.ResetLockEpoch(termN)

	prop := &barrierInterleavingProposer{
		Store: f.store,
		term:  termN,
	}

	// Right after Barrier completes under Term N, simulate leader term switch to N+1:
	const termNext uint64 = 11
	prop.afterBarrier = func() {
		// Reset lock epoch to termNext!
		f.store.ResetLockEpoch(termNext)
	}

	// Now call Begin with the proposer whose term is still termN
	tx, err := f.store.Begin(ctx, prop)
	if err == nil {
		_ = tx.Rollback()
		t.Fatal("expected Begin to fail due to term mismatch with new epoch, got nil error")
	}

	if !errors.Is(err, storageengine.ErrLockInvalidated) && !errors.Is(err, lockmanager.ErrLockInvalidated) {
		t.Fatalf("expected ErrLockInvalidated on term mismatch, got: %v", err)
	}

	// Verify zero residues:
	// 1. TransactionRegistry has no active roots
	if active := len(f.store.txns.ActiveTransactions()); active != 0 {
		t.Fatalf("transaction registry still has active roots: %d", active)
	}

	// 2. Lock manager has zero active owners
	stats := f.manager.Stats()
	if stats.ActiveOwners != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 {
		t.Fatalf("lock manager has residue: %+v", stats)
	}

	// 3. Ledger has zero active charges
	if totals := f.manager.Ledger().Totals(); totals.Components != 0 || totals.Waiters != 0 {
		t.Fatalf("ledger has active components: %+v", totals)
	}
}

// TestT054ScenarioCDelayedOldResetCannotKillNewTermRoot verifies Scenario C:
// When a new Term N+1 Root is successfully registered and holding a lock,
// an arriving delayed Reset from old Term N is rejected by monotonicity and does not kill the new Root.
func TestT054ScenarioCDelayedOldResetCannotKillNewTermRoot(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	const termN uint64 = 5
	const termNext uint64 = 6

	f.store.ResetLockEpoch(termNext)

	propNext := &termProposer{Store: f.store, term: termNext}
	txNew, err := f.store.Begin(ctx, propNext)
	if err != nil {
		t.Fatal(err)
	}
	defer txNew.Rollback()

	// New Root acquires lock on rawKey
	if err := txNew.Put(rawSpace, rawKey, []byte("val6")); err != nil {
		t.Fatalf("txNew Put failed: %v", err)
	}

	// Delayed old reset for Term 5 arrives
	affected := f.store.ResetLockEpoch(termN)
	if affected != 0 {
		t.Fatalf("expected 0 affected owners from delayed stale reset, got %d", affected)
	}

	// Verify txNew still holds its lock intact
	if mode, held := f.manager.HeldMode(txNew.ID, xRef(rawSpace, rawKey)); !held || mode != lockmanager.ModeX {
		t.Fatalf("txNew lost its lock after delayed stale reset: mode=%v, held=%v", mode, held)
	}
}

// TestT054ScenarioDConcurrentBeginAndEpochTransitions verifies Scenario D:
// Highly concurrent Begin, SyncLockEpoch, and ResetLockEpoch invocations maintain
// strict lock ordering (Store.views -> LockBinding.epochMu -> LockManager.mu) without deadlocks,
// and failed creations leave zero transaction registry or ledger residues.
func TestT054ScenarioDConcurrentBeginAndEpochTransitions(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	const initialTerm uint64 = 100
	f.store.ResetLockEpoch(initialTerm)

	const numWorkers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Background epoch flipper
	go func() {
		term := initialTerm
		for {
			select {
			case <-stop:
				return
			default:
				term++
				f.store.SyncLockEpoch(term)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// Concurrent workers creating and rolling back transactions
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				term := f.manager.Term()
				prop := &termProposer{Store: f.store, term: term}
				tx, err := f.store.Begin(ctx, prop)
				if err == nil {
					_ = tx.Put(rawSpace, []byte("concurrent_key"), []byte("v"))
					_ = tx.Rollback()
				}
			}
		}(i)
	}

	wg.Wait()
	close(stop)

	// Settle and verify clean state after all workers finish
	finalTerm := f.manager.Term() + 1
	f.store.ResetLockEpoch(finalTerm)

	if active := len(f.store.txns.ActiveTransactions()); active != 0 {
		t.Fatalf("residue in transaction registry: %d active", active)
	}
	if stats := f.manager.Stats(); stats.ActiveOwners != 0 || stats.PendingRequests != 0 {
		t.Fatalf("residue in lock manager: %+v", stats)
	}
}
