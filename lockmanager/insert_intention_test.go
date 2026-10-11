package lockmanager

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// insert_intention_test.go is T021: the Insert Intention (II) of the B02 interval stage, driven
// through the exported manager rather than only through the pure predicates.
//
// The distinction matters. interval_test.go (T020) proves that `componentsConflict` and
// `gapBlocksInsertion` answer containment correctly for a component pair; it cannot prove that the
// manager ever asks that question of a *request*. The manager keys its lock table by
// `Identity.ResourceKey()`, and an interval-mode identity is keyed by its domain — generation and
// namespace, without bounds and without the insertion key — precisely so that a Gap and an
// Insert Intention of one namespace meet in one queue and are decided by containment. Every case
// below therefore asserts the request outcome *and* the resource state, because a grant that was
// never really decided would satisfy an outcome-only assertion.
//
// The model under test is the contract's:
//
//   - an Insert Intention is `Kind: KindRecord, Mode: ModeInsertIntention, Key: <the key to be
//     created>`. It is one half of an insertion claim: the other half is the exclusive record lock
//     of the same key, and `ValidateBundle` refuses the intention without it
//     (`ErrInsertIntentionRequiresRecord`). The intention answers "may I insert at this position",
//     which is what a covering Gap blocks; the record lock answers "is this key already claimed",
//     which is how two inserts of one key serialise;
//   - a Gap is `Kind: KindInterval, Mode: ModeGap, Lower/Upper`, with nil meaning infinity and an
//     empty non-nil slice meaning a real bound that sorts first;
//   - a held Gap blocks an II only when it contains the insertion point, and the same question
//     asked from the other side (held II, requested Gap) must answer the same;
//   - II against II is always compatible: the interval matrix keeps two intentions from excluding
//     each other, and the record X half of the claim is what serialises them.
//
// No test here sleeps, polls or infers order from elapsed time. Where a request has to be observed
// as queued before an event, the observation is the manager's own queue, graph and status state.

// iiSpace is the namespace every Insert Intention and Gap of this file addresses. It is the
// `index/<table>/<index>` layout the write path uses, so the resource identity a test asserts is
// the same identity production would build.
const iiSpace = "index/t1/PRIMARY"

// tableIX is the table intention every record component requires in its own bundle. It is shared
// across owners and is never the point of an assertion here: it only makes each bundle legal, so a
// failure cannot be blamed on the intention prerequisite instead of on the insertion rule.
func tableIX() ResourceRef { return ixIntent("t1") }

// insertIntention builds the Insert Intention component for one key: a record-shaped component in
// insertion mode. The kind is `KindRecord` on purpose — the interval *identity* is the domain, so
// this component and a record X on the same key are deliberately different resources: one is
// decided by containment, the other by the record matrix.
func insertIntention(key []byte) ResourceRef {
	return ResourceRef{Identity: Identity{
		Kind:    KindRecord,
		Mode:    ModeInsertIntention,
		Space:   iiSpace,
		TableID: "t1",
		IndexID: "PRIMARY",
		Key:     key,
	}}
}

// rowX builds the exclusive record reservation of one key in the insertion namespace.
func rowX(key []byte) ResourceRef {
	ref := indexRecord("t1", "PRIMARY", key)
	ref.Space = iiSpace
	return ref
}

// insertBundle is the atomic insertion claim: the intention on the position, the exclusive record
// reservation of the same key, and the table intention both require. It is one request, so the two
// answers of an insertion — "may I insert here" and "is the key free" — can never be observed
// apart.
func insertBundle(key []byte) []ResourceRef {
	return []ResourceRef{insertIntention(key), rowX(key), tableIX()}
}

// rowBundleOf is the reservation-only claim of one key: what an UPDATE of an existing row takes.
func rowBundleOf(key []byte) []ResourceRef { return rowBundle(rowX(key)) }

// iiRef is the interval-domain identity of one insertion position. The mode stays
// `ModeInsertIntention`, because that is what puts the lookup on the interval domain key; a
// component without the mode would be addressed as a record and would not find the entry at all.
func iiRef(key []byte) ResourceRef { return insertIntention(key) }

// gapRef builds the Gap component of one range. The bounds are cloned so a test's own slice can
// never change a granted claim.
func gapRef(lower, upper []byte, lowerInclusive, upperInclusive bool) ResourceRef {
	return ResourceRef{Identity: Identity{
		Kind:           KindInterval,
		Mode:           ModeGap,
		Space:          iiSpace,
		TableID:        "t1",
		IndexID:        "PRIMARY",
		Lower:          bytes.Clone(lower),
		Upper:          bytes.Clone(upper),
		LowerInclusive: lowerInclusive,
		UpperInclusive: upperInclusive,
	}}
}

// gapBundle pairs one Gap with the table intention a bundle requires.
func gapBundle(ref ResourceRef) []ResourceRef {
	return []ResourceRef{ref, tableIX()}
}

// spanGap returns a gap over [lower, upper] written in text, where "" on either side means the
// unbounded side. It is the readable form of gapRef for the range cases.
func spanGap(lower, upper string, lowerInclusive, upperInclusive bool) ResourceRef {
	var low, high []byte
	if lower != "" {
		low = []byte(lower)
	}
	if upper != "" {
		high = []byte(upper)
	}
	return gapRef(low, high, lowerInclusive, upperInclusive)
}

// iiFixture is one manager plus the owners the insertion cases need.
type iiFixture struct {
	t     *testing.T
	m     *Manager
	clock *FakeClock
}

// newIIFixture returns one manager and the roots root-a and root-b, registered in that order so
// the registration ordinal — and therefore the deadlock victim rule — is fixed.
func newIIFixture(t *testing.T) (*iiFixture, publicOwner, publicOwner) {
	t.Helper()
	m, clock := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")
	return &iiFixture{t: t, m: m, clock: clock}, a, b
}

// owner registers one more root and opens its statement scope.
func (f *iiFixture) owner(rootID string) publicOwner {
	f.t.Helper()
	return newPublicOwner(f.t, f.m, rootID)
}

// requireHeldMode asserts the mode one root holds on one component through the manager façade.
//
// It is spelled here rather than delegated to another file's fixture type: the helpers of
// lock_acquisition_test.go and test_helpers_test.go live on their own fixtures, and the façade's
// HeldMode is the contract-level observation every case in this file is about.
func (f *iiFixture) requireHeldMode(rootID string, ref ResourceRef, want Mode) {
	f.t.Helper()
	got, ok := f.m.HeldMode(rootID, ref)
	if !ok {
		f.t.Fatalf("%s does not hold %s on %q, want %s", rootID, ref.Mode, ref.Key, want)
	}
	if got != want {
		f.t.Fatalf("%s holds %s on %q, want %s", rootID, got, ref.Key, want)
	}
}

// requireNoGrant asserts one root holds nothing on one component.
func (f *iiFixture) requireNoGrant(rootID string, ref ResourceRef) {
	f.t.Helper()
	if mode, ok := f.m.HeldMode(rootID, ref); ok {
		f.t.Fatalf("%s unexpectedly holds %s on %q", rootID, mode, ref.Key)
	}
}

// release drops one owner's grant on every component of one bundle.
func (f *iiFixture) release(rootID string, components ...ResourceRef) {
	f.t.Helper()
	for _, component := range components {
		if _, err := f.m.Release(rootID, component); err != nil {
			f.t.Fatalf("release %s on %q: %v", rootID, component.Key, err)
		}
	}
}

// requireNoEdgeBetween asserts that no wait edge joins two roots in either direction.
func (f *iiFixture) requireNoEdgeBetween(a, b string) {
	f.t.Helper()
	for _, edge := range f.m.WaitEdges() {
		if (edge.FromRootID == a && edge.ToRootID == b) || (edge.FromRootID == b && edge.ToRootID == a) {
			f.t.Fatalf("unexpected edge %s -> %s (%s); graph = %v",
				edge.FromRootID, edge.ToRootID, edge.Reason, f.m.WaitEdges())
		}
	}
}

// requireHolderEdges counts the HOLDER edges from one root to one root.
func (f *iiFixture) requireHolderEdges(from, to string, want int) {
	f.t.Helper()
	got := 0
	for _, edge := range f.m.WaitEdges() {
		if edge.FromRootID == from && edge.ToRootID == to && edge.Reason == EdgeHolder {
			got++
		}
	}
	if got != want {
		f.t.Fatalf("HOLDER edges %s -> %s = %d, want %d; graph = %v", from, to, got, want, f.m.WaitEdges())
	}
}

// countEdges counts the wait edges from one root to one root, of either reason.
func (f *iiFixture) countEdges(from, to string) int {
	f.t.Helper()
	got := 0
	for _, edge := range f.m.WaitEdges() {
		if edge.FromRootID == from && edge.ToRootID == to {
			got++
		}
	}
	return got
}

