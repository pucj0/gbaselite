package mvcc

import (
	"context"
	"sync"
	"testing"
	"time"
)

// B01 test-support primitives. Test-only: no production behaviour lives here.
//
// Transaction concurrency tests must synchronize on protocol boundaries instead
// of guessing with sleeps. These primitives give the B01 tests those boundaries:
//
//   - publicationGate parks a transaction exactly when a chosen command kind is
//     about to be proposed. Parking on "commit" is the only way to observe the
//     window between "versions staged durably" and "publication marker durable",
//     which is the boundary the spec treats as the commit point (R9 / D5 /
//     INV-008). It works for the staged/Raft-shaped path because that path is
//     selected whenever the proposer is not the Store itself.
//   - awaitSignal / awaitCommit wait for a deterministic signal with a bounded
//     failure guard. The guard exists only so a broken implementation fails one
//     test with a clear message instead of hanging the suite; no assertion
//     depends on how long the wait takes.
const boundaryGuard = 30 * time.Second

// commitOutcome is the result of a commit running on another goroutine.
type commitOutcome struct {
	sequence uint64
	err      error
}

// publicationGate wraps a Proposer so the first proposal of gateOn waits for
// releaseNow before reaching the underlying proposer.
type publicationGate struct {
	inner   Proposer
	gateOn  string
	parked  chan struct{}
	release chan struct{}

	parkOnce    sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	kinds       []string
}

func newPublicationGate(inner Proposer, gateOn string) *publicationGate {
	return &publicationGate{inner: inner, gateOn: gateOn, parked: make(chan struct{}), release: make(chan struct{})}
}

func (g *publicationGate) Barrier(ctx context.Context) error { return g.inner.Barrier(ctx) }

func (g *publicationGate) Propose(ctx context.Context, command Command) (Result, error) {
	g.mu.Lock()
	g.kinds = append(g.kinds, command.Kind)
	g.mu.Unlock()
	if command.Kind == g.gateOn {
		g.parkOnce.Do(func() { close(g.parked) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	return g.inner.Propose(ctx, command)
}

// releaseNow lets the parked proposal continue. It is idempotent and safe to
// call from a cleanup function.
func (g *publicationGate) releaseNow() { g.releaseOnce.Do(func() { close(g.release) }) }

// observedKinds returns the command kinds seen so far, in order.
func (g *publicationGate) observedKinds() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.kinds...)
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	guard := time.NewTimer(boundaryGuard)
	defer guard.Stop()
	select {
	case <-signal:
	case <-guard.C:
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitCommit(t *testing.T, done <-chan commitOutcome) commitOutcome {
	t.Helper()
	guard := time.NewTimer(boundaryGuard)
	defer guard.Stop()
	select {
	case outcome := <-done:
		return outcome
	case <-guard.C:
		t.Fatal("timed out waiting for transaction commit")
		return commitOutcome{}
	}
}
