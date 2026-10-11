package lockmanager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// waiting_test.go covers the blocking half of the lock manager: wake-up order, deadlines,
// cancellation, the race between a release and a timeout, and the queue hygiene that keeps a
// withdrawn request from blocking anyone or being granted twice.
//
// Every ordering claim is established with a start gate or the virtual clock. No test sleeps to
// guess which event happened first.

// TestB02OneHolderWakesEveryWaiterInQueueOrder covers the basic wake-up contract.
//
// A single holder releasing must wake its waiters one at a time in queue order, not all at once:
// the first waiter gets the lock, and the rest stay queued behind it.
func TestB02OneHolderWakesEveryWaiterInQueueOrder(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-h", "w1", "w2", "w3")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-h").grant(ctx, rowBundle(row)...)

	waiters := make([]*LockHandle, 0, 3)
	for _, rootID := range []string{"w1", "w2", "w3"} {
		waiters = append(waiters, f.owner(rootID).wait(ctx, rowBundle(row)...).Wait)
	}
	// The queue order is asserted, not inferred: each successive request has a larger ordinal.
	for i := 1; i < len(waiters); i++ {
		if waiters[i-1].EnqueueOrdinal >= waiters[i].EnqueueOrdinal {
			t.Fatalf("waiter %d ordinal %d is not behind waiter %d ordinal %d",
				i, waiters[i].EnqueueOrdinal, i-1, waiters[i-1].EnqueueOrdinal)
		}
	}
	for i, handle := range waiters {
		if status := handle.Status(); status != RequestPending {
			t.Fatalf("waiter %d status = %s before any release, want PENDING", i, status)
		}
	}

	// The holder's release wakes exactly one waiter.
	if _, err := f.m.Release("root-h", row); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.WaitForGrant(ctx, "w1", waiters[0]); err != nil {
		t.Fatalf("w1 was not granted after the release: %v", err)
	}
	for i := 1; i < len(waiters); i++ {
		if status := waiters[i].Status(); status != RequestPending {
			t.Fatalf("waiter %d status = %s while w1 holds the row, want PENDING", i, status)
		}
	}

	// Each release hands the row to the next waiter in order.
	for i := 1; i < len(waiters); i++ {
		prev := fmt.Sprintf("w%d", i)
		if _, err := f.m.Release(prev, row); err != nil {
			t.Fatal(err)
		}
		next := fmt.Sprintf("w%d", i+1)
		if _, err := f.m.WaitForGrant(ctx, next, waiters[i]); err != nil {
			t.Fatalf("%s was not granted after %s released: %v", next, prev, err)
		}
	}
}

// TestB02WaitersBelowTheGrantLevelAreWokenTogether covers a shared wake-up.
//
// Once the exclusive holder is gone, every shared waiter at the head of the queue may be granted
// in one pass. The point is that the release is not "one waiter per release" when the modes are
// compatible.
func TestB02WaitersBelowTheGrantLevelAreWokenTogether(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-h", "r1", "r2", "r3")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-h").grant(ctx, rowBundle(row)...)
	handles := make([]*LockHandle, 0, 3)
	roots := []string{"r1", "r2", "r3"}
	for _, rootID := range roots {
		handles = append(handles, f.owner(rootID).wait(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...).Wait)
	}

	if _, err := f.m.Release("root-h", row); err != nil {
		t.Fatal(err)
	}
	for i, rootID := range roots {
		if _, err := f.m.WaitForGrant(ctx, rootID, handles[i]); err != nil {
			t.Fatalf("%s was not granted after the exclusive holder released: %v", rootID, err)
		}
		if mode, ok := f.m.HeldMode(rootID, row); !ok || mode != ModeS {
			t.Fatalf("%s holds %v/%v, want S", rootID, mode, ok)
		}
	}
	if size := f.m.PendingRequests("root-h"); size != 0 {
		t.Fatalf("the released holder still has %d pending requests", size)
	}
}

