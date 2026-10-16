package replication

import "gbaselite/mvcc"

// export_test.go exports testing seams exclusively for test packages without exposing
// test-only methods in the production binary API.

func SetWaitStartedHook(hook func()) func() {
	waitHookMu.Lock()
	defer waitHookMu.Unlock()
	prev := waitHook
	waitHook = hook
	return prev
}

func (n *Node) HandleLeaderLossForTesting(tenure, term uint64) {
	n.onLeaderLoss(tenure, term, 0)
}
func (n *Node) HandleLeaderLossWithGenForTesting(tenure, term, gen uint64) {
	n.onLeaderLoss(tenure, term, gen)
}
func (n *Node) HandleLeaderElectedForTesting(term uint64) { n.onLeaderElected(term) }
func (n *Node) CurrentTenureForTesting() (uint64, uint64) { return n.currentTenureAndTerm() }
func (n *Node) StoreForTesting() *mvcc.Store              { return n.store }
