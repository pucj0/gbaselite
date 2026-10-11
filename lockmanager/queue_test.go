package lockmanager

import (
	"context"
	"testing"
	"time"
)

// queue_test.go pins the fairness rule and the single-terminal-status rule.
//
// The two rules are what separate a row lock manager from a global transaction gate, so
// they are asserted as behaviour rather than inferred:
//
//   - Fairness: a young compatible request may not overtake an older incompatible waiter,
//     while an unrelated resource keeps making progress.
//   - Single outcome: a request reaches exactly one status, and a cancellation that races
//     a grant learns that the grant won instead of silently dropping a held lock.
//
// Every test drives the manager through Acquire/Release and observes the result, so a
// regression in the queue is a failing assertion rather than a slow test.

// TestB02OlderExclusiveWaiterBlocksYoungerShared proves the anti-starvation rule: a new
// shared request may not join the holders in front of an older exclusive waiter.
func TestB02OlderExclusiveWaiterBlocksYoungerShared(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")
	c := newLockFixture(t, m, clock, "root-c")

	key := []byte("k1")
	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: key}}

	// root-a holds S, so root-b's X request queues.
	a.acquire(recordRequest("rows", "t1", key, ModeS)...)
	a.requireGranted()
	b.acquire(recordRequest("rows", "t1", key, ModeX)...)
	older := b.requireWaiting()

	// root-c's S request is compatible with the holder but not with the older waiter, so
	// it must queue as well: granting it would let every later shared request extend the
	// exclusive waiter's wait for ever.
	c.acquire(recordRequest("rows", "t1", key, ModeS)...)
	younger := c.requireWaiting()
	if younger.EnqueueOrdinal <= older.EnqueueOrdinal {
		t.Fatalf("younger ordinal %d must be greater than older ordinal %d", younger.EnqueueOrdinal, older.EnqueueOrdinal)
	}
	c.expectNoStatus(younger)
	a.requireNoGrant("root-c", record)

	// The holder releasing hands the lock to the older waiter, not to the younger one.
	a.release(recordRequest("rows", "t1", key, ModeS)...)
	b.expectStatus(older, RequestGranted)
	a.requireHeldMode("root-b", record, ModeX)
	c.expectNoStatus(younger)
	a.requireNoGrant("root-c", record)

	// Only when the exclusive holder is done does the young shared request proceed.
	b.release(recordRequest("rows", "t1", key, ModeX)...)
	c.expectStatus(younger, RequestGranted)
	a.requireHeldMode("root-c", record, ModeS)

	// Releasing everything leaves no entry behind.
	c.release(recordRequest("rows", "t1", key, ModeS)...)
	if m.LockTableSize() != 0 {
		t.Fatalf("lock table = %d entries after a full release, want 0", m.LockTableSize())
	}
}

// TestB02UnrelatedResourcesMakeProgress proves the manager is not a global gate: while
// one key is contended, another key and another table proceed normally.
func TestB02UnrelatedResourcesMakeProgress(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")

	row := func(k string) ResourceRef {
		return ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte(k)}}
	}

	// A holds X on row 1 of table t1, and B waits for that exact row.
	a.acquire(recordRequest("rows", "t1", []byte("k1"), ModeX)...)
	a.requireGranted()
	b.acquire(recordRequest("rows", "t1", []byte("k1"), ModeX)...)
	waiter := b.requireWaiting()

	// A different row is a different resource and must not be blocked by A's row lock.
	b.acquire(recordRequest("rows", "t1", []byte("k2"), ModeX)...)
	b.requireGranted()
	b.requireHeldMode("root-b", row("k2"), ModeX)
	// B's own new grant must not have released or completed its wait.
	b.expectNoStatus(waiter)

	// Another table is likewise independent, and A can still make progress there.
	a.acquire(recordRequest("rows", "t2", []byte("k3"), ModeX)...)
	a.requireGranted()
	a.requireHeldMode("root-a", ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k3")}}, ModeX)

	// The contended row is still the only thing B waits for, and A's release hands it over.
	if got := m.PendingRequests("root-b"); got != 1 {
		t.Fatalf("root-b pending = %d, want only the contended row", got)
	}
	a.release(recordRequest("rows", "t1", []byte("k1"), ModeX)...)
	b.expectStatus(waiter, RequestGranted)
}

