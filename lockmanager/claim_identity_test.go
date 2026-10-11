package lockmanager

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// claim_identity_test.go is the B07 reproduction suite for the claim-identity defect of the B02
// lock manager's interval stage.
//
// The defect is a conflation of two different questions that `Identity.ResourceKey()` cannot answer
// as one. `ResourceKey()` is the **conflict domain**: for an interval-type mode (a Gap or an Insert
// Intention) it is (Generation, Space) with no bounds and no insertion key, and that is deliberate —
// a Gap and an Insert Intention of one namespace have to meet in one place so `componentsConflict`
// can decide them by containment, and two gaps of one namespace have to be compared with each other
// as well. The bug is that the same string was *also* used as if it named one specific claim, so
// distinct locks collapsed into one:
//
//   - two different gaps in one namespace were treated as one lock, so a second gap request was
//     answered `LockOpReentrant` although nothing protected the second range;
//   - two different insertion positions in one namespace were treated as one lock, so an earlier
//     `II(b)` "satisfied" a request for `II(y)`;
//   - a queued `Gap(a,m)` was treated as already covering a new request for `Gap(n,z)`, so the new
//     acquisition joined the old queued request instead of taking its own protection;
//   - releasing one claim could release the whole conflict domain, and `HeldMode` reported a claim
//     the caller had never asked about;
//   - in a bundle of `[Gap(a,m), II(b), X(b)]` the dedupe (by domain key, keeping the stronger mode
//     rank) dropped the Gap entirely, because the insertion intention outranks it.
//
// The correct model this file pins is:
//
//   - the **conflict domain** decides *which* claims must be compared with each other;
//   - **claim identity** is the exact resource: the same generation, kind and space, plus the same
//     bounds and inclusivity for a Gap or the same key for an Insert Intention, with the mode
//     compared separately as a strength;
//   - a claim satisfies a request only when the resource is *the same* and the held mode is
//     equal-or-stronger (`upgradeNeeded(request, held) == false`);
//   - two different gaps are two claims, two different insertion positions are two claims, and a
//     bigger gap does not satisfy a smaller one: the two are simply compatible, so the smaller one
//     is granted as its own claim.
//
// Every case asserts both halves of an outcome: the operation the manager reported (the op, or the
// absence of an error) *and* the resource state that must accompany it — exact held claims through
// `HeldMode` on the exact interval or key, the number of queued requests, the wait edges, and a
// final idle assertion after every owner is released. An outcome-only assertion would accept a grant
// that was never really decided, and a state-only assertion would accept state that was never
// cleaned up.
//
// Nothing here sleeps, polls or infers order from elapsed time. The fake clock is never advanced, so
// no request can time out, and every wait these tests observe was settled synchronously by the
// manager call that caused it. `go test -race` is unavailable on this host (no C toolchain), so no
// case in this file claims race coverage.
//
// One deliberate limitation: because the interval domain key is (Generation, Space), two gaps of one
// namespace live in ONE lock-table entry. The exact-interval assertions below therefore go through
// `HeldMode(rootID, <exact interval>)` — and never through `LockTableSize()` — so a manager that
// keeps one entry per domain (which is correct) is not mistaken for one that keeps one claim.

const (
	// t07Space is the namespace every component of this file addresses. It is the
	// `index/<table>/<index>` layout the write path uses, so the identity a case asserts is the same
	// identity production builds.
	t07Space = "index/t07/PRIMARY"
	// t07Table is the opaque table label the table-intent prerequisite needs.
	t07Table = "t07"
)

// t07Gap builds a closed Gap over [lower, upper] in the shared namespace. The bounds are always
// non-nil and inclusive, so a nil bound can never be confused with a real one in a comparison.
func t07Gap(lower, upper string) ResourceRef {
	return ResourceRef{Identity: Identity{
		Kind:           KindInterval,
		Mode:           ModeGap,
		Space:          t07Space,
		TableID:        t07Table,
		IndexID:        "PRIMARY",
		Lower:          []byte(lower),
		Upper:          []byte(upper),
		LowerInclusive: true,
		UpperInclusive: true,
	}}
}

// t07II builds the Insert Intention on one exact key. The kind is `KindRecord` on purpose: the
// interval *identity* is the domain, so this component and a record X on the same key are different
// resources — one is decided by containment, the other by the record matrix.
func t07II(key string) ResourceRef {
	return ResourceRef{Identity: Identity{
		Kind:    KindRecord,
		Mode:    ModeInsertIntention,
		Space:   t07Space,
		TableID: t07Table,
		IndexID: "PRIMARY",
		Key:     []byte(key),
	}}
}

// t07RowX builds the exclusive record reservation of one key in the insertion namespace. It is the
// second half of an insertion claim: without it the bundle is refused by
// `ErrInsertIntentionRequiresRecord`.
func t07RowX(key string) ResourceRef {
	return ResourceRef{Identity: Identity{
		Kind:    KindRecord,
		Mode:    ModeX,
		Space:   t07Space,
		TableID: t07Table,
		IndexID: "PRIMARY",
		Key:     []byte(key),
	}}
}

// t07TableIX is the table intention a record X or an Insert Intention requires in its own bundle. It
// is shared by every owner and is never the point of an assertion here: it only makes the bundle
// legal, so a failure can never be blamed on the intention prerequisite.
func t07TableIX() ResourceRef {
	return ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: t07Table}}
}

// t07InsertClaim is the atomic insertion claim of one key: the intention on the position, the
// exclusive record reservation of the same key, and the table intention both require.
func t07InsertClaim(key string) []ResourceRef {
	return []ResourceRef{t07II(key), t07RowX(key), t07TableIX()}
}

// t07GapBundle pairs one Gap with the table intention a bundle requires.
func t07GapBundle(lower, upper string) []ResourceRef {
	return []ResourceRef{t07Gap(lower, upper), t07TableIX()}
}

// t07SameBound compares two bounds with nil distinct from an empty non-nil slice, because the
// contract makes nil "infinite" and an empty slice "a real bound that sorts first". Treating them as
// equal would let a test accept an unbounded gap for a bounded one.
func t07SameBound(a, b []byte) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return bytes.Equal(a, b)
}

