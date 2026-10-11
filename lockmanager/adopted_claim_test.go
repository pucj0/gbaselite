package lockmanager

import "testing"

// TestB02AdoptedComponentLeavesNoResidualClaim pins the "duplicate request and upgrade
// residue" half of the DML wake investigation.
//
// A bundle can carry a component its owner already holds through an earlier request; that
// component is *adopted* rather than claimed a second time. The adopted component must
// leave the owner with exactly the claim it already had: a second, never-retracted claim
// would keep the resource locked after the owner released the lock it actually took, and
// would report a mode the owner never requested.
func TestB02AdoptedComponentLeavesNoResidualClaim(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")

	first := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k1")}}
	second := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k2")}}

	// A holds k1; B holds k2.
	a.acquire(recordRequest("rows", "t1", []byte("k1"), ModeX)...)
	a.requireGranted()
	b.acquire(recordRequest("rows", "t1", []byte("k2"), ModeX)...)
	b.requireGranted()

	// A's next bundle covers k1 (already held, so adopted) and k2 (blocked by B).
	components := append(recordRequest("rows", "t1", []byte("k1"), ModeX), recordRequest("rows", "t1", []byte("k2"), ModeX)...)
	a.acquire(components...)
	handle := a.requireWaiting()

	// B ends, which is what lets A's bundle complete.
	m.ReleaseOwner("root-b")
	a.expectStatus(handle, RequestGranted)
	a.requireHeldMode("root-a", first, ModeX)
	a.requireHeldMode("root-a", second, ModeX)

	// Releasing k1 must actually release it. A residual adopted claim keeps the resource
	// exclusive and a later request on it would wait for a lock nobody holds.
	a.release(first)
	if state := m.LockStateFor(first); state != LockStateFree {
		t.Fatalf("k1 state after its release = %s, want %s (a claim survived the release)", state, LockStateFree)
	}
	m.ReleaseOwner("root-a")
	requireBudgetZero(t, m.Ledger())
}
