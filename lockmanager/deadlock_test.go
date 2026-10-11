package lockmanager

import (
	"context"
	"sync"
	"testing"
)

// deadlock_test.go is T048: the wait-for graph, cycle detection and the contract's victim rule.
//
// Every test drives the graph through the exported façade and asserts on graph state rather than
// on elapsed time, so a failure names the edge or the victim that is wrong. No test sleeps.

// detectFixture is one manager plus the helpers these tests need.
type detectFixture struct {
	t      *testing.T
	m      *Manager
	clock  *FakeClock
	owners map[string]publicOwner
}

func newDetectFixture(t *testing.T, limits BudgetLimits, rootIDs ...string) *detectFixture {
	t.Helper()
	m, clock := newPublicManager(t, limits)
	f := &detectFixture{t: t, m: m, clock: clock, owners: make(map[string]publicOwner, len(rootIDs))}
	for _, rootID := range rootIDs {
		f.owners[rootID] = newPublicOwner(t, m, rootID)
	}
	return f
}

func (f *detectFixture) owner(rootID string) publicOwner { return f.owners[rootID] }

// edgesFor reports the graph edges leaving one root.
func (f *detectFixture) edgesFor(rootID string) []LockEdge {
	f.t.Helper()
	var out []LockEdge
	for _, edge := range f.m.WaitEdges() {
		if edge.FromRootID == rootID {
			out = append(out, edge)
		}
	}
	return out
}

// requireEdge asserts one (from, to, reason) edge exists.
func (f *detectFixture) requireEdge(from, to string, reason WaitEdgeReason) LockEdge {
	f.t.Helper()
	for _, edge := range f.edgesFor(from) {
		if edge.ToRootID == to && edge.Reason == reason {
			return edge
		}
	}
	f.t.Fatalf("no %s edge %s -> %s; graph = %v", reason, from, to, f.m.WaitEdges())
	return LockEdge{}
}

// requireNoEdge asserts no edge of any reason joins two roots in either direction.
func (f *detectFixture) requireNoEdge(a, b string) {
	f.t.Helper()
	for _, edge := range f.m.WaitEdges() {
		if (edge.FromRootID == a && edge.ToRootID == b) || (edge.FromRootID == b && edge.ToRootID == a) {
			f.t.Fatalf("unexpected edge %s -> %s (%s); graph = %v", edge.FromRootID, edge.ToRootID, edge.Reason, f.m.WaitEdges())
		}
	}
}

// TestB02WaitForGraphRecordsHolderAndQueueEdges pins the two edge kinds.
//
// A HOLDER edge is the classic "this root holds what I need". An OLDER_WAITER edge is the fair
// queue's own dependency: a younger request that would hinder an older waiter waits for that
// waiter, and that edge is a real deadlock participant. Reporting only holder edges would leave
// a queue-order cycle undetected.
func TestB02WaitForGraphRecordsHolderAndQueueEdges(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
	ctx := context.Background()
	row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
	row2 := indexRecord("t1", "PRIMARY", []byte("k2"))

	// The graph of an idle manager has no edges and no revision churn.
	before := f.m.Stats().Revision
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("idle graph has %d edges, want 0", len(edges))
	}

	// root-a holds row1. root-b waits for it: one HOLDER edge.
	f.owner("root-a").grant(ctx, rowBundle(row1)...)
	f.owner("root-b").wait(ctx, rowBundle(row1)...)
	holder := f.requireEdge("root-b", "root-a", EdgeHolder)
	if holder.RequestID == 0 {
		t.Fatal("the holder edge carries no request id")
	}
	if holder.ResourceID == 0 {
		t.Fatal("the holder edge carries no anonymous resource id")
	}
	if holder.Revision <= before {
		t.Fatalf("edge revision %d did not advance past %d", holder.Revision, before)
	}

	// root-c asks for row1 with an exclusive lock while root-b's exclusive request is older, so
	// root-c is held back by the queue order as well as by root-a.
	f.owner("root-c").wait(ctx, rowBundle(row1)...)
	f.requireEdge("root-c", "root-a", EdgeHolder)
	f.requireEdge("root-c", "root-b", EdgeOlderWaiter)

	// An unrelated resource changes nothing for the roots already in the graph: root-b taking row2
	// adds no new edge, and root-b's own wait on row1 stays a single holder edge even though
	// root-c now also waits on root-b.
	f.owner("root-b").grant(ctx, rowBundle(row2)...)
	if got := len(f.edgesFor("root-b")); got != 1 {
		t.Fatalf("root-b has %d edges, want the single holder edge on row1: %v", got, f.edgesFor("root-b"))
	}
}

