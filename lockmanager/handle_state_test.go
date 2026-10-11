package lockmanager

import (
	"context"
	"sync"
	"testing"
	"time"
)

// handle_state_test.go is B02 defect #2: the synchronization of the shared handle decision.
//
// LockHandle exists so a caller can ask what happened to a queued request without racing the
// notification channel: Status is idempotent, never consumes the notification, and a caller
// that is granted a lock must be able to learn that even when its own cancellation or its wait
// deadline fired at the same moment. The defect was that LockHandle.Status read
// requestState.status with no synchronization at all while the manager wrote it under the
// manager mutex only, so a concurrent Status or Wait raced a grant/cancel/timeout decision.
//
// Every test here drives that decision from more than one goroutine. Ordering is established
// by channels, barriers and the WaitGroup hand-off, and deadlines come from the virtual clock:
// no test sleeps to create an order. Where a race has two legal winners the test accepts either
// but asserts the invariants that must hold for each, and it always asserts that the recorded
// terminal status never changes afterwards.

// handleRaceFixture is one manager with a holder and a waiter on the same record.
//
// It is the shape every handle-state race needs: root-holder holds X on the record, root-waiter
// is queued for an identical bundle (the record plus its IX table intention), and the single
// release of the holder is what makes the waiter's decision terminal. Building it in one helper
// keeps each test reading as the race it describes.
type handleRaceFixture struct {
	t      *testing.T
	m      *manager
	clock  *FakeClock
	holder *lockFixture
	waiter *lockFixture
	// bundle is the waiter's request, and therefore exactly the components whose grant the
	// handle must cover when the waiter wins.
	bundle []ResourceRef
	// record is the record component, which is the one the holder blocks on.
	record ResourceRef
	handle *LockHandle
}

// newHandleRaceFixture queues one waiter behind one holder on the same record.
func newHandleRaceFixture(t *testing.T, holderID, waiterID string) *handleRaceFixture {
	t.Helper()
	m, clock := newTestManager(t, BudgetLimits{})
	holder := newLockFixture(t, m, clock, holderID)
	waiter := newLockFixture(t, m, clock, waiterID)
	record := indexRecord("t1", "PRIMARY", []byte("k1"))
	bundle := rowBundle(record)
	holder.acquire(bundle...)
	holder.requireGranted()
	waiter.acquire(bundle...)
	handle := waiter.requireWaiting()
	return &handleRaceFixture{
		t:      t,
		m:      m,
		clock:  clock,
		holder: holder,
		waiter: waiter,
		bundle: bundle,
		record: record,
		handle: handle,
	}
}

// releaseHolder drops every component of the holder's bundle, which is what decides the
// waiter's request. It reports the first error instead of calling t.Fatalf so it can be called
// from a goroutine: FailNow must run on the test goroutine.
func (f *handleRaceFixture) releaseHolder() error {
	for _, component := range f.bundle {
		if _, err := f.m.Release(f.holder.rootID, component); err != nil {
			return err
		}
	}
	return nil
}

// requireHoldsBundle asserts that one root really holds every component of the waiter's bundle,
// which is what makes a GRANTED status trustworthy rather than a claim. situation names the
// branch of the race the assertion belongs to, so a failure reads as the outcome it contradicts.
func (f *handleRaceFixture) requireHoldsBundle(rootID string, situation string) {
	f.t.Helper()
	for _, component := range f.bundle {
		mode, ok := f.m.HeldMode(rootID, component)
		if !ok || mode != component.Mode {
			f.t.Fatalf("%s: %s holds %q/%v on %s, want %s", situation, rootID, mode, ok, component.Key, component.Mode)
		}
	}
}

// requireHoldsNothing asserts that one root holds no component of the bundle.
func (f *handleRaceFixture) requireHoldsNothing(rootID string, situation string) {
	f.t.Helper()
	for _, component := range f.bundle {
		if mode, ok := f.m.HeldMode(rootID, component); ok {
			f.t.Fatalf("%s: %s unexpectedly holds %q on %s", situation, rootID, mode, component.Key)
		}
	}
}

// requireSettledStatus asserts that the handle's request has reached its terminal status and
// returns it. The broadcast makes the check repeatable: nothing is consumed, so it may be called
// as often as a test likes.
func (f *handleRaceFixture) requireSettledStatus() RequestStatus {
	f.t.Helper()
	select {
	case <-f.handle.settled():
	default:
		f.t.Fatal("the handle was not settled")
		return RequestCanceled
	}
	status := f.handle.Status()
	if !status.Terminal() {
		f.t.Fatalf("the settled handle reports %s, want a terminal status", status)
	}
	return status
}