// TestB02WaiterTimesOutOnTheManagerClock covers the lock wait timeout.
//
// The deadline comes from the manager's clock, so the test advances the clock to the deadline and
// observes the outcome. Waiting for real time would make the assertion a race against the
// scheduler; here the order is a fact.
func TestB02WaiterTimesOutOnTheManagerClock(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	handle := f.owner("root-b").wait(ctx, rowBundle(row)...).Wait
	if status := handle.Status(); status != RequestPending {
		t.Fatalf("status = %s before the deadline, want PENDING", status)
	}

	// Nothing has passed yet, so the sweep must complete nothing.
	if completed := f.m.CheckTimeouts(); completed != 0 {
		t.Fatalf("the sweep completed %d requests before the deadline", completed)
	}

	// Advance the manager's clock past the default wait deadline.
	f.clock.Advance(defaultLockWaitTimeout + time.Millisecond)
	if completed := f.m.CheckTimeouts(); completed != 1 {
		t.Fatalf("the sweep completed %d requests, want the single expired waiter", completed)
	}
	if status := handle.Status(); status != RequestTimedOut {
		t.Fatalf("status = %s after the deadline, want TIMED_OUT", status)
	}
	if _, err := f.m.WaitForGrantResult(ctx, "root-b", handle); !errors.Is(err, ErrLockWaitTimeout) {
		t.Fatalf("wait error = %v, want ErrLockWaitTimeout", err)
	}
	// The timed-out request left no trace: nothing is pending and the graph has no edge for it.
	if pending := f.m.PendingRequests("root-b"); pending != 0 {
		t.Fatalf("the timed-out request is still pending %d times", pending)
	}
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("a timed-out request left %d edges: %v", len(edges), edges)
	}
	if stats := f.m.Stats(); stats.Timeouts != 1 {
		t.Fatalf("timeout counter = %d, want 1", stats.Timeouts)
	}
	// The holder is untouched: a timeout ends the waiter's statement, not the holder's lock.
	if mode, ok := f.m.HeldMode("root-a", row); !ok || mode != ModeX {
		t.Fatalf("the holder lost its lock to a waiter's timeout: %v/%v", mode, ok)
	}
}