// t07SameLock reports whether two components name the very same lock: same generation, kind, space,
// labels, bounds or key, inclusivity and mode. It is the test-side equality the grant-content
// assertions need, and it is deliberately stricter than "the manager would compare these two".
func t07SameLock(a, b ResourceRef) bool {
	return a.Generation == b.Generation &&
		a.Kind == b.Kind &&
		a.Space == b.Space &&
		a.TableID == b.TableID &&
		a.IndexID == b.IndexID &&
		a.Mode == b.Mode &&
		bytes.Equal(a.Key, b.Key) &&
		t07SameBound(a.Lower, b.Lower) &&
		t07SameBound(a.Upper, b.Upper) &&
		a.LowerInclusive == b.LowerInclusive &&
		a.UpperInclusive == b.UpperInclusive
}

// t07Render renders one component for a failure message, so a failing assertion names the exact
// interval or key instead of only its mode.
func t07Render(ref ResourceRef) string {
	rendered := fmt.Sprintf("%s/%s@%s", ref.Mode, ref.Kind, ref.Space)
	if ref.Mode == ModeGap || ref.Kind == KindInterval {
		rendered += fmt.Sprintf("[%q,%q]", string(ref.Lower), string(ref.Upper))
	}
	if ref.Key != nil {
		rendered += fmt.Sprintf(" key=%q", string(ref.Key))
	}
	return rendered
}

// t07RenderRefs renders a component list for a failure message.
func t07RenderRefs(refs []ResourceRef) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, t07Render(ref))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// t07Describe renders an acquisition outcome, including the request it joined when it queued. That
// matters for this defect: the pre-fix failure mode is a second acquisition that joins the first
// queued request, and the message must show which request it joined.
func t07Describe(result AcquireResult) string {
	if result.Wait != nil {
		return fmt.Sprintf("%s(request %d, ordinal %d)", result.Op, result.Wait.RequestID, result.Wait.EnqueueOrdinal)
	}
	if result.Op == LockOpGranted {
		return fmt.Sprintf("%s(grant %d)", result.Op, result.Grant.GrantID)
	}
	return string(result.Op)
}

// t07RequireGrantNames asserts that a grant's component list names every wanted lock exactly.
func t07RequireGrantNames(t *testing.T, what string, grant LockGrant, want ...ResourceRef) {
	t.Helper()
	for _, expected := range want {
		found := false
		for _, got := range grant.Components {
			if t07SameLock(got, expected) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s components = %s, want it to name %s", what, t07RenderRefs(grant.Components), t07Render(expected))
		}
	}
}

// t07RequireNoForeignIntervalClaim asserts that every interval-type component of a grant is one of
// the allowed exact claims. A grant that named an interval nobody asked for would be a claim the
// caller never took, and a grant that named a *different* interval would be the collapse this file
// exists to catch.
func t07RequireNoForeignIntervalClaim(t *testing.T, what string, grant LockGrant, allowed ...ResourceRef) {
	t.Helper()
	for _, got := range grant.Components {
		if got.Kind != KindInterval && got.Mode != ModeInsertIntention {
			continue
		}
		matched := false
		for _, expected := range allowed {
			if t07SameLock(got, expected) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("%s components = %s, want no interval claim beyond %s",
				what, t07RenderRefs(grant.Components), t07RenderRefs(allowed))
		}
	}
}

// t07RequireRefsName asserts that a component list names every wanted lock exactly. It is the dedupe
// half of the assertions: dedupe must keep the caller's distinct locks.
func t07RequireRefsName(t *testing.T, what string, refs []ResourceRef, want ...ResourceRef) {
	t.Helper()
	for _, expected := range want {
		found := false
		for _, got := range refs {
			if t07SameLock(got, expected) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s = %s, want it to name %s", what, t07RenderRefs(refs), t07Render(expected))
		}
	}
}

// t07RequireHeld asserts that one root holds the mode on the *exact* resource. This is the assertion
// the defect breaks first: a domain-wide answer would report a Gap(a,m) as held when the caller asked
// about Gap(n,z).
func t07RequireHeld(t *testing.T, m *Manager, rootID string, ref ResourceRef, want Mode) {
	t.Helper()
	got, ok := m.HeldMode(rootID, ref)
	if !ok {
		t.Fatalf("%s does not hold %s, want %s: a held claim must be observable on its own exact identity", rootID, t07Render(ref), want)
	}
	if got != want {
		t.Fatalf("%s holds %s on %s, want %s", rootID, got, t07Render(ref), want)
	}
}

// t07RequireGrantHeld asserts that every component a grant names is really held at the granted mode.
//
// It is the one-bundle-two-claims check: a grant is built from the request's component list, so it
// can name a component whose claim was never recorded for that exact identity. Reporting the grant
// and the snapshot together makes that contradiction visible instead of merely "not held".
func t07RequireGrantHeld(t *testing.T, m *Manager, rootID string, grant LockGrant, want ...ResourceRef) {
	t.Helper()
	for _, ref := range want {
		mode, ok := m.HeldMode(rootID, ref)
		if !ok || mode != ref.Mode {
			t.Fatalf("grant %d names %s, but %s holds %v/%v on it; grant components = %s, snapshot = %+v",
				grant.GrantID, t07Render(ref), rootID, mode, ok, t07RenderRefs(grant.Components), m.Snapshot())
		}
	}
}

// t07RequireNotHeld asserts that one root holds nothing on the exact resource. `HeldMode` counts
// applied claims only, so a claim belonging to a request that is still queued is correctly "not
// held" here.
func t07RequireNotHeld(t *testing.T, m *Manager, rootID string, ref ResourceRef) {
	t.Helper()
	if mode, ok := m.HeldMode(rootID, ref); ok {
		t.Fatalf("%s unexpectedly holds %s on %s, want no held claim on that exact resource", rootID, mode, t07Render(ref))
	}
}

