package lockmanager

import "sort"

// graph.go holds the wait-for graph of one lock manager and its bounded deadlock detection.
//
// The graph is derived state. It is rebuilt from the live queues on every change rather than
// maintained incrementally, because a queue entry can be removed by a grant, a cancellation, a
// scope close, an owner release or an epoch reset, and an incrementally maintained graph would
// have to be corrected on every one of those paths. Rebuilding from the queues means the graph
// cannot disagree with the locks it describes, and the change revision makes a stale
// observation detectable instead of silently wrong.
//
// The two edge kinds are the contract's:
//
//   - HOLDER: the request waits for an incompatible grant another root holds.
//   - OLDER_WAITER: the request is held back by the fair queue, because an older request it
//     would hinder has not been granted yet.
//
// Both kinds are needed for detection. A cycle made only of HOLDER edges is a classic deadlock,
// but the queue-order rule can close a cycle that no holder edge closes on its own — two
// upgrades waiting for each other is the canonical case — so omitting OLDER_WAITER edges would
// leave a real deadlock undetected.

// waitGraph is the wait-for graph of one manager.
//
// Edges are keyed by (from root, to root, request) so an owner blocked by several resources of
// the same blocker stays one edge, and a request blocked by two different holders stays two
// edges with two reasons.
type waitGraph struct {
	// out maps a waiting root to the edges leaving it.
	out map[string]map[graphEdgeKey]LockEdge
	// revision advances on every structural change, so a detection or a diagnostics page can
	// tell whether the graph it read is still the graph it is looking at.
	revision uint64
}

// graphEdgeKey identifies one edge.
type graphEdgeKey struct {
	to      string
	request uint64
	reason  WaitEdgeReason
}

func newWaitGraph() *waitGraph {
	return &waitGraph{out: make(map[string]map[graphEdgeKey]LockEdge)}
}

// Revision reports the current graph revision.
func (g *waitGraph) Revision() uint64 { return g.revision }

// Size reports how many distinct edges the graph holds.
func (g *waitGraph) Size() int {
	total := 0
	for _, edges := range g.out {
		total += len(edges)
	}
	return total
}

// setEdges replaces the edges leaving one root with the given set.
//
// Replacing rather than merging is what keeps the graph free of dangling edges: a request that
// shrank its blocker set has its old edges removed here, so no edge can survive the condition
// that produced it. The revision advances only when the set actually changed, so an unchanged
// queue does not make every observation look stale.
func (g *waitGraph) setEdges(from string, edges []LockEdge) {
	// The revision is advanced first and stamped onto the stored edges, so an edge reports the
	// revision of the graph it is part of. Stamping a future revision would make an unchanged
	// graph look newer than it is, and leaving the source root unset would store every edge as
	// one that came from nobody.
	g.revision++
	next := make(map[graphEdgeKey]LockEdge, len(edges))
	for _, edge := range edges {
		key := graphEdgeKey{to: edge.ToRootID, request: edge.RequestID, reason: edge.Reason}
		edge.FromRootID = from
		edge.Revision = g.revision
		next[key] = edge
	}
	current := g.out[from]
	if sameEdges(current, next) {
		// The set is unchanged, so the revision advance was not warranted.
		g.revision--
		return
	}
	if len(next) == 0 {
		delete(g.out, from)
	} else {
		g.out[from] = next
	}
}

// removeRoot drops every edge leaving one root. It is the owner-release path.
func (g *waitGraph) removeRoot(from string) {
	if _, ok := g.out[from]; !ok {
		return
	}
	delete(g.out, from)
	g.revision++
}

// clear drops the whole graph. It is the epoch-reset and close path.
func (g *waitGraph) clear() {
	if len(g.out) == 0 {
		return
	}
	g.out = make(map[string]map[graphEdgeKey]LockEdge)
	g.revision++
}

// edges returns every edge in a stable order.
func (g *waitGraph) edges() []LockEdge {
	out := make([]LockEdge, 0, g.Size())
	for _, edges := range g.out {
		for _, edge := range edges {
			out = append(out, edge)
		}
	}
	sortEdges(out)
	return out
}

// edgesFrom returns the edges leaving one root in a stable order.
func (g *waitGraph) edgesFrom(from string) []LockEdge {
	edges := make([]LockEdge, 0, len(g.out[from]))
	for _, edge := range g.out[from] {
		edges = append(edges, edge)
	}
	sortEdges(edges)
	return edges
}