// TestB02TimeoutUsesTheScopeDeadlineWhenItIsShorter proves the effective deadline is the earlier of
// the manager default and the scope deadline.
func TestB02TimeoutUsesTheScopeDeadlineWhenItIsShorter(t *testing.T) {
	m, clock := newPublicManager(t, BudgetLimits{})
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	if _, err := m.RegisterOwner("root-a", 1, 0, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RegisterOwner("root-b", 1, 0, 10); err != nil {
		t.Fatal(err)
	}
	holderScope, err := m.OpenScope("root-a", "a-txn", 0, ScopeStatement, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	holder, err := m.Acquire(ctx, "root-a", holderScope.ID, rowBundle(row))
	if err != nil || holder.Op != LockOpGranted {
		t.Fatalf("holder: op=%s err=%v", holder.Op, err)
	}
	// The waiter's scope carries a two-second deadline, far shorter than the 50-second default.
	waiterScope, err := m.OpenScope("root-b", "b-txn", 0, ScopeStatement, clock.Now().Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := m.Acquire(ctx, "root-b", waiterScope.ID, rowBundle(row))
	if err != nil || queued.Op != LockOpWait {
		t.Fatalf("waiter: op=%s err=%v", queued.Op, err)
	}

	// One second later is not enough, so the request is still live.
	clock.Advance(time.Second)
	if completed := m.CheckTimeouts(); completed != 0 {
		t.Fatalf("the sweep completed %d requests one second into a two-second deadline", completed)
	}
	// Past the scope deadline it expires, which proves the scope bound is the one in force.
	clock.Advance(2 * time.Second)
	if completed := m.CheckTimeouts(); completed != 1 {
		t.Fatalf("the sweep completed %d requests past the scope deadline, want 1", completed)
	}
	if _, err := m.WaitForGrantResult(ctx, "root-b", queued.Wait); !errors.Is(err, ErrLockWaitTimeout) {
		t.Fatalf("wait error = %v, want ErrLockWaitTimeout", err)
	}
}

// TestB02ContextCancellationWithdrawsTheWaiter covers caller cancellation.
//
// Cancelling the wait must withdraw the request and wake the caller, and it must not touch the
// holder. The caller's context is the bound on the wait, so this is the path a cancelled statement
// takes.
func TestB02ContextCancellationWithdrawsTheWaiter(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	queueCtx, cancelQueue := context.WithCancel(context.Background())
	handle := f.owner("root-b").wait(ctx, rowBundle(row)...).Wait
	// root-c queues behind root-b, so a correct withdrawal must not disturb the queue order.
	cHandle := f.owner("root-c").wait(ctx, rowBundle(row)...).Wait

	cancelQueue()
	if _, err := f.m.WaitForGrantResult(queueCtx, "root-b", handle); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v, want context.Canceled", err)
	}
	// The request is withdrawn, so a fresh acquisition can take the position root-b left: the
	// cancellation must have removed the queue entry rather than only stopped the caller waiting.
	if pending := f.m.PendingRequests("root-b"); pending != 0 {
		t.Fatalf("the cancelled request is still pending %d times", pending)
	}
	if _, err := f.m.Release("root-a", row); err != nil {
		t.Fatal(err)
	}
	// root-c is next in line and must be the one granted, not root-b.
	if _, err := f.m.WaitForGrant(ctx, "root-c", cHandle); err != nil {
		t.Fatalf("root-c was not granted: %v", err)
	}
	if mode, ok := f.m.HeldMode("root-b", row); ok {
		t.Fatalf("the cancelled waiter holds a lock it never received: %v", mode)
	}

	// Cancelling an already-settled wait reports the settled status, never a second outcome.
	if status := f.m.Cancel("root-c", cHandle); status != RequestGranted {
		t.Fatalf("cancelling a granted request reported %s, want GRANTED so the caller knows to release", status)
	}
}

// TestB02ContextCancelledBeforeTheReleaseStillAllowsTheWinnerToProceed covers the race the contract
// calls out explicitly: a cancellation and a grant decided in the same instant.
//
// Whichever the manager decides first is the single outcome, and if the grant won, the caller must
// be able to learn that and release rather than discard a lock it holds.
func TestB02ContextCancelledBeforeTheReleaseStillAllowsTheWinnerToProceed(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	waitCtx, cancelWait := context.WithCancel(context.Background())
	handle := f.owner("root-b").wait(ctx, rowBundle(row)...).Wait

	// The grant is applied first, then the caller's context ends. The recorded decision is the
	// grant, so the caller learns it holds the lock.
	if _, err := f.m.Release("root-a", row); err != nil {
		t.Fatal(err)
	}
	cancelWait()
	status, err := handle.Wait(waitCtx)
	if err != nil {
		t.Fatalf("wait error = %v, want the recorded grant", err)
	}
	if status != RequestGranted {
		t.Fatalf("status = %s, want GRANTED because the grant was decided first", status)
	}
	grant, err := f.m.WaitForGrant(context.Background(), "root-b", handle)
	if err != nil {
		t.Fatalf("grant lookup: %v", err)
	}
	if grant.OwnerID != "root-b" {
		t.Fatalf("grant owner = %s, want root-b", grant.OwnerID)
	}
	if _, err := f.m.Release("root-b", row); err != nil {
		t.Fatal(err)
	}
}

// TestB02ReleaseAndTimeoutRacingProducesOneOutcome covers the release-versus-deadline race.
//
// Both a grant and an expiry can be decided at the same moment. The manager records exactly one
// terminal status, so the caller can never see two outcomes or a lock that was granted and then
// silently withdrawn.
func TestB02ReleaseAndTimeoutRacingProducesOneOutcome(t *testing.T) {
	for i := 0; i < 60; i++ {
		f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
		ctx := context.Background()
		row := indexRecord("t1", "PRIMARY", []byte("k1"))

		f.owner("root-a").grant(ctx, rowBundle(row)...)
		handle := f.owner("root-b").wait(ctx, rowBundle(row)...).Wait

		// Admit the deadline and the release concurrently through one gate.
		start := make(chan struct{})
		var releaseDone, sweepDone sync.WaitGroup
		releaseDone.Add(1)
		sweepDone.Add(1)
		go func() {
			defer releaseDone.Done()
			<-start
			f.m.Release("root-a", row)
		}()
		go func() {
			defer sweepDone.Done()
			<-start
			// Advancing the clock here is safe: the fake clock serialises its own state, and the
			// point is to make both decisions contend for the same manager critical section.
			f.clock.Advance(defaultLockWaitTimeout + time.Millisecond)
			f.m.CheckTimeouts()
		}()
		close(start)
		releaseDone.Wait()
		sweepDone.Wait()

		status := handle.Status()
		switch status {
		case RequestGranted:
			// The grant won, so the waiter holds the row and must be able to release it.
			if mode, ok := f.m.HeldMode("root-b", row); !ok || mode != ModeX {
				t.Fatalf("round %d: GRANTED but root-b holds %v/%v", i, mode, ok)
			}
			if _, err := f.m.Release("root-b", row); err != nil {
				t.Fatal(err)
			}
		case RequestTimedOut:
			// The timeout won, so root-b holds nothing and root-a (or nobody) holds the row.
			if _, ok := f.m.HeldMode("root-b", row); ok {
				t.Fatalf("round %d: TIMED_OUT but root-b holds a lock", i)
			}
		default:
			t.Fatalf("round %d: status = %s, want exactly GRANTED or TIMED_OUT", i, status)
		}
		// Reading the status again reports the same single outcome.
		if again := f.m.WaitStatus("root-b", handle); again != status {
			t.Fatalf("round %d: status changed from %s to %s", i, status, again)
		}
		if pending := f.m.PendingRequests("root-b"); pending != 0 {
			t.Fatalf("round %d: %d pending requests left behind", i, pending)
		}
		f.m.ReleaseOwner("root-b")
		f.m.ReleaseOwner("root-a")
	}
}

// TestB02WithdrawnWaiterIsRemovedFromTheQueueWithoutDoubleGrant covers queue hygiene for every
// withdrawal path.
//
// A request that timed out or was cancelled must leave its resource's queue completely, or a later
// release could grant a request nobody is waiting for and the next owner would find the resource
// held by a transaction that already gave up.
func TestB02WithdrawnWaiterIsRemovedFromTheQueueWithoutDoubleGrant(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "t1", "t2", "next")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)

	// t1 times out, t2 is cancelled, and both are withdrawn.
	timedOut := f.owner("t1").wait(ctx, rowBundle(row)...).Wait
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelled := f.owner("t2").wait(ctx, rowBundle(row)...).Wait
	if status := f.m.Cancel("t2", cancelled); status != RequestCanceled {
		t.Fatalf("cancel status = %s, want CANCELED", status)
	}
	f.clock.Advance(defaultLockWaitTimeout + time.Millisecond)
	if completed := f.m.CheckTimeouts(); completed != 1 {
		t.Fatalf("the sweep completed %d requests, want only the expired one", completed)
	}
	if status := timedOut.Status(); status != RequestTimedOut {
		t.Fatalf("status = %s, want TIMED_OUT", status)
	}
	_ = cancelCtx
	cancel()

	// A new waiter now takes the queue, and the release grants it exactly once.
	next := f.owner("next").wait(ctx, rowBundle(row)...).Wait
	if _, err := f.m.Release("root-a", row); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.WaitForGrant(ctx, "next", next); err != nil {
		t.Fatalf("the surviving waiter was not granted: %v", err)
	}
	// The withdrawn requests must not have been granted afterwards.
	for _, handle := range []*LockHandle{timedOut, cancelled} {
		if status := handle.Status(); status == RequestGranted {
			t.Fatal("a withdrawn request was granted")
		}
	}
	if pending := f.m.PendingRequests("t1") + f.m.PendingRequests("t2"); pending != 0 {
		t.Fatalf("withdrawn requests are still pending %d times", pending)
	}
}