// TestB02TableIntentionIsCompatibleAcrossDistinctRows proves the intention rule is stated
// correctly rather than over-applied: two owners may both hold IX on the same table while
// locking different rows, because IX/IX is compatible by contract.
//
// This is the "judgement" the spec asks for at table level: the manager must NOT turn a
// shared table intention into a serialization point between unrelated rows. A test that
// expected the opposite would be demanding a global table gate, which the contract and
// the constitution forbid. The old-waiter rule is what protects a queued request on the
// same row, and that is asserted by
// TestB02OlderExclusiveWaiterBlocksYoungerShared.
func TestB02TableIntentionIsCompatibleAcrossDistinctRows(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")

	// A holds X on row 1, which implies IX on table t1.
	a.acquire(recordRequest("rows", "t1", []byte("k1"), ModeX)...)
	a.requireGranted()
	// B takes IX on the same table for a different row: compatible, so granted.
	b.acquire(recordRequest("rows", "t1", []byte("k2"), ModeX)...)
	b.requireGranted()

	intention := ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: "t1"}}
	if got := holderCountOf(m.Snapshot(), intention); got != 2 {
		t.Fatalf("table t1 IX holders = %d, want 2: IX/IX must be compatible", got)
	}
	// The rows themselves are disjoint and both held.
	if mode, ok := m.HeldMode("root-a", ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k1")}}); !ok || mode != ModeX {
		t.Fatalf("root-a row 1 = %v/%v, want X", mode, ok)
	}
	if mode, ok := m.HeldMode("root-b", ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k2")}}); !ok || mode != ModeX {
		t.Fatalf("root-b row 2 = %v/%v, want X", mode, ok)
	}
	// A third owner that wants row 1 conflicts with A and waits, so the table intention is
	// part of real protection rather than decoration.
	c := newLockFixture(t, m, clock, "root-c")
	c.acquire(recordRequest("rows", "t1", []byte("k1"), ModeX)...)
	handle := c.requireWaiting()
	c.expectNoStatus(handle)
	// A's unrelated row-1 grant is what blocks it, and the edge names that holder.
	edges := m.Edges()
	if len(edges) != 1 || edges[0].ToRootID != "root-a" || edges[0].Reason != EdgeHolder {
		t.Fatalf("edges = %+v, want one HOLDER edge to root-a", edges)
	}
}

// TestB02BatchSharedGrant covers batched acquisition: several shared requests for one row
// are all granted, while an exclusive request that arrives afterwards waits for all of
// them and is then granted once.
func TestB02BatchSharedGrant(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	key := []byte("k1")
	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: key}}

	var holders []*lockFixture
	for i := 0; i < 4; i++ {
		holder := newLockFixture(t, m, clock, rootName(i))
		holder.acquire(recordRequest("rows", "t1", key, ModeS)...)
		holder.requireGranted()
		holders = append(holders, holder)
	}
	if got := holderCountOf(m.Snapshot(), record); got != 4 {
		t.Fatalf("holders = %d, want 4 shared holders", got)
	}

	writer := newLockFixture(t, m, clock, "root-writer")
	writer.acquire(recordRequest("rows", "t1", key, ModeX)...)
	handle := writer.requireWaiting()
	writer.expectNoStatus(handle)

	// Releasing three of the four holders is not enough.
	for i := 0; i < 3; i++ {
		holders[i].release(recordRequest("rows", "t1", key, ModeS)...)
	}
	writer.expectNoStatus(handle)
	// The last release completes the wait.
	holders[3].release(recordRequest("rows", "t1", key, ModeS)...)
	writer.expectStatus(handle, RequestGranted)
	writer.requireHeldMode("root-writer", record, ModeX)
}