// sortEdges orders edges deterministically so a graph observation, a diagnostics page and a
// recorded summary do not depend on map iteration order.
func sortEdges(edges []LockEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].FromRootID != edges[j].FromRootID {
			return edges[i].FromRootID < edges[j].FromRootID
		}
		if edges[i].RequestID != edges[j].RequestID {
			return edges[i].RequestID < edges[j].RequestID
		}
		if edges[i].ToRootID != edges[j].ToRootID {
			return edges[i].ToRootID < edges[j].ToRootID
		}
		return edges[i].Reason < edges[j].Reason
	})
}

// sameEdges reports whether two edge sets are identical.
func sameEdges(a, b map[graphEdgeKey]LockEdge) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if _, ok := b[key]; !ok {
			return false
		}
	}
	return true
}

// detectCycle searches the wait-for graph for a cycle.
//
// The search is a bounded depth-first walk from each root in ordinal order, so the first cycle
// found is deterministic. It returns the cycle as a root list closing on its own start, plus
// the set of roots and edges that form it.
//
// The work budget bounds the traversal rather than the graph size: a traversal that exceeds it
// reports ErrBudgetGraph and no cycle, which is the contract's rule that an exhausted budget is
// a resource failure and must never be reported as a deadlock.
func (g *waitGraph) detectCycle(ordinalOf func(string) (uint64, bool), workLimit int) (cycleRoots []string, cycleEdges []LockEdge, exhausted bool) {
	roots := make([]string, 0, len(g.out))
	for from := range g.out {
		roots = append(roots, from)
	}
	sort.Slice(roots, func(i, j int) bool {
		left, leftOK := ordinalOf(roots[i])
		right, rightOK := ordinalOf(roots[j])
		if leftOK != rightOK {
			return leftOK
		}
		if left != right {
			return left < right
		}
		return roots[i] < roots[j]
	})

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(roots))
	stack := make([]string, 0, len(roots))
	edgeStack := make([]LockEdge, 0, len(roots))
	work := 0

	var walk func(from string) bool
	walk = func(from string) bool {
		color[from] = gray
		stack = append(stack, from)
		for _, edge := range g.edgesFrom(from) {
			if edge.ToRootID == from {
				// A self edge is never a cycle: one owner cannot wait for itself.
				continue
			}
			work++
			if workLimit > 0 && work > workLimit {
				exhausted = true
				return true
			}
			switch color[edge.ToRootID] {
			case gray:
				// Found a back edge: the cycle is the stack from the target onwards.
				start := 0
				for i, root := range stack {
					if root == edge.ToRootID {
						start = i
						break
					}
				}
				cycleRoots = append(cycleRoots, stack[start:]...)
				cycleEdges = append(cycleEdges, edgeStack[start:]...)
				cycleEdges = append(cycleEdges, edge)
				return true
			case white:
				edgeStack = append(edgeStack, edge)
				if walk(edge.ToRootID) {
					return true
				}
				edgeStack = edgeStack[:len(edgeStack)-1]
			}
		}
		stack = stack[:len(stack)-1]
		color[from] = black
		return false
	}

	for _, root := range roots {
		if color[root] != white {
			continue
		}
		stack = stack[:0]
		edgeStack = edgeStack[:0]
		if walk(root) {
			return cycleRoots, cycleEdges, exhausted
		}
	}
	return nil, nil, false
}

// pickVictim chooses the root to abort from one cycle.
//
// The contract fixes the rule: the victim is the root with the largest registration ordinal in
// the cycle. The ordinal is the root's registration order, so the rule is deterministic and
// independent of which request happened to be observed first — the transaction that has been
// running longest is the one asked to retry.
func pickVictim(cycleRoots []string, ordinalOf func(string) (uint64, bool)) (string, uint64) {
	victim := ""
	victimOrdinal := uint64(0)
	found := false
	for _, root := range cycleRoots {
		ordinal, ok := ordinalOf(root)
		if !ok {
			continue
		}
		if !found || ordinal > victimOrdinal || (ordinal == victimOrdinal && root < victim) {
			victim = root
			victimOrdinal = ordinal
			found = true
		}
	}
	return victim, victimOrdinal
}