// requireNoPending asserts one root has no queued request left, which is what proves a terminal
// request also left its queue.
func (f *iiFixture) requireNoPending(rootID string) {
	f.t.Helper()
	if pending := f.m.PendingRequests(rootID); pending != 0 {
		f.t.Fatalf("%s still has %d queued requests", rootID, pending)
	}
}

// requireEmptyLockTable asserts that a finished scenario left no resource behind. It is the
// cleanup half of every case: a manager that grew an entry per acquisition would pass an
// outcome-only assertion and still be wrong.
func (f *iiFixture) requireEmptyLockTable() {
	f.t.Helper()
	if size := f.m.LockTableSize(); size != 0 {
		f.t.Fatalf("lock table = %d entries after every release, want 0; snapshot = %v", size, f.m.Snapshot())
	}
}

// requireBundleHalvesSurviveDedupe asserts that an insertion claim keeps both of its halves after
// the manager has deduplicated it.
//
// This is not a detail of the queue: the manager deduplicates the bundle by canonical resource key
// before it builds any entry, so a key layout that made the intention and the reservation share one
// key would collapse them to the stronger mode and silently drop the interval half of the claim —
// after which no gap could ever block an insertion again, even though every pure predicate stayed
// correct. The kind byte and the key bytes keep the two apart, and the case states that explicitly
// rather than leaving it implied by the conflict cases.
func (f *iiFixture) requireBundleHalvesSurviveDedupe(key []byte) {
	f.t.Helper()
	if insertIntention(key).ResourceKey() == rowX(key).ResourceKey() {
		f.t.Fatalf("the intention and the reservation of %q share one resource key", key)
	}
	deduped := DedupeResourceRefs(insertBundle(key))
	modes := make(map[Mode]bool, len(deduped))
	for _, component := range deduped {
		modes[component.Mode] = true
	}
	if len(deduped) != len(insertBundle(key)) {
		f.t.Fatalf("the insertion claim deduplicated to %d components, want all %d",
			len(deduped), len(insertBundle(key)))
	}
	for _, want := range []Mode{ModeInsertIntention, ModeX, ModeIX} {
		if !modes[want] {
			f.t.Fatalf("the deduplicated insertion claim lost its %s component: %v", want, deduped)
		}
	}
}

// TestB02IITwoInsertersAtDifferentKeysAreBothGranted covers the compatible half of the insertion
// matrix through the manager.
//
// Two transactions inserting two different rows claim two different record keys, so the exclusive
// half of their claims cannot collide, and both are granted in their own critical section: this is
// the concurrent-insert case the whole interval rule exists to protect. Two transactions inserting
// the *same* key are equally compatible at the intention layer — every intention of one namespace
// shares one interval-domain entry and II/II never excludes — so a manager that let one intention
// block the other would turn a legal concurrent attempt into a serialised one. What serialises
// them is the record X half of the claim, which the same-key case pins.
func TestB02IITwoInsertersAtDifferentKeysAreBothGranted(t *testing.T) {
	ctx := context.Background()

	t.Run("different keys are granted and hold separate reservations", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		k1, k2 := []byte("k1"), []byte("k2")

		// The two halves of one claim must survive the manager's own deduplication, or the
		// containment rule would never see an intention at all.
		f.requireBundleHalvesSurviveDedupe(k1)

		for _, outcome := range []struct {
			owner publicOwner
			key   []byte
		}{{a, k1}, {b, k2}} {
			result := outcome.owner.grant(ctx, insertBundle(outcome.key)...)
			if result.Op != LockOpGranted {
				t.Fatalf("%s: insertion at %q op = %s, want GRANTED",
					outcome.owner.rootID, outcome.key, result.Op)
			}
			if result.Grant.GrantID == 0 {
				t.Fatalf("%s: a fresh grant carries no grant id", outcome.owner.rootID)
			}
			// The whole claim is recorded: the insertion half and the reservation half.
			if len(result.Grant.Components) != len(insertBundle(outcome.key)) {
				t.Fatalf("%s: the grant names %d components, want the whole %d-component bundle",
					outcome.owner.rootID, len(result.Grant.Components), len(insertBundle(outcome.key)))
			}
		}

		f.requireHeldMode("root-a", iiRef(k1), ModeInsertIntention)
		f.requireHeldMode("root-a", rowX(k1), ModeX)
		f.requireHeldMode("root-b", iiRef(k2), ModeInsertIntention)
		f.requireHeldMode("root-b", rowX(k2), ModeX)
		// Neither owner can reach the other's key, at either half of the claim.
		f.requireNoGrant("root-a", rowX(k2))
		f.requireNoGrant("root-b", rowX(k1))

		// Both insertions are held, and nobody waits. HolderMode is read through HeldMode above
		// rather than from a holder count, because a snapshot entry also reports the *pending*
		// claims of queued requests, and a count would therefore include an insertion that is only
		// waiting.
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("two insertions at different keys produced wait edges: %v", edges)
		}
		f.requireNoEdgeBetween("root-a", "root-b")
		f.requireNoPending("root-a")
		f.requireNoPending("root-b")

		f.release("root-a", insertBundle(k1)...)
		f.release("root-b", insertBundle(k2)...)
		f.requireEmptyLockTable()
	})

	t.Run("the same key keeps the intentions compatible and the reservations serialise", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		key := []byte("dup")

		first := a.grant(ctx, insertBundle(key)...)
		// The second insertion queues: the intention half is compatible, but the reservation half
		// is the same record key, and every one of this file's cases is a real request, so a
		// queued request is a WAIT rather than a grant. The intention it already "offered" while
		// queued must not be mistaken for a held intention, which is what the snapshot counts.
		second := b.wait(ctx, insertBundle(key)...)
		if second.Wait.Status() != RequestPending {
			t.Fatalf("the second insertion of one key is %s, want PENDING", second.Wait.Status())
		}
		f.requireHolderEdges("root-b", "root-a", 1)

		// The intention half is the interval diagonal: root-a holds it, and root-b has merely
		// offered it while its claim waits. The distinction is the contract's applied-versus-pending
		// rule, and it is visible through HeldMode.
		f.requireHeldMode("root-a", iiRef(key), ModeInsertIntention)
		f.requireNoGrant("root-b", iiRef(key))
		f.requireNoGrant("root-b", rowX(key))
		if first.Grant.GrantID == 0 {
			t.Fatal("the first insertion carries no grant id")
		}
		entry := snapshotFor(t, f.m.Snapshot(), rowX(key))
		if entry.Holders != 2 || entry.Waiters != 1 {
			t.Fatalf("record entry = %+v, want the applied claim, the pending offer and the queue entry",
				entry)
		}

		f.release("root-a", insertBundle(key)...)
		if _, err := f.m.WaitForGrant(ctx, "root-b", second.Wait); err != nil {
			t.Fatalf("the queued insertion was not granted after the key was released: %v", err)
		}
		f.requireHeldMode("root-b", iiRef(key), ModeInsertIntention)
		f.requireHeldMode("root-b", rowX(key), ModeX)
		f.requireNoPending("root-b")

		f.release("root-b", insertBundle(key)...)
		f.requireEmptyLockTable()
	})

	t.Run("the intention diagonal and the record serialisation are one rule each", func(t *testing.T) {
		// The pure mode matrix, stated directly so a regression in either half is named: intentions
		// never exclude each other, while the reservation of one key does. The mode-only question
		// has no keys to compare, so it answers the conservative way for an X against an intention
		// on another key as well — that is what the manager's resource identity is for, and the
		// case above is where the key-level answer is asserted.
		if componentsConflict(insertIntention([]byte("k")), insertIntention([]byte("k"))) {
			t.Fatal("II/II reported a conflict (same key)")
		}
		if componentsConflict(insertIntention([]byte("k1")), insertIntention([]byte("k2"))) {
			t.Fatal("II/II reported a conflict (different keys)")
		}
		if !componentsConflict(rowX([]byte("k")), insertIntention([]byte("k"))) {
			t.Fatal("a reservation did not conflict with an intention at the same key")
		}
		if !componentsConflict(insertIntention([]byte("k")), rowX([]byte("k"))) {
			t.Fatal("an intention did not conflict with a reservation at the same key")
		}
		if !componentsConflict(rowX([]byte("k1")), insertIntention([]byte("k2"))) {
			t.Fatal("the mode-only rule stopped being conservative for X against an intention")
		}
		if !componentsConflict(insertIntention([]byte("k")), rowX([]byte("k1"))) {
			t.Fatal("the mode-only rule stopped being conservative for an intention against X")
		}
	})

	t.Run("a bare intention is refused while the full claim is accepted", func(t *testing.T) {
		// This is the invariant that makes "an insertion is one claim" structural: the intention
		// cannot be acquired without the reservation, so a caller can never take the half that a
		// covering gap checks and skip the half that protects the key.
		bare := []ResourceRef{insertIntention([]byte("q")), tableIX()}
		if err := ValidateBundle(bare, nil); !errors.Is(err, ErrInsertIntentionRequiresRecord) {
			t.Fatalf("a bare intention bundle error = %v, want ErrInsertIntentionRequiresRecord", err)
		}
		if err := ValidateBundle(insertBundle([]byte("q")), nil); err != nil {
			t.Fatalf("the full insertion claim was refused: %v", err)
		}
		if missing := MissingRowClaims(bare); len(missing) != 1 || !bytes.Equal(missing[0], []byte("q")) {
			t.Fatalf("MissingRowClaims = %v, want the single insertion key q", missing)
		}
		if missing := MissingRowClaims(insertBundle([]byte("q"))); len(missing) != 0 {
			t.Fatalf("MissingRowClaims on a full claim = %v, want none", missing)
		}

		// The manager refuses it through the same path, with no queue entry left behind.
		f, a, _ := newIIFixture(t)
		if _, err := f.m.Acquire(context.Background(), a.rootID, a.scopeID, bare); !errors.Is(err, ErrInsertIntentionRequiresRecord) {
			t.Fatalf("Acquire of a bare intention error = %v, want ErrInsertIntentionRequiresRecord", err)
		}
		f.requireNoPending(a.rootID)
		f.requireEmptyLockTable()
	})
}

