package lockmanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// concurrency_test.go is the T063 suite: 100-run deterministic concurrency matrix across
// all 10 lock manager contention scenarios using FakeClock and causal barriers.

func t063RowRef(space, tableID, key string, mode Mode) ResourceRef {
	return ResourceRef{
		Identity: Identity{
			Kind:    KindRecord,
			Mode:    mode,
			Space:   space,
			TableID: tableID,
			Key:     []byte(key),
		},
	}
}

func t063TableIntentRef(space, tableID string, mode Mode) ResourceRef {
	return ResourceRef{
		Identity: Identity{
			Kind:    KindTableIntent,
			Mode:    mode,
			Space:   space,
			TableID: tableID,
		},
	}
}

func t063GapRef(space, tableID string, lower, upper string) ResourceRef {
	return ResourceRef{
		Identity: Identity{
			Kind:           KindInterval,
			Mode:           ModeGap,
			Space:          space,
			TableID:        tableID,
			Lower:          []byte(lower),
			Upper:          []byte(upper),
			LowerInclusive: true,
			UpperInclusive: true,
		},
	}
}

func TestT063RecordSXCompatibilityAndQueueing100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		m, err := NewManager(BudgetLimits{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)
		_, _ = m.RegisterOwner("root-c", 1, 0, 10)

		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{})
		sC, _ := m.OpenScope("root-c", "s-c", 0, ScopeStatement, time.Time{})

		res := t063RowRef("row/t1", "t1", "k1", ModeS)
		tiS := t063TableIntentRef("row/t1", "t1", ModeIS)
		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)

		// A and B both acquire S -> granted
		rA, err := m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiS, res})
		if err != nil || rA.Op != LockOpGranted {
			t.Fatalf("run %d: A Acquire failed: %v", run, err)
		}
		rB, err := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiS, res})
		if err != nil || rB.Op != LockOpGranted {
			t.Fatalf("run %d: B Acquire failed: %v", run, err)
		}

		// C requests X -> must WAIT
		resX := t063RowRef("row/t1", "t1", "k1", ModeX)
		rC, err := m.Acquire(ctx, "root-c", sC.ID, []ResourceRef{tiX, resX})
		if err != nil || rC.Op != LockOpWait {
			t.Fatalf("run %d: C expected LockOpWait, got: %v, op=%v", run, err, rC.Op)
		}

		// Release A -> C still waits
		m.ReleaseOwner("root-a")
		if rC.Wait.Status() != RequestPending {
			t.Fatalf("run %d: C prematurely granted after only A released", run)
		}

		// Release B -> C is granted!
		m.ReleaseOwner("root-b")
		status, err := rC.Wait.Wait(ctx)
		if err != nil || status != RequestGranted {
			t.Fatalf("run %d: C wait failed after B released: %v, status=%v", run, err, status)
		}

		m.ReleaseOwner("root-c")
		if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 {
			t.Fatalf("run %d: residue left after run: size=%d", run, m.LockTableSize())
		}
		m.Close()
	}
}

func TestT063SXUpgrade100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		m, err := NewManager(BudgetLimits{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)

		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{})

		resS := t063RowRef("row/t1", "t1", "k1", ModeS)
		tiS := t063TableIntentRef("row/t1", "t1", ModeIS)
		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
		resX := t063RowRef("row/t1", "t1", "k1", ModeX)

		_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiS, resS})
		_, _ = m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiS, resS})

		// A upgrades S -> X: must wait for B
		rUpgrade, err := m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, resX})
		if err != nil || rUpgrade.Op != LockOpWait {
			t.Fatalf("run %d: upgrade expected LockOpWait, got %v", run, rUpgrade.Op)
		}

		m.ReleaseOwner("root-b")
		status, err := rUpgrade.Wait.Wait(ctx)
		if err != nil || status != RequestGranted {
			t.Fatalf("run %d: upgrade wait failed: %v", run, err)
		}

		m.ReleaseOwner("root-a")
		if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 {
			t.Fatalf("run %d: residue left", run)
		}
		m.Close()
	}
}