// TestB02CancelProducesOneTerminalStatus proves a cancelled request reaches exactly one
// status, is removed from the queue, and hands the resource to the next waiter.
func TestB02CancelProducesOneTerminalStatus(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")
	c := newLockFixture(t, m, clock, "root-c")

	key := []byte("k1")
	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: key}}
	a.acquire(recordRequest("rows", "t1", key, ModeX)...)
	a.requireGranted()

	b.acquire(recordRequest("rows", "t1", key, ModeX)...)
	older := b.requireWaiting()
	c.acquire(recordRequest("rows", "t1", key, ModeX)...)
	younger := c.requireWaiting()

	// Cancelling the older request makes the younger one the head of the queue.
	if status := m.Cancel("root-b", older); status != RequestCanceled {
		t.Fatalf("cancel status = %s, want CANCELED", status)
	}
	if got := m.PendingRequests("root-b"); got != 0 {
		t.Fatalf("root-b pending = %d after cancel, want 0", got)
	}
	// A second cancel reports the same single terminal status and changes nothing.
	if status := m.Cancel("root-b", older); status != RequestCanceled {
		t.Fatalf("second cancel status = %s, want the same CANCELED", status)
	}
	a.release(recordRequest("rows", "t1", key, ModeX)...)
	c.expectStatus(younger, RequestGranted)
	c.requireHeldMode("root-c", record, ModeX)
	// The cancelled request never acquired anything.
	b.requireNoGrant("root-b", record)
	c.release(recordRequest("rows", "t1", key, ModeX)...)
	if m.LockTableSize() != 0 {
		t.Fatalf("lock table = %d entries, want 0", m.LockTableSize())
	}
}

// TestB02GrantBeatsCancel pins the invariant the contract calls out explicitly: when a
// grant and a cancellation race, the grant wins and the caller is told, because a
// cancellation must never silently drop a lock the manager already handed out.
func TestB02GrantBeatsCancel(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")

	key := []byte("k1")
	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: key}}
	a.acquire(recordRequest("rows", "t1", key, ModeX)...)
	a.requireGranted()
	b.acquire(recordRequest("rows", "t1", key, ModeX)...)
	handle := b.requireWaiting()

	// The holder releases first, so the manager grants the queued request.
	a.release(recordRequest("rows", "t1", key, ModeX)...)
	if status := m.WaitStatus("root-b", handle); status != RequestGranted {
		t.Fatalf("wait status = %s, want GRANTED", status)
	}
	// A cancellation arriving after the grant reports the grant that already won and
	// leaves the lock in place.
	if status := m.Cancel("root-b", handle); status != RequestGranted {
		t.Fatalf("cancel after grant = %s, want the winning GRANTED", status)
	}
	b.requireHeldMode("root-b", record, ModeX)
	if got := m.PendingRequests("root-b"); got != 0 {
		t.Fatalf("root-b pending = %d after a grant, want 0", got)
	}
	// The lock is genuinely held: it must be released explicitly.
	b.release(recordRequest("rows", "t1", key, ModeX)...)
	if m.LockTableSize() != 0 {
		t.Fatalf("lock table = %d entries, want 0", m.LockTableSize())
	}
}

// TestB02FailFastRequestReturnsConflict proves a caller that opts out of waiting gets a
// conflict immediately, and that the refused request leaves no queue entry and no charge.
func TestB02FailFastRequestReturnsConflict(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")

	key := []byte("k1")
	a.acquire(recordRequest("rows", "t1", key, ModeX)...)
	a.requireGranted()

	if err := b.acquireNoWait(recordRequest("rows", "t1", key, ModeX)...); err != ErrConflict {
		t.Fatalf("fail-fast acquire = %v, want ErrConflict", err)
	}
	if got := m.PendingRequests("root-b"); got != 0 {
		t.Fatalf("root-b pending = %d after a refused request, want 0", got)
	}
	b.requireNoGrant("root-b", ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: key}})
	// The refused bundle must not have charged the root either: only root-a's record and
	// table intention remain.
	if components := m.Ledger().Totals().Components; components != 2 {
		t.Fatalf("total components = %d, want only root-a's record and intention", components)
	}
}