// TestB02IIGapContainmentInBothDirections is the interval half of the matrix: the same containment
// question asked from either side.
//
// The pure function is exercised with the full endpoint table first, because that is where the
// open/closed rule is decided and a table makes every boundary visible at once. The manager cases
// then ask it as a request: a Gap covering the insertion position must make the insertion WAIT, and
// a Gap that excludes it must let the insertion through. They are deliberately stated in both
// directions — held Gap versus requested II, and held II versus requested Gap — because the
// contract states the rule once and the manager must not answer the two callers differently. A
// manager that compared modes alone, or that kept gaps and intentions in different entries, would
// answer "compatible" in both directions: precisely the failure these cases name.
func TestB02IIGapContainmentInBothDirections(t *testing.T) {
	t.Run("endpoint semantics agree in both directions", func(t *testing.T) {
		cases := []struct {
			name         string
			gap          ResourceRef
			position     string
			wantConflict bool
			because      string
		}{
			{name: "inside open interval", gap: spanGap("a", "z", false, false), position: "m", wantConflict: true,
				because: "m is strictly inside (a,z)"},
			{name: "open lower endpoint", gap: spanGap("a", "z", false, false), position: "a", wantConflict: false,
				because: "(a,z) excludes its lower endpoint"},
			{name: "closed lower endpoint", gap: spanGap("a", "z", true, false), position: "a", wantConflict: true,
				because: "[a,z) includes its lower endpoint"},
			{name: "open upper endpoint", gap: spanGap("a", "z", false, false), position: "z", wantConflict: false,
				because: "(a,z) excludes its upper endpoint"},
			{name: "closed upper endpoint", gap: spanGap("a", "z", false, true), position: "z", wantConflict: true,
				because: "(a,z] includes its upper endpoint"},
			{name: "below the range", gap: spanGap("b", "y", true, true), position: "a", wantConflict: false,
				because: "a sorts before the lower bound"},
			{name: "above the range", gap: spanGap("b", "y", true, true), position: "z", wantConflict: false,
				because: "z sorts after the upper bound"},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				forward := componentsConflict(testCase.gap, insertIntention([]byte(testCase.position)))
				if forward != testCase.wantConflict {
					t.Fatalf("gap vs II = %v, want %v: %s", forward, testCase.wantConflict, testCase.because)
				}
				backward := componentsConflict(insertIntention([]byte(testCase.position)), testCase.gap)
				if backward != forward {
					t.Fatalf("the mirrored direction disagrees: gap vs II = %v, II vs gap = %v: %s",
						forward, backward, testCase.because)
				}
			})
		}
	})

	t.Run("a held gap makes an insertion inside it wait", func(t *testing.T) {
		ctx := context.Background()
		f, a, b := newIIFixture(t)
		held := spanGap("a", "z", false, false)

		a.grant(ctx, gapBundle(held)...)
		f.requireHeldMode("root-a", held, ModeGap)

		queued := b.wait(ctx, insertBundle([]byte("m"))...)
		if queued.Op != LockOpWait || queued.Wait == nil {
			t.Fatalf("an insertion inside a held gap op = %s, want WAIT", queued.Op)
		}
		if queued.Wait.Status() != RequestPending {
			t.Fatalf("the blocked insertion status = %s, want PENDING", queued.Wait.Status())
		}
		if pending := f.m.PendingRequests("root-b"); pending != 1 {
			t.Fatalf("root-b pending requests = %d, want the queued insertion", pending)
		}
		// The blocker is the gap holder, named on the graph rather than inferred from timing, and
		// the intention it blocks is the one whose position the gap contains.
		f.requireHolderEdges("root-b", "root-a", 1)
		f.requireNoGrant("root-b", rowX([]byte("m")))
		if mode, ok := f.m.HeldMode("root-b", iiRef([]byte("m"))); ok {
			t.Fatalf("the blocked insertion already holds %s", mode)
		}

		// Releasing the gap is what decides the pending request.
		f.release("root-a", gapBundle(held)...)
		if _, err := f.m.WaitForGrant(ctx, "root-b", queued.Wait); err != nil {
			t.Fatalf("the insertion was not granted after the gap was released: %v", err)
		}
		f.requireHeldMode("root-b", iiRef([]byte("m")), ModeInsertIntention)
		f.requireHeldMode("root-b", rowX([]byte("m")), ModeX)
		f.requireNoPending("root-b")
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("the released gap left %d edges: %v", len(edges), f.m.WaitEdges())
		}

		f.release("root-b", insertBundle([]byte("m"))...)
		f.requireEmptyLockTable()
	})

	t.Run("an insertion outside the gap does not wait for it", func(t *testing.T) {
		ctx := context.Background()
		f, a, b := newIIFixture(t)
		held := spanGap("b", "y", false, false)

		a.grant(ctx, gapBundle(held)...)
		// "aa" sorts strictly before the lower bound "b", so the gap does not contain it.
		result := b.grant(ctx, insertBundle([]byte("aa"))...)
		if result.Op != LockOpGranted {
			t.Fatalf("an insertion below the gap op = %s, want GRANTED", result.Op)
		}
		f.requireHeldMode("root-b", iiRef([]byte("aa")), ModeInsertIntention)
		f.requireHeldMode("root-a", held, ModeGap)
		f.requireNoEdgeBetween("root-a", "root-b")
		f.requireNoPending("root-b")

		f.release("root-a", gapBundle(held)...)
		f.release("root-b", insertBundle([]byte("aa"))...)
		f.requireEmptyLockTable()
	})

	t.Run("a held insertion makes a covering gap wait and the mirror agrees", func(t *testing.T) {
		ctx := context.Background()
		f, a, b := newIIFixture(t)
		held := spanGap("a", "z", true, true)

		// The insertion is granted first, so the requested Gap has to respect it. The contract asks
		// the same containment question from this side, and the answer must be the same.
		b.grant(ctx, insertBundle([]byte("m"))...)

		queued := a.wait(ctx, gapBundle(held)...)
		if queued.Op != LockOpWait || queued.Wait == nil {
			t.Fatalf("a gap containing a held insertion op = %s, want WAIT", queued.Op)
		}
		f.requireHolderEdges("root-a", "root-b", 1)
		if mode, ok := f.m.HeldMode("root-a", held); ok {
			t.Fatalf("the blocked gap already holds %s", mode)
		}

		f.release("root-b", insertBundle([]byte("m"))...)
		if _, err := f.m.WaitForGrant(ctx, "root-a", queued.Wait); err != nil {
			t.Fatalf("the gap was not granted after the insertion was released: %v", err)
		}
		f.requireHeldMode("root-a", held, ModeGap)
		f.requireNoPending("root-a")

		f.release("root-a", gapBundle(held)...)
		f.requireEmptyLockTable()
	})
}