func TestT063QueueFIFOOlderWaiter100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		m, err := NewManager(BudgetLimits{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)
		_, _ = m.RegisterOwner("root-c", 1, 0, 10)

		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{})
		sC, _ := m.OpenScope("root-c", "s-c", 0, ScopeStatement, time.Time{})

		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
		tiS := t063TableIntentRef("row/t1", "t1", ModeIS)
		resX := t063RowRef("row/t1", "t1", "k1", ModeX)
		resS := t063RowRef("row/t1", "t1", "k1", ModeS)

		// A holds X
		_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, resX})

		// B requests X (older waiter)
		rB, _ := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, resX})
		// C requests S (younger waiter, blocked by older exclusive waiter B)
		rC, _ := m.Acquire(ctx, "root-c", sC.ID, []ResourceRef{tiS, resS})

		if rB.Op != LockOpWait || rC.Op != LockOpWait {
			t.Fatalf("run %d: expected both B and C to wait", run)
		}

		// When A releases: B must be granted first, C must NOT be granted
		m.ReleaseOwner("root-a")
		if rB.Wait.Status() != RequestGranted {
			t.Fatalf("run %d: older waiter B was not granted", run)
		}
		if rC.Wait.Status() != RequestPending {
			t.Fatalf("run %d: younger waiter C was granted ahead of B", run)
		}

		m.ReleaseOwner("root-b")
		if rC.Wait.Status() != RequestGranted {
			t.Fatalf("run %d: C was not granted after B released", run)
		}

		m.ReleaseOwner("root-c")
		m.Close()
	}
}

func TestT063TwoTransactionDeadlockCycle100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		m, err := NewManager(BudgetLimits{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)

		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{})

		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
		r1 := t063RowRef("row/t1", "t1", "k1", ModeX)
		r2 := t063RowRef("row/t1", "t1", "k2", ModeX)

		// A holds R1, B holds R2
		_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r1})
		_, _ = m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r2})

		// A requests R2 -> waits on B
		rA2, _ := m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r2})
		// B requests R1 -> waits on A -> Deadlock!
		rB1, _ := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r1})

		summary, found := m.DetectDeadlock()
		if !found {
			t.Fatalf("run %d: deadlock cycle not found", run)
		}
		// Deterministic victim: root-b (higher ordinal)
		if summary.VictimRootID != "root-b" {
			t.Fatalf("run %d: expected victim root-b, got %s", run, summary.VictimRootID)
		}
		if rB1.Wait.Status() != RequestDeadlockVictim {
			t.Fatalf("run %d: victim request not marked RequestDeadlockVictim, got %v", run, rB1.Wait.Status())
		}

		// Resolve victim B
		m.ReleaseOwner("root-b")

		// Survivor A receives grant on R2!
		status, err := rA2.Wait.Wait(ctx)
		if err != nil || status != RequestGranted {
			t.Fatalf("run %d: survivor A wait failed: %v", run, err)
		}

		m.ReleaseOwner("root-a")
		if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 {
			t.Fatalf("run %d: residue left after deadlock resolution", run)
		}
		m.Close()
	}
}

func TestT063ThreeTransactionDeadlockCycle100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		m, err := NewManager(BudgetLimits{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)
		_, _ = m.RegisterOwner("root-c", 1, 0, 10)

		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{})
		sC, _ := m.OpenScope("root-c", "s-c", 0, ScopeStatement, time.Time{})

		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
		r1 := t063RowRef("row/t1", "t1", "k1", ModeX)
		r2 := t063RowRef("row/t1", "t1", "k2", ModeX)
		r3 := t063RowRef("row/t1", "t1", "k3", ModeX)

		_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r1})
		_, _ = m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r2})
		_, _ = m.Acquire(ctx, "root-c", sC.ID, []ResourceRef{tiX, r3})

		rA, _ := m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r2})
		rB, _ := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r3})
		rC, _ := m.Acquire(ctx, "root-c", sC.ID, []ResourceRef{tiX, r1})

		summary, found := m.DetectDeadlock()
		if !found {
			t.Fatalf("run %d: 3-cycle deadlock not found", run)
		}
		if summary.VictimRootID != "root-c" {
			t.Fatalf("run %d: expected victim root-c, got %s", run, summary.VictimRootID)
		}
		if rC.Wait.Status() != RequestDeadlockVictim {
			t.Fatalf("run %d: victim C not marked RequestDeadlockVictim, got %v", run, rC.Wait.Status())
		}

		m.ReleaseOwner("root-c")
		_ = rB.Wait.Status()
		m.ReleaseOwner("root-b")
		_ = rA.Wait.Status()
		m.ReleaseOwner("root-a")

		m.Close()
	}
}

func TestT063GrantAndCancelRace100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		m, err := NewManager(BudgetLimits{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)
		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{})

		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
		r1 := t063RowRef("row/t1", "t1", "k1", ModeX)

		_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r1})
		rB, _ := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r1})

		// Concurrent Cancel of B vs Release of A
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = m.Cancel("root-b", rB.Wait)
		}()
		go func() {
			defer wg.Done()
			m.ReleaseOwner("root-a")
		}()
		wg.Wait()

		// Outcome must be strictly one of GRANTED or CANCELED, and immutable
		st := rB.Wait.Status()
		if st != RequestGranted && st != RequestCanceled {
			t.Fatalf("run %d: invalid terminal status: %v", run, st)
		}
		if rB.Wait.Status() != st {
			t.Fatalf("run %d: status mutated from %v to %v", run, st, rB.Wait.Status())
		}

		m.ReleaseOwner("root-b")
		m.Close()
	}
}

