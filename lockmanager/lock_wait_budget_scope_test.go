package lockmanager

import (
	"context"
	"testing"
	"time"
)

// lock_wait_budget_scope_test.go checks *which* bound `innodb_lock_wait_timeout` is.
//
// The approved contract (contracts/lock-manager.md, contracts/sql-locking.md §Session options) makes
// it the bound of **one lock wait**: "等待期限是会话 lock 期限与 ctx 期限的较早者", where the session value
// is a duration (default 50s, settable to 86400s) and the context is the statement's own deadline. A
// statement that takes two waits of 0.6s each with a 1-second session budget is therefore within budget
// on both, while a *statement-wide* interpretation of the same value would kill the second wait for the
// first wait's elapsed time — and would report a timeout the caller cannot explain from the contract.
//
// The test drives the manager's virtual clock, so the two readings are told apart without sleeping.

// t09Holder keeps one record so another owner has to queue for it.
func t09Holder(t *testing.T, m *Manager, rootID, key string) {
	t.Helper()
	if _, err := m.RegisterOwner(rootID, 1, 0, 10); err != nil {
		t.Fatalf("RegisterOwner(%s): %v", rootID, err)
	}
	scope, err := m.OpenScope(rootID, rootID+"-txn", 0, ScopeStatement, time.Time{})
	if err != nil {
		t.Fatalf("OpenScope(%s): %v", rootID, err)
	}
	owner := publicOwner{t: t, manager: m, rootID: rootID, scopeID: scope.ID}
	owner.grant(context.Background(), indexRecord("t09", "PRIMARY", []byte(key)), ixIntent("t09"))
}

// TestB09LockWaitBudgetAppliesToEachWaitNotToTheStatement shows that a one-second session budget allows
// two waits of 0.6 seconds each inside one statement scope, and still ends a wait that exceeds it.
//
// This is the case that distinguished the two readings of the contract: with the budget carried as the
// scope's absolute deadline, the second wait was expired by the first wait's elapsed time. It is a
// regression test for that, and it fails against that implementation.
func TestB09LockWaitBudgetAppliesToEachWaitNotToTheStatement(t *testing.T) {
	ctx := context.Background()
	m, clock := newPublicManager(t, BudgetLimits{})

	// The statement scope carries the session budget: one second, applied to each wait.
	owner := t08Waiter(t, m, "root-b09", 1*time.Second)

	// First wait: queued behind a holder, released after 0.6s.
	t09Holder(t, m, "root-b09-holder-1", "k1")
	first := owner.acquire(ctx, indexRecord("t09", "PRIMARY", []byte("k1")), ixIntent("t09"))
	if first.Op != LockOpWait {
		t.Fatalf("the first request op = %s, want WAIT", first.Op)
	}
	clock.Advance(600 * time.Millisecond)
	m.ReleaseOwner("root-b09-holder-1")
	if status := m.WaitStatus("root-b09", first.Wait); status != RequestGranted {
		t.Fatalf("the first wait (0.6s into a 1s budget) = %s, want GRANTED", status)
	}

	// Second wait in the same scope: another 0.6s, which is 1.2s of *statement* elapsed time but only
	// 0.6s of waiting. The session budget is per wait, so this must still be inside it.
	t09Holder(t, m, "root-b09-holder-2", "k2")
	second := owner.acquire(ctx, indexRecord("t09", "PRIMARY", []byte("k2")), ixIntent("t09"))
	if second.Op != LockOpWait {
		t.Fatalf("the second request op = %s, want WAIT", second.Op)
	}
	clock.Advance(600 * time.Millisecond)
	if expired := m.CheckTimeouts(); expired != 0 {
		t.Fatalf("%d requests timed out: the first wait's elapsed time was charged against the second wait's budget", expired)
	}
	if status := m.WaitStatus("root-b09", second.Wait); status != RequestPending {
		t.Fatalf("the second wait (0.6s of waiting) = %s, want still PENDING", status)
	}

	// Releasing the second holder grants it: both waits stayed inside a one-second session budget.
	m.ReleaseOwner("root-b09-holder-2")
	if status := m.WaitStatus("root-b09", second.Wait); status != RequestGranted {
		t.Fatalf("the second wait = %s after its holder ended, want GRANTED", status)
	}

	// A wait that really exceeds the budget still ends, so the rule is a bound and not an exemption.
	t09Holder(t, m, "root-b09-holder-3", "k3")
	third := owner.acquire(ctx, indexRecord("t09", "PRIMARY", []byte("k3")), ixIntent("t09"))
	if third.Op != LockOpWait {
		t.Fatalf("the third request op = %s, want WAIT", third.Op)
	}
	clock.Advance(2 * time.Second)
	if expired := m.CheckTimeouts(); expired != 1 {
		t.Fatalf("CheckTimeouts = %d past the session budget, want 1", expired)
	}
	if status := m.WaitStatus("root-b09", third.Wait); status != RequestTimedOut {
		t.Fatalf("the over-budget wait = %s, want TIMED_OUT", status)
	}

	m.ReleaseOwner("root-b09-holder-3")
	m.ReleaseOwner("root-b09")
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after cleanup, want 0", size)
	}
}

// TestB09BudgetIsReadFromTheRequestingOwnersScope guards the lookup itself.
//
// Scope ids restart at one for every owner, so a manager that searched all owners' scope trees would
// hand a request the same-numbered scope of an unrelated transaction — and with it that transaction's
// budget. Two owners are therefore opened with different budgets, and each one's wait must end at its
// own bound: 1s and 300s respectively are far enough apart to tell them apart on the virtual clock.
func TestB09BudgetIsReadFromTheRequestingOwnersScope(t *testing.T) {
	ctx := context.Background()
	m, clock := newPublicManager(t, BudgetLimits{})

	short := t08Waiter(t, m, "root-b09-short", 1*time.Second)
	long := t08Waiter(t, m, "root-b09-long", 300*time.Second)
	if short.scopeID != long.scopeID {
		t.Fatalf("the fixture no longer exercises the shared-id case: %d vs %d", short.scopeID, long.scopeID)
	}

	t09Holder(t, m, "root-b09-holder", "k1")
	shortWait := short.acquire(ctx, indexRecord("t09", "PRIMARY", []byte("k1")), ixIntent("t09"))
	longWait := long.acquire(ctx, indexRecord("t09", "PRIMARY", []byte("k1")), ixIntent("t09"))
	if shortWait.Op != LockOpWait || longWait.Op != LockOpWait {
		t.Fatalf("wait ops = %s / %s, want both WAIT", shortWait.Op, longWait.Op)
	}

	// Past the 1s budget: the short-budget owner is done, the 300s one is untouched.
	clock.Advance(2 * time.Second)
	if expired := m.CheckTimeouts(); expired != 1 {
		t.Fatalf("CheckTimeouts = %d at +2s, want exactly the 1s-budget owner", expired)
	}
	if status := m.WaitStatus("root-b09-short", shortWait.Wait); status != RequestTimedOut {
		t.Fatalf("the 1s-budget owner = %s, want TIMED_OUT", status)
	}
	if status := m.WaitStatus("root-b09-long", longWait.Wait); status != RequestPending {
		t.Fatalf("the 300s-budget owner = %s at +2s, want PENDING", status)
	}

	m.ReleaseOwner("root-b09-holder")
	m.ReleaseOwner("root-b09-short")
	m.ReleaseOwner("root-b09-long")
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after cleanup, want 0", size)
	}
}
