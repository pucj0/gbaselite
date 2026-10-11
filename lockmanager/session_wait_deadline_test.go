package lockmanager

import (
	"context"
	"testing"
	"time"
)

// session_wait_deadline_test.go is the lock-manager half of the `innodb_lock_wait_timeout` contract:
// the session value is the bound of *one wait*, and the scope's context deadline caps the scope.
//
// The contract default is 50 seconds, but `SET SESSION innodb_lock_wait_timeout` accepts up to 86400
// seconds, so a scope opened with a longer budget must be honoured rather than clamped. A silent clamp
// is the worst kind of limit: the session reports 300 seconds and the request dies at 50 with no way
// for the caller to tell why.

// t08Holder keeps one record so a second owner has to queue for it.
func t08Holder(t *testing.T, m *Manager, key string) publicOwner {
	t.Helper()
	holder := newPublicOwner(t, m, "root-t08-holder")
	holder.grant(context.Background(), indexRecord("t08", "PRIMARY", []byte(key)), ixIntent("t08"))
	return holder
}

// t08Waiter opens an owner whose statement scope carries one wait budget and returns it.
//
// The budget is the session's `innodb_lock_wait_timeout`; a zero budget leaves the manager's default.
func t08Waiter(t *testing.T, m *Manager, rootID string, budget time.Duration) publicOwner {
	t.Helper()
	return t08WaiterCapped(t, m, rootID, budget, time.Time{})
}

// t08WaiterCapped additionally gives the scope a context deadline, which caps every wait of the scope.
func t08WaiterCapped(t *testing.T, m *Manager, rootID string, budget time.Duration, deadline time.Time) publicOwner {
	t.Helper()
	if _, err := m.RegisterOwner(rootID, 1, 0, 10); err != nil {
		t.Fatalf("RegisterOwner(%s): %v", rootID, err)
	}
	scope, err := m.OpenScopeWithBudget(rootID, rootID+"-txn", 0, ScopeStatement, deadline, budget)
	if err != nil {
		t.Fatalf("OpenScopeWithBudget(%s): %v", rootID, err)
	}
	return publicOwner{t: t, manager: m, rootID: rootID, scopeID: scope.ID}
}

// TestB08ScopeDeadlineLongerThanTheDefaultIsHonoured proves a 300-second session budget survives past
// the 50-second default.
func TestB08ScopeDeadlineLongerThanTheDefaultIsHonoured(t *testing.T) {
	ctx := context.Background()
	m, clock := newPublicManager(t, BudgetLimits{})
	t08Holder(t, m, "k1")

	waiter := t08Waiter(t, m, "root-t08-long", 300*time.Second)
	result := waiter.acquire(ctx, indexRecord("t08", "PRIMARY", []byte("k1")), ixIntent("t08"))
	if result.Op != LockOpWait {
		t.Fatalf("the request op = %s, want WAIT behind the holder", result.Op)
	}

	// 100 seconds is past the contract default and still inside the session's 300: the request must
	// still be waiting. Under the old clamp it would already be terminal here.
	clock.Advance(100 * time.Second)
	if expired := m.CheckTimeouts(); expired != 0 {
		t.Fatalf("%d requests expired 100s into a 300s budget", expired)
	}
	if status := m.WaitStatus("root-t08-long", result.Wait); status != RequestPending {
		t.Fatalf("status 100s into a 300s budget = %s, want PENDING", status)
	}

	// Past the session budget it ends, with the timeout status the caller expects.
	clock.Advance(250 * time.Second)
	if expired := m.CheckTimeouts(); expired != 1 {
		t.Fatalf("CheckTimeouts = %d past the session budget, want 1", expired)
	}
	if status := m.WaitStatus("root-t08-long", result.Wait); status != RequestTimedOut {
		t.Fatalf("status past the session budget = %s, want TIMED_OUT", status)
	}
	if _, err := m.WaitForGrantResult(ctx, "root-t08-long", result.Wait); err != ErrLockWaitTimeout {
		t.Fatalf("wait result err = %v, want ErrLockWaitTimeout", err)
	}
	m.ReleaseOwner("root-t08-holder")
	m.ReleaseOwner("root-t08-long")
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after cleanup, want 0", size)
	}
}