// TestB02WaitForGraphHasNoDanglingEdges proves the graph is derived, not incrementally patched.
//
// Releasing the blocker grants the waiter, so the edge that recorded the wait must be gone:
// an edge that outlived its condition would make a later detection choose a victim for a
// deadlock that no longer exists.
func TestB02WaitForGraphHasNoDanglingEdges(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	waiter := f.owner("root-b").wait(ctx, rowBundle(row)...)
	f.requireEdge("root-b", "root-a", EdgeHolder)

	if _, err := f.m.Release("root-a", row); err != nil {
		t.Fatal(err)
	}
	f.owner("root-b").t.Helper()
	if _, err := f.m.WaitForGrant(ctx, "root-b", waiter.Wait); err != nil {
		t.Fatalf("waiter was not granted after the release: %v", err)
	}
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("granted waiter left %d edges behind: %v", len(edges), edges)
	}
	if _, found := f.m.DetectDeadlock(); found {
		t.Fatal("a resolved wait was reported as a deadlock")
	}
}

// TestB02TwoTransactionDeadlockChoosesTheLaterRoot is the contract's victim rule on the smallest
// possible cycle.
//
// The victim must be the root with the largest registration ordinal, so the transaction that has
// been running longest survives and the newest one retries.
func TestB02TwoTransactionDeadlockChoosesTheLaterRoot(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
	row2 := indexRecord("t1", "PRIMARY", []byte("k2"))

	f.owner("root-a").grant(ctx, rowBundle(row1)...)
	f.owner("root-b").grant(ctx, rowBundle(row2)...)
	a := f.owner("root-a").wait(ctx, rowBundle(row2)...)
	b := f.owner("root-b").wait(ctx, rowBundle(row1)...)

	f.requireEdge("root-a", "root-b", EdgeHolder)
	f.requireEdge("root-b", "root-a", EdgeHolder)

	summary, found := f.m.DetectDeadlock()
	if !found {
		t.Fatalf("the cycle was not detected; graph = %v", f.m.WaitEdges())
	}
	if summary.VictimRootID != "root-b" {
		t.Fatalf("victim = %s, want the larger ordinal root-b", summary.VictimRootID)
	}
	if len(summary.CycleRootIDs) < 2 {
		t.Fatalf("cycle = %v, want at least two roots", summary.CycleRootIDs)
	}
	if summary.GraphRevision == 0 {
		t.Fatal("the summary records no graph revision")
	}

	// The victim's wait is withdrawn with one definite status, so a caller blocked on it wakes.
	if status := b.Wait.Status(); status != RequestDeadlockVictim {
		t.Fatalf("victim request status = %s, want DEADLOCK_VICTIM", status)
	}
	if _, err := f.m.WaitForGrantResult(ctx, "root-b", b.Wait); err != ErrDeadlock {
		t.Fatalf("victim wait error = %v, want ErrDeadlock", err)
	}

	// The detector only asks the victim to stop: its grant is still held until the victim's own
	// cleanup releases it. Releasing it here is what lets the survivor proceed.
	if mode, ok := f.m.HeldMode("root-b", row2); !ok || mode != ModeX {
		t.Fatalf("victim grant was dropped by the detector: mode=%v ok=%v", mode, ok)
	}
	if status := a.Wait.Status(); status != RequestPending {
		t.Fatalf("survivor status = %s, want it still waiting", status)
	}

	f.m.ReleaseOwner("root-b")
	if _, err := f.m.WaitForGrant(ctx, "root-a", a.Wait); err != nil {
		t.Fatalf("survivor was not granted after the victim released: %v", err)
	}
	if mode, ok := f.m.HeldMode("root-a", row2); !ok || mode != ModeX {
		t.Fatalf("survivor holds %v/%v, want X on the contended row", mode, ok)
	}
}