// TestB02HandleGrantRacingCancelPicksOneWinner is scenario (a): one goroutine cancels while
// another releases the holder's grant, so the manager may decide either outcome first.
//
// Both outcomes are legal, because the manager mutex decides which critical section runs first.
// What must never differ is the invariant behind the status:
//
//   - a GRANTED status means the waiter really holds every component it asked for, and the
//     cancel that lost must not drop that held lock (the caller has to be told to release it);
//   - a CANCELED status means the waiter holds nothing, and the release that arrived afterwards
//     must not grant a request that was already withdrawn.
//
// The rounds exist to let a real scheduler produce both interleavings; each round uses a fresh
// manager, so no round can observe another round's state. The gate and the WaitGroups create the
// overlap, not a sleep.
func TestB02HandleGrantRacingCancelPicksOneWinner(t *testing.T) {
	for round := 0; round < 40; round++ {
		f := newHandleRaceFixture(t, "root-holder", "root-waiter")

		start := make(chan struct{})
		var releaseDone, cancelDone sync.WaitGroup
		var releaseErr error
		var cancelStatus RequestStatus
		releaseDone.Add(1)
		cancelDone.Add(1)
		go func() {
			defer releaseDone.Done()
			<-start
			releaseErr = f.releaseHolder()
		}()
		go func() {
			defer cancelDone.Done()
			<-start
			cancelStatus = f.m.Cancel(f.waiter.rootID, f.handle)
		}()
		close(start)
		releaseDone.Wait()
		cancelDone.Wait()
		if releaseErr != nil {
			t.Fatalf("round %d: release: %v", round, releaseErr)
		}

		status := f.handle.Status()
		if status != RequestGranted && status != RequestCanceled {
			t.Fatalf("round %d: status = %s, want exactly GRANTED or CANCELED", round, status)
		}
		if cancelStatus != status {
			t.Fatalf("round %d: Cancel reported %s but the handle records %s", round, cancelStatus, status)
		}
		if delivered := f.requireSettledStatus(); delivered != status {
			t.Fatalf("round %d: the delivered notification %s disagrees with the recorded status %s", round, delivered, status)
		}

		switch status {
		case RequestGranted:
			f.requireHoldsBundle(f.waiter.rootID, "grant won")
			// A second cancel finds no queued request and must report the recorded grant without
			// touching the held lock.
			if again := f.m.Cancel(f.waiter.rootID, f.handle); again != RequestGranted {
				t.Fatalf("round %d: Cancel of a granted request reported %s, want GRANTED", round, again)
			}
			f.requireHoldsBundle(f.waiter.rootID, "cancel lost to the grant")
		case RequestCanceled:
			f.requireHoldsNothing(f.waiter.rootID, "cancel won")
			// The holder's release already happened; it must not have granted the withdrawn request.
			if again := f.handle.Status(); again != RequestCanceled {
				t.Fatalf("round %d: the cancelled request changed to %s", round, again)
			}
			if completed := f.m.CheckTimeouts(); completed != 0 {
				t.Fatalf("round %d: the sweep completed %d settled requests", round, completed)
			}
			f.requireHoldsNothing(f.waiter.rootID, "release lost to the cancel")
		}
		if pending := f.m.PendingRequests(f.waiter.rootID); pending != 0 {
			t.Fatalf("round %d: %d requests are still pending after a terminal status", round, pending)
		}
	}
}