func TestT063TimeoutAndGrantRace100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		clock := NewFakeClock(time.Unix(1000, 0))
		m, err := NewManager(BudgetLimits{}, 1, clock)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)
		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{}.Add(5*time.Second))

		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
		r1 := t063RowRef("row/t1", "t1", "k1", ModeX)

		_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r1})
		rB, _ := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r1})

		// Advance clock past deadline and check timeouts
		clock.Advance(6 * time.Second)
		timedOut := m.CheckTimeouts()
		if timedOut != 1 {
			t.Fatalf("run %d: expected 1 timeout, got %d", run, timedOut)
		}
		if rB.Wait.Status() != RequestTimedOut {
			t.Fatalf("run %d: expected RequestTimedOut, got %v", run, rB.Wait.Status())
		}

		m.ReleaseOwner("root-a")
		m.ReleaseOwner("root-b")
		m.Close()
	}
}

func TestT063EpochResetAndWaiterWake100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		m, err := NewManager(BudgetLimits{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)
		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{})

		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
		r1 := t063RowRef("row/t1", "t1", "k1", ModeX)

		_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r1})
		rB, _ := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r1})

		// Reset epoch to generation 2
		m.ResetEpoch(2, 0)

		if rB.Wait.Status() != RequestInvalidated {
			t.Fatalf("run %d: expected RequestInvalidated, got %v", run, rB.Wait.Status())
		}
		if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 {
			t.Fatalf("run %d: lock manager has residue after ResetEpoch: size=%d", run, m.LockTableSize())
		}
		m.Close()
	}
}

func TestT063ResourceBudgetBoundary100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		limits := BudgetLimits{
			RootRequests: 2,
		}
		m, err := NewManager(limits, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})

		tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
		r1 := t063RowRef("row/t1", "t1", "k1", ModeX)
		r2 := t063RowRef("row/t1", "t1", "k2", ModeX)

		// 2 components: exactly at budget
		res, err := m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r1})
		if err != nil || res.Op != LockOpGranted {
			t.Fatalf("run %d: acquire within budget failed: %v", run, err)
		}

		// 3rd and 4th components: exceeds RootRequests limit 2
		_, errExceed := m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r2})
		if !errors.Is(errExceed, ErrBudgetRootRequests) && !errors.Is(errExceed, ErrLockResourceLimit) {
			t.Fatalf("run %d: expected ErrBudgetRootRequests, got: %v", run, errExceed)
		}

		m.ReleaseOwner("root-a")
		m.Close()
	}
}

func TestT063MultiComponentBundleAtomicGrantAndRevoke100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		m, err := NewManager(BudgetLimits{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		_, _ = m.RegisterOwner("root-a", 1, 0, 10)
		_, _ = m.RegisterOwner("root-b", 1, 0, 10)
		sA, _ := m.OpenScope("root-a", "s-a", 0, ScopeStatement, time.Time{})
		sB, _ := m.OpenScope("root-b", "s-b", 0, ScopeStatement, time.Time{})

		tiIX := t063TableIntentRef("row/t1", "t1", ModeIX)
		r1 := t063RowRef("row/t1", "t1", "k1", ModeX)
		gap := t063GapRef("row/t1", "t1", "10", "20")

		// A acquires 3 components atomically
		rA, err := m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiIX, r1, gap})
		if err != nil || rA.Op != LockOpGranted {
			t.Fatalf("run %d: multi-component acquire failed: %v", run, err)
		}

		// B conflicts on r1 -> entire bundle must wait, not partial grant
		r2 := t063RowRef("row/t1", "t1", "k2", ModeX)
		rB, err := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiIX, r1, r2})
		if err != nil || rB.Op != LockOpWait {
			t.Fatalf("run %d: expected bundle wait, got %v", run, rB.Op)
		}

		// R2 must NOT be held by B while waiting for R1!
		mode, held := m.HeldMode("root-b", r2)
		if held {
			t.Fatalf("run %d: component r2 was partially granted to waiting root-b: mode=%v", run, mode)
		}

		m.ReleaseOwner("root-a")
		status, err := rB.Wait.Wait(ctx)
		if err != nil || status != RequestGranted {
			t.Fatalf("run %d: B bundle wait failed: %v", run, err)
		}

		m.ReleaseOwner("root-b")
		if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 {
			t.Fatalf("run %d: residue left after atomic bundle release", run)
		}
		m.Close()
	}
}