// TestB08ScopeDeadlineShorterThanTheDefaultIsHonoured proves the other direction: a short session
// budget ends the wait before the default would.
func TestB08ScopeDeadlineShorterThanTheDefaultIsHonoured(t *testing.T) {
	ctx := context.Background()
	m, clock := newPublicManager(t, BudgetLimits{})
	t08Holder(t, m, "k1")

	waiter := t08Waiter(t, m, "root-t08-short", 2*time.Second)
	result := waiter.acquire(ctx, indexRecord("t08", "PRIMARY", []byte("k1")), ixIntent("t08"))
	if result.Op != LockOpWait {
		t.Fatalf("the request op = %s, want WAIT", result.Op)
	}
	clock.Advance(3 * time.Second)
	if expired := m.CheckTimeouts(); expired != 1 {
		t.Fatalf("CheckTimeouts = %d, want 1 past a 2s budget", expired)
	}
	if status := m.WaitStatus("root-t08-short", result.Wait); status != RequestTimedOut {
		t.Fatalf("status = %s, want TIMED_OUT", status)
	}
	m.ReleaseOwner("root-t08-holder")
	m.ReleaseOwner("root-t08-short")
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after cleanup, want 0", size)
	}
}

// TestB08ScopeWithoutABudgetUsesTheContractDefault pins that the default still applies when the caller
// configured nothing: honouring a session budget must not have removed the 50-second bound.
func TestB08ScopeWithoutABudgetUsesTheContractDefault(t *testing.T) {
	ctx := context.Background()
	m, clock := newPublicManager(t, BudgetLimits{})
	t08Holder(t, m, "k1")

	// A zero budget is "no session budget": the manager's own default applies.
	waiter := t08Waiter(t, m, "root-t08-default", 0)
	result := waiter.acquire(ctx, indexRecord("t08", "PRIMARY", []byte("k1")), ixIntent("t08"))
	if result.Op != LockOpWait {
		t.Fatalf("the request op = %s, want WAIT", result.Op)
	}
	clock.Advance(49 * time.Second)
	if expired := m.CheckTimeouts(); expired != 0 {
		t.Fatalf("%d requests expired before the 50s default", expired)
	}
	clock.Advance(2 * time.Second)
	if expired := m.CheckTimeouts(); expired != 1 {
		t.Fatalf("CheckTimeouts = %d past the 50s default, want 1", expired)
	}
	m.ReleaseOwner("root-t08-holder")
	m.ReleaseOwner("root-t08-default")
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after cleanup, want 0", size)
	}
}

// TestB08ContextDeadlineCapsTheSessionBudget proves the other half of the contract's "earlier of the
// two" rule: a query context that ends sooner ends the wait, however generous the session budget is.
//
// A locked statement may wait much longer than its caller allows only if the caller's bound is
// ignored; here the context bound of one second wins over a 300-second session budget.
func TestB08ContextDeadlineCapsTheSessionBudget(t *testing.T) {
	ctx := context.Background()
	m, clock := newPublicManager(t, BudgetLimits{})
	t08Holder(t, m, "k1")

	waiter := t08WaiterCapped(t, m, "root-t08-capped", 300*time.Second, clock.Now().Add(1*time.Second))
	result := waiter.acquire(ctx, indexRecord("t08", "PRIMARY", []byte("k1")), ixIntent("t08"))
	if result.Op != LockOpWait {
		t.Fatalf("the request op = %s, want WAIT", result.Op)
	}
	// 0.9s is inside the context bound: still waiting, even though the session budget is 300s.
	clock.Advance(900 * time.Millisecond)
	if expired := m.CheckTimeouts(); expired != 0 {
		t.Fatalf("%d requests expired inside the context bound", expired)
	}
	if status := m.WaitStatus("root-t08-capped", result.Wait); status != RequestPending {
		t.Fatalf("status inside the context bound = %s, want PENDING", status)
	}
	// Past the context bound the wait ends, long before the session budget would have.
	clock.Advance(200 * time.Millisecond)
	if expired := m.CheckTimeouts(); expired != 1 {
		t.Fatalf("CheckTimeouts = %d past the context bound, want 1", expired)
	}
	if status := m.WaitStatus("root-t08-capped", result.Wait); status != RequestTimedOut {
		t.Fatalf("status past the context bound = %s, want TIMED_OUT", status)
	}
	m.ReleaseOwner("root-t08-holder")
	m.ReleaseOwner("root-t08-capped")
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after cleanup, want 0", size)
	}
}