// TestB02HandleGrantRacingDeadlineProducesOneOutcome is scenario (b): the wait deadline fires at
// the same moment the holder's release grants the request.
//
// The first subtest is the deterministic half of FR-007: once a grant is recorded, passing the
// deadline and sweeping must not rewrite it as TIMED_OUT. The second subtest admits both
// decisions through one gate so they contend for the manager's critical section, and then
// re-applies the loser to prove the recorded terminal status is final. A request that became
// TIMED_OUT must never be granted by the late release, and a request that became GRANTED must
// never be timed out by the late sweep.
func TestB02HandleGrantRacingDeadlineProducesOneOutcome(t *testing.T) {
	t.Run("deadline after a settled grant is ignored", func(t *testing.T) {
		f := newHandleRaceFixture(t, "root-holder", "root-waiter")
		if err := f.releaseHolder(); err != nil {
			t.Fatalf("release: %v", err)
		}
		if status := f.handle.Status(); status != RequestGranted {
			t.Fatalf("status = %s, want GRANTED", status)
		}
		// The deadline is reached after the grant. The sweep and the per-handle check must both see
		// a settled request and leave it alone.
		f.clock.Advance(defaultLockWaitTimeout + time.Millisecond)
		if f.m.timeOutIfExpired(f.waiter.rootID, f.handle) {
			t.Fatal("timeOutIfExpired converted a granted request into a timeout")
		}
		if completed := f.m.CheckTimeouts(); completed != 0 {
			t.Fatalf("the sweep completed %d settled requests, want 0", completed)
		}
		if status := f.handle.Status(); status != RequestGranted {
			t.Fatalf("status = %s after the deadline passed, want GRANTED", status)
		}
		f.requireHoldsBundle(f.waiter.rootID, "grant before the deadline")
	})

	t.Run("concurrent release and deadline sweep", func(t *testing.T) {
		for round := 0; round < 40; round++ {
			f := newHandleRaceFixture(t, "root-holder", "root-waiter")

			// The release and the deadline sweep are admitted through one gate. Advancing the virtual
			// clock is what reaches the request's deadline; CheckTimeouts is the manager-side half
			// that records the timeout, so both decisions genuinely contend for the manager mutex.
			start := make(chan struct{})
			var releaseDone, sweepDone sync.WaitGroup
			var releaseErr error
			releaseDone.Add(1)
			sweepDone.Add(1)
			go func() {
				defer releaseDone.Done()
				<-start
				releaseErr = f.releaseHolder()
			}()
			go func() {
				defer sweepDone.Done()
				<-start
				f.clock.Advance(defaultLockWaitTimeout + time.Millisecond)
				f.m.CheckTimeouts()
			}()
			close(start)
			releaseDone.Wait()
			sweepDone.Wait()
			if releaseErr != nil {
				t.Fatalf("round %d: release: %v", round, releaseErr)
			}

			status := f.handle.Status()
			switch status {
			case RequestGranted:
				f.requireHoldsBundle(f.waiter.rootID, "grant won the race")
			case RequestTimedOut:
				f.requireHoldsNothing(f.waiter.rootID, "timeout won the race")
			default:
				t.Fatalf("round %d: status = %s after a release and a deadline, want exactly GRANTED or TIMED_OUT", round, status)
			}
			if delivered := f.requireSettledStatus(); delivered != status {
				t.Fatalf("round %d: the delivered notification %s disagrees with the recorded status %s", round, delivered, status)
			}

			// Re-apply both losers after the outcome is recorded: the status must not move, and the
			// loser must not create a second grant or a timeout after a grant.
			f.clock.Advance(defaultLockWaitTimeout + time.Millisecond)
			if f.m.timeOutIfExpired(f.waiter.rootID, f.handle) {
				t.Fatalf("round %d: timeOutIfExpired converted a %s request into a timeout", round, status)
			}
			if completed := f.m.CheckTimeouts(); completed != 0 {
				t.Fatalf("round %d: the sweep completed %d settled requests", round, completed)
			}
			if err := f.releaseHolder(); err != nil {
				t.Fatalf("round %d: late release: %v", round, err)
			}
			if again := f.handle.Status(); again != status {
				t.Fatalf("round %d: the recorded status changed from %s to %s after the losing decisions ran", round, status, again)
			}
			if observed := f.m.WaitStatus(f.waiter.rootID, f.handle); observed != status {
				t.Fatalf("round %d: WaitStatus = %s, want the recorded %s", round, observed, status)
			}
		}
	})
}

