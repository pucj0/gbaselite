package lockmanager

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

// grant_contract_test.go covers two Lock Manager contract defects that are invisible to a single
// transaction and only appear when an owner already has protection of its own:
//
//  1. A claim that is only *queued* is not protection. An owner that holds S and has asked for X
//     does not hold X, so repeating the X request must not report REENTRANT, must not be granted,
//     and must not become a second request; it joins the request that is already waiting, which is
//     the only owner of that request's single terminal status.
//  2. The grant a waiter receives must be the grant *of its own request*: its own GrantID, its own
//     origin scope, exactly the components that request acquired or legitimately adopted, and the
//     owner's live generation and term. It must never be the owner's whole lock inventory.
//
// Every ordering claim here is established with a deliberately queued request and the exported
// façade. Nothing sleeps, and nothing depends on map iteration order: the assertions hold for
// every interleaving the manager can produce.

// ownedComponent is one (resource, mode) pair, which is what "the grant covers exactly this" means.
type ownedComponent struct {
	key  string
	mode Mode
}

// grantContents renders a component list as a sorted set of resource identities and modes.
func grantContents(components []ResourceRef) []ownedComponent {
	out := make([]ownedComponent, 0, len(components))
	for _, component := range components {
		out = append(out, ownedComponent{key: component.ResourceKey(), mode: component.Mode})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key != out[j].key {
			return out[i].key < out[j].key
		}
		return out[i].mode < out[j].mode
	})
	return out
}

// requireGrantCoversExactly asserts that a grant covers one component set and nothing else.
func requireGrantCoversExactly(t *testing.T, what string, grant LockGrant, want ...ResourceRef) {
	t.Helper()
	got := grantContents(grant.Components)
	expected := grantContents(want)
	if len(got) != len(expected) {
		t.Fatalf("%s covers %d components (%v), want %d (%v)", what, len(got), got, len(expected), expected)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("%s covers %v, want exactly %v", what, got, expected)
		}
	}
}

// requireNoAppliedClaim asserts that an owner holds no applied claim on a resource, which is how a
// test proves that a queued request is not protection.
func requireNoAppliedClaim(t *testing.T, m *Manager, rootID string, resource ResourceRef) {
	t.Helper()
	if mode, held := m.HeldMode(rootID, resource); held {
		t.Fatalf("%s already holds %s on %s; the request is only queued", rootID, mode, resource.Key)
	}
}

// requireAppliedMode asserts the exact mode an owner holds on a resource. It is the assertion that
// separates "asked for X" from "granted X" when the owner already holds a weaker mode.
func requireAppliedMode(t *testing.T, m *Manager, rootID string, resource ResourceRef, want Mode) {
	t.Helper()
	mode, held := m.HeldMode(rootID, resource)
	if !held {
		t.Fatalf("%s holds nothing on %s, want %s", rootID, resource.Key, want)
	}
	if mode != want {
		t.Fatalf("%s holds %s on %s, want %s (an upgrade that only queued is not a grant)", rootID, mode, resource.Key, want)
	}
}

