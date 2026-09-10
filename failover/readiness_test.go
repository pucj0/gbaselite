package failover

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Readiness belongs to the integration fixture, never to production routing.
// Every observation is a completed real discovery confirmation, not elapsed time.
type stableLeader struct {
	mutex         sync.Mutex
	address       string
	generation    uint64
	confirmations int
	changed       chan struct{}
}

func newStableLeader() *stableLeader { return &stableLeader{changed: make(chan struct{}, 1)} }
func (s *stableLeader) observe(e leaderTransition) {
	s.mutex.Lock()
	if (e.Reason == "initial-discovery" || e.Reason == "confirmed-current-leader" || e.Reason == "confirmed-new-leader") && e.To != "" {
		if e.To != s.address || e.Generation != s.generation {
			s.confirmations = 0
		}
		s.address, s.generation = e.To, e.Generation
		s.confirmations++
	} else {
		s.confirmations = 0
	}
	s.mutex.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
func (s *stableLeader) ready(address string, generation uint64, previous string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return address != "" && address != previous && address == s.address && generation == s.generation && s.confirmations >= 3
}
func awaitStableLeader(t *testing.T, ctx context.Context, r *Router, s *stableLeader, previous string) (string, uint64) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for {
		r.mutex.Lock()
		address, generation := r.leader, r.generation
		ready := s.ready(address, generation, previous)
		r.mutex.Unlock()
		if ready {
			return address, generation
		}
		select {
		case <-s.changed:
		case <-deadline.Done():
			t.Fatal("cluster did not reach three stable discovery confirmations:", deadline.Err())
			return "", 0
		}
	}
}
func TestProxyWaitsForStableInitialLeader(t *testing.T) {
	s := newStableLeader()
	s.observe(leaderTransition{To: "A", Generation: 1, Reason: "initial-discovery"})
	if s.ready("A", 1, "") {
		t.Fatal("initial discovery exposed as ready")
	}
	s.observe(leaderTransition{From: "A", To: "A", Generation: 1, Reason: "confirmed-current-leader"})
	s.observe(leaderTransition{From: "A", To: "A", Generation: 1, Reason: "probe-miss"})
	s.observe(leaderTransition{From: "A", To: "A", Generation: 1, Reason: "confirmed-current-leader"})
	if s.ready("A", 1, "") {
		t.Fatal("miss failed to reset confirmations")
	}
	for i := 0; i < 2; i++ {
		s.observe(leaderTransition{From: "A", To: "A", Generation: 1, Reason: "confirmed-current-leader"})
	}
	if !s.ready("A", 1, "") {
		t.Fatal("stable confirmations did not release fixture")
	}
	if s.ready("A", 2, "") || s.ready("A", 1, "A") {
		t.Fatal("stale generation or previous leader accepted")
	}
	s.observe(leaderTransition{From: "A", To: "B", Generation: 2, Reason: "confirmed-new-leader"})
	if s.ready("B", 2, "") {
		t.Fatal("leader transition failed to reset readiness")
	}
}