// t07Release drops one claim of one root per component, asserting that each release really retracted
// a claim. A release that reported false would leave the test's cleanup incomplete while looking
// like it succeeded.
func t07Release(t *testing.T, m *Manager, rootID string, components ...ResourceRef) {
	t.Helper()
	for _, component := range components {
		released, err := m.Release(rootID, component)
		if err != nil {
			t.Fatalf("%s: release %s: %v", rootID, t07Render(component), err)
		}
		if !released {
			t.Fatalf("%s: release %s retracted no claim, want the owner's claim on that exact resource dropped",
				rootID, t07Render(component))
		}
	}
}

// t07RequireHolderEdge asserts that exactly one HOLDER edge runs from one root to another.
func t07RequireHolderEdge(t *testing.T, m *Manager, from, to string) {
	t.Helper()
	edges := m.WaitEdges()
	count := 0
	for _, edge := range edges {
		if edge.FromRootID == from && edge.ToRootID == to && edge.Reason == EdgeHolder {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("HOLDER edges %s -> %s = %d, want 1; graph = %s", from, to, count, t07RenderEdges(edges))
	}
}

// t07RequireEdgeCount asserts the size of the whole wait graph.
func t07RequireEdgeCount(t *testing.T, m *Manager, want int) {
	t.Helper()
	if edges := m.WaitEdges(); len(edges) != want {
		t.Fatalf("wait graph = %s, want %d edge(s)", t07RenderEdges(edges), want)
	}
}

// t07RequireNoPending asserts one or more roots have no queued request left, which is what proves a
// terminal request also left its queue.
func t07RequireNoPending(t *testing.T, m *Manager, roots ...string) {
	t.Helper()
	for _, rootID := range roots {
		if pending := m.PendingRequests(rootID); pending != 0 {
			t.Fatalf("%s still has %d queued request(s), want none", rootID, pending)
		}
	}
}

// t07RequireIdle is the cleanup half of every case: the lock table, the wait graph, the statistics
// and the ledger must all return to nothing once every owner is released. Without it a manager that
// leaked a claim, an edge or a charge would still pass an outcome-only assertion.
func t07RequireIdle(t *testing.T, m *Manager) {
	t.Helper()
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d entries after every release, want 0; snapshot = %+v", size, m.Snapshot())
	}
	if edges := m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("wait graph = %s after every release, want no edge", t07RenderEdges(edges))
	}
	if stats := m.Stats(); stats.PendingRequests != 0 || stats.GrantedComponents != 0 || stats.WaitEdges != 0 {
		t.Fatalf("stats after every release = %+v, want no pending request, no granted component and no edge", stats)
	}
	requireBudgetZero(t, m.Ledger())
}