// TestB02HandleReleaseWakesTheWaiterExactlyOnce is scenario (c): the holder releases while the
// waiter is queued, and the waiter's own goroutine is blocked in Wait.
//
// The wake is a real hand-off: the waiting goroutine is inside LockHandle.Wait before the
// release runs, so the grant has to travel through the recorded decision and the notification
// channel. The test asserts that the waiter reaches GRANTED exactly once, that the grant covers
// every component it asked for, and that the release produced no second notification.
func TestB02HandleReleaseWakesTheWaiterExactlyOnce(t *testing.T) {
	f := newHandleRaceFixture(t, "root-holder", "root-waiter")

	// The context is a wall-clock safety net for a stuck waiter, never an ordering device: the
	// grant is what ends the wait, and the 30 seconds are only there so a regression fails the
	// test instead of hanging the package until the go test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type waitOutcome struct {
		status RequestStatus
		err    error
	}
	waited := make(chan waitOutcome, 1)
	entered := NewBarrier()
	go func() {
		entered.Arrive()
		status, err := f.handle.Wait(ctx)
		waited <- waitOutcome{status: status, err: err}
	}()
	if !entered.Wait(ctx) {
		t.Fatal("the waiting goroutine never started")
	}

	var releaseDone sync.WaitGroup
	var releaseErr error
	releaseDone.Add(1)
	go func() {
		defer releaseDone.Done()
		releaseErr = f.releaseHolder()
	}()
	releaseDone.Wait()
	if releaseErr != nil {
		t.Fatalf("release: %v", releaseErr)
	}

	outcome := <-waited
	if outcome.err != nil {
		t.Fatalf("Wait returned an error (%v); a grant must be reported without one", outcome.err)
	}
	if outcome.status != RequestGranted {
		t.Fatalf("Wait reported %s, want GRANTED", outcome.status)
	}
	if status := f.handle.Status(); status != RequestGranted {
		t.Fatalf("Status = %s after the grant, want GRANTED", status)
	}
	f.requireHoldsBundle(f.waiter.rootID, "waiter woken by the release")

	// The terminal status is a broadcast rather than a value handed to one receiver: reading it
	// again returns the same status, and a second Wait on the same handle reports the recorded
	// grant immediately instead of blocking.
	if status := f.requireSettledStatus(); status != RequestGranted {
		t.Fatalf("the settled handle reports %s, want GRANTED", status)
	}
	status, err := f.handle.Wait(ctx)
	if err != nil || status != RequestGranted {
		t.Fatalf("the second Wait reported %s/%v, want GRANTED with no error", status, err)
	}
	if pending := f.m.PendingRequests(f.waiter.rootID); pending != 0 {
		t.Fatalf("%d requests are still pending after the grant", pending)
	}
}

// TestB02HandleStatusIsSafeUnderConcurrentPolling is scenario (d): many goroutines read
// Status while the manager records the terminal status.
//
// THIS IS THE TEST THAT MUST BE RUN WITH -race ON A RACE-CAPABLE MACHINE. The race detector
// cannot run in this environment (runtime/cgo needs a C compiler and none is installed), so the
// assertions here check the properties a race would destroy rather than the race itself: every
// observation is PENDING or the one terminal status, never an unknown or torn value, never a
// second terminal status, never a regression from terminal back to PENDING, and the polling
// never consumes the notification. The pollers stop on a shared done channel and the test is
// provably deadlock-free: after the grant the status is terminal for ever, so every poller
// observes it and the main goroutine can wait for all of them before closing the channel.
func TestB02HandleStatusIsSafeUnderConcurrentPolling(t *testing.T) {
	const pollers = 16
	f := newHandleRaceFixture(t, "root-holder", "root-waiter")

	type observations struct {
		terminal  RequestStatus
		unknown   RequestStatus
		mismatch  bool
		regressed bool
	}
	results := make([]observations, pollers)
	// ready is sent once per poller after its first read, so the main goroutine knows every
	// poller has observed the pending status before it grants.
	ready := make(chan struct{}, pollers)
	// sawTerminal is sent once per poller when it first observes a terminal status.
	sawTerminal := make(chan RequestStatus, pollers)
	done := make(chan struct{})
	var wg sync.WaitGroup

	for slot := 0; slot < pollers; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			observed := &results[slot]
			first := true
			for {
				select {
				case <-done:
					return
				default:
				}
				status := f.handle.Status()
				if first {
					first = false
					ready <- struct{}{}
				}
				switch status {
				case RequestPending:
					if observed.terminal != "" {
						// A terminal status must never regress to PENDING.
						observed.regressed = true
					}
				case RequestGranted, RequestTimedOut, RequestContextCanceled, RequestCanceled,
					RequestDeadlockVictim, RequestInvalidated, RequestResourceRejected:
					if observed.terminal == "" {
						observed.terminal = status
						sawTerminal <- status
					} else if status != observed.terminal {
						// Two different terminal statuses would mean two decisions won.
						observed.mismatch = true
					}
				default:
					// Anything outside the defined set is a torn or unknown read.
					observed.unknown = status
				}
			}
		}(slot)
	}

	// Every poller is polling and has seen the pending status before the grant is recorded.
	for i := 0; i < pollers; i++ {
		<-ready
	}
	if err := f.releaseHolder(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// The grant is terminal for ever, so every poller observes it; waiting here is bounded by the
	// pollers' own progress, not by a timeout.
	for i := 0; i < pollers; i++ {
		<-sawTerminal
	}
	close(done)
	wg.Wait()

	terminal := f.handle.Status()
	if terminal != RequestGranted {
		t.Fatalf("the recorded status is %s, want GRANTED", terminal)
	}
	seen := map[RequestStatus]bool{}
	for slot, observed := range results {
		if observed.unknown != "" {
			t.Fatalf("poller %d observed the unknown status %q", slot, observed.unknown)
		}
		if observed.mismatch {
			t.Fatalf("poller %d observed two different terminal statuses", slot)
		}
		if observed.regressed {
			t.Fatalf("poller %d observed a terminal status and then PENDING again", slot)
		}
		if observed.terminal != terminal {
			t.Fatalf("poller %d observed terminal %q, want %q", slot, observed.terminal, terminal)
		}
		seen[observed.terminal] = true
	}
	if len(seen) != 1 {
		t.Fatalf("the pollers observed %d distinct terminal statuses: %v", len(seen), seen)
	}
	// Polling is a read: the one notification is still queued for the real waiter.
	if delivered := f.requireSettledStatus(); delivered != terminal {
		t.Fatalf("the notification %s disagrees with the recorded status %s", delivered, terminal)
	}
}