// TestB02VictimSelectionIsIndependentOfDetectionOrder proves the rule does not depend on which
// root the traversal happened to start from.
//
// The same three-root cycle is built twice with the operations interleaved differently. Both runs
// must name the same victim, because the rule is the ordinal and nothing else.
func TestB02VictimSelectionIsIndependentOfDetectionOrder(t *testing.T) {
	build := func(t *testing.T, order []int) string {
		t.Helper()
		f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
		ctx := context.Background()
		rows := []ResourceRef{
			indexRecord("t1", "PRIMARY", []byte("k1")),
			indexRecord("t1", "PRIMARY", []byte("k2")),
			indexRecord("t1", "PRIMARY", []byte("k3")),
		}
		roots := []string{"root-a", "root-b", "root-c"}
		// Each root holds the next root's row, closing a three-node cycle.
		for i, rootID := range roots {
			f.owner(rootID).grant(ctx, rowBundle(rows[i])...)
		}
		for i := range order {
			idx := order[i]
			f.owner(roots[idx]).wait(ctx, rowBundle(rows[(idx+2)%3])...)
		}
		summary, found := f.m.DetectDeadlock()
		if !found {
			t.Fatalf("cycle not detected; graph = %v", f.m.WaitEdges())
		}
		return summary.VictimRootID
	}

	first := build(t, []int{0, 1, 2})
	second := build(t, []int{2, 1, 0})
	if first != second {
		t.Fatalf("victim depends on detection order: %s vs %s", first, second)
	}
	if first != "root-c" {
		t.Fatalf("victim = %s, want root-c (the largest ordinal)", first)
	}
}