// TestB02OwnerReleaseWakesEveryWaiterAndLeavesNoTraces covers the cleanup path.
func TestB02OwnerReleaseWakesEveryWaiterAndLeavesNoTraces(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-h", "w1", "w2", "w3")
	ctx := context.Background()
	rows := []ResourceRef{
		indexRecord("t1", "PRIMARY", []byte("k1")),
		indexRecord("t1", "PRIMARY", []byte("k2")),
		indexRecord("t1", "PRIMARY", []byte("k3")),
	}
	roots := []string{"w1", "w2", "w3"}
	for _, row := range rows {
		f.owner("root-h").grant(ctx, rowBundle(row)...)
	}
	handles := make([]*LockHandle, 0, len(roots))
	for i, rootID := range roots {
		handles = append(handles, f.owner(rootID).wait(ctx, rowBundle(rows[i])...).Wait)
	}
	f.requireEdge("w1", "root-h", EdgeHolder)
	f.requireEdge("w2", "root-h", EdgeHolder)
	f.requireEdge("w3", "root-h", EdgeHolder)

	f.m.ReleaseOwner("root-h")

	// Every waiter is granted, and the graph holds no edge to the released root.
	for i, rootID := range roots {
		if _, err := f.m.WaitForGrant(ctx, rootID, handles[i]); err != nil {
			t.Fatalf("%s was not granted after the holder was released: %v", rootID, err)
		}
	}
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("the released root left %d edges: %v", len(edges), edges)
	}
	if got := f.m.PendingRequests("root-h"); got != 0 {
		t.Fatalf("the released root is still pending %d times", got)
	}
	// Three rows are held, and the three holders share one table-intention entry, so four entries
	// remain. The released root's own table intention is gone with it.
	if size := f.m.LockTableSize(); size != 4 {
		t.Fatalf("lock table size = %d, want three held rows plus one shared table intention", size)
	}
	// The holder's release left no waiter entry behind on any resource.
	for _, row := range rows {
		if state := f.m.LockStateFor(row); state != LockStateExclusive {
			t.Fatalf("row %s state = %s after the holder released, want EXCLUSIVE", row.Key, state)
		}
	}
	// Three rows and one shared table intention per holder is six components, all charged: the
	// release of the previous holder transferred the locks rather than leaving them uncharged.
	usage := f.m.Ledger().Totals()
	if usage.Components != 6 || usage.Waiters != 0 || usage.GraphEdges != 0 {
		t.Fatalf("ledger after the release = %+v, want six held components and nothing waiting", usage)
	}
	for _, rootID := range roots {
		f.m.ReleaseOwner(rootID)
	}
	requireBudgetZero(t, f.m.Ledger())
}