// TestB02HandleStatusNeverChangesAfterItBecomesTerminal is scenario (e): once a request is
// terminal, every later decision attempt observes the recorded status instead of replacing it.
//
// It covers a repeated Cancel, a deadline that passes after a grant, a timeout sweep over a
// settled request, and a repeated WaitForGrantResult. The invariant is one line: the status
// after all of them is the status that was recorded first.
func TestB02HandleStatusNeverChangesAfterItBecomesTerminal(t *testing.T) {
	t.Run("cancel twice on a queued request", func(t *testing.T) {
		f := newHandleRaceFixture(t, "root-holder", "root-waiter")
		if first := f.m.Cancel(f.waiter.rootID, f.handle); first != RequestCanceled {
			t.Fatalf("first Cancel = %s, want CANCELED", first)
		}
		if second := f.m.Cancel(f.waiter.rootID, f.handle); second != RequestCanceled {
			t.Fatalf("second Cancel = %s, want the recorded CANCELED", second)
		}
		// The late release must not grant the withdrawn request, and the late deadline must not
		// rewrite it either.
		if err := f.releaseHolder(); err != nil {
			t.Fatalf("late release: %v", err)
		}
		f.clock.Advance(defaultLockWaitTimeout + time.Millisecond)
		if f.m.timeOutIfExpired(f.waiter.rootID, f.handle) {
			t.Fatal("timeOutIfExpired rewrote a cancelled request as TIMED_OUT")
		}
		if completed := f.m.CheckTimeouts(); completed != 0 {
			t.Fatalf("the sweep completed %d settled requests", completed)
		}
		if status := f.handle.Status(); status != RequestCanceled {
			t.Fatalf("status = %s, want the recorded CANCELED", status)
		}
		if status := f.m.WaitStatus(f.waiter.rootID, f.handle); status != RequestCanceled {
			t.Fatalf("WaitStatus = %s, want the recorded CANCELED", status)
		}
		f.requireHoldsNothing(f.waiter.rootID, "cancelled request")
	})

	t.Run("timeout and cancel after a grant", func(t *testing.T) {
		f := newHandleRaceFixture(t, "root-holder", "root-waiter")
		if err := f.releaseHolder(); err != nil {
			t.Fatalf("release: %v", err)
		}
		if status := f.handle.Status(); status != RequestGranted {
			t.Fatalf("status = %s, want GRANTED", status)
		}
		if f.m.timeOutIfExpired(f.waiter.rootID, f.handle) {
			t.Fatal("timeOutIfExpired rewrote a granted request as TIMED_OUT")
		}
		f.clock.Advance(defaultLockWaitTimeout + time.Millisecond)
		if completed := f.m.CheckTimeouts(); completed != 0 {
			t.Fatalf("the sweep completed %d settled requests", completed)
		}
		if status := f.m.Cancel(f.waiter.rootID, f.handle); status != RequestGranted {
			t.Fatalf("Cancel = %s, want GRANTED so the caller knows to release", status)
		}
		if status := f.handle.Status(); status != RequestGranted {
			t.Fatalf("status = %s after every later attempt, want GRANTED", status)
		}
		f.requireHoldsBundle(f.waiter.rootID, "grant survived every later attempt")
	})

	t.Run("WaitForGrantResult twice on the same handle", func(t *testing.T) {
		f := newDetectFixture(t, BudgetLimits{}, "root-holder", "root-waiter")
		ctx := context.Background()
		row := indexRecord("t1", "PRIMARY", []byte("k1"))
		f.owner("root-holder").grant(ctx, rowBundle(row)...)
		handle := f.owner("root-waiter").wait(ctx, rowBundle(row)...).Wait
		if _, err := f.m.Release("root-holder", row); err != nil {
			t.Fatalf("release: %v", err)
		}

		first, err := f.m.WaitForGrantResult(ctx, "root-waiter", handle)
		if err != nil || first.Op != LockOpGranted {
			t.Fatalf("first WaitForGrantResult = %s/%v, want a grant", first.Op, err)
		}
		second, err := f.m.WaitForGrantResult(ctx, "root-waiter", handle)
		if err != nil || second.Op != LockOpGranted {
			t.Fatalf("second WaitForGrantResult = %s/%v, want the same recorded grant", second.Op, err)
		}
		if second.Grant.OwnerID != "root-waiter" {
			t.Fatalf("second grant owner = %q, want root-waiter", second.Grant.OwnerID)
		}
		if status := handle.Status(); status != RequestGranted {
			t.Fatalf("status = %s, want GRANTED", status)
		}
		if status := f.m.WaitStatus("root-waiter", handle); status != RequestGranted {
			t.Fatalf("WaitStatus = %s, want GRANTED", status)
		}
		if pending := f.m.PendingRequests("root-waiter"); pending != 0 {
			t.Fatalf("%d requests are still pending after a granted request", pending)
		}
		if _, err := f.m.Release("root-waiter", row); err != nil {
			t.Fatalf("release: %v", err)
		}
	})
}