// TestB02IISameKeyExclusiveReservation covers what actually serialises two inserts of one key: the
// record X half of the insertion claim.
//
// The interval matrix makes II/II compatible, so an implementation that stopped at the intention
// would let two transactions both believe they may create the same row. The contract closes that by
// requiring every intention to arrive with the exclusive reservation of its own key, so the two
// owners meet on the record resource. The cases therefore pin the claim-versus-claim conflict, the
// claim-versus-bare-reservation conflict in both directions, the non-conflict at a different key,
// and — because a root that reserved a row and then inserts into it is the common case — that the
// owner's own repeat is re-entrant rather than a wait on itself.
func TestB02IISameKeyExclusiveReservation(t *testing.T) {
	ctx := context.Background()

	t.Run("a held reservation blocks an insertion at the same key", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		key := []byte("k1")

		a.grant(ctx, rowBundleOf(key)...)
		queued := b.wait(ctx, insertBundle(key)...)
		if queued.Wait.Status() != RequestPending {
			t.Fatalf("the insertion behind a reservation is %s, want PENDING", queued.Wait.Status())
		}
		f.requireHolderEdges("root-b", "root-a", 1)
		f.requireNoGrant("root-b", rowX(key))

		f.release("root-a", rowBundleOf(key)...)
		if _, err := f.m.WaitForGrant(ctx, "root-b", queued.Wait); err != nil {
			t.Fatalf("the insertion was not granted after the reservation was released: %v", err)
		}
		f.requireHeldMode("root-b", iiRef(key), ModeInsertIntention)
		f.requireHeldMode("root-b", rowX(key), ModeX)
		f.requireNoPending("root-b")

		f.release("root-b", insertBundle(key)...)
		f.requireEmptyLockTable()
	})

	t.Run("a held insertion blocks a reservation at the same key", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		key := []byte("k2")

		a.grant(ctx, insertBundle(key)...)
		queued := b.wait(ctx, rowBundleOf(key)...)
		if queued.Wait.Status() != RequestPending {
			t.Fatalf("the reservation behind an insertion is %s, want PENDING", queued.Wait.Status())
		}
		f.requireHolderEdges("root-b", "root-a", 1)

		f.release("root-a", insertBundle(key)...)
		if _, err := f.m.WaitForGrant(ctx, "root-b", queued.Wait); err != nil {
			t.Fatalf("the reservation was not granted after the insertion was released: %v", err)
		}
		f.requireHeldMode("root-b", rowX(key), ModeX)
		f.requireNoPending("root-b")

		f.release("root-b", rowBundleOf(key)...)
		f.requireEmptyLockTable()
	})

	t.Run("a reservation on another key leaves the insertion alone", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		held := []byte("k1")
		inserted := []byte("k2")

		a.grant(ctx, rowBundleOf(held)...)
		result := b.grant(ctx, insertBundle(inserted)...)
		if result.Op != LockOpGranted {
			t.Fatalf("an insertion at a different key op = %s, want GRANTED", result.Op)
		}
		f.requireHeldMode("root-b", iiRef(inserted), ModeInsertIntention)
		f.requireHeldMode("root-a", rowX(held), ModeX)
		f.requireNoEdgeBetween("root-a", "root-b")

		f.release("root-a", rowBundleOf(held)...)
		f.release("root-b", insertBundle(inserted)...)
		f.requireEmptyLockTable()
	})

	t.Run("a second insertion of the same key waits and the winner survives", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		key := []byte("k3")

		a.grant(ctx, insertBundle(key)...)
		queued := b.wait(ctx, insertBundle(key)...)
		f.requireHolderEdges("root-b", "root-a", 1)

		f.release("root-a", insertBundle(key)...)
		if _, err := f.m.WaitForGrant(ctx, "root-b", queued.Wait); err != nil {
			t.Fatalf("the second insertion was not granted: %v", err)
		}
		f.requireHeldMode("root-b", rowX(key), ModeX)
		// The winner kept exactly one reservation: the claim moved, it did not multiply.
		if got := holderCountOf(f.m.Snapshot(), rowX(key)); got != 1 {
			t.Fatalf("record entry holders = %d after the hand-over, want 1", got)
		}
		f.requireNoPending("root-b")

		f.release("root-b", insertBundle(key)...)
		f.requireEmptyLockTable()
	})

	t.Run("an owner that already claimed the key does not wait on itself", func(t *testing.T) {
		f, a, _ := newIIFixture(t)
		key := []byte("k4")

		a.grant(ctx, insertBundle(key)...)
		// The same root now asks for the reservation alone, which it already holds as the second
		// half of its insertion claim. A self-wait here would block the transaction on its own
		// grant, so this must be a re-entrant no-op.
		result := a.acquire(ctx, rowBundleOf(key)...)
		if result.Op != LockOpReentrant {
			t.Fatalf("repeating the reservation under an insertion claim op = %s, want REENTRANT", result.Op)
		}
		if result.Grant.GrantID != 0 {
			t.Fatalf("a re-entrant acquisition carries grant id %d, want 0", result.Grant.GrantID)
		}
		f.requireHeldMode("root-a", rowX(key), ModeX)
		if got := holderCountOf(f.m.Snapshot(), rowX(key)); got != 1 {
			t.Fatalf("record entry holders = %d after a re-entrant repeat, want 1", got)
		}
		f.requireNoPending("root-a")
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("a re-entrant repeat produced wait edges: %v", edges)
		}

		// Repeating the whole claim is the other re-entrant case: nothing new is taken.
		repeated := a.acquire(ctx, insertBundle(key)...)
		if repeated.Op != LockOpReentrant {
			t.Fatalf("repeating the insertion claim op = %s, want REENTRANT", repeated.Op)
		}

		f.release("root-a", insertBundle(key)...)
		f.requireEmptyLockTable()
	})
}

// TestB02IIWholeDomainGapAndEmptyIndex covers the two degenerate index shapes.
//
// The first is the lock a scan of an empty or unprovable range takes: an interval with both
// endpoints unbounded, which must block an insertion anywhere in its namespace. The second is the
// mirror: an insertion into a namespace where nothing is held must be granted immediately, without
// being made to wait for a gap that does not exist. Between them they pin that "no rows" is a real
// state rather than an error, and that an unbounded gap is expressed as missing bounds, never as a
// zero bound.
func TestB02IIWholeDomainGapAndEmptyIndex(t *testing.T) {
	ctx := context.Background()

	t.Run("a whole-domain gap blocks an insertion anywhere", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		whole := gapRef(nil, nil, false, false)

		a.grant(ctx, gapBundle(whole)...)
		f.requireHeldMode("root-a", whole, ModeGap)
		for _, position := range []string{"a", "m", "z"} {
			queued := b.wait(ctx, insertBundle([]byte(position))...)
			if queued.Wait.Status() != RequestPending {
				t.Fatalf("insertion %q inside the whole domain is %s, want PENDING", position, queued.Wait.Status())
			}
			if pending := f.m.PendingRequests("root-b"); pending != 1 {
				t.Fatalf("root-b pending requests = %d, want 1 after queueing %q", pending, position)
			}
			// Withdraw the probe before trying the next position, so each position is decided
			// independently and a queue never masks a wrong answer.
			if status := f.m.Cancel("root-b", queued.Wait); status != RequestCanceled {
				t.Fatalf("cancelling the probe at %q reported %s, want CANCELED", position, status)
			}
			f.requireNoPending("root-b")
		}

		f.release("root-a", gapBundle(whole)...)
		f.requireEmptyLockTable()
	})

	t.Run("an insertion in an empty namespace is granted", func(t *testing.T) {
		f, a, _ := newIIFixture(t)
		if entries := f.m.Snapshot(); len(entries) != 0 {
			t.Fatalf("a fresh manager is not empty: %v", entries)
		}

		result := a.grant(ctx, insertBundle([]byte("first"))...)
		if result.Op != LockOpGranted {
			t.Fatalf("the first insertion in an empty namespace op = %s, want GRANTED", result.Op)
		}
		f.requireHeldMode("root-a", iiRef([]byte("first")), ModeInsertIntention)
		if state := f.m.LockStateFor(iiRef([]byte("first"))); state != LockStateShared {
			t.Fatalf("state = %s, want SHARED for a single insertion intention", state)
		}
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("an insertion into an empty namespace produced wait edges: %v", edges)
		}

		f.release("root-a", insertBundle([]byte("first"))...)
		f.requireEmptyLockTable()
	})

	t.Run("a whole-domain gap is compatible with a reservation on another key", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		whole := gapRef(nil, nil, false, false)

		// The contract keeps the interval rule independent of the record rule: a gap suppresses the
		// creation of a key and does not protect an existing value, so an existing row on a
		// different key is not something a gap may block.
		a.grant(ctx, gapBundle(whole)...)
		other := []byte("existing")
		result := b.grant(ctx, rowBundleOf(other)...)
		if result.Op != LockOpGranted {
			t.Fatalf("a reservation under a whole-domain gap op = %s, want GRANTED", result.Op)
		}
		f.requireHeldMode("root-b", rowX(other), ModeX)
		f.requireHeldMode("root-a", whole, ModeGap)
		f.requireNoEdgeBetween("root-a", "root-b")

		f.release("root-a", gapBundle(whole)...)
		f.release("root-b", rowBundleOf(other)...)
		f.requireEmptyLockTable()
	})
}