// t07RenderEdges renders a wait graph for a failure message.
func t07RenderEdges(edges []LockEdge) string {
	parts := make([]string, 0, len(edges))
	for _, edge := range edges {
		parts = append(parts, fmt.Sprintf("%s->%s[%s,req=%d]", edge.FromRootID, edge.ToRootID, edge.Reason, edge.RequestID))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// TestB07TwoGapsByOneRootAreTwoLocks pins case A: two gaps of one namespace taken by one root are two
// locks, not one.
//
// Holding Gap(a,m) says nothing about a request for Gap(n,z) even though the two share the interval
// conflict domain, so the second acquisition must be a fresh grant — never `LockOpReentrant`, which
// would tell the caller it already protects a range nothing protects. The state half proves the claim
// is real: the exact interval is reported held, and an insertion claim from another root inside each
// range must wait, because both ranges are really protected.
func TestB07TwoGapsByOneRootAreTwoLocks(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	insideFirst := newPublicOwner(t, m, "root-inside-a-m")
	insideSecond := newPublicOwner(t, m, "root-inside-n-z")

	first := a.acquire(ctx, t07GapBundle("a", "m")...)
	if first.Op != LockOpGranted {
		t.Fatalf("first gap acquisition = %s, want GRANTED", t07Describe(first))
	}
	t07RequireGrantNames(t, "first gap grant", first.Grant, t07Gap("a", "m"))
	t07RequireHeld(t, m, "root-a", t07Gap("a", "m"), ModeGap)

	// The defect: the second gap shares only the conflict domain with the first, yet the pre-fix
	// re-entrancy check answered from the domain and reported REENTRANT. A REENTRANT answer here is
	// the exact reproduction, so the failure message says so.
	second := a.acquire(ctx, t07GapBundle("n", "z")...)
	if second.Op != LockOpGranted {
		t.Fatalf("second gap acquisition = %s, want GRANTED: Gap(n,z) is a different lock from Gap(a,m), so holding the first may never satisfy, join or adopt the second", t07Describe(second))
	}
	if second.Wait != nil {
		t.Fatalf("a GRANTED acquisition returned the wait handle of request %d", second.Wait.RequestID)
	}
	t07RequireGrantNames(t, "second gap grant", second.Grant, t07Gap("n", "z"))
	t07RequireNoForeignIntervalClaim(t, "second gap grant", second.Grant, t07Gap("n", "z"))

	// Both exact intervals are held. A domain-wide HeldMode would report the second as held even if
	// only the first had been granted, which is why the interval is named exactly here.
	t07RequireHeld(t, m, "root-a", t07Gap("a", "m"), ModeGap)
	t07RequireHeld(t, m, "root-a", t07Gap("n", "z"), ModeGap)

	// An insertion inside the first range waits on the first gap.
	firstInsertion := insideFirst.acquire(ctx, t07InsertClaim("b")...)
	if firstInsertion.Op != LockOpWait {
		t.Fatalf("insertion inside (a,m) = %s, want WAIT on the held Gap(a,m)", t07Describe(firstInsertion))
	}
	t07RequireNotHeld(t, m, "root-inside-a-m", t07II("b"))

	// ... and an insertion inside the *second* range waits too. This is the assertion that fails
	// while the second gap is collapsed into the first claim.
	secondInsertion := insideSecond.acquire(ctx, t07InsertClaim("y")...)
	if secondInsertion.Op != LockOpWait {
		t.Fatalf("insertion inside (n,z) = %s, want WAIT on the held Gap(n,z): a range with no held claim would let this insertion through", t07Describe(secondInsertion))
	}
	t07RequireNotHeld(t, m, "root-inside-n-z", t07II("y"))

	// The queue is exactly the two insertion claims, each blocked by the gap that contains it.
	t07RequireHolderEdge(t, m, "root-inside-a-m", "root-a")
	t07RequireHolderEdge(t, m, "root-inside-n-z", "root-a")
	t07RequireEdgeCount(t, m, 2)
	if pending := m.PendingRequests("root-a"); pending != 0 {
		t.Fatalf("root-a has %d queued request(s), want 0: both of its gaps were granted inline", pending)
	}
	if pending := m.PendingRequests("root-inside-a-m"); pending != 1 {
		t.Fatalf("root-inside-a-m has %d queued request(s), want 1", pending)
	}
	if pending := m.PendingRequests("root-inside-n-z"); pending != 1 {
		t.Fatalf("root-inside-n-z has %d queued request(s), want 1", pending)
	}

	m.ReleaseAll()
	t07RequireIdle(t, m)
}

// TestB07TwoGapsInOneBundleBothExist pins case B: one atomic bundle carrying two gaps of one
// namespace keeps both.
//
// Dedupe runs by canonical identity, and the pre-fix identity of an interval is its conflict domain,
// so `[Gap(a,m), Gap(n,z)]` collapsed to one interval. The bundle must keep two components, the
// acquisition must take both, and the state half proves each range independently blocks its own
// insertions while a position between the two ranges stays free.
func TestB07TwoGapsInOneBundleBothExist(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	insideFirst := newPublicOwner(t, m, "root-inside-a-m")
	insideSecond := newPublicOwner(t, m, "root-inside-n-z")
	between := newPublicOwner(t, m, "root-between")

	deduped := DedupeResourceRefs([]ResourceRef{t07Gap("a", "m"), t07Gap("n", "z")})
	if len(deduped) != 2 {
		t.Fatalf("DedupeResourceRefs([Gap(a,m), Gap(n,z)]) kept %d component(s) %s, want 2: two gaps of one namespace are two locks in one conflict domain",
			len(deduped), t07RenderRefs(deduped))
	}
	t07RequireRefsName(t, "deduped gaps", deduped, t07Gap("a", "m"), t07Gap("n", "z"))

	result := a.acquire(ctx, []ResourceRef{t07Gap("a", "m"), t07Gap("n", "z"), t07TableIX()}...)
	if result.Op != LockOpGranted {
		t.Fatalf("two-gap bundle acquisition = %s, want GRANTED", t07Describe(result))
	}
	t07RequireGrantNames(t, "two-gap grant", result.Grant, t07Gap("a", "m"), t07Gap("n", "z"))
	t07RequireNoForeignIntervalClaim(t, "two-gap grant", result.Grant, t07Gap("a", "m"), t07Gap("n", "z"))
	// Both intervals the grant names must really be held: one request in one conflict-domain entry
	// owns one claim per component, so a grant that names two gaps while only one claim was recorded
	// would leave the second range unprotected.
	t07RequireGrantHeld(t, m, "root-a", result.Grant, t07Gap("a", "m"), t07Gap("n", "z"))
	t07RequireHeld(t, m, "root-a", t07Gap("a", "m"), ModeGap)
	t07RequireHeld(t, m, "root-a", t07Gap("n", "z"), ModeGap)

	firstInsertion := insideFirst.acquire(ctx, t07InsertClaim("b")...)
	if firstInsertion.Op != LockOpWait {
		t.Fatalf("insertion inside (a,m) = %s, want WAIT", t07Describe(firstInsertion))
	}
	secondInsertion := insideSecond.acquire(ctx, t07InsertClaim("y")...)
	if secondInsertion.Op != LockOpWait {
		t.Fatalf("insertion inside (n,z) = %s, want WAIT: the second gap of the bundle must protect its own range", t07Describe(secondInsertion))
	}

	// A position between the two ranges is inside neither gap, so it is granted as its own claim.
	// That is what separates "two ranges" from "one over-broad range".
	betweenResult := between.acquire(ctx, t07InsertClaim("m0")...)
	if betweenResult.Op != LockOpGranted {
		t.Fatalf("insertion between (a,m) and (n,z) = %s, want GRANTED: the two gaps do not cover this position", t07Describe(betweenResult))
	}
	t07RequireHeld(t, m, "root-between", t07II("m0"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-between", t07RowX("m0"), ModeX)

	t07RequireHolderEdge(t, m, "root-inside-a-m", "root-a")
	t07RequireHolderEdge(t, m, "root-inside-n-z", "root-a")
	t07RequireNoPending(t, m, "root-a", "root-between")
	if pending := m.PendingRequests("root-inside-a-m"); pending != 1 {
		t.Fatalf("root-inside-a-m has %d queued request(s), want 1", pending)
	}
	if pending := m.PendingRequests("root-inside-n-z"); pending != 1 {
		t.Fatalf("root-inside-n-z has %d queued request(s), want 1", pending)
	}

	m.ReleaseAll()
	t07RequireIdle(t, m)
}

// TestB07TwoInsertionPositionsByOneRootAreTwoClaims pins case C: two insertion positions of one
// namespace taken by one root are two claims, not one.
//
// `[II(b), X(b)]` and `[II(y), X(y)]` share only the conflict domain, so the second acquisition must
// be a fresh grant. The state half is the observable consequence: a covering `Gap(x,z)` that contains
// `y` but not `b` must wait, because root-a really does claim the position `y`; the control case
// `Gap(a,c)` (containing `b`) must wait as well, which proves the fix widened nothing.
func TestB07TwoInsertionPositionsByOneRootAreTwoClaims(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	coveringY := newPublicOwner(t, m, "root-gap-x-z")
	coveringB := newPublicOwner(t, m, "root-gap-a-c")

	first := a.acquire(ctx, t07InsertClaim("b")...)
	if first.Op != LockOpGranted {
		t.Fatalf("first insertion claim = %s, want GRANTED", t07Describe(first))
	}
	t07RequireHeld(t, m, "root-a", t07II("b"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-a", t07RowX("b"), ModeX)

	second := a.acquire(ctx, t07InsertClaim("y")...)
	if second.Op != LockOpGranted {
		t.Fatalf("second insertion claim = %s, want GRANTED: II(y) is a different lock from II(b), so the held intention at b may never satisfy the intention at y", t07Describe(second))
	}
	if second.Wait != nil {
		t.Fatalf("a GRANTED acquisition returned the wait handle of request %d", second.Wait.RequestID)
	}
	t07RequireGrantNames(t, "second insertion grant", second.Grant, t07II("y"), t07RowX("y"))
	t07RequireNoForeignIntervalClaim(t, "second insertion grant", second.Grant, t07II("y"))
	t07RequireHeld(t, m, "root-a", t07II("y"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-a", t07RowX("y"), ModeX)
	// The first position is still held as well; taking the second replaced nothing.
	t07RequireHeld(t, m, "root-a", t07II("b"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-a", t07RowX("b"), ModeX)

	// A gap over (x,z) contains y but not b, so only the newly taken intention can block it. Before
	// the fix the second request was REENTRANT, root-a held no intention at y, and this gap was
	// granted immediately while the transaction believed it owned the position.
	coveringYResult := coveringY.acquire(ctx, t07GapBundle("x", "z")...)
	if coveringYResult.Op != LockOpWait {
		t.Fatalf("Gap(x,z) = %s, want WAIT on the held insertion intention at y: the gap contains y, which root-a really claims", t07Describe(coveringYResult))
	}
	t07RequireNotHeld(t, m, "root-gap-x-z", t07Gap("x", "z"))

	// The control: a gap containing b still waits, so the fix must not have relaxed the containment
	// rule while it made the two positions distinct.
	coveringBResult := coveringB.acquire(ctx, t07GapBundle("a", "c")...)
	if coveringBResult.Op != LockOpWait {
		t.Fatalf("Gap(a,c) = %s, want WAIT on the held insertion intention at b", t07Describe(coveringBResult))
	}
	t07RequireNotHeld(t, m, "root-gap-a-c", t07Gap("a", "c"))

	t07RequireHolderEdge(t, m, "root-gap-x-z", "root-a")
	t07RequireHolderEdge(t, m, "root-gap-a-c", "root-a")
	t07RequireEdgeCount(t, m, 2)
	t07RequireNoPending(t, m, "root-a")
	if pending := m.PendingRequests("root-gap-x-z"); pending != 1 {
		t.Fatalf("root-gap-x-z has %d queued request(s), want 1", pending)
	}
	if pending := m.PendingRequests("root-gap-a-c"); pending != 1 {
		t.Fatalf("root-gap-a-c has %d queued request(s), want 1", pending)
	}

	m.ReleaseAll()
	t07RequireIdle(t, m)
}

// TestB07QueuedGapIsNotJoinedByADifferentGap pins case D: a queued gap is not "already covering" a
// different gap.
//
// Root-b holds an intention inside (a,m), which makes root-a's Gap(a,m) wait. A request for
// Gap(n,z) — a range with no held claim inside it — must then be its own inline grant: joining the
// queued request would hand back a handle for a range the caller never asked for and leave (n,z)
// protected only by accident. The state half checks the queue depth (still exactly one queued
// request), the queue identity of the first request, and that releasing root-b completes the first
// request as well.
func TestB07QueuedGapIsNotJoinedByADifferentGap(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	holder := b.acquire(ctx, t07InsertClaim("k")...)
	if holder.Op != LockOpGranted {
		t.Fatalf("root-b insertion at k = %s, want GRANTED", t07Describe(holder))
	}
	t07RequireHeld(t, m, "root-b", t07II("k"), ModeInsertIntention)

	// The held intention inside the range makes the gap request wait.
	firstGap := a.acquire(ctx, t07GapBundle("a", "m")...)
	if firstGap.Op != LockOpWait {
		t.Fatalf("Gap(a,m) = %s, want WAIT: root-b holds an insertion intention inside (a,m)", t07Describe(firstGap))
	}
	if firstGap.Wait == nil {
		t.Fatal("a WAIT outcome returned no handle")
	}
	firstRequest := firstGap.Wait.RequestID
	if pending := m.PendingRequests("root-a"); pending != 1 {
		t.Fatalf("root-a has %d queued request(s), want the single queued Gap(a,m)", pending)
	}
	t07RequireNotHeld(t, m, "root-a", t07Gap("a", "m"))
	t07RequireHolderEdge(t, m, "root-a", "root-b")
	t07RequireEdgeCount(t, m, 1)

	// The defect: the pre-fix join matched by conflict domain, so this acquisition returned
	// LockOpWait with a handle for the *first* request. It must instead be a fresh inline grant.
	secondGap := a.acquire(ctx, t07GapBundle("n", "z")...)
	if secondGap.Op != LockOpGranted {
		joined := ""
		if secondGap.Wait != nil {
			joined = fmt.Sprintf(" (joined request %d, first queued request %d)", secondGap.Wait.RequestID, firstRequest)
		}
		t.Fatalf("second Gap(n,z) = %s%s, want GRANTED: a different gap is a different claim and must never join the queued Gap(a,m)", t07Describe(secondGap), joined)
	}
	if secondGap.Wait != nil {
		t.Fatalf("a GRANTED acquisition returned the wait handle of request %d", secondGap.Wait.RequestID)
	}
	t07RequireGrantNames(t, "second gap grant", secondGap.Grant, t07Gap("n", "z"))
	t07RequireNoForeignIntervalClaim(t, "second gap grant", secondGap.Grant, t07Gap("n", "z"))

	// The first request is still the one and only queued request, still pending, and the second
	// range is already held.
	if pending := m.PendingRequests("root-a"); pending != 1 {
		t.Fatalf("root-a has %d queued request(s) after the second gap was granted, want 1: only Gap(a,m) may still be queued", pending)
	}
	if status := m.WaitStatus("root-a", firstGap.Wait); status != RequestPending {
		t.Fatalf("the original Gap(a,m) request status = %s, want PENDING", status)
	}
	t07RequireHeld(t, m, "root-a", t07Gap("n", "z"), ModeGap)
	t07RequireNotHeld(t, m, "root-a", t07Gap("a", "m"))

	// Releasing the blocker completes the queued request too.
	t07Release(t, m, "root-b", t07InsertClaim("k")...)
	if _, err := m.WaitForGrant(ctx, "root-a", firstGap.Wait); err != nil {
		t.Fatalf("waiting for the first Gap(a,m) after root-b released: %v", err)
	}
	if status := m.WaitStatus("root-a", firstGap.Wait); status != RequestGranted {
		t.Fatalf("the first Gap(a,m) status = %s, want GRANTED", status)
	}
	t07RequireHeld(t, m, "root-a", t07Gap("a", "m"), ModeGap)
	t07RequireHeld(t, m, "root-a", t07Gap("n", "z"), ModeGap)
	t07RequireNoPending(t, m, "root-a", "root-b")
	t07RequireEdgeCount(t, m, 0)

	m.ReleaseAll()
	t07RequireIdle(t, m)
}

// TestB07BundleKeepsItsGapAndItsIntention pins case E: one bundle keeps its Gap *and* its Insert
// Intention.
//
// Before the fix the dedupe key was the conflict domain, which an interval Gap and an interval Insert
// Intention share, and the mode rank orders the intention above the gap — so
// `[Gap(a,m), II(b), X(b)]` silently lost the Gap while still validating. The bundle must keep three
// components, the acquisition must take all three, and the reverse input order must give the same
// result, because dedupe is defined on the caller's components and not on their order.
func TestB07BundleKeepsItsGapAndItsIntention(t *testing.T) {
	cases := []struct {
		name string
		refs []ResourceRef
	}{
		{"gap first", []ResourceRef{t07Gap("a", "m"), t07II("b"), t07RowX("b")}},
		{"intention first", []ResourceRef{t07RowX("b"), t07II("b"), t07Gap("a", "m")}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			m, _ := newPublicManager(t, BudgetLimits{})
			a := newPublicOwner(t, m, "root-a")

			deduped := DedupeResourceRefs(testCase.refs)
			if len(deduped) != 3 {
				t.Fatalf("DedupeResourceRefs(%s) kept %d component(s) %s, want 3: the Gap, the intention and the record reservation are three claims",
					testCase.name, len(deduped), t07RenderRefs(deduped))
			}
			t07RequireRefsName(t, "deduped bundle", deduped, t07Gap("a", "m"), t07II("b"), t07RowX("b"))

			// The table intention is added for the acquisition only, so the dedupe assertion above
			// counts exactly the three claims under test.
			bundle := make([]ResourceRef, 0, len(testCase.refs)+1)
			bundle = append(bundle, testCase.refs...)
			bundle = append(bundle, t07TableIX())

			result := a.acquire(ctx, bundle...)
			if result.Op != LockOpGranted {
				t.Fatalf("bundle acquisition = %s, want GRANTED", t07Describe(result))
			}
			t07RequireGrantNames(t, "bundle grant", result.Grant, t07Gap("a", "m"), t07II("b"), t07RowX("b"))
			t07RequireNoForeignIntervalClaim(t, "bundle grant", result.Grant, t07Gap("a", "m"), t07II("b"))
			// All three claims the grant names must really be held. The Gap and the intention share
			// one conflict-domain entry, so a bundle that keeps them in its component list must also
			// record one claim per component in that entry.
			t07RequireGrantHeld(t, m, "root-a", result.Grant, t07Gap("a", "m"), t07II("b"), t07RowX("b"))

			t07RequireHeld(t, m, "root-a", t07Gap("a", "m"), ModeGap)
			t07RequireHeld(t, m, "root-a", t07II("b"), ModeInsertIntention)
			t07RequireHeld(t, m, "root-a", t07RowX("b"), ModeX)

			// The Gap half of the bundle must really protect its range: another root's insertion
			// claim at b waits, which it could not do if the Gap had been dropped.
			other := newPublicOwner(t, m, "root-other")
			otherInsertion := other.acquire(ctx, t07InsertClaim("b")...)
			if otherInsertion.Op != LockOpWait {
				t.Fatalf("another root's insertion at b = %s, want WAIT: the bundle's Gap(a,m) and X(b) both protect b", t07Describe(otherInsertion))
			}
			t07RequireHolderEdge(t, m, "root-other", "root-a")

			m.ReleaseAll()
			t07RequireIdle(t, m)
		})
	}

	// The same bundle granted after it had to queue must keep its claims too. The two components that
	// share the interval conflict domain are enqueued as two entries of one FIFO and then applied
	// together, so "one claim per component" has to hold on the queued path as well as on the inline
	// one. The assertion is read from the manager's own status instead of blocking on the wait: the
	// release path settles the request synchronously, so a request that is still PENDING after its
	// only blocker is gone is a manager that never granted it, and the test must report that rather
	// than hang until the suite timeout.
	t.Run("queued then granted", func(t *testing.T) {
		ctx := context.Background()
		m, _ := newPublicManager(t, BudgetLimits{})
		a := newPublicOwner(t, m, "root-a")
		blocker := newPublicOwner(t, m, "root-blocker")

		// The blocker holds the exclusive record lock of b, which conflicts with the bundle's X(b)
		// half and with its insertion intention, so the whole bundle queues.
		blocking := blocker.acquire(ctx, []ResourceRef{t07RowX("b"), t07TableIX()}...)
		if blocking.Op != LockOpGranted {
			t.Fatalf("blocker acquisition = %s, want GRANTED", t07Describe(blocking))
		}

		waiting := a.acquire(ctx, []ResourceRef{t07Gap("a", "m"), t07II("b"), t07RowX("b"), t07TableIX()}...)
		if waiting.Op != LockOpWait {
			t.Fatalf("bundle behind another root's X(b) = %s, want WAIT", t07Describe(waiting))
		}
		if pending := m.PendingRequests("root-a"); pending != 1 {
			t.Fatalf("root-a has %d queued request(s), want 1", pending)
		}

		t07Release(t, m, "root-blocker", t07RowX("b"), t07TableIX())

		// Nothing blocks the bundle any more, so it must have been granted by the release's own pass.
		if status := m.WaitStatus("root-a", waiting.Wait); status != RequestGranted {
			t.Fatalf("queued bundle status = %s after its only blocker released, want GRANTED: every queued component must become grantable once the other owners are gone", status)
		}
		// The request is already terminal, so resolving its grant never blocks.
		grant, err := m.WaitForGrant(ctx, "root-a", waiting.Wait)
		if err != nil {
			t.Fatalf("resolving the granted queued bundle: %v", err)
		}
		t07RequireGrantHeld(t, m, "root-a", grant, t07Gap("a", "m"), t07II("b"), t07RowX("b"))
		t07RequireHeld(t, m, "root-a", t07Gap("a", "m"), ModeGap)
		t07RequireHeld(t, m, "root-a", t07II("b"), ModeInsertIntention)
		t07RequireHeld(t, m, "root-a", t07RowX("b"), ModeX)

		// Releasing the exact claims must retract exactly those claims, including the intention that
		// shares its conflict domain with the gap.
		t07Release(t, m, "root-a", t07Gap("a", "m"), t07II("b"), t07RowX("b"), t07TableIX())
		if edges, pending := len(m.WaitEdges()), m.PendingRequests("root-a"); edges != 0 || pending != 0 {
			t.Fatalf("after releasing every exact claim: edges = %d, root-a pending = %d, want 0 and 0", edges, pending)
		}

		m.ReleaseAll()
		t07RequireIdle(t, m)
	})
}

// TestB07EachGrantedRangeBlocksItsOwnInsertions pins case F: each grant of a gap really blocks the
// insertions inside *its* range, and releasing the owner hands both ranges over.
//
// Two separate acquisitions give root-a two gap claims in one conflict domain. An insertion inside
// each range must wait, while an insertion between the ranges is granted — that is the difference
// between two ranges and one over-broad range. Releasing root-a entirely must then grant both
// waiters, and the fairness order must be the enqueue order: the request that queued first is the one
// with the smaller enqueue ordinal, and that is asserted rather than the grant order, because two
// grants completed in one retry pass may be recorded in any order.
func TestB07EachGrantedRangeBlocksItsOwnInsertions(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")
	c := newPublicOwner(t, m, "root-c")
	d := newPublicOwner(t, m, "root-d")

	first := a.acquire(ctx, t07GapBundle("a", "m")...)
	if first.Op != LockOpGranted {
		t.Fatalf("Gap(a,m) = %s, want GRANTED", t07Describe(first))
	}
	second := a.acquire(ctx, t07GapBundle("n", "z")...)
	if second.Op != LockOpGranted {
		t.Fatalf("Gap(n,z) = %s, want GRANTED: it is a second claim in the same conflict domain", t07Describe(second))
	}
	t07RequireHeld(t, m, "root-a", t07Gap("a", "m"), ModeGap)
	t07RequireHeld(t, m, "root-a", t07Gap("n", "z"), ModeGap)

	bWait := b.acquire(ctx, t07InsertClaim("b")...)
	if bWait.Op != LockOpWait {
		t.Fatalf("insertion inside (a,m) = %s, want WAIT", t07Describe(bWait))
	}
	cWait := c.acquire(ctx, t07InsertClaim("y")...)
	if cWait.Op != LockOpWait {
		t.Fatalf("insertion inside (n,z) = %s, want WAIT: the second gap must block its own range", t07Describe(cWait))
	}
	dResult := d.acquire(ctx, t07InsertClaim("m0")...)
	if dResult.Op != LockOpGranted {
		t.Fatalf("insertion between the ranges = %s, want GRANTED", t07Describe(dResult))
	}
	t07RequireHeld(t, m, "root-d", t07II("m0"), ModeInsertIntention)

	t07RequireHolderEdge(t, m, "root-b", "root-a")
	t07RequireHolderEdge(t, m, "root-c", "root-a")
	t07RequireEdgeCount(t, m, 2)
	// The enqueue ordinal is the fairness order: b queued before c, so b's ordinal is the smaller.
	if bWait.Wait.EnqueueOrdinal >= cWait.Wait.EnqueueOrdinal {
		t.Fatalf("enqueue ordinals root-b=%d root-c=%d, want root-b enqueued first",
			bWait.Wait.EnqueueOrdinal, cWait.Wait.EnqueueOrdinal)
	}
	if bWait.Wait.RequestID >= cWait.Wait.RequestID {
		t.Fatalf("request ids root-b=%d root-c=%d, want root-b queued first",
			bWait.Wait.RequestID, cWait.Wait.RequestID)
	}

	// Releasing root-a entirely hands both ranges over.
	m.ReleaseOwner("root-a")
	if _, err := m.WaitForGrant(ctx, "root-b", bWait.Wait); err != nil {
		t.Fatalf("root-b after root-a released: %v", err)
	}
	if _, err := m.WaitForGrant(ctx, "root-c", cWait.Wait); err != nil {
		t.Fatalf("root-c after root-a released: %v", err)
	}
	t07RequireHeld(t, m, "root-b", t07II("b"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-b", t07RowX("b"), ModeX)
	t07RequireHeld(t, m, "root-c", t07II("y"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-c", t07RowX("y"), ModeX)
	t07RequireNoPending(t, m, "root-a", "root-b", "root-c", "root-d")
	t07RequireEdgeCount(t, m, 0)

	m.ReleaseAll()
	t07RequireIdle(t, m)
}

// TestB07ReleasingOneClaimDoesNotReleaseTheDomain pins case G: releasing one claim releases exactly
// that claim.
//
// Both gaps of root-a live in one conflict-domain entry, so a release keyed by the domain would
// retract the wrong range — or the whole domain. The state half is the discriminating evidence: after
// releasing Gap(a,m) an insertion inside (a,m) is granted while an insertion inside (n,z) still
// waits, and the exact-interval `HeldMode` answers differ for the two ranges.
func TestB07ReleasingOneClaimDoesNotReleaseTheDomain(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	insideFirst := newPublicOwner(t, m, "root-inside-a-m")
	insideSecond := newPublicOwner(t, m, "root-inside-n-z")

	first := a.acquire(ctx, t07GapBundle("a", "m")...)
	if first.Op != LockOpGranted {
		t.Fatalf("Gap(a,m) = %s, want GRANTED", t07Describe(first))
	}
	second := a.acquire(ctx, t07GapBundle("n", "z")...)
	if second.Op != LockOpGranted {
		t.Fatalf("Gap(n,z) = %s, want GRANTED", t07Describe(second))
	}
	t07RequireHeld(t, m, "root-a", t07Gap("a", "m"), ModeGap)
	t07RequireHeld(t, m, "root-a", t07Gap("n", "z"), ModeGap)

	// Releasing one exact interval must retract exactly that claim.
	t07Release(t, m, "root-a", t07Gap("a", "m"))
	t07RequireNotHeld(t, m, "root-a", t07Gap("a", "m"))
	t07RequireHeld(t, m, "root-a", t07Gap("n", "z"), ModeGap)

	// The released range is free now.
	firstInsertion := insideFirst.acquire(ctx, t07InsertClaim("b")...)
	if firstInsertion.Op != LockOpGranted {
		t.Fatalf("insertion inside the released (a,m) = %s, want GRANTED", t07Describe(firstInsertion))
	}
	t07RequireHeld(t, m, "root-inside-a-m", t07II("b"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-inside-a-m", t07RowX("b"), ModeX)

	// ... and the range that was not released is still protected.
	secondInsertion := insideSecond.acquire(ctx, t07InsertClaim("y")...)
	if secondInsertion.Op != LockOpWait {
		t.Fatalf("insertion inside the retained (n,z) = %s, want WAIT: releasing Gap(a,m) must not release Gap(n,z)", t07Describe(secondInsertion))
	}
	t07RequireNotHeld(t, m, "root-inside-n-z", t07II("y"))
	t07RequireHolderEdge(t, m, "root-inside-n-z", "root-a")
	t07RequireEdgeCount(t, m, 1)

	// Releasing the owner ends the remaining range as well.
	m.ReleaseOwner("root-a")
	if _, err := m.WaitForGrant(ctx, "root-inside-n-z", secondInsertion.Wait); err != nil {
		t.Fatalf("root-inside-n-z after root-a released: %v", err)
	}
	t07RequireHeld(t, m, "root-inside-n-z", t07II("y"), ModeInsertIntention)

	m.ReleaseAll()
	t07RequireIdle(t, m)
}

// TestB07InsertIntentionsStayCompatibleExceptOnTheSameKey pins case H: the fix must not change the
// insertion matrix itself.
//
// Two roots' intentions at *different* keys are both granted and produce no wait edge; the same key
// from two roots serialises on the exclusive record half, and the intention half stays compatible —
// asserted directly through `componentsConflict` with two bare intentions so the compatibility rule is
// pinned separately from the queue's behaviour.
func TestB07InsertIntentionsStayCompatibleExceptOnTheSameKey(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")
	c := newPublicOwner(t, m, "root-c")

	// The pure predicate: two insertion intentions never conflict, not even on the same key, because
	// the record X half of the claim is what serialises them.
	if componentsConflict(t07II("b"), t07II("y")) {
		t.Fatal("componentsConflict(II(b), II(y)) = true, want false: two insertion intentions are compatible")
	}
	if componentsConflict(t07II("b"), t07II("b")) {
		t.Fatal("componentsConflict(II(b), II(b)) = true, want false: II/II stays compatible on the same key")
	}

	// Different keys: both granted, no edges.
	first := a.acquire(ctx, t07InsertClaim("b")...)
	if first.Op != LockOpGranted {
		t.Fatalf("insertion claim at b = %s, want GRANTED", t07Describe(first))
	}
	second := b.acquire(ctx, t07InsertClaim("y")...)
	if second.Op != LockOpGranted {
		t.Fatalf("insertion claim at y = %s, want GRANTED: a different key never conflicts", t07Describe(second))
	}
	t07RequireHeld(t, m, "root-a", t07II("b"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-a", t07RowX("b"), ModeX)
	t07RequireHeld(t, m, "root-b", t07II("y"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-b", t07RowX("y"), ModeX)
	t07RequireEdgeCount(t, m, 0)
	t07RequireNoPending(t, m, "root-a", "root-b")

	// The same key from a third root serialises on the record X half: the intention halves stay
	// compatible, so exactly one HOLDER edge appears and it names the exclusive record claim.
	third := c.acquire(ctx, t07InsertClaim("b")...)
	if third.Op != LockOpWait {
		t.Fatalf("second insertion claim at b = %s, want WAIT on root-a's X(b)", t07Describe(third))
	}
	t07RequireNotHeld(t, m, "root-c", t07II("b"))
	t07RequireNotHeld(t, m, "root-c", t07RowX("b"))
	t07RequireHolderEdge(t, m, "root-c", "root-a")
	t07RequireEdgeCount(t, m, 1)
	if pending := m.PendingRequests("root-c"); pending != 1 {
		t.Fatalf("root-c has %d queued request(s), want 1", pending)
	}

	// Releasing root-a's insertion claim at b hands the key to root-c and leaves root-b's claim at y
	// untouched.
	t07Release(t, m, "root-a", t07InsertClaim("b")...)
	if _, err := m.WaitForGrant(ctx, "root-c", third.Wait); err != nil {
		t.Fatalf("root-c after root-a released the key: %v", err)
	}
	t07RequireHeld(t, m, "root-c", t07II("b"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-c", t07RowX("b"), ModeX)
	t07RequireHeld(t, m, "root-b", t07II("y"), ModeInsertIntention)
	t07RequireHeld(t, m, "root-b", t07RowX("y"), ModeX)
	t07RequireEdgeCount(t, m, 0)

	m.ReleaseAll()
	t07RequireIdle(t, m)
}