// TestB02ScopeCloseCancelsScopeWaitersKeepsGrants proves closing a scope cancels only
// that scope's pending requests and never releases a grant.
//
// It also proves the reverse half of the rule: a grant requested from a scope that is then
// closed stays with the root, because a scope records where a request came from and never
// owns the lock.
func TestB02ScopeCloseCancelsScopeWaitersKeepsGrants(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")

	row := func(k string) ResourceRef {
		return ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte(k)}}
	}

	// A holds two keys of the same table and an intention on it.
	a.acquire(recordRequest("rows", "t1", []byte("k0"), ModeX)...)
	a.requireGranted()
	a.acquire(recordRequest("rows", "t1", []byte("k1"), ModeX)...)
	a.requireGranted()

	// The child scope takes its own resource first, so it holds a grant before it is closed.
	child, err := m.OpenScope("root-b", "root-b-child", b.scopeID, ScopeChild, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	childGrant, err := m.Acquire(context.Background(), "root-b", child.ID,
		[]ResourceRef{{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "index/u1", Key: []byte("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	if childGrant.Op != LockOpGranted {
		t.Fatalf("child acquisition op = %s, want GRANTED", childGrant.Op)
	}

	// The statement scope then waits for a key A holds.
	b.acquire(recordRequest("rows", "t1", []byte("k0"), ModeX)...)
	held := b.requireWaiting()
	if got := m.PendingRequests("root-b"); got != 1 {
		t.Fatalf("root-b pending = %d, want 1 wait", got)
	}

	// Closing the child scope cancels nothing (it has no pending request) and keeps the
	// grant it took.
	if _, err := m.CloseScope("root-b", child.ID); err != nil {
		t.Fatal(err)
	}
	if got := m.PendingRequests("root-b"); got != 1 {
		t.Fatalf("root-b pending = %d after closing the child scope, want the statement wait", got)
	}
	b.requireHeldMode("root-b", ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "index/u1", Key: []byte("x")}}, ModeS)

	// A scope that does have a pending request cancels it when closed.
	waiter, err := m.OpenScope("root-b", "root-b-waiter", b.scopeID, ScopeChild, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	blockedResult, err := m.Acquire(context.Background(), "root-b", waiter.ID, recordRequest("rows", "t1", []byte("k1"), ModeX))
	if err != nil {
		t.Fatal(err)
	}
	if blockedResult.Op != LockOpWait {
		t.Fatalf("waiter acquisition op = %s, want WAIT", blockedResult.Op)
	}
	if got := m.PendingRequests("root-b"); got != 2 {
		t.Fatalf("root-b pending = %d, want 2 waits", got)
	}
	if _, err := m.CloseScope("root-b", waiter.ID); err != nil {
		t.Fatal(err)
	}
	if got := m.PendingRequests("root-b"); got != 1 {
		t.Fatalf("root-b pending = %d after closing the waiting scope, want 1", got)
	}
	b.expectStatus(blockedResult.Wait, RequestCanceled)

	// The surviving statement wait is granted when A releases, and the child scope's grant
	// is untouched throughout.
	a.release(recordRequest("rows", "t1", []byte("k0"), ModeX)...)
	b.expectStatus(held, RequestGranted)
	b.requireHeldMode("root-b", row("k0"), ModeX)
	b.requireHeldMode("root-b", ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "index/u1", Key: []byte("x")}}, ModeS)
}

// TestB02ReleaseOwnerWakesEveryWaiter proves a root that disappears does not leave a
// caller blocked: its waits are cancelled and its grants are dropped.
func TestB02ReleaseOwnerWakesEveryWaiter(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")

	key := []byte("k1")
	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: key}}
	a.acquire(recordRequest("rows", "t1", key, ModeX)...)
	a.requireGranted()
	b.acquire(recordRequest("rows", "t1", key, ModeX)...)
	handle := b.requireWaiting()
	b.expectNoStatus(handle)

	// Releasing the waiting root wakes its request with one definite status.
	m.ReleaseOwner("root-b")
	b.expectStatus(handle, RequestCanceled)
	if got := m.PendingRequests("root-b"); got != 0 {
		t.Fatalf("root-b pending = %d after release, want 0", got)
	}
	// The other root's grant is untouched and the row is still held by it.
	a.requireHeldMode("root-a", record, ModeX)

	// Releasing the holder returns the table to empty.
	m.ReleaseOwner("root-a")
	if m.LockTableSize() != 0 {
		t.Fatalf("lock table = %d entries after releasing both roots, want 0", m.LockTableSize())
	}
	requireBudgetZero(t, m.Ledger())
}