// TestB02IIUnboundedAndSupremumRanges covers half-open ranges and the last key of an index.
//
// A lock plan that can only prove one side of a range produces exactly these shapes: `[m, +inf)`
// for "everything from m on" and `(-inf, m]` for "everything up to m". They are also where the
// bounds-versus-infinity confusion is easiest to make, because a missing bound and an intentionally
// infinite one are the same Go value. The supremum case then pins the terminal gap: an interval
// whose lower bound is the largest key of the index and whose upper bound is missing, where an
// insertion after that key is blocked and one at or before it is not.
func TestB02IIUnboundedAndSupremumRanges(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		held      ResourceRef
		blocked   []string
		grantable []string
		because   string
	}{
		{
			name:      "only a lower bound",
			held:      gapRef([]byte("m"), nil, true, false),
			blocked:   []string{"m", "z"},
			grantable: []string{"a"},
			because:   "[m,+inf) blocks m and everything after it, and nothing before it",
		},
		{
			name:      "only an upper bound",
			held:      gapRef(nil, []byte("m"), false, true),
			blocked:   []string{"m"},
			grantable: []string{"z"},
			because:   "(-inf,m] blocks m and everything before it, and nothing after it",
		},
		{
			name:      "supremum lower bound with no upper bound",
			held:      gapRef([]byte("z"), nil, false, false),
			blocked:   []string{"z\x00", "\xff"},
			grantable: []string{"a", "z"},
			because:   "an insertion strictly after the largest key is blocked, and one at or before it is not",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f, a, b := newIIFixture(t)
			a.grant(ctx, gapBundle(testCase.held)...)

			for _, position := range testCase.blocked {
				queued := b.wait(ctx, insertBundle([]byte(position))...)
				if queued.Wait.Status() != RequestPending {
					t.Fatalf("insertion %q is %s, want PENDING: %s", position, queued.Wait.Status(), testCase.because)
				}
				f.requireHolderEdges("root-b", "root-a", 1)
				if status := f.m.Cancel("root-b", queued.Wait); status != RequestCanceled {
					t.Fatalf("cancelling the probe at %q reported %s", position, status)
				}
				f.requireNoPending("root-b")
			}
			for _, position := range testCase.grantable {
				result := b.grant(ctx, insertBundle([]byte(position))...)
				if result.Op != LockOpGranted {
					t.Fatalf("insertion %q op = %s, want GRANTED: %s", position, result.Op, testCase.because)
				}
				f.requireHeldMode("root-b", iiRef([]byte(position)), ModeInsertIntention)
				f.release("root-b", insertBundle([]byte(position))...)
			}

			f.release("root-a", gapBundle(testCase.held)...)
			f.requireEmptyLockTable()
		})
	}
}

// TestB02IIEndpointFlagsAndAdjacentGaps covers the boundaries where two ranges touch.
//
// The four endpoint combinations of one gap must each admit or refuse exactly the two endpoints,
// and — the case a bounds comparison usually gets wrong — two *adjacent* gaps must not collide with
// each other, while an insertion at their shared point is blocked by the gap that includes it and
// only by that one. Two gaps of one namespace now share one interval-domain entry, so this is also
// where "one queue holds many ranges" is pinned: the claims are distinguished by their bounds, not
// by their key.
func TestB02IIEndpointFlagsAndAdjacentGaps(t *testing.T) {
	ctx := context.Background()

	endpoints := []struct {
		name           string
		lowerInclusive bool
		upperInclusive bool
		blocked        []string
		notBlocked     []string
	}{
		{name: "open on both ends", lowerInclusive: false, upperInclusive: false,
			blocked: []string{"m"}, notBlocked: []string{"a", "z"}},
		{name: "closed lower only", lowerInclusive: true, upperInclusive: false,
			blocked: []string{"a", "m"}, notBlocked: []string{"z"}},
		{name: "closed upper only", lowerInclusive: false, upperInclusive: true,
			blocked: []string{"m", "z"}, notBlocked: []string{"a"}},
		{name: "closed on both ends", lowerInclusive: true, upperInclusive: true,
			blocked: []string{"a", "m", "z"}, notBlocked: nil},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			f, a, b := newIIFixture(t)
			held := spanGap("a", "z", endpoint.lowerInclusive, endpoint.upperInclusive)
			a.grant(ctx, gapBundle(held)...)

			for _, position := range endpoint.blocked {
				queued := b.wait(ctx, insertBundle([]byte(position))...)
				if queued.Wait.Status() != RequestPending {
					t.Fatalf("insertion %q is %s, want PENDING with flags lower=%v upper=%v",
						position, queued.Wait.Status(), endpoint.lowerInclusive, endpoint.upperInclusive)
				}
				if status := f.m.Cancel("root-b", queued.Wait); status != RequestCanceled {
					t.Fatalf("cancelling the probe at %q reported %s", position, status)
				}
				f.requireNoPending("root-b")
			}
			for _, position := range endpoint.notBlocked {
				result := b.grant(ctx, insertBundle([]byte(position))...)
				if result.Op != LockOpGranted {
					t.Fatalf("insertion %q op = %s, want GRANTED with flags lower=%v upper=%v",
						position, result.Op, endpoint.lowerInclusive, endpoint.upperInclusive)
				}
				f.release("root-b", insertBundle([]byte(position))...)
			}

			f.release("root-a", gapBundle(held)...)
			f.requireEmptyLockTable()
		})
	}

	t.Run("adjacent gaps never conflict and only the containing one blocks", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		first := spanGap("a", "m", true, false) // [a, m)
		second := spanGap("m", "z", true, true) // [m, z]

		a.grant(ctx, gapBundle(first)...)
		// A gap that shares the domain with an already held one must still be a second claim, and
		// the insertion at the shared point below is what shows the manager kept them apart: the
		// bounds live on each claim, and neither gap may be read as the other.
		secondResult := b.grant(ctx, gapBundle(second)...)
		if secondResult.Op == LockOpWait {
			t.Fatalf("two adjacent gaps conflicted: %s could not take %v beside %v", b.rootID, second, first)
		}
		f.requireHeldMode("root-a", first, ModeGap)
		f.requireHeldMode("root-b", second, ModeGap)
		f.requireNoEdgeBetween("root-a", "root-b")
		f.requireNoPending("root-b")

		c := f.owner("root-c")
		// An insertion at the shared point m is blocked by [m,z] and not by [a,m).
		queued := c.wait(ctx, insertBundle([]byte("m"))...)
		if queued.Wait.Status() != RequestPending {
			t.Fatalf("insertion at the shared point m is %s, want PENDING", queued.Wait.Status())
		}
		f.requireHolderEdges("root-c", "root-b", 1)
		f.requireHolderEdges("root-c", "root-a", 0)

		// The endpoint a is blocked only by the first gap, which is closed at a.
		d := f.owner("root-d")
		atLower := d.wait(ctx, insertBundle([]byte("a"))...)
		if atLower.Wait.Status() != RequestPending {
			t.Fatalf("insertion at a is %s, want PENDING for a gap closed at a", atLower.Wait.Status())
		}
		f.requireHolderEdges("root-d", "root-a", 1)
		f.requireHolderEdges("root-d", "root-b", 0)

		// The endpoint z is blocked only by the second gap, which is closed at z.
		e := f.owner("root-e")
		atUpper := e.wait(ctx, insertBundle([]byte("z"))...)
		if atUpper.Wait.Status() != RequestPending {
			t.Fatalf("insertion at z is %s, want PENDING for a gap closed at z", atUpper.Wait.Status())
		}
		f.requireHolderEdges("root-e", "root-b", 1)
		f.requireHolderEdges("root-e", "root-a", 0)

		// Releasing each gap grants exactly the insertion it was blocking, and nothing else: the
		// three waiters are distinguished by the range that contains them, not by queue order.
		f.release("root-a", gapBundle(first)...)
		if _, err := f.m.WaitForGrant(ctx, "root-d", atLower.Wait); err != nil {
			t.Fatalf("the insertion at a was not granted after [a,m) was released: %v", err)
		}
		if status := atUpper.Wait.Status(); status != RequestPending {
			t.Fatalf("the insertion at z became %s while [m,z] is still held", status)
		}
		if status := queued.Wait.Status(); status != RequestPending {
			t.Fatalf("the insertion at m became %s while [m,z] is still held", status)
		}

		f.release("root-b", gapBundle(second)...)
		if _, err := f.m.WaitForGrant(ctx, "root-c", queued.Wait); err != nil {
			t.Fatalf("the insertion at m was not granted after [m,z] was released: %v", err)
		}
		if _, err := f.m.WaitForGrant(ctx, "root-e", atUpper.Wait); err != nil {
			t.Fatalf("the insertion at z was not granted after [m,z] was released: %v", err)
		}
		f.requireHeldMode("root-c", iiRef([]byte("m")), ModeInsertIntention)
		f.requireHeldMode("root-d", iiRef([]byte("a")), ModeInsertIntention)
		f.requireHeldMode("root-e", iiRef([]byte("z")), ModeInsertIntention)

		f.release("root-c", insertBundle([]byte("m"))...)
		f.release("root-d", insertBundle([]byte("a"))...)
		f.release("root-e", insertBundle([]byte("z"))...)
		f.requireEmptyLockTable()
	})
}

