package lockmanager

import (
	"testing"
)

// TestB02WaiterWakesAfterHolderReleaseFollowingACompletedRound reproduces the
// reported UPDATE/DELETE wake failure at the lock-manager level.
//
// Shape (recorded in tasks.md / .tmp/b02-phase13-dml-validation.md §5.1):
//
//	round 1: A1 and B1 each take an exclusive record lock on a *different* row and
//	         then both end, releasing everything;
//	round 2: A2 takes the row A1 had, B2 requests the same row and must wait,
//	         then A2 ends and B2 must be woken with a grant.
//
// The failure under test is "the waiter stays queued past its holder's release".
// It is asserted without a deadline: the manager's clock is virtual, so the pass
// must be decided by the release and never by a timeout.
func TestB02WaiterWakesAfterHolderReleaseFollowingACompletedRound(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})

	key10 := []byte("k10")
	key11 := []byte("k11")
	row10 := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: key10}}

	// Round 1: two owners take different rows, then both end.
	a1 := newLockFixture(t, m, clock, "round1-a")
	b1 := newLockFixture(t, m, clock, "round1-b")
	a1.acquire(recordRequest("rows", "t1", key10, ModeX)...)
	a1.requireGranted()
	b1.acquire(recordRequest("rows", "t1", key11, ModeX)...)
	b1.requireGranted()
	m.ReleaseOwner("round1-a")
	m.ReleaseOwner("round1-b")
	requireBudgetZero(t, m.Ledger())
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d entries after every owner ended, want 0", size)
	}

	// Round 2: one holder, one waiter on the same row.
	a2 := newLockFixture(t, m, clock, "round2-a")
	b2 := newLockFixture(t, m, clock, "round2-b")
	a2.acquire(recordRequest("rows", "t1", key10, ModeX)...)
	a2.requireGranted()
	b2.acquire(recordRequest("rows", "t1", key10, ModeX)...)
	handle := b2.requireWaiting()
	if pending := m.PendingRequests("round2-b"); pending != 1 {
		t.Fatalf("round2-b pending requests = %d, want 1", pending)
	}
	if state := m.LockStateFor(row10); state != LockStateWaiting {
		t.Fatalf("row state = %s while a request is queued, want %s", state, LockStateWaiting)
	}

	// The holder ends. That release is the only event that may decide the waiter.
	m.ReleaseOwner("round2-a")

	b2.expectStatus(handle, RequestGranted)
	b2.requireHeldMode("round2-b", row10, ModeX)
	m.ReleaseOwner("round2-b")
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d entries after the second round ended, want 0", size)
	}
	requireBudgetZero(t, m.Ledger())
}