// TestB02PendingUpgradeIsNotReportedAsGranted is the defect this round measured first.
//
// Two owners hold S on one row; the first asks to upgrade to X and therefore waits. Repeating the
// upgrade while it waits used to be answered with LockOpReentrant — "you already hold it" — for a
// mode the owner had never been granted. The repeat must instead join the queued request.
func TestB02PendingUpgradeIsNotReportedAsGranted(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")
	key := []byte("k1")

	shared := sharedBundle("t1", "PRIMARY", key)
	exclusive := rowBundle(indexRecordOf(ModeX, "t1", "PRIMARY", key))
	a.grant(ctx, shared...)
	b.grant(ctx, shared...)

	// The upgrade cannot join b's shared lock, so it becomes the owner's queued request. The owner
	// still holds only S: the X it asked for is queued, not applied.
	upgrade := a.wait(ctx, exclusive...)
	exclusiveRecord := indexRecordOf(ModeX, "t1", "PRIMARY", key)
	requireAppliedMode(t, m, "root-a", exclusiveRecord, ModeS)

	before, _ := m.Ledger().Usage("root-a")

	// The repeat must not be answered as a grant. It must resolve to the request already waiting,
	// so both callers observe that request's one terminal status.
	repeat := a.acquire(ctx, exclusive...)
	switch repeat.Op {
	case LockOpWait:
	case LockOpReentrant, LockOpGranted:
		t.Fatalf("a repeat of a pending upgrade was reported as %s; the owner holds S, not X", repeat.Op)
	default:
		t.Fatalf("repeat op = %s, want WAIT", repeat.Op)
	}
	if repeat.Wait == nil || repeat.Wait.RequestID != upgrade.Wait.RequestID {
		t.Fatalf("the repeat did not join request %d (got %+v)", upgrade.Wait.RequestID, repeat.Wait)
	}
	if pending := m.PendingRequests("root-a"); pending != 1 {
		t.Fatalf("pending requests for root-a = %d, want the single queued upgrade", pending)
	}
	after, _ := m.Ledger().Usage("root-a")
	if after.Components != before.Components || after.MemoryBytes != before.MemoryBytes {
		t.Fatalf("a repeat of a queued request changed the accounting: %+v -> %+v", before, after)
	}
	requireAppliedMode(t, m, "root-a", exclusiveRecord, ModeS)

	// When the blocker ends, the one request is granted and both callers see it.
	if _, err := m.Release("root-b", indexRecordOf(ModeS, "t1", "PRIMARY", key)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("root-b", isIntent("t1")); err != nil {
		t.Fatal(err)
	}
	grant, err := m.WaitForGrant(ctx, "root-a", upgrade.Wait)
	if err != nil {
		t.Fatalf("the upgrade was not granted after the blocker released: %v", err)
	}
	if status := repeat.Wait.Status(); status != RequestGranted {
		t.Fatalf("the joined handle reports %s, want GRANTED", status)
	}
	if mode, held := m.HeldMode("root-a", exclusiveRecord); !held || mode != ModeX {
		t.Fatalf("root-a holds %s (held=%v) after the upgrade, want X", mode, held)
	}
	requireGrantCoversExactly(t, "the upgrade grant", grant, exclusive...)
}

// TestB02QueuedOwnClaimIsNotProtection pins the second face of the same defect: a queued request of
// the same owner must not let a later request settle as if that protection were already held.
//
// The owner queues [k1 S, k2 S] and is blocked on k2, so its k1 claim is queued, not applied. A
// second request [k1 S, k4 S] must not be completed by adopting that queued claim when k4 becomes
// free: doing so would report a grant whose k1 protection does not exist.
func TestB02QueuedOwnClaimIsNotProtection(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	k1 := indexRecordOf(ModeS, "t1", "PRIMARY", []byte("k1"))
	k2 := indexRecordOf(ModeS, "t1", "PRIMARY", []byte("k2"))
	k4 := indexRecordOf(ModeS, "t1", "PRIMARY", []byte("k4"))
	intentS := isIntent("t1")

	// b holds k2 and k4 exclusively, so a's shared requests for them cannot be satisfied.
	b.grant(ctx, rowBundle(indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k2")))...)
	b.grant(ctx, rowBundle(indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k4")))...)

	first := a.wait(ctx, k1, k2, intentS)
	requireNoAppliedClaim(t, m, "root-a", k1)

	second := a.wait(ctx, k1, k4, intentS)
	requireNoAppliedClaim(t, m, "root-a", k1)

	// k4 becomes free. The second request still needs k1, whose only claim is the first request's
	// queued one, so it must stay queued rather than be reported as granted.
	if _, err := m.Release("root-b", indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k4"))); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("root-b", ixIntent("t1")); err != nil {
		t.Fatal(err)
	}
	if status := second.Wait.Status(); status != RequestPending {
		t.Fatalf("the second request is %s while its k1 claim is only queued, want PENDING", status)
	}
	requireNoAppliedClaim(t, m, "root-a", k1)

	// Once the first request is granted its claim is applied, and the second request may adopt it.
	if _, err := m.Release("root-b", indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k2"))); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitForGrant(ctx, "root-a", first.Wait); err != nil {
		t.Fatalf("the first request was not granted: %v", err)
	}
	grant, err := m.WaitForGrant(ctx, "root-a", second.Wait)
	if err != nil {
		t.Fatalf("the second request was not granted after the first one was: %v", err)
	}
	if mode, held := m.HeldMode("root-a", k1); !held || mode != ModeS {
		t.Fatalf("root-a holds %s (held=%v) on k1, want S", mode, held)
	}
	requireGrantCoversExactly(t, "the second grant", grant, k1, k4, intentS)
}

// TestB02RepeatRequestJoinsTheQueuedRequest proves the join keeps one request, one queue position
// and one terminal status for every caller.
func TestB02RepeatRequestJoinsTheQueuedRequest(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	k1 := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k1"))
	k2 := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k2"))
	intentX := ixIntent("t1")

	b.grant(ctx, k1, k2, intentX)
	first := a.wait(ctx, k1, k2, intentX)
	repeat := a.acquire(ctx, k1, k2, intentX)
	if repeat.Op != LockOpWait || repeat.Wait.RequestID != first.Wait.RequestID {
		t.Fatalf("repeat = %+v, want the queued request %d", repeat, first.Wait.RequestID)
	}
	if pending := m.PendingRequests("root-a"); pending != 1 {
		t.Fatalf("pending requests = %d, want 1", pending)
	}

	// A partial release leaves the joined request queued: it still needs both components.
	if _, err := m.Release("root-b", k1); err != nil {
		t.Fatal(err)
	}
	if status := first.Wait.Status(); status != RequestPending {
		t.Fatalf("status after a partial release = %s, want PENDING", status)
	}

	if _, err := m.Release("root-b", k2); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("root-b", intentX); err != nil {
		t.Fatal(err)
	}
	one, err := m.WaitForGrant(ctx, "root-a", first.Wait)
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.WaitForGrant(ctx, "root-a", repeat.Wait)
	if err != nil {
		t.Fatal(err)
	}
	if one.GrantID == 0 || one.GrantID != two.GrantID {
		t.Fatalf("the two callers saw grants %d and %d, want one identity", one.GrantID, two.GrantID)
	}
	requireGrantCoversExactly(t, "the joined grant", one, k1, k2, intentX)
}

// TestB02TryAcquireOnOwnQueuedRequestConflicts proves a fail-fast acquisition never reports a queued
// request as held protection: it refuses instead of waiting or claiming a grant.
func TestB02TryAcquireOnOwnQueuedRequestConflicts(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	k1 := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k1"))
	intentX := ixIntent("t1")
	b.grant(ctx, k1, intentX)
	a.wait(ctx, k1, intentX)

	if _, err := m.TryAcquire(ctx, "root-a", a.scopeID, []ResourceRef{k1, intentX}); !errors.Is(err, ErrConflict) {
		t.Fatalf("TryAcquire on the owner's own queued request = %v, want ErrConflict", err)
	}
	requireNoAppliedClaim(t, m, "root-a", k1)
}

// TestB02WaitedGrantCarriesOnlyItsOwnIdentity is the second defect: a grant rebuilt after a wait
// used to carry every resource the owner held anywhere, the request id in place of the origin
// scope, and no grant id at all.
func TestB02WaitedGrantCarriesOnlyItsOwnIdentity(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	// The owner already holds an unrelated row, which must not leak into the new grant.
	unrelated := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k9"))
	earlier := a.grant(ctx, rowBundle(unrelated)...)

	// A second, distinct scope is the origin of the request that has to wait.
	second, err := m.OpenScope("root-a", "root-a-second", 0, ScopeStatement, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	wanted := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k2"))
	intentX := ixIntent("t1")
	b.grant(ctx, wanted, intentX)

	queued, err := m.Acquire(ctx, "root-a", second.ID, []ResourceRef{wanted, intentX})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Op != LockOpWait || queued.Wait == nil {
		t.Fatalf("acquisition op = %s, want WAIT", queued.Op)
	}
	if _, err := m.Release("root-b", wanted); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("root-b", intentX); err != nil {
		t.Fatal(err)
	}

	grant, err := m.WaitForGrant(ctx, "root-a", queued.Wait)
	if err != nil {
		t.Fatalf("the queued request was not granted: %v", err)
	}
	if grant.GrantID == 0 {
		t.Fatal("the grant a waiter received carries no grant id")
	}
	if grant.GrantID == earlier.Grant.GrantID {
		t.Fatalf("the waited grant reused the identity %d of the owner's earlier request; each request that takes new protection gets its own", grant.GrantID)
	}
	if grant.OriginScopeID != second.ID {
		t.Fatalf("origin scope = %d, want the requesting scope %d", grant.OriginScopeID, second.ID)
	}
	if grant.OwnerID != "root-a" {
		t.Fatalf("owner = %q, want root-a", grant.OwnerID)
	}
	owner, ok := m.Owner("root-a")
	if !ok {
		t.Fatal("root-a is not registered")
	}
	if grant.Generation != owner.Generation || grant.Term != owner.Term {
		t.Fatalf("grant epoch = (%d,%d), want the owner's (%d,%d)", grant.Generation, grant.Term, owner.Generation, owner.Term)
	}
	requireGrantCoversExactly(t, "the waited grant", grant, wanted, intentX)
}

// TestB02MultiResourceWaitedGrantCoversItsOwnBundle proves a multi-component bundle's grant is the
// bundle's own components, not a prefix or a superset of the owner's inventory.
func TestB02MultiResourceWaitedGrantCoversItsOwnBundle(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	heldElsewhere := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k7"))
	a.grant(ctx, rowBundle(heldElsewhere)...)

	first := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k1"))
	second := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k2"))
	intentX := ixIntent("t1")
	b.grant(ctx, first, second, intentX)

	queued := a.wait(ctx, first, second, intentX)
	for _, resource := range []ResourceRef{first, second} {
		if _, err := m.Release("root-b", resource); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Release("root-b", intentX); err != nil {
		t.Fatal(err)
	}
	grant, err := m.WaitForGrant(ctx, "root-a", queued.Wait)
	if err != nil {
		t.Fatal(err)
	}
	requireGrantCoversExactly(t, "the multi-resource grant", grant, first, second, intentX)
}

// TestB02WaitedGrantIsTheSameForEveryWaiter proves the resolved grant is stable: two reads of one
// handle agree on the identity, and the returned byte slices are independent copies.
func TestB02WaitedGrantIsTheSameForEveryWaiter(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	wanted := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k1"))
	intentX := ixIntent("t1")
	b.grant(ctx, wanted, intentX)
	queued := a.wait(ctx, wanted, intentX)
	if _, err := m.Release("root-b", wanted); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("root-b", intentX); err != nil {
		t.Fatal(err)
	}

	first, err := m.WaitForGrant(ctx, "root-a", queued.Wait)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.WaitForGrant(ctx, "root-a", queued.Wait)
	if err != nil {
		t.Fatal(err)
	}
	if first.GrantID != second.GrantID || first.OriginScopeID != second.OriginScopeID {
		t.Fatalf("two reads of one handle disagree: %+v vs %+v", first, second)
	}
	requireGrantCoversExactly(t, "the repeated read", second, wanted, intentX)
	if len(first.Components) > 0 && len(second.Components) > 0 {
		if &first.Components[0] == &second.Components[0] {
			t.Fatal("two readers share one component slice; a caller could corrupt the other's grant")
		}
		first.Components[0].Key = []byte("mutated")
		if string(second.Components[0].Key) == "mutated" {
			t.Fatal("mutating one reader's grant changed another reader's grant")
		}
	}
}

// TestB02GrantIdentitySurvivesReentrantAndAdoptedAcquisitions proves the two legitimate reuse paths
// keep working: a re-entrant acquisition reports the protection the owner already has and no new
// grant identity, and an adopted component is part of the waiting request's own grant.
func TestB02GrantIdentitySurvivesReentrantAndAdoptedAcquisitions(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	held := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k1"))
	intentX := ixIntent("t1")
	first := a.grant(ctx, held, intentX)

	repeat := a.acquire(ctx, held, intentX)
	if repeat.Op != LockOpReentrant {
		t.Fatalf("repeat op = %s, want REENTRANT", repeat.Op)
	}
	if repeat.Grant.GrantID != 0 {
		t.Fatalf("a re-entrant acquisition reported grant id %d; it took no new lock", repeat.Grant.GrantID)
	}
	if repeat.Grant.OriginScopeID != a.scopeID {
		t.Fatalf("re-entrant origin scope = %d, want the requesting scope %d", repeat.Grant.OriginScopeID, a.scopeID)
	}
	requireGrantCoversExactly(t, "the re-entrant grant", repeat.Grant, held, intentX)
	if first.Grant.GrantID == 0 {
		t.Fatal("the first acquisition carries no grant id")
	}

	// An adopted component: the owner already holds k1, so the waiting bundle adopts it and takes
	// its own claim only for k2. The grant of that request covers both.
	blocked := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k2"))
	b.grant(ctx, blocked, intentX)
	queued := a.wait(ctx, held, blocked, intentX)
	for _, resource := range []ResourceRef{blocked, intentX} {
		if _, err := m.Release("root-b", resource); err != nil {
			t.Fatal(err)
		}
	}
	grant, err := m.WaitForGrant(ctx, "root-a", queued.Wait)
	if err != nil {
		t.Fatal(err)
	}
	requireGrantCoversExactly(t, "the adop",
		grant, held, blocked, intentX)
	// Releasing one lock must really release it: an adopted component leaves no second claim.
	if _, err := m.Release("root-a", held); err != nil {
		t.Fatal(err)
	}
	if state := m.LockStateFor(held); state != LockStateFree {
		t.Fatalf("k1 state after its release = %s, want FREE (an adopted claim survived)", state)
	}
}

// TestB02WaitedGrantFailsClosedOnceItsOwnerEnds proves the capability is not resurrected after the
// root that owns it finishes: the wait resolves once, and the ended owner's handle is refused.
func TestB02WaitedGrantFailsClosedOnceItsOwnerEnds(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	wanted := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k1"))
	intentX := ixIntent("t1")
	b.grant(ctx, wanted, intentX)
	queued := a.wait(ctx, wanted, intentX)
	if _, err := m.Release("root-b", wanted); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("root-b", intentX); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitForGrant(ctx, "root-a", queued.Wait); err != nil {
		t.Fatalf("the grant was not resolved: %v", err)
	}

	m.ReleaseOwner("root-a")
	if _, err := m.WaitForGrant(ctx, "root-a", queued.Wait); !errors.Is(err, ErrOwnerUnknown) {
		t.Fatalf("a released owner's handle resolved to %v, want ErrOwnerUnknown", err)
	}
}

// TestB02CancelledWaiterResolvesNoGrant proves a cancelled request never produces a grant, and that
// the cancel is the single terminal status the waiter observes.
func TestB02CancelledWaiterResolvesNoGrant(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	wanted := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k1"))
	intentX := ixIntent("t1")
	b.grant(ctx, wanted, intentX)
	queued := a.wait(ctx, wanted, intentX)

	if status := m.Cancel("root-a", queued.Wait); status != RequestCanceled {
		t.Fatalf("cancel = %s, want CANCELED", status)
	}
	if _, err := m.WaitForGrant(ctx, "root-a", queued.Wait); !errors.Is(err, ErrConflict) {
		t.Fatalf("a cancelled waiter resolved to %v, want ErrConflict", err)
	}
	// The blocker's release must not resurrect the cancelled request.
	if _, err := m.Release("root-b", wanted); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("root-b", intentX); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitForGrant(ctx, "root-a", queued.Wait); !errors.Is(err, ErrConflict) {
		t.Fatalf("after the release a cancelled waiter resolved to %v, want ErrConflict", err)
	}
	requireNoAppliedClaim(t, m, "root-a", wanted)
}