// TestB02IIConcurrentInsertersInsideOneGap covers the property the interval matrix exists for: a
// gap protects positions, not a resource.
//
// The naive rule — a gap blocks every insertion in its namespace — is not only stronger than the
// contract, it is observably wrong: it serialises inserters that target different positions of one
// range, which is the concurrent-insertion case a gap lock must permit. The first half clears the
// gap entirely, so the two inserters cannot be said to have been admitted by a containing range;
// the second half queues them behind one holder to prove the FIFO order the contract requires. Two
// gaps over one range stay compatible throughout, because neither claims a key.
func TestB02IIConcurrentInsertersInsideOneGap(t *testing.T) {
	ctx := context.Background()

	t.Run("two inserters with no gap held are both granted", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		// The namespace is empty: neither insertion may be made to wait for a range that does not
		// exist.
		if entries := f.m.Snapshot(); len(entries) != 0 {
			t.Fatalf("a fresh manager is not empty: %v", entries)
		}

		a.grant(ctx, insertBundle([]byte("b"))...)
		b.grant(ctx, insertBundle([]byte("y"))...)
		f.requireHeldMode("root-a", iiRef([]byte("b")), ModeInsertIntention)
		f.requireHeldMode("root-b", iiRef([]byte("y")), ModeInsertIntention)
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("two independent inserters produced wait edges: %v", edges)
		}
		if stats := f.m.Stats(); stats.PendingRequests != 0 {
			t.Fatalf("pending requests = %d, want 0", stats.PendingRequests)
		}

		f.release("root-a", insertBundle([]byte("b"))...)
		f.release("root-b", insertBundle([]byte("y"))...)
		f.requireEmptyLockTable()
	})

	t.Run("inserters inside a held gap are blocked by it and then run in FIFO order", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		held := spanGap("a", "z", false, false)
		a.grant(ctx, gapBundle(held)...)

		first := b.wait(ctx, insertBundle([]byte("m"))...)
		if first.Wait.Status() != RequestPending {
			t.Fatalf("the first insertion inside the gap is %s, want PENDING", first.Wait.Status())
		}
		c := f.owner("root-c")
		second := c.wait(ctx, insertBundle([]byte("t"))...)
		if second.Wait.Status() != RequestPending {
			t.Fatalf("the second insertion inside the gap is %s, want PENDING", second.Wait.Status())
		}
		// Both wait on the holder of the range, and the order between them is the queue order the
		// handles expose, not the order two goroutines happened to run.
		f.requireHolderEdges("root-b", "root-a", 1)
		f.requireHolderEdges("root-c", "root-a", 1)
		if first.Wait.EnqueueOrdinal >= second.Wait.EnqueueOrdinal {
			t.Fatalf("enqueue ordinals = %d then %d, want the first request older",
				first.Wait.EnqueueOrdinal, second.Wait.EnqueueOrdinal)
		}

		f.release("root-a", gapBundle(held)...)
		if _, err := f.m.WaitForGrant(ctx, "root-b", first.Wait); err != nil {
			t.Fatalf("the older insertion was not granted after the gap was released: %v", err)
		}
		if _, err := f.m.WaitForGrant(ctx, "root-c", second.Wait); err != nil {
			t.Fatalf("the younger insertion was not granted after the gap was released: %v", err)
		}
		f.requireHeldMode("root-b", iiRef([]byte("m")), ModeInsertIntention)
		f.requireHeldMode("root-c", iiRef([]byte("t")), ModeInsertIntention)
		f.requireNoPending("root-b")
		f.requireNoPending("root-c")
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("the released gap left %d edges: %v", len(edges), f.m.WaitEdges())
		}

		f.release("root-b", insertBundle([]byte("m"))...)
		f.release("root-c", insertBundle([]byte("t"))...)
		f.requireEmptyLockTable()
	})

	t.Run("an older insertion does not starve a younger one", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		ownerC := f.owner("root-c")
		key := []byte("same")

		// root-b holds the insertion claim; root-c queues behind it, so root-c's request is a WAIT
		// rather than a reject. A third owner arriving later must queue behind root-c, not overtake
		// it, and its blocker set is both the holder and the older waiter.
		b.grant(ctx, insertBundle(key)...)
		queued := ownerC.wait(ctx, insertBundle(key)...)
		if queued.Wait.Status() != RequestPending {
			t.Fatalf("the queued insertion is %s, want PENDING", queued.Wait.Status())
		}
		third := a.wait(ctx, insertBundle(key)...)
		f.requireHolderEdges("root-c", "root-b", 1)
		f.requireHolderEdges("root-a", "root-b", 1)
		if third.Wait.EnqueueOrdinal <= queued.Wait.EnqueueOrdinal {
			t.Fatalf("the later arrival has ordinal %d, want it after %d",
				third.Wait.EnqueueOrdinal, queued.Wait.EnqueueOrdinal)
		}
		foundOlderWaiterEdge := false
		for _, edge := range f.m.WaitEdges() {
			if edge.FromRootID == "root-a" && edge.ToRootID == "root-c" && edge.Reason == EdgeOlderWaiter {
				foundOlderWaiterEdge = true
			}
		}
		if !foundOlderWaiterEdge {
			t.Fatalf("the FIFO check did not record root-a waiting behind root-c: %v", f.m.WaitEdges())
		}

		// The order the queue grants in is the enqueue order: root-c first, even though root-a is
		// the older *root*. Granting root-a first would be the starvation the FIFO rule prevents.
		f.release("root-b", insertBundle(key)...)
		if _, err := f.m.WaitForGrant(ctx, "root-c", queued.Wait); err != nil {
			t.Fatalf("the queued insertion was not granted: %v", err)
		}
		if status := third.Wait.Status(); status != RequestPending {
			t.Fatalf("root-a's insertion is %s while root-c holds the key, want PENDING", status)
		}
		f.requireHeldMode("root-c", iiRef(key), ModeInsertIntention)

		f.release("root-c", insertBundle(key)...)
		if _, err := f.m.WaitForGrant(ctx, "root-a", third.Wait); err != nil {
			t.Fatalf("the third insertion was not granted after root-c released: %v", err)
		}
		f.requireHeldMode("root-a", iiRef(key), ModeInsertIntention)

		f.release("root-a", insertBundle(key)...)
		f.requireEmptyLockTable()
	})

	t.Run("two gaps over one range are compatible", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		shared := spanGap("a", "z", false, false)

		// The namespace holds no intention here, so this is purely the gap/gap diagonal.
		a.grant(ctx, gapBundle(shared)...)
		result := b.grant(ctx, gapBundle(shared)...)
		if result.Op == LockOpWait {
			t.Fatalf("two owners could not both hold the same gap: op = %s", result.Op)
		}
		f.requireHeldMode("root-a", shared, ModeGap)
		f.requireHeldMode("root-b", shared, ModeGap)
		f.requireNoEdgeBetween("root-a", "root-b")

		f.release("root-a", gapBundle(shared)...)
		f.release("root-b", gapBundle(shared)...)
		f.requireEmptyLockTable()
	})

	t.Run("a pending intention in the range blocks a third owner's gap", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		holding := spanGap("a", "z", false, false)
		a.grant(ctx, gapBundle(holding)...)

		// root-b's insertion is queued inside the range, so its intention is a pending claim. A
		// third owner's gap over that position contains it, so the containment rule makes the gap
		// wait for the queued intention: a pending claim still protects.
		queuedInsert := b.wait(ctx, insertBundle([]byte("m"))...)
		c := f.owner("root-c")
		small := spanGap("m", "n", true, true)
		waitingGap := c.wait(ctx, gapBundle(small)...)
		if waitingGap.Wait.Status() != RequestPending {
			t.Fatalf("the gap over a pending intention is %s, want PENDING", waitingGap.Wait.Status())
		}
		// The blocker is the queued insertion, not the first gap: two gaps never conflict, so only
		// the intention inside the new range can be what the gap waits for. Both the holder form of
		// the edge and the FIFO form are accepted, because which one the graph reports depends on
		// whether the older waiter is a holder as well.
		f.requireNoEdgeBetween("root-c", "root-a")
		if f.countEdges("root-c", "root-b") == 0 {
			t.Fatalf("the gap did not wait for the queued intention inside it: %v", f.m.WaitEdges())
		}

		f.release("root-a", gapBundle(holding)...)
		if _, err := f.m.WaitForGrant(ctx, "root-b", queuedInsert.Wait); err != nil {
			t.Fatalf("the queued insertion was not granted: %v", err)
		}
		// The intention is applied now, and it still blocks the gap: what the gap's owner has to
		// wait for is the insertion claim itself, not the queue entry behind it.
		if status := waitingGap.Wait.Status(); status != RequestPending {
			t.Fatalf("the gap became %s while the insertion is held, want PENDING", status)
		}
		f.requireHolderEdges("root-c", "root-b", 1)
		f.requireHeldMode("root-b", iiRef([]byte("m")), ModeInsertIntention)
		if mode, ok := f.m.HeldMode("root-c", small); ok {
			t.Fatalf("the blocked gap already holds %s", mode)
		}

		// Ending the insertion is what lets the gap in, and nothing else was needed to retry it.
		f.release("root-b", insertBundle([]byte("m"))...)
		if _, err := f.m.WaitForGrant(ctx, "root-c", waitingGap.Wait); err != nil {
			t.Fatalf("the gap was not granted after the insertion was released: %v", err)
		}
		f.requireHeldMode("root-c", small, ModeGap)
		f.requireNoPending("root-c")

		f.release("root-c", gapBundle(small)...)
		f.requireEmptyLockTable()
	})
}

