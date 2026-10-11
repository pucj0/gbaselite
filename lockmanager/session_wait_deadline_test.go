package lockmanager

import (
	"context"
	"testing"
	"time"
)

// session_wait_deadline_test.go is the lock-manager half of the `innodb_lock_wait_timeout` contract:
// a scope's own deadline *is* the wait bound, in both directions.
//
// The contract default is 50 seconds, but `SET SESSION innodb_lock_wait_timeout` accepts up to 86400
// seconds, so a scope opened with a longer deadline must be honoured rather than clamped. A silent
// clamp is the worst kind of limit: the session reports 300 seconds and the request dies at 50 with
// no way for the caller to tell why.

// t08Holder keeps one record so a second owner has to queue for it.
func t08Holder(t *testing.T, m *Manager, key string) publicOwner {
	t.Helper()
	holder := newPublicOwner(t, m, "root-t08-holder")
	holder.grant(context.Background(), indexRecord("t08", "PRIMARY", []byte(key)), ixIntent("t08"))
	return holder
}

// t08Waiter opens an owner whose statement scope carries one explicit deadline and returns it.
func t08Waiter(t *testing.T, m *Manager, rootID string, deadline time.Time) publicOwner {
	t.Helper()
	if _, err := m.RegisterOwner(rootID, 1, 0, 10); err != nil {
		t.Fatalf("RegisterOwner(%s): %v", rootID, err)
	}
	scope, err := m.OpenScope(rootID, rootID+"-txn", 0, ScopeStatement, deadline)
	if err != nil {
		t.Fatalf("OpenScope(%s): %v", rootID, err)
	}
	return publicOwner{t: t, manager: m, rootID: rootID, scopeID: scope.ID}
}

// TestB08ScopeDeadlineLongerThanTheDefaultIsHonoured proves a 300-second session budget survives past
// the 50-second default.
func TestB08ScopeDeadlineLongerThanTheDefaultIsHonoured(t *testing.T) {
	ctx := context.Background()
	m, clock := newPublicManager(t, BudgetLimits{})
	t08Holder(t, m, "k1")

	waiter := t08Waiter(t, m, "root-t08-long", clock.Now().Add(300*time.Second))
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

	waiter := t08Waiter(t, m, "root-t08-short", clock.Now().Add(2*time.Second))
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

// TestB08ScopeWithoutADeadlineUsesTheContractDefault pins that the default still applies when the
// caller configured nothing: the un-clamping must not have removed the 50-second bound.
func TestB08ScopeWithoutADeadlineUsesTheContractDefault(t *testing.T) {
	ctx := context.Background()
	m, clock := newPublicManager(t, BudgetLimits{})
	t08Holder(t, m, "k1")

	// A zero deadline is "no scope deadline": the manager's own default applies.
	waiter := t08Waiter(t, m, "root-t08-default", time.Time{})
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