// TestB02CloseWakesEveryWaiter covers the shutdown path: a closed manager must not leave a caller
// blocked on a handle nobody will ever complete.
func TestB02CloseWakesEveryWaiter(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "w1", "w2")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	handles := []*LockHandle{
		f.owner("w1").wait(ctx, rowBundle(row)...).Wait,
		f.owner("w2").wait(ctx, rowBundle(row)...).Wait,
	}
	f.m.Close()
	for i, handle := range handles {
		if status := handle.Status(); status != RequestCanceled {
			t.Fatalf("waiter %d status = %s after Close, want CANCELED so the caller wakes", i, status)
		}
		if _, err := f.m.WaitForGrantResult(ctx, fmt.Sprintf("w%d", i+1), handle); err == nil {
			t.Fatalf("waiter %d reported a successful wait after Close", i)
		}
	}
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("a closed manager reports %d edges: %v", len(edges), edges)
	}
}

// TestB02ConcurrentWaitingAndDetectionIsDataRaceFree hammers the waiting, detection and
// diagnostics paths together.
//
// The point is not the specific sequence but that no combination of them observes a torn view or
// leaves the manager inconsistent. Goroutines are admitted through one gate, and the run must end
// with the accounting exactly at zero. With a race detector available this same test is the
// threading check; without one it still catches lost grants and leaked entries.
func TestB02ConcurrentWaitingAndDetectionIsDataRaceFree(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
	ctx := context.Background()

	const workers = 8
	const rounds = 40
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, workers*3)

	for w := 0; w < workers; w++ {
		wg.Add(3)
		// A worker that holds, waits and releases.
		go func(w int) {
			defer wg.Done()
			<-start
			rootID := fmt.Sprintf("root-%c", 'a'+w%3)
			for i := 0; i < rounds; i++ {
				row := indexRecord("t1", "PRIMARY", []byte(fmt.Sprintf("k%d-%d", w, i)))
				result, err := f.m.AcquireWith(ctx, rootID, f.owners[rootID].scopeID, rowBundle(row), func() bool { return true })
				if err != nil {
					// A resource rejection or owner ending is a legitimate outcome under contention.
					if errors.Is(err, ErrOwnerEnding) || errors.Is(err, ErrLockResourceLimit) {
						continue
					}
					errs <- fmt.Errorf("acquire: %w", err)
					return
				}
				if result.Op == LockOpWait {
					if _, err := f.m.WaitForGrantResult(ctx, rootID, result.Wait); err != nil {
						if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrLockWaitTimeout) && !errors.Is(err, ErrDeadlock) {
							errs <- fmt.Errorf("wait: %w", err)
							return
						}
					}
				}
				if _, err := f.m.Release(rootID, row); err != nil && !errors.Is(err, ErrOwnerUnknown) {
					errs <- fmt.Errorf("release: %w", err)
					return
				}
			}
		}(w)
		// A worker that runs detection.
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				if summary, found := f.m.DetectDeadlock(); found {
					if summary.VictimRootID == "" {
						errs <- errors.New("a detected deadlock named no victim")
						return
					}
				}
			}
		}()
		// A worker that reads diagnostics.
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				page := f.m.Diagnostics(DiagnosticsOptions{Limit: 32, IncludeWaiters: true, IncludeDeadlocks: true})
				for _, resource := range page.Resources {
					if len(resource.HolderIDs) != len(resource.HolderModes) {
						errs <- errors.New("resource row has mismatched holder lists")
						return
					}
				}
				for _, wait := range page.Waits {
					if wait.OwnerID == "" {
						errs <- errors.New("wait row has no owner")
						return
					}
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Every root is released, so the manager must return to a clean state.
	for _, rootID := range []string{"root-a", "root-b", "root-c"} {
		f.m.ReleaseOwner(rootID)
	}
	if size := f.m.LockTableSize(); size != 0 {
		t.Fatalf("lock table size = %d after the run, want 0", size)
	}
	if edges := f.m.WaitEdges(); len(edges) != 0 {
		t.Fatalf("%d edges survived the run: %v", len(edges), edges)
	}
	requireBudgetZero(t, f.m.Ledger())
}