// TestB02IIWaitsInsideAProtectedInterval is the wait itself, not only the outcome.
//
// An insertion into a protected range must become a queue entry — WAIT, not GRANTED and not an
// error — and it must be granted only when the range's owner ends. The intermediate state is
// asserted through the manager's own observables (the pending index, the wait-for graph and the
// counters) before the release, so "it waited" is a fact about the manager rather than an inference
// from elapsed time.
func TestB02IIWaitsInsideAProtectedInterval(t *testing.T) {
	ctx := context.Background()
	f, a, b := newIIFixture(t)
	held := spanGap("a", "z", true, true)

	a.grant(ctx, gapBundle(held)...)
	before := f.m.Stats()

	queued := b.wait(ctx, insertBundle([]byte("m"))...)
	if queued.Op != LockOpWait {
		t.Fatalf("op = %s, want WAIT for an insertion inside a held gap", queued.Op)
	}
	if pending := f.m.PendingRequests("root-b"); pending != 1 {
		t.Fatalf("pending requests = %d, want 1", pending)
	}
	edges := f.m.WaitEdges()
	if len(edges) != 1 || edges[0].FromRootID != "root-b" || edges[0].ToRootID != "root-a" || edges[0].Reason != EdgeHolder {
		t.Fatalf("wait edges = %v, want the single HOLDER edge root-b -> root-a", edges)
	}
	after := f.m.Stats()
	if after.PendingRequests != before.PendingRequests+1 {
		t.Fatalf("pending counter = %d, want %d", after.PendingRequests, before.PendingRequests+1)
	}
	if after.WaitEdges != before.WaitEdges+1 {
		t.Fatalf("wait edge counter = %d, want %d", after.WaitEdges, before.WaitEdges+1)
	}
	f.requireNoGrant("root-b", rowX([]byte("m")))

	// Nothing but the gap's own release may decide the request: a clock advance that reaches no
	// deadline leaves it pending.
	f.clock.Advance(time.Second)
	if status := queued.Wait.Status(); status != RequestPending {
		t.Fatalf("status = %s after an unrelated clock advance, want PENDING", status)
	}

	f.release("root-a", gapBundle(held)...)
	if _, err := f.m.WaitForGrant(ctx, "root-b", queued.Wait); err != nil {
		t.Fatalf("the insertion was not granted after the gap's owner ended: %v", err)
	}
	f.requireHeldMode("root-b", iiRef([]byte("m")), ModeInsertIntention)
	f.requireNoPending("root-b")
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("the granted insertion left %d edges: %v", len(edges), edges)
	}
	if final := f.m.Stats(); final.WaitEdges != before.WaitEdges {
		t.Fatalf("wait edge counter = %d after the grant, want %d", final.WaitEdges, before.WaitEdges)
	}

	f.release("root-b", insertBundle([]byte("m"))...)
	f.requireEmptyLockTable()
}

// TestB02IIQueueTimeoutCancelAndDeadlock covers the four ways a queued insertion leaves the queue.
//
// A queued request that could not be woken, withdrawn, expired or reported as a deadlock victim is
// the failure mode this file exists to prevent, so each ending is asserted for both halves: the
// status the caller observes and the queue or graph state the manager is left in. The timeout is
// driven by the fake clock and the deadlock by the detector the manager exposes, so neither is a
// race against the scheduler.
func TestB02IIQueueTimeoutCancelAndDeadlock(t *testing.T) {
	ctx := context.Background()

	t.Run("a queued insertion appears in the graph and leaves when granted", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		key := []byte("k1")

		a.grant(ctx, rowBundleOf(key)...)
		queued := b.wait(ctx, insertBundle(key)...)

		f.requireHolderEdges("root-b", "root-a", 1)
		if edges := f.m.WaitEdges(); len(edges) != 1 {
			t.Fatalf("edges = %v, want exactly the insertion's HOLDER edge", edges)
		}
		if pending := f.m.PendingRequests("root-b"); pending != 1 {
			t.Fatalf("pending requests = %d, want 1", pending)
		}

		f.release("root-a", rowBundleOf(key)...)
		if _, err := f.m.WaitForGrant(ctx, "root-b", queued.Wait); err != nil {
			t.Fatalf("the insertion was not granted after the reservation was released: %v", err)
		}
		f.requireHolderEdges("root-b", "root-a", 0)
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("a granted insertion left %d edges: %v", len(edges), edges)
		}
		f.requireNoPending("root-b")

		f.release("root-b", insertBundle(key)...)
		f.requireEmptyLockTable()
	})

	t.Run("the wait deadline ends a queued insertion", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		key := []byte("k1")
		a.grant(ctx, rowBundleOf(key)...)

		// The waiter's own scope carries a short deadline, so the effective wait bound is a fact
		// the test sets rather than the manager default.
		waiterScope, err := f.m.OpenScope(b.rootID, "b-short", 0, ScopeStatement, f.clock.Now().Add(2*time.Second))
		if err != nil {
			t.Fatalf("open the short-deadline scope: %v", err)
		}
		queued, err := f.m.Acquire(ctx, b.rootID, waiterScope.ID, insertBundle(key))
		if err != nil || queued.Op != LockOpWait {
			t.Fatalf("queueing the insertion: op=%s err=%v", queued.Op, err)
		}
		if status := queued.Wait.Status(); status != RequestPending {
			t.Fatalf("status = %s before the deadline, want PENDING", status)
		}
		if pending := f.m.PendingRequests("root-b"); pending != 1 {
			t.Fatalf("pending requests = %d, want 1", pending)
		}
		f.requireHolderEdges("root-b", "root-a", 1)

		// One second in, the deadline has not been reached: the sweep must decide nothing.
		f.clock.Advance(time.Second)
		if completed := f.m.CheckTimeouts(); completed != 0 {
			t.Fatalf("the sweep completed %d requests one second into a two-second deadline", completed)
		}
		f.clock.Advance(2 * time.Second)
		if completed := f.m.CheckTimeouts(); completed != 1 {
			t.Fatalf("the sweep completed %d requests past the deadline, want 1", completed)
		}
		if status := queued.Wait.Status(); status != RequestTimedOut {
			t.Fatalf("status = %s after the deadline, want TIMED_OUT", status)
		}
		if _, err := f.m.WaitForGrantResult(ctx, b.rootID, queued.Wait); !errors.Is(err, ErrLockWaitTimeout) {
			t.Fatalf("wait error = %v, want ErrLockWaitTimeout", err)
		}
		// The expired request left the queue and the graph, and the holder kept its lock.
		f.requireNoPending("root-b")
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("a timed-out insertion left %d edges: %v", len(edges), edges)
		}
		if stats := f.m.Stats(); stats.Timeouts != 1 {
			t.Fatalf("timeout counter = %d, want 1", stats.Timeouts)
		}
		f.requireHeldMode("root-a", rowX(key), ModeX)

		f.release("root-a", rowBundleOf(key)...)
		f.requireEmptyLockTable()
	})

	t.Run("cancelling a queued insertion removes its queue entry", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		key := []byte("k1")

		a.grant(ctx, rowBundleOf(key)...)
		queued := b.wait(ctx, insertBundle(key)...)
		f.requireHolderEdges("root-b", "root-a", 1)
		if pending := f.m.PendingRequests("root-b"); pending != 1 {
			t.Fatalf("pending requests = %d, want 1", pending)
		}

		if status := f.m.Cancel("root-b", queued.Wait); status != RequestCanceled {
			t.Fatalf("cancel reported %s, want CANCELED", status)
		}
		if status := queued.Wait.Status(); status != RequestCanceled {
			t.Fatalf("status = %s after the cancel, want CANCELED", status)
		}
		f.requireNoPending("root-b")
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("a cancelled insertion left %d edges: %v", len(edges), edges)
		}
		if _, err := f.m.WaitForGrantResult(ctx, "root-b", queued.Wait); !errors.Is(err, ErrConflict) {
			t.Fatalf("wait error = %v, want ErrConflict", err)
		}
		// The holder is untouched and the cancelled waiter holds nothing.
		f.requireHeldMode("root-a", rowX(key), ModeX)
		f.requireNoGrant("root-b", rowX(key))
		f.requireNoGrant("root-b", iiRef(key))
		// Cancelling again reports the settled status rather than deciding a second outcome.
		if status := f.m.Cancel("root-b", queued.Wait); status != RequestCanceled {
			t.Fatalf("a second cancel reported %s, want the settled CANCELED", status)
		}

		f.release("root-a", rowBundleOf(key)...)
		f.requireEmptyLockTable()
	})

	t.Run("a cycle through an insertion is detected and the victim's request is withdrawn", func(t *testing.T) {
		f, a, b := newIIFixture(t)
		k1, k2 := []byte("k1"), []byte("k2")

		// root-a holds the insertion claim on k1 and wants the reservation of k2. root-b holds the
		// reservation of k2 and wants to insert k1. The insertion is the edge that closes the
		// cycle, so the detector has to treat an insertion claim as a real participant rather than
		// as a wait nobody can be blocked by.
		//
		// The order matters: root-a takes k1 first and only then waits for k2, so root-b's later
		// request for k1 queues behind an older claim instead of being refused by the FIFO rule.
		a.grant(ctx, insertBundle(k1)...)
		b.grant(ctx, rowBundleOf(k2)...)
		aWait := a.wait(ctx, rowBundleOf(k2)...)
		bInsert := b.wait(ctx, insertBundle(k1)...)

		f.requireHolderEdges("root-a", "root-b", 1)
		f.requireHolderEdges("root-b", "root-a", 1)

		summary, found := f.m.DetectDeadlock()
		if !found {
			t.Fatalf("the cycle through an insertion was not detected; graph = %v", f.m.WaitEdges())
		}
		// root-b registered second, so the contract's victim is the newer root.
		if summary.VictimRootID != "root-b" {
			t.Fatalf("victim = %s, want root-b (the larger ordinal)", summary.VictimRootID)
		}
		if status := bInsert.Wait.Status(); status != RequestDeadlockVictim {
			t.Fatalf("the victim insertion status = %s, want DEADLOCK_VICTIM", status)
		}
		if _, err := f.m.WaitForGrantResult(ctx, "root-b", bInsert.Wait); !errors.Is(err, ErrDeadlock) {
			t.Fatalf("victim wait error = %v, want ErrDeadlock", err)
		}
		// The victim's insertion is gone from the queue, so it cannot keep blocking anyone, and the
		// detector deliberately left the victim's existing reservation in place: ending the root is
		// the victim's own job.
		f.requireNoPending("root-b")
		f.requireHolderEdges("root-b", "root-a", 0)
		f.requireHeldMode("root-b", rowX(k2), ModeX)
		if stats := f.m.Stats(); stats.Deadlocks != 1 {
			t.Fatalf("deadlock counter = %d, want 1", stats.Deadlocks)
		}

		// The survivor stays queued while the victim still holds the reservation it wanted. This is
		// the contract's ordering: a detector never releases a grant, so the survivor must not be
		// granted merely because the cycle was reported.
		f.requireHolderEdges("root-a", "root-b", 1)
		if status := aWait.Wait.Status(); status != RequestPending {
			t.Fatalf("the survivor is %s while the victim still holds the reservation, want PENDING", status)
		}

		// The victim ending its root is what releases the reservation and wakes the survivor.
		f.m.ReleaseOwner("root-b")
		f.requireGrantWithin(ctx, "root-a", aWait.Wait, "the survivor's reservation after the victim ended")
		f.requireHeldMode("root-a", rowX(k2), ModeX)
		f.requireNoPending("root-a")
		if edges := f.m.WaitEdges(); len(edges) != 0 {
			t.Fatalf("the resolved cycle left %d edges: %v", len(edges), f.m.WaitEdges())
		}

		f.release("root-a", rowBundleOf(k2)...)
		f.release("root-a", insertBundle(k1)...)
		f.requireEmptyLockTable()
	})
}