// TestB02EpochResetInvalidatesWaiters proves an epoch change wakes blocked callers with an
// invalidation rather than leaving them waiting for a generation that no longer exists.
func TestB02EpochResetInvalidatesWaiters(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")

	key := []byte("k1")
	a.acquire(recordRequest("rows", "t1", key, ModeX)...)
	a.requireGranted()
	b.acquire(recordRequest("rows", "t1", key, ModeX)...)
	handle := b.requireWaiting()

	if invalidated := m.ResetEpoch(2, 7); invalidated != 2 {
		t.Fatalf("invalidated owners = %d, want 2", invalidated)
	}
	b.expectStatus(handle, RequestInvalidated)
	if got := m.PendingRequests("root-b"); got != 0 {
		t.Fatalf("root-b pending = %d after an epoch reset, want 0", got)
	}
	// A root from the replaced generation no longer exists as a lock owner, so it cannot request a
	// lock at all. It is reported as unknown rather than as "ending", because "ending" describes a
	// known owner whose outcome is decided while this one was removed with its generation.
	if err := b.acquireFail(recordRequest("rows", "t1", []byte("k2"), ModeX)...); err != ErrOwnerUnknown {
		t.Fatalf("acquire after an epoch reset = %v, want ErrOwnerUnknown", err)
	}
	// The owner is really gone, not merely blocked: the registry is empty again.
	if owners := m.Stats().ActiveOwners; owners != 0 {
		t.Fatalf("%d lock owners survived the epoch reset, want 0", owners)
	}
}

// TestB02UpgradeWaitsWithoutReleasingShared proves the upgrade rule: a shared holder that
// asks to upgrade queues behind the other shared holders and keeps its S while it waits,
// so a third owner cannot slip in during the upgrade.
func TestB02UpgradeWaitsWithoutReleasingShared(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")
	c := newLockFixture(t, m, clock, "root-c")

	key := []byte("k1")
	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: key}}
	a.acquire(recordRequest("rows", "t1", key, ModeS)...)
	a.requireGranted()
	b.acquire(recordRequest("rows", "t1", key, ModeS)...)
	b.requireGranted()

	// root-a upgrades to X. It must wait for root-b's S.
	a.acquire(recordRequest("rows", "t1", key, ModeX)...)
	upgrade := a.requireWaiting()
	// The S it already held is still held: an upgrade never drops its own protection.
	a.requireHeldMode("root-a", record, ModeS)
	a.requireLockState(record, LockStateWaiting)

	// A third owner cannot enter while the upgrade waits.
	c.acquire(recordRequest("rows", "t1", key, ModeS)...)
	young := c.requireWaiting()
	c.expectNoStatus(young)

	// root-b releasing lets root-a's upgrade through, and root-a now holds X.
	b.release(recordRequest("rows", "t1", key, ModeS)...)
	a.expectStatus(upgrade, RequestGranted)
	a.requireHeldMode("root-a", record, ModeX)
	// The young shared request is still blocked by the new exclusive holder.
	c.expectNoStatus(young)

	// Releasing root-a's exclusive mode hands the row to the young shared request. The row
	// is released twice because root-a holds both the S it started with and the X it
	// upgraded to, and each release retracts exactly one of them.
	a.release(recordRequest("rows", "t1", key, ModeX)...)
	a.release(recordRequest("rows", "t1", key, ModeX)...)
	c.expectStatus(young, RequestGranted)
	c.requireHeldMode("root-c", record, ModeS)
}