// TestB02ThreeTransactionCycleIsDetectedAndResolvable covers the three-node ring end to end.
func TestB02ThreeTransactionCycleIsDetectedAndResolvable(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
	ctx := context.Background()
	row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
	row2 := indexRecord("t1", "PRIMARY", []byte("k2"))
	row3 := indexRecord("t1", "PRIMARY", []byte("k3"))

	// a→row1, b→row2, c→row3, then a waits for row2, b waits for row3, c waits for row1.
	f.owner("root-a").grant(ctx, rowBundle(row1)...)
	f.owner("root-b").grant(ctx, rowBundle(row2)...)
	f.owner("root-c").grant(ctx, rowBundle(row3)...)
	a := f.owner("root-a").wait(ctx, rowBundle(row2)...)
	b := f.owner("root-b").wait(ctx, rowBundle(row3)...)
	c := f.owner("root-c").wait(ctx, rowBundle(row1)...)
	_ = b

	f.requireEdge("root-a", "root-b", EdgeHolder)
	f.requireEdge("root-b", "root-c", EdgeHolder)
	f.requireEdge("root-c", "root-a", EdgeHolder)

	summary, found := f.m.DetectDeadlock()
	if !found {
		t.Fatalf("three-node cycle not detected; graph = %v", f.m.WaitEdges())
	}
	if summary.VictimRootID != "root-c" {
		t.Fatalf("victim = %s, want root-c", summary.VictimRootID)
	}
	if got := len(summary.CycleRootIDs); got != 3 {
		t.Fatalf("cycle length = %d, want 3 (%v)", got, summary.CycleRootIDs)
	}
	if status := c.Wait.Status(); status != RequestDeadlockVictim {
		t.Fatalf("victim status = %s, want DEADLOCK_VICTIM", status)
	}

	// Breaking the ring on the victim lets the ring unroll one link at a time. Ending root-c
	// releases row3, which root-b takes; root-b releasing row2 lets root-a take it. Each step is
	// asserted, so a stall names the link that did not move.
	f.m.ReleaseOwner("root-c")
	if _, err := f.m.WaitForGrant(ctx, "root-b", b.Wait); err != nil {
		t.Fatalf("root-b was not granted row3 after the victim released: %v", err)
	}
	if mode, ok := f.m.HeldMode("root-b", row3); !ok || mode != ModeX {
		t.Fatalf("root-b holds %v/%v on row3, want X", mode, ok)
	}
	if status := a.Wait.Status(); status != RequestPending {
		t.Fatalf("root-a status = %s while root-b still holds row2, want PENDING", status)
	}
	if _, err := f.m.Release("root-b", row2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.WaitForGrant(ctx, "root-a", a.Wait); err != nil {
		t.Fatalf("root-a was not granted row2 after root-b released it: %v", err)
	}
	for _, edge := range f.m.WaitEdges() {
		if edge.FromRootID == "root-c" {
			t.Fatalf("the released victim still has an edge: %v", edge)
		}
	}
}

// TestB02UpgradeDeadlockIsDetectedAndKeepsSurvivorShared covers the cycle that only exists
// because both roots upgraded.
//
// Two roots hold shared locks on one row and both request an exclusive upgrade. Neither can be
// granted while the other holds shared, so the pair is deadlocked. This is the shape that a
// holder-only graph reports as two independent waits and misses entirely.
func TestB02UpgradeDeadlockIsDetectedAndKeepsSurvivorShared(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
	f.owner("root-b").grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
	a := f.owner("root-a").wait(ctx, rowBundle(row)...)
	b := f.owner("root-b").wait(ctx, rowBundle(row)...)

	f.requireEdge("root-a", "root-b", EdgeHolder)
	f.requireEdge("root-b", "root-a", EdgeHolder)

	summary, found := f.m.DetectDeadlock()
	if !found {
		t.Fatalf("upgrade deadlock not detected; graph = %v", f.m.WaitEdges())
	}
	if summary.VictimRootID != "root-b" {
		t.Fatalf("victim = %s, want root-b", summary.VictimRootID)
	}
	if status := b.Wait.Status(); status != RequestDeadlockVictim {
		t.Fatalf("victim upgrade status = %s, want DEADLOCK_VICTIM", status)
	}
	// The victim kept the shared lock it already held: only its upgrade was withdrawn.
	if mode, ok := f.m.HeldMode("root-b", row); !ok || mode != ModeS {
		t.Fatalf("victim holds %v/%v after its upgrade was withdrawn, want S", mode, ok)
	}
	// The survivor is still waiting, because the victim's shared lock is still held.
	if status := a.Wait.Status(); status != RequestPending {
		t.Fatalf("survivor status = %s, want still waiting", status)
	}
	// Releasing the victim's shared lock lets the survivor's upgrade through.
	if _, err := f.m.Release("root-b", row); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.WaitForGrant(ctx, "root-a", a.Wait); err != nil {
		t.Fatalf("survivor upgrade was not granted: %v", err)
	}
	if mode, ok := f.m.HeldMode("root-a", row); !ok || mode != ModeX {
		t.Fatalf("survivor holds %v/%v, want X", mode, ok)
	}
}

// TestB02DeadlockDoesNotReleaseVictimGrants proves the cleanup contract.
//
// The detector must not "fix" a deadlock by deleting wait edges or by releasing grants: that
// would leave the victim's write set usable and produce a lost update. It only asks the root to
// stop; the root's own cleanup is what releases everything, and until then the graph still shows
// the holder edges that describe reality.
func TestB02DeadlockDoesNotReleaseVictimGrants(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
	row2 := indexRecord("t1", "PRIMARY", []byte("k2"))

	a := f.owner("root-a")
	b := f.owner("root-b")
	a.grant(ctx, rowBundle(row1)...)
	b.grant(ctx, rowBundle(row2)...)
	aWait := a.wait(ctx, rowBundle(row2)...)
	b.wait(ctx, rowBundle(row1)...)

	if _, found := f.m.DetectDeadlock(); !found {
		t.Fatal("deadlock not detected")
	}

	// The victim's grant is untouched and the survivor's wait on it is still recorded.
	if mode, ok := f.m.HeldMode("root-b", row2); !ok || mode != ModeX {
		t.Fatalf("victim lost its grant: %v/%v", mode, ok)
	}
	f.requireEdge("root-a", "root-b", EdgeHolder)
	// The survivor is still blocked, which is the "no live lock" part of the contract: deleting
	// the edge without ending the victim would have granted it a lock that is still held.
	if status := aWait.Wait.Status(); status != RequestPending {
		t.Fatalf("survivor status = %s, want still waiting", status)
	}

	// Full cleanup ends the root: grants, pending requests and edges all go.
	f.m.ReleaseOwner("root-b")
	if _, err := f.m.WaitForGrant(ctx, "root-a", aWait.Wait); err != nil {
		t.Fatalf("survivor was not granted after the victim root ended: %v", err)
	}
	if got := f.m.PendingRequests("root-b"); got != 0 {
		t.Fatalf("victim still has %d pending requests", got)
	}
	if _, ok := f.m.HeldMode("root-b", row2); ok {
		t.Fatal("victim still holds a grant after ReleaseOwner")
	}
	f.requireNoEdge("root-a", "root-b")
}

// TestB02DeadlockIsReportedOnceUntilTheVictimIsCleanedUp proves a single deadlock produces one
// victim.
//
// Reporting the same cycle again would pick a second victim, and the second abort could end a
// transaction that was about to make progress. The choice is idempotent until the victim's
// cleanup has actually happened.
func TestB02DeadlockIsReportedOnceUntilTheVictimIsCleanedUp(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
	row2 := indexRecord("t1", "PRIMARY", []byte("k2"))

	f.owner("root-a").grant(ctx, rowBundle(row1)...)
	f.owner("root-b").grant(ctx, rowBundle(row2)...)
	f.owner("root-a").wait(ctx, rowBundle(row2)...)
	f.owner("root-b").wait(ctx, rowBundle(row1)...)

	first, found := f.m.DetectDeadlock()
	if !found {
		t.Fatal("deadlock not detected")
	}
	if _, again := f.m.DetectDeadlock(); again {
		t.Fatal("the same deadlock was reported twice, which would pick a second victim")
	}
	// The victim's own cleanup is what makes the next report possible.
	f.m.ReleaseOwner(first.VictimRootID)
	if _, third := f.m.DetectDeadlock(); third {
		t.Fatal("a resolved deadlock was still reported")
	}
	if stats := f.m.Stats(); stats.Deadlocks != 1 {
		t.Fatalf("deadlock count = %d, want 1", stats.Deadlocks)
	}
}

// TestB02DetectionFailsClosedOnAnExhaustedWorkBudget pins the contract's budget rule.
//
// A traversal that runs out of its work budget is a resource failure. Reporting it as a deadlock
// would abort an innocent transaction, so it reports nothing; and a real deadlock must then still
// be resolvable by the timeout path.
func TestB02DetectionFailsClosedOnAnExhaustedWorkBudget(t *testing.T) {
	// A budget of one step cannot finish even the smallest walk.
	f := newDetectFixture(t, BudgetLimits{GraphWork: 1}, "root-a", "root-b", "root-c")
	ctx := context.Background()
	row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
	row2 := indexRecord("t1", "PRIMARY", []byte("k2"))
	row3 := indexRecord("t1", "PRIMARY", []byte("k3"))

	f.owner("root-a").grant(ctx, rowBundle(row1)...)
	f.owner("root-b").grant(ctx, rowBundle(row2)...)
	f.owner("root-c").grant(ctx, rowBundle(row3)...)
	a := f.owner("root-a").wait(ctx, rowBundle(row2)...)
	f.owner("root-b").wait(ctx, rowBundle(row3)...)
	f.owner("root-c").wait(ctx, rowBundle(row1)...)

	if summary, found := f.m.DetectDeadlock(); found {
		t.Fatalf("an exhausted budget chose a victim: %+v", summary)
	}
	// Nothing was aborted as a side effect of the failed traversal.
	if status := a.Wait.Status(); status != RequestPending {
		t.Fatalf("root-a status = %s after a failed traversal, want PENDING", status)
	}
	if stats := f.m.Stats(); stats.Deadlocks != 0 {
		t.Fatalf("deadlock count = %d after a failed traversal, want 0", stats.Deadlocks)
	}
}

// TestB02DetectionIgnoresNonCycles keeps a busy graph from being reported as a deadlock.
//
// A long chain of waiters, a diamond and a resource shared by many readers are all acyclic, and
// none of them may produce a victim.
func TestB02DetectionIgnoresNonCycles(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c", "root-d")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))
	other := indexRecord("t1", "PRIMARY", []byte("k2"))

	// A chain: a holds, b waits, c waits behind b, d waits behind c.
	f.owner("root-a").grant(ctx, rowBundle(row)...)
	f.owner("root-b").wait(ctx, rowBundle(row)...)
	f.owner("root-c").wait(ctx, rowBundle(row)...)
	f.owner("root-d").wait(ctx, rowBundle(row)...)

	// A diamond on a second resource: a holds it, b and c wait for it, d waits for b.
	f.owner("root-a").grant(ctx, rowBundle(other)...)
	f.owner("root-b").wait(ctx, rowBundle(other)...)
	f.owner("root-c").wait(ctx, rowBundle(other)...)

	if summary, found := f.m.DetectDeadlock(); found {
		t.Fatalf("an acyclic graph was reported as a deadlock: victim=%s cycle=%v", summary.VictimRootID, summary.CycleRootIDs)
	}
	// Many readers on one resource is the last acyclic shape worth pinning.
	f2 := newDetectFixture(t, BudgetLimits{}, "r1", "r2", "r3")
	f2.owner("r1").grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
	f2.owner("r2").grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
	f2.owner("r3").grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
	if _, found := f2.m.DetectDeadlock(); found {
		t.Fatal("three compatible shared holders were reported as a deadlock")
	}
}

// TestB02SelfDeadlockIsNeverReported guards the rule that one owner cannot deadlock with itself.
//
// A root that holds a lock and asks for it again re-enters; it must never appear as a cycle of
// length one, because aborting it would be wrong and pointless.
func TestB02SelfDeadlockIsNeverReported(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	result := f.owner("root-a").grant(ctx, rowBundle(row)...)
	if result.Op != LockOpGranted {
		t.Fatalf("first acquire op = %s, want GRANTED", result.Op)
	}
	again := f.owner("root-a").acquire(ctx, rowBundle(row)...)
	if again.Op != LockOpReentrant {
		t.Fatalf("second acquire op = %s, want REENTRANT", again.Op)
	}
	if _, found := f.m.DetectDeadlock(); found {
		t.Fatal("a re-entrant acquisition was reported as a deadlock")
	}
}

// TestB02VictimWakesEveryWaiterOfItsOwnRequests proves the abort notification is complete.
//
// A victim root can be waiting for several resources at once. Every one of those waits must end
// with the same verdict, or a blocked goroutine would be left with no status to observe.
func TestB02VictimWakesEveryWaiterOfItsOwnRequests(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
	row2 := indexRecord("t1", "PRIMARY", []byte("k2"))
	row3 := indexRecord("t1", "PRIMARY", []byte("k3"))

	// root-a holds three rows that root-b needs, and root-b holds one that root-a needs, which
	// closes the cycle while leaving root-b with three separate blocked requests.
	f.owner("root-a").grant(ctx, rowBundle(row1)...)
	f.owner("root-a").grant(ctx, rowBundle(row2)...)
	f.owner("root-a").grant(ctx, rowBundle(row3)...)
	f.owner("root-b").grant(ctx, rowBundle(indexRecord("t1", "PRIMARY", []byte("k4")))...)

	// root-b waits on three separate resources; each is its own queued request.
	waits := []*LockHandle{
		f.owner("root-b").wait(ctx, rowBundle(row1)...).Wait,
		f.owner("root-b").wait(ctx, rowBundle(row2)...).Wait,
		f.owner("root-b").wait(ctx, rowBundle(row3)...).Wait,
	}
	closeRow := indexRecord("t1", "PRIMARY", []byte("k4"))
	f.owner("root-a").wait(ctx, rowBundle(closeRow)...)

	summary, found := f.m.DetectDeadlock()
	if !found {
		t.Fatalf("cycle not detected; graph = %v", f.m.WaitEdges())
	}
	if summary.VictimRootID != "root-b" {
		t.Fatalf("victim = %s, want root-b", summary.VictimRootID)
	}
	if got := f.m.PendingRequests("root-b"); got != 0 {
		t.Fatalf("victim still has %d pending requests after being chosen", got)
	}
	for i, handle := range waits {
		if status := handle.Status(); status != RequestDeadlockVictim {
			t.Fatalf("victim request %d status = %s, want DEADLOCK_VICTIM", i, status)
		}
	}
}

// TestB02DeadlockStatisticsAndRingBound keeps the retained history bounded and counted.
func TestB02DeadlockStatisticsAndRingBound(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()

	// Repeat a two-root cycle more times than the ring can hold, resolving each one so the next
	// cycle is detectable.
	rounds := maxDeadlockSummaries + 3
	for i := 0; i < rounds; i++ {
		row1 := indexRecord("t1", "PRIMARY", []byte{'k', byte('0' + i%10), byte(i / 10)})
		row2 := indexRecord("t2", "PRIMARY", []byte{'k', byte('0' + i%10), byte(i / 10)})
		f.m.ReleaseOwner("root-a")
		f.m.ReleaseOwner("root-b")
		f.owners["root-a"] = newPublicOwner(t, f.m, "root-a")
		f.owners["root-b"] = newPublicOwner(t, f.m, "root-b")
		f.owner("root-a").grant(ctx, rowBundle(row1)...)
		f.owner("root-b").grant(ctx, rowBundle(row2)...)
		f.owner("root-a").wait(ctx, rowBundle(row2)...)
		f.owner("root-b").wait(ctx, rowBundle(row1)...)
		if _, found := f.m.DetectDeadlock(); !found {
			t.Fatalf("round %d: deadlock not detected", i)
		}
	}

	summaries, truncated := f.m.Deadlocks(0)
	if len(summaries) != maxDeadlockSummaries {
		t.Fatalf("retained summaries = %d, want the ring bound %d", len(summaries), maxDeadlockSummaries)
	}
	if truncated {
		t.Fatal("the ring reported truncation without an explicit smaller limit")
	}
	// Newest first, with strictly increasing detection ordinals.
	for i := 1; i < len(summaries); i++ {
		if summaries[i-1].DetectedOrdinal <= summaries[i].DetectedOrdinal {
			t.Fatalf("summaries are not newest first at %d: %d then %d", i, summaries[i-1].DetectedOrdinal, summaries[i].DetectedOrdinal)
		}
	}
	if stats := f.m.Stats(); stats.Deadlocks != uint64(rounds) {
		t.Fatalf("deadlock counter = %d, want %d (the counter is a lifetime total, not the ring size)", stats.Deadlocks, rounds)
	}

	// An explicit smaller limit truncates and says so.
	limited, limitedTruncated := f.m.Deadlocks(4)
	if len(limited) != 4 || !limitedTruncated {
		t.Fatalf("limit 4 returned %d rows, truncated=%v", len(limited), limitedTruncated)
	}
}

// TestB02ConcurrentDetectionNeverPicksTwoVictimsForOneCycle drives detection from several
// goroutines while a cycle is being set up.
//
// The contract requires one victim per deadlock. Concurrent detectors must therefore either
// observe the cycle once or observe that a victim was already chosen; a second victim would end a
// transaction that was about to make progress.
func TestB02ConcurrentDetectionNeverPicksTwoVictimsForOneCycle(t *testing.T) {
	const rounds = 50
	for i := 0; i < rounds; i++ {
		f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
		ctx := context.Background()
		row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
		row2 := indexRecord("t1", "PRIMARY", []byte("k2"))
		f.owner("root-a").grant(ctx, rowBundle(row1)...)
		f.owner("root-b").grant(ctx, rowBundle(row2)...)
		f.owner("root-a").wait(ctx, rowBundle(row2)...)
		f.owner("root-b").wait(ctx, rowBundle(row1)...)

		const detectors = 4
		start := make(chan struct{})
		var wg sync.WaitGroup
		victims := make(chan string, detectors)
		for d := 0; d < detectors; d++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if summary, found := f.m.DetectDeadlock(); found {
					victims <- summary.VictimRootID
				}
			}()
		}
		close(start)
		wg.Wait()
		close(victims)

		distinct := make(map[string]bool)
		count := 0
		for victim := range victims {
			distinct[victim] = true
			count++
		}
		if len(distinct) > 1 {
			t.Fatalf("round %d: concurrent detectors named %v", i, distinct)
		}
		if count > 1 {
			t.Fatalf("round %d: %d detectors reported a deadlock for a single cycle", i, count)
		}
		if f.m.Stats().Deadlocks > 1 {
			t.Fatalf("round %d: deadlock counter = %d, want at most one", i, f.m.Stats().Deadlocks)
		}
	}
}

