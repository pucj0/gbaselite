package lockmanager

import (
	"context"
	"testing"
)

// deadlock_edges_test.go is the small, precise graph check the round asks for: the two-root cycle
// whose edges are of *different* kinds, and the graph returning to nothing once the victim has gone.
//
// The distinction matters because the two edges are found by completely different rules. A HOLDER
// edge is the resource rule: this root is waiting for something that root holds. An OLDER_WAITER edge
// is the fairness rule: a younger request that would hinder an older waiter has to wait for that
// waiter instead. A detector that only walked holder edges would miss a cycle that exists purely
// because two requests queued in an order that traps them, so a cycle must be recognised when the two
// kinds meet — and the smallest such cycle is two roots, one edge each, in opposite directions.
//
// Nothing here drives production code differently from the acceptance suite: the manager is built
// through the public façade, the requests are ordinary bundles, and detection is asked for explicitly
// because that is what makes the graph observable at the instant the cycle exists.

// TestB02TwoRootCycleMixesHolderAndOlderWaiterEdges builds the mixed two-root cycle, checks each edge
// by kind, and then checks that ending the victim leaves no edge behind.
func TestB02TwoRootCycleMixesHolderAndOlderWaiterEdges(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row1 := indexRecord("t1", "PRIMARY", []byte("k1"))
	row2 := indexRecord("t1", "PRIMARY", []byte("k2"))

	// root-a takes row1. root-b wants row1 *and* row2 in one atomic bundle, so it queues as a whole
	// and holds nothing: that is what makes it an older waiter on row2 without holding it.
	f.owner("root-a").grant(ctx, rowBundle(row1)...)
	twoRows := []ResourceRef{row1, row2, intentFor(ModeX, "t1")}
	blocked := f.owner("root-b").wait(ctx, twoRows...)

	// root-a now wants row2, which only root-b is waiting for. Being younger, root-a must wait for
	// root-b rather than take the row ahead of it — the fairness rule, and the second half of the cycle.
	newcomer := f.owner("root-a").wait(ctx, rowBundle(row2)...)

	// The cycle exists, and its two edges are of different kinds. Asserting them by kind is the point:
	// a single-kind view of this graph would show only one edge and no cycle at all.
	f.requireEdge("root-b", "root-a", EdgeHolder)
	f.requireEdge("root-a", "root-b", EdgeOlderWaiter)

	// The pair is joined in both directions, and nothing else is in the graph.
	if edges := f.m.WaitEdges(); len(edges) != 2 {
		t.Fatalf("the two-root cycle produced %d edges, want exactly 2: %v", len(edges), edges)
	}

	summary, found := f.m.DetectDeadlock()
	if !found {
		t.Fatalf("the mixed-kind cycle was not detected; graph = %v", f.m.WaitEdges())
	}
	if summary.VictimRootID != "root-b" {
		t.Fatalf("victim = %s, want the later-registered root-b", summary.VictimRootID)
	}
	if len(summary.CycleRootIDs) != 2 {
		t.Fatalf("cycle = %v, want both roots", summary.CycleRootIDs)
	}
	if status := blocked.Wait.Status(); status != RequestDeadlockVictim {
		t.Fatalf("victim request status = %s, want DEADLOCK_VICTIM", status)
	}
	if _, err := f.m.WaitForGrantResult(ctx, "root-b", blocked.Wait); err != ErrDeadlock {
		t.Fatalf("victim wait error = %v, want ErrDeadlock", err)
	}

	// Detection withdraws the victim's queued request — that is how the cycle is broken — so the
	// victim's own edges are already gone by the time it is asked to stop. This is the boundary the
	// round asks about: no edge may outlive the victim, and none does.
	for _, edge := range f.m.WaitEdges() {
		if edge.FromRootID == "root-b" || edge.ToRootID == "root-b" {
			t.Fatalf("edge %s -> %s (%s) outlived the victim's withdrawal; graph = %v", edge.FromRootID, edge.ToRootID, edge.Reason, f.m.WaitEdges())
		}
	}
	f.m.ReleaseOwner("root-b")

	// The victim has left: every edge that named it is gone, in both directions.
	for _, edge := range f.m.WaitEdges() {
		if edge.FromRootID == "root-b" || edge.ToRootID == "root-b" {
			t.Fatalf("edge %s -> %s (%s) outlived the victim; graph = %v", edge.FromRootID, edge.ToRootID, edge.Reason, f.m.WaitEdges())
		}
	}
	if stats := f.m.Stats(); stats.PendingRequests != 0 {
		t.Fatalf("the victim's request survived it: %+v", stats)
	}

	// The survivor is no longer hindered, so it is granted the row the victim was waiting for.
	if _, err := f.m.WaitForGrant(ctx, "root-a", newcomer.Wait); err != nil {
		t.Fatalf("the survivor was not granted after the victim left: %v", err)
	}
	if mode, ok := f.m.HeldMode("root-a", row2); !ok || mode != ModeX {
		t.Fatalf("the survivor holds %v/%v on the contended row, want X", mode, ok)
	}

	// Both roots gone: the graph is empty and nothing is queued.
	f.m.ReleaseOwner("root-a")
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("the graph still has %d edges after both roots ended: %v", len(edges), edges)
	}
	stats := f.m.Stats()
	if stats.WaitEdges != 0 || stats.PendingRequests != 0 || stats.ActiveOwners != 0 {
		t.Fatalf("lock statistics after cleanup = %+v, want an idle manager", stats)
	}
}