// requireGrantWithin blocks for one request's grant with a hang detector, and reports the whole
// manager state when the grant never arrives.
//
// The detector is not an ordering device: every ordering claim in this file is an observable queue
// or graph fact. It exists because a surviving waiter that is never woken is exactly the defect
// these cases are looking for, and a test that exits with its queue state printed names the defect,
// while one that hangs only times out the whole package.
func (f *iiFixture) requireGrantWithin(ctx context.Context, rootID string, handle *LockHandle, because string) {
	f.t.Helper()
	outcomes := make(chan error, 1)
	go func() {
		_, err := f.m.WaitForGrant(ctx, rootID, handle)
		outcomes <- err
	}()
	select {
	case err := <-outcomes:
		if err != nil {
			f.t.Fatalf("%s: wait error = %v; edges = %v", because, err, f.m.WaitEdges())
		}
	case <-time.After(5 * time.Second):
		f.t.Fatalf("%s: %s was never woken (status=%s); edges = %v; snapshot = %v",
			because, rootID, handle.Status(), f.m.WaitEdges(), f.m.Snapshot())
	}
}

// TestB02IIAtomicBundleIsAllOrNothing proves the components of an insertion claim cannot be
// observed apart.
//
// An insertion is never one lock: the intention on the position, the exclusive reservation of the
// key and the IX on the table are requested together, and the contract is explicit that a bundle is
// granted as a whole or becomes exactly one pending request. The case checks the granted half — the
// grant names every component — and the waiting half, where a claim blocked on its key must not
// already hold its table intention, and a cancelled claim must leave neither half behind. It is the
// shape a Next-Key request uses as well.
func TestB02IIAtomicBundleIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	f, a, b := newIIFixture(t)
	key := []byte("k1")

	granted := a.grant(ctx, insertBundle(key)...)
	if len(granted.Grant.Components) != len(insertBundle(key)) {
		t.Fatalf("the grant names %d components, want the whole bundle (%d)",
			len(granted.Grant.Components), len(insertBundle(key)))
	}
	f.requireHeldMode("root-a", iiRef(key), ModeInsertIntention)
	f.requireHeldMode("root-a", rowX(key), ModeX)
	f.requireHeldMode("root-a", tableIX(), ModeIX)

	f.release("root-a", insertBundle(key)...)
	f.requireEmptyLockTable()

	// A claim that cannot take its key must not take its table intention either: it is queued as
	// one request, and a cancelled one leaves neither component held.
	a.grant(ctx, rowBundleOf(key)...)
	queued := b.wait(ctx, insertBundle(key)...)
	if pending := f.m.PendingRequests("root-b"); pending != 1 {
		t.Fatalf("the queued claim is counted %d times, want one request", pending)
	}
	f.requireNoGrant("root-b", tableIX())
	if status := f.m.Cancel("root-b", queued.Wait); status != RequestCanceled {
		t.Fatalf("cancel reported %s, want CANCELED", status)
	}
	f.requireNoPending("root-b")
	f.requireNoGrant("root-b", tableIX())
	f.requireNoGrant("root-b", rowX(key))

	f.release("root-a", rowBundleOf(key)...)
	f.requireEmptyLockTable()
}

// TestB02IINamespaceIsolation proves an insertion intention and a gap are only comparable inside one
// namespace.
//
// The interval domain is (Generation, Space), so a gap in another table's index is a different
// domain and must not block anything here. Without this case a manager that keyed every interval
// together across namespaces would pass all the containment cases above while over-locking across
// tables, which is the failure a single global interval registry would introduce.
func TestB02IINamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	f, a, b := newIIFixture(t)

	held := spanGap("a", "z", true, true)
	a.grant(ctx, gapBundle(held)...)

	// The same insertion position, in a different index namespace.
	otherII := insertIntention([]byte("m"))
	otherII.Space = "index/t1/idx_secondary"
	otherII.IndexID = "idx_secondary"
	otherX := rowX([]byte("m"))
	otherX.Space = "index/t1/idx_secondary"
	otherX.IndexID = "idx_secondary"
	otherBundle := []ResourceRef{otherII, otherX, tableIX()}

	result := b.grant(ctx, otherBundle...)
	if result.Op != LockOpGranted {
		t.Fatalf("an insertion in another namespace op = %s, want GRANTED", result.Op)
	}
	f.requireHeldMode("root-b", otherII, ModeInsertIntention)
	f.requireHeldMode("root-a", held, ModeGap)
	f.requireNoEdgeBetween("root-a", "root-b")
	f.requireNoPending("root-b")

	f.release("root-a", gapBundle(held)...)
	f.release("root-b", otherBundle...)
	f.requireEmptyLockTable()
}