// TestB02EpochResetAndCloseClearTheGraph proves an invalidated generation leaves no stale edge
// for a later detection to act on.
func TestB02EpochResetAndCloseClearTheGraph(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	waiter := f.owner("root-b").wait(ctx, rowBundle(row)...)
	f.requireEdge("root-b", "root-a", EdgeHolder)

	// ResetEpoch reports the number of owners it marked for finishing, which is two here: both
	// roots are invalidated even though only one of them was waiting.
	if invalidated := f.m.ResetEpoch(2, 0); invalidated != 2 {
		t.Fatalf("invalidated owners = %d, want both roots", invalidated)
	}
	if stats := f.m.Stats(); stats.EpochInvalidations != 1 {
		t.Fatalf("epoch reset count = %d, want 1", stats.EpochInvalidations)
	}
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("epoch reset left %d edges: %v", len(edges), edges)
	}
	if status := waiter.Wait.Status(); status != RequestInvalidated {
		t.Fatalf("waiter status = %s after an epoch reset, want INVALIDATED", status)
	}
	if _, err := f.m.WaitForGrantResult(ctx, "root-b", waiter.Wait); err != ErrLockInvalidated {
		t.Fatalf("waiter error = %v, want ErrLockInvalidated", err)
	}
	if _, found := f.m.DetectDeadlock(); found {
		t.Fatal("an invalidated wait was reported as a deadlock")
	}
}