// TestB02HandleWaitAndStatusAgreeUnderConcurrency is scenario (f): several goroutines call Wait
// on the same handle while the status is settled, and a concurrent poller reads Status.
//
// Every waiter is woken by the request's own broadcast, and the contexts end in the same instant
// as the grant. The contract says Wait re-checks the recorded decision before reporting a
// cancellation, so every one of them must report the same GRANTED status: a caller whose context
// ended in the same instant as the grant must not be told the lock was cancelled.
func TestB02HandleWaitAndStatusAgreeUnderConcurrency(t *testing.T) {
	const waiters = 8
	f := newHandleRaceFixture(t, "root-holder", "root-waiter")

	type waitOutcome struct {
		status RequestStatus
		err    error
	}
	outcomes := make(chan waitOutcome, waiters)
	cancels := make([]context.CancelFunc, waiters)
	entered := make(chan struct{}, waiters)
	for i := 0; i < waiters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go func(ctx context.Context) {
			entered <- struct{}{}
			status, err := f.handle.Wait(ctx)
			outcomes <- waitOutcome{status: status, err: err}
		}(ctx)
	}
	// Every waiting goroutine has reached its Wait call before the grant is decided.
	for i := 0; i < waiters; i++ {
		<-entered
	}

	// One goroutine polls Status while the others wait, so the two read paths run concurrently on
	// the same handle.
	observed := make(chan RequestStatus, 1)
	go func() {
		for {
			status := f.handle.Status()
			if status.Terminal() {
				observed <- status
				return
			}
		}
	}()

	// The grant is decided first; only then do the contexts end. A waiter that selects the context
	// branch must still find the recorded grant when it re-checks.
	if err := f.releaseHolder(); err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, cancel := range cancels {
		cancel()
	}

	for i := 0; i < waiters; i++ {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf("waiter %d returned %v; the recorded grant must win over the context ending", i, outcome.err)
		}
		if outcome.status != RequestGranted {
			t.Fatalf("waiter %d reported %s, want GRANTED", i, outcome.status)
		}
	}
	if status := <-observed; status != RequestGranted {
		t.Fatalf("the concurrent Status poller observed %s, want GRANTED", status)
	}
	if status := f.handle.Status(); status != RequestGranted {
		t.Fatalf("Status = %s, want GRANTED", status)
	}
	// The outcome is a broadcast every waiter reads from the decision, so no caller can consume
	// another caller's wake-up or leave one behind.
	if status := f.requireSettledStatus(); status != RequestGranted {
		t.Fatalf("the settled handle reports %s, want GRANTED", status)
	}
	if _, err := f.handle.Wait(context.Background()); err != nil {
		t.Fatalf("a later Wait on the settled handle returned %v, want the recorded grant", err)
	}
}
