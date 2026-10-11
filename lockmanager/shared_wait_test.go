package lockmanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// shared_wait_test.go covers the contract of one pending request shared by several callers of the
// same root.
//
// A joined acquisition is a caller waiting for a request it does not own. That has two
// consequences this file pins:
//
//   - the wake-up has to reach *every* caller, not whichever one happens to consume a single
//     notification, and each of them must observe one identical terminal status and grant;
//   - a caller that joined must not be able to destroy the request: its own cancellation stops its
//     own wait, while the owning caller's cancellation withdraws the request for everybody.
//
// Every ordering claim is established by parking the waiters inside the blocking wait with the
// package's test hook and then deciding the outcome, so "all of them were woken" is a fact rather
// than a scheduling guess. The only timeout in this file is a hang detector for a waiter nobody
// woke; it never establishes an order.

// waitOutcome is what one waiting caller observed.
type waitOutcome struct {
	result AcquireResult
	err    error
}

// sharedWaitFixture is one root with two scopes, one blocker, and one contended bundle.
type sharedWaitFixture struct {
	t       *testing.T
	m       *Manager
	clock   *FakeClock
	rootID  string
	creator uint64
	joiner  uint64
	blocker publicOwner
	bundle  []ResourceRef
	first   *LockHandle
}

// contendedRow is the row every caller of the fixture asks for.
var contendedRow = indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k1"))

func newSharedWaitFixture(t *testing.T) *sharedWaitFixture {
	t.Helper()
	m, clock := newPublicManager(t, BudgetLimits{})
	f := &sharedWaitFixture{t: t, m: m, clock: clock, rootID: "root-a"}
	// The blocker registers first, so the waiting root has the larger registration ordinal. A
	// deadlock test can then make the waiting root the contract's victim (largest ordinal).
	f.blocker = newPublicOwner(t, m, "root-b")
	if _, err := m.RegisterOwner(f.rootID, 1, 0, 10); err != nil {
		t.Fatal(err)
	}
	f.creator = f.openScope("a-creator")
	f.joiner = f.openScope("a-joiner")
	f.bundle = []ResourceRef{contendedRow, ixIntent("t1")}
	f.blocker.grant(context.Background(), contendedRow, ixIntent("t1"))
	f.first = f.queue(f.creator)
	return f
}

// openScope opens one more request origin for the waiting root.
func (f *sharedWaitFixture) openScope(origin string) uint64 {
	f.t.Helper()
	scope, err := f.m.OpenScope(f.rootID, origin, 0, ScopeStatement, time.Time{})
	if err != nil {
		f.t.Fatal(err)
	}
	return scope.ID
}

// queue acquires the contended bundle from one scope and returns the queued request handle.
func (f *sharedWaitFixture) queue(scopeID uint64) *LockHandle {
	f.t.Helper()
	result, err := f.m.AcquireWith(context.Background(), f.rootID, scopeID, f.bundle, func() bool { return true })
	if err != nil {
		f.t.Fatalf("acquire: %v", err)
	}
	if result.Op != LockOpWait || result.Wait == nil {
		f.t.Fatalf("acquisition op = %s, want WAIT", result.Op)
	}
	return result.Wait
}

// join queues the same bundle from the second scope, which the manager answers by joining the
// request that already waits instead of starting a second one.
func (f *sharedWaitFixture) join() *LockHandle {
	f.t.Helper()
	handle := f.queue(f.joiner)
	if handle.RequestID != f.first.RequestID {
		f.t.Fatalf("the second acquisition started request %d, want to join request %d", handle.RequestID, f.first.RequestID)
	}
	if pending := f.m.PendingRequests(f.rootID); pending != 1 {
		f.t.Fatalf("pending requests = %d, want the single shared request", pending)
	}
	return handle
}

// releaseBlocker ends the blocker's hold on the contended bundle. It tolerates a repeated call so
// a fixture teardown can use it unconditionally.
func (f *sharedWaitFixture) releaseBlocker() {
	f.t.Helper()
	for _, component := range []ResourceRef{contendedRow, ixIntent("t1")} {
		if _, err := f.m.Release(f.blocker.rootID, component); err != nil && !errors.Is(err, ErrOwnerUnknown) {
			f.t.Fatalf("release blocker: %v", err)
		}
	}
}

// watch starts one goroutine that waits for the request behind a handle.
func (f *sharedWaitFixture) watch(ctx context.Context, handle *LockHandle) <-chan waitOutcome {
	outcomes := make(chan waitOutcome, 1)
	go func() {
		result, err := f.m.WaitForGrantResult(ctx, f.rootID, handle)
		outcomes <- waitOutcome{result: result, err: err}
	}()
	return outcomes
}

// waiterPark parks every caller that reaches the blocking wait.
type waiterPark struct {
	arrived chan struct{}
	gate    chan struct{}
	once    sync.Once
}

// parkWaiters installs the wait hook so that count callers can be held inside the blocking wait.
func parkWaiters(t *testing.T, count int) *waiterPark {
	t.Helper()
	park := &waiterPark{arrived: make(chan struct{}, count), gate: make(chan struct{})}
	previous := waitHook
	waitHook = func(*LockHandle) {
		park.arrived <- struct{}{}
		<-park.gate
	}
	t.Cleanup(func() { waitHook = previous })
	return park
}

// awaitAll blocks until every expected caller is parked inside the wait.
func (p *waiterPark) awaitAll(t *testing.T, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		select {
		case <-p.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d waiting callers reached the blocking wait", i, count)
		}
	}
}

// release lets every parked caller proceed.
func (p *waiterPark) release() { p.once.Do(func() { close(p.gate) }) }

// awaitOutcome reads one caller's outcome and fails the test if it never returns. The bound is a
// hang detector, not an ordering device.
func awaitOutcome(t *testing.T, outcomes <-chan waitOutcome, what string) waitOutcome {
	t.Helper()
	select {
	case outcome := <-outcomes:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was never woken", what)
		return waitOutcome{}
	}
}

// requireOneGrantForAll asserts that every caller observed the same granted outcome.
func requireOneGrantForAll(t *testing.T, outcomes []waitOutcome, what string) uint64 {
	t.Helper()
	if len(outcomes) == 0 {
		t.Fatal("no outcomes to compare")
	}
	grantID := uint64(0)
	for i, outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("%s %d returned %v, want a grant", what, i, outcome.err)
		}
		if outcome.result.Op != LockOpGranted {
			t.Fatalf("%s %d op = %s, want GRANTED", what, i, outcome.result.Op)
		}
		if outcome.result.Grant.GrantID == 0 {
			t.Fatalf("%s %d carries no grant id", what, i)
		}
		if i == 0 {
			grantID = outcome.result.Grant.GrantID
			continue
		}
		if outcome.result.Grant.GrantID != grantID {
			t.Fatalf("%s %d observed grant %d, want the shared grant %d", what, i, outcome.result.Grant.GrantID, grantID)
		}
	}
	return grantID
}

// requireNoResidue asserts that no waiter, queue entry, graph edge or budget charge survived, and
// releases everything the fixture still holds so the terminal-state check is meaningful.
//
// A leaked waiting caller is asserted through the manager's own accounting rather than through a
// goroutine count: a caller still blocked inside the wait keeps its request queued and its waiter
// charge, so zero pending requests, zero wait edges, zero accounted waiters and a zero ledger
// together say that no caller was left behind. The goroutines themselves are accounted for by the
// tests: each one's outcome is received, and a caller that never returned would trip the hang
// detector in awaitOutcome instead of leaking silently.
func requireNoResidue(t *testing.T, f *sharedWaitFixture) {
	t.Helper()
	if pending := f.m.PendingRequests(f.rootID); pending != 0 {
		t.Fatalf("%d requests of %s are still pending", pending, f.rootID)
	}
	// Release everything the fixture still holds: the blocker's claim and the waiting root's own
	// grants. A deadlock test deliberately leaves a second request of the blocker queued behind one
	// of those grants, so the global counters only reach zero after the owners end.
	f.releaseBlocker()
	f.m.ReleaseOwner(f.rootID)
	f.m.ReleaseOwner(f.blocker.rootID)
	if waiters := f.m.Ledger().Waiters(); waiters != 0 {
		t.Fatalf("the manager still accounts %d waiters after every owner ended, want 0", waiters)
	}
	stats := f.m.Stats()
	if stats.PendingRequests != 0 || stats.WaitEdges != 0 {
		t.Fatalf("statistics after every owner ended = pending %d / edges %d, want zero", stats.PendingRequests, stats.WaitEdges)
	}
	if size := f.m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d entries after every owner ended, want 0", size)
	}
	requireBudgetZero(t, f.m.Ledger())
}

// TestB02TwoWaitersOfOneSharedRequestBothObserveTheGrant is the reproduction of the shared
// notification defect.
//
// Both callers are parked inside the wait — provably unsettled and provably not taking the
// "already decided" fast path — and only then is the request granted. Every one of them must
// return with the same grant; a wake-up that reaches one caller and leaves the other blocked is
// the defect.
func TestB02TwoWaitersOfOneSharedRequestBothObserveTheGrant(t *testing.T) {
	f := newSharedWaitFixture(t)
	joiner := f.join()

	park := parkWaiters(t, 2)
	creatorDone := f.watch(context.Background(), f.first)
	joinerDone := f.watch(context.Background(), joiner)
	park.awaitAll(t, 2)

	f.releaseBlocker()
	park.release()

	outcomes := []waitOutcome{
		awaitOutcome(t, creatorDone, "the owning caller"),
		awaitOutcome(t, joinerDone, "the joined caller"),
	}
	requireOneGrantForAll(t, outcomes, "a caller of the shared request")
	for _, handle := range []*LockHandle{f.first, joiner} {
		if status := handle.Status(); status != RequestGranted {
			t.Fatalf("handle %d status = %s, want GRANTED", handle.RequestID, status)
		}
	}
	requireNoResidue(t, f)
}

// TestB02TenWaitersOfOneSharedRequestAllObserveTheGrant proves the wake-up is a broadcast: any
// number of callers share one request, and every one of them observes the same outcome.
func TestB02TenWaitersOfOneSharedRequestAllObserveTheGrant(t *testing.T) {
	f := newSharedWaitFixture(t)
	handles := []*LockHandle{f.first}
	for i := 0; i < 9; i++ {
		handles = append(handles, f.join())
	}
	if pending := f.m.PendingRequests(f.rootID); pending != 1 {
		t.Fatalf("pending requests = %d, want the single shared request", pending)
	}

	park := parkWaiters(t, len(handles))
	done := make([]<-chan waitOutcome, len(handles))
	for i, handle := range handles {
		done[i] = f.watch(context.Background(), handle)
	}
	park.awaitAll(t, len(handles))

	f.releaseBlocker()
	park.release()

	outcomes := make([]waitOutcome, len(handles))
	for i := range handles {
		outcomes[i] = awaitOutcome(t, done[i], "a caller of the shared request")
	}
	requireOneGrantForAll(t, outcomes, "a caller of the shared request")
	requireNoResidue(t, f)
}

// TestB02JoinerContextEndDoesNotWithdrawTheSharedRequest pins the cancellation ownership rule: a
// caller that joined stops waiting when its own context ends, and the request it joined stays
// queued for the caller that owns it.
func TestB02JoinerContextEndDoesNotWithdrawTheSharedRequest(t *testing.T) {
	f := newSharedWaitFixture(t)
	joiner := f.join()

	park := parkWaiters(t, 2)
	joinerCtx, cancelJoiner := context.WithCancel(context.Background())
	defer cancelJoiner()
	creatorDone := f.watch(context.Background(), f.first)
	joinerDone := f.watch(joinerCtx, joiner)
	park.awaitAll(t, 2)

	cancelJoiner()
	park.release()

	joined := awaitOutcome(t, joinerDone, "the joined caller")
	if !errors.Is(joined.err, context.Canceled) {
		t.Fatalf("the joined caller returned %v, want its own context error", joined.err)
	}
	// The shared request is untouched: it is still queued, has no terminal status, and the owning
	// caller is still waiting for it.
	if status := joiner.Status(); status != RequestPending {
		t.Fatalf("the shared request is %s after a joiner cancelled, want PENDING", status)
	}
	if pending := f.m.PendingRequests(f.rootID); pending != 1 {
		t.Fatalf("pending requests = %d after a joiner cancelled, want the shared request", pending)
	}

	f.releaseBlocker()
	owner := awaitOutcome(t, creatorDone, "the owning caller")
	requireOneGrantForAll(t, []waitOutcome{owner}, "the owning caller")
	requireNoResidue(t, f)
}

// TestB02OwningCallerContextEndWithdrawsTheSharedRequest proves the fix does not disable
// cancellation: the caller that owns the request still withdraws it, and every joiner is woken by
// the recorded terminal status instead of being left behind.
func TestB02OwningCallerContextEndWithdrawsTheSharedRequest(t *testing.T) {
	f := newSharedWaitFixture(t)
	joiner := f.join()

	park := parkWaiters(t, 2)
	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	defer cancelCreator()
	creatorDone := f.watch(creatorCtx, f.first)
	joinerDone := f.watch(context.Background(), joiner)
	park.awaitAll(t, 2)

	cancelCreator()
	park.release()

	owner := awaitOutcome(t, creatorDone, "the owning caller")
	if !errors.Is(owner.err, context.Canceled) {
		t.Fatalf("the owning caller returned %v, want its own context error", owner.err)
	}
	if status := joiner.Status(); status != RequestContextCanceled {
		t.Fatalf("the shared request is %s after its owner withdrew it, want CONTEXT_CANCELED", status)
	}
	joined := awaitOutcome(t, joinerDone, "the joined caller")
	if !errors.Is(joined.err, ErrConflict) {
		t.Fatalf("the joined caller returned %v, want ErrConflict for a withdrawn request", joined.err)
	}
	requireNoResidue(t, f)
}

// TestB02JoinerCannotWithdrawTheSharedRequest proves the ownership rule also holds for an explicit
// cancellation: a joiner's cancel finds no request of its own to retract.
func TestB02JoinerCannotWithdrawTheSharedRequest(t *testing.T) {
	f := newSharedWaitFixture(t)
	joiner := f.join()

	if status := f.m.Cancel(f.rootID, joiner); status != RequestPending {
		t.Fatalf("cancelling a joined handle reported %s, want the still-pending shared request", status)
	}
	if pending := f.m.PendingRequests(f.rootID); pending != 1 {
		t.Fatalf("pending requests = %d after a joiner cancelled, want the shared request", pending)
	}

	// The owner's cancel does retract the shared request, and the joiner observes that outcome.
	f.releaseBlocker()
	if _, err := f.m.WaitForGrant(context.Background(), f.rootID, f.first); err != nil {
		t.Fatalf("the shared request was not granted after the blocker released: %v", err)
	}
	if status := joiner.Status(); status != RequestGranted {
		t.Fatalf("the joined handle reports %s after the shared grant, want GRANTED", status)
	}
	requireNoResidue(t, f)
}

// TestB02SharedTerminalStatusIsObservableRepeatedly proves the shared outcome is a broadcast and
// not a value that one caller consumes: after the grant, every caller of the request can read the
// same status and the same grant as often as it likes.
func TestB02SharedTerminalStatusIsObservableRepeatedly(t *testing.T) {
	f := newSharedWaitFixture(t)
	joiner := f.join()

	park := parkWaiters(t, 2)
	creatorDone := f.watch(context.Background(), f.first)
	joinerDone := f.watch(context.Background(), joiner)
	park.awaitAll(t, 2)

	f.releaseBlocker()
	park.release()

	first := awaitOutcome(t, creatorDone, "the owning caller")
	second := awaitOutcome(t, joinerDone, "the joined caller")
	grantID := requireOneGrantForAll(t, []waitOutcome{first, second}, "a caller of the shared request")

	for _, handle := range []*LockHandle{f.first, joiner} {
		// The broadcast is closed, so a later waiter never blocks...
		select {
		case <-handle.settled():
		default:
			t.Fatalf("handle %d has no settled broadcast after the grant", handle.RequestID)
		}
		// ...and every read of the outcome agrees, as many times as it is asked.
		for attempt := 0; attempt < 3; attempt++ {
			if status := handle.Status(); status != RequestGranted {
				t.Fatalf("handle %d status = %s on read %d, want GRANTED", handle.RequestID, status, attempt)
			}
			if status := f.m.WaitStatus(f.rootID, handle); status != RequestGranted {
				t.Fatalf("handle %d WaitStatus = %s on read %d, want GRANTED", handle.RequestID, status, attempt)
			}
			if status, err := handle.Wait(context.Background()); err != nil || status != RequestGranted {
				t.Fatalf("handle %d Wait = %s/%v on read %d, want GRANTED", handle.RequestID, status, err, attempt)
			}
			grant, err := f.m.WaitForGrant(context.Background(), f.rootID, handle)
			if err != nil {
				t.Fatalf("handle %d WaitForGrant = %v on read %d", handle.RequestID, err, attempt)
			}
			if grant.GrantID != grantID {
				t.Fatalf("handle %d resolved grant %d on read %d, want the shared grant %d", handle.RequestID, grant.GrantID, attempt, grantID)
			}
		}
	}
	requireNoResidue(t, f)
}

// TestB02JoinerScopeCloseEndsOnlyTheJoinersWait is the lifecycle boundary this round settles.
//
// A statement scope that joined a request belongs to a statement that can be discarded while the
// shared request is still queued. When that happens the caller must stop waiting — a closed scope
// must not keep a caller blocked, and it must certainly not hand it a grant it is no longer allowed
// to act on — while the request itself, which belongs to the scope that created it, stays queued
// for its owner. This is the difference between "the joiner stops waiting" and "the request is
// cancelled": only the owning scope, the root and an epoch change may do the latter.
func TestB02JoinerScopeCloseEndsOnlyTheJoinersWait(t *testing.T) {
	f := newSharedWaitFixture(t)

	// One request created by the owning scope, joined twice from two other scopes of the same root.
	firstJoin := f.join()
	secondJoin := f.queue(f.openScope("a-third"))
	if secondJoin.RequestID != f.first.RequestID {
		t.Fatalf("the third scope started request %d, want to join request %d", secondJoin.RequestID, f.first.RequestID)
	}

	park := parkWaiters(t, 3)
	ownerDone := f.watch(context.Background(), f.first)
	firstDone := f.watch(context.Background(), firstJoin)
	secondDone := f.watch(context.Background(), secondJoin)
	park.awaitAll(t, 3)

	// Only the first joining scope closes: its caller stops waiting, the other two keep waiting.
	if _, err := f.m.CloseScope(f.rootID, f.joiner); err != nil {
		t.Fatal(err)
	}
	park.release()

	stopped := awaitOutcome(t, firstDone, "the caller whose scope closed")
	if !errors.Is(stopped.err, ErrScopeUnknown) {
		t.Fatalf("the caller whose scope closed returned %v, want ErrScopeUnknown", stopped.err)
	}
	if status := firstJoin.Status(); status != RequestPending {
		t.Fatalf("the shared request is %s, want PENDING while its owner still waits", status)
	}
	// A closed scope can no longer start or join work at all.
	if _, err := f.m.Acquire(context.Background(), f.rootID, f.joiner, f.bundle); !errors.Is(err, ErrScopeUnknown) {
		t.Fatalf("a closed scope started a request: err = %v, want ErrScopeUnknown", err)
	}
	// Closing the same scope twice must be harmless.
	if _, err := f.m.CloseScope(f.rootID, f.joiner); err != nil {
		t.Fatalf("closing the same scope twice: %v", err)
	}

	// The surviving callers are still waiting and are granted together.
	f.releaseBlocker()
	owner := awaitOutcome(t, ownerDone, "the owning caller")
	survivor := awaitOutcome(t, secondDone, "the caller whose scope is still open")
	requireOneGrantForAll(t, []waitOutcome{owner, survivor}, "a surviving caller")
	requireNoResidue(t, f)
}

// TestB02JoinerScopeCloseRacingTheGrantHasOneVisibleOutcome pins the precedence rule between the
// two events that can end a joined wait: the shared request being decided and the joining scope
// being discarded.
//
// Both orders are legal — the manager mutex decides which critical section runs first — but only one
// outcome may be observable per caller, and an outcome the manager has already recorded must never
// be replaced: a decided grant is reported as a grant even when the scope closes in the same
// instant, exactly as a grant racing the caller's context is. When the request is still queued, the
// scope close is what ends the join.
func TestB02JoinerScopeCloseRacingTheGrantHasOneVisibleOutcome(t *testing.T) {
	t.Run("a decided grant wins over the scope closing", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()
		park := parkWaiters(t, 2)
		ownerDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		// The grant is decided first, the scope closes immediately afterwards, and only then are the
		// callers released: both wake-up channels are ready at the same moment.
		f.releaseBlocker()
		if _, err := f.m.CloseScope(f.rootID, f.joiner); err != nil {
			t.Fatal(err)
		}
		park.release()

		owner := awaitOutcome(t, ownerDone, "the owning caller")
		joined := awaitOutcome(t, joinerDone, "the joined caller")
		requireOneGrantForAll(t, []waitOutcome{owner, joined}, "a caller of the granted request")
		requireNoResidue(t, f)
	})

	t.Run("a scope close on a queued request ends only the join", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()
		park := parkWaiters(t, 2)
		ownerDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		// The scope closes while the request is still queued, so the join ends.
		if _, err := f.m.CloseScope(f.rootID, f.joiner); err != nil {
			t.Fatal(err)
		}
		park.release()
		joined := awaitOutcome(t, joinerDone, "the joined caller after its scope closed")
		if !errors.Is(joined.err, ErrScopeUnknown) {
			t.Fatalf("the joined caller returned %v, want ErrScopeUnknown", joined.err)
		}
		if status := joiner.Status(); status != RequestPending {
			t.Fatalf("the shared request is %s, want PENDING", status)
		}

		// The request is granted afterwards to the callers that are still entitled to it.
		f.releaseBlocker()
		owner := awaitOutcome(t, ownerDone, "the owning caller")
		requireOneGrantForAll(t, []waitOutcome{owner}, "the owning caller")
		requireNoResidue(t, f)
	})
}

// TestB02EveryTerminalOutcomeWakesEveryWaiter proves the broadcast covers every way a request can
// end: an explicit cancel, the wait deadline, a deadlock victim and an epoch reset.
func TestB02EveryTerminalOutcomeWakesEveryWaiter(t *testing.T) {
	t.Run("explicit cancel", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()
		park := parkWaiters(t, 2)
		creatorDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		if status := f.m.Cancel(f.rootID, f.first); status != RequestCanceled {
			t.Fatalf("cancel = %s, want CANCELED", status)
		}
		park.release()

		for i, done := range []<-chan waitOutcome{creatorDone, joinerDone} {
			outcome := awaitOutcome(t, done, "a caller of the cancelled request")
			if !errors.Is(outcome.err, ErrConflict) {
				t.Fatalf("caller %d returned %v, want ErrConflict", i, outcome.err)
			}
		}
		requireNoResidue(t, f)
	})

	t.Run("wait deadline", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()
		park := parkWaiters(t, 2)
		creatorDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		// The virtual clock is advanced past the request's wait bound while both callers are
		// parked: the deadline is the request's own, so it ends the request for all of them.
		if fired := f.clock.Advance(51 * time.Second); fired == 0 {
			t.Fatal("advancing past the wait bound fired no timer")
		}
		park.release()

		for i, done := range []<-chan waitOutcome{creatorDone, joinerDone} {
			outcome := awaitOutcome(t, done, "a caller of the timed-out request")
			if !errors.Is(outcome.err, ErrLockWaitTimeout) {
				t.Fatalf("caller %d returned %v, want ErrLockWaitTimeout", i, outcome.err)
			}
		}
		requireNoResidue(t, f)
	})

	t.Run("deadlock victim", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()

		// Build a real cycle with the waiting root as the contract's victim: it holds a row the
		// blocker waits for, and it waits for the row the blocker holds. The waiting root has the
		// larger registration ordinal, so the detector must choose it.
		held := indexRecordOf(ModeX, "t1", "PRIMARY", []byte("k2"))
		if _, err := f.m.Acquire(context.Background(), f.rootID, f.creator, []ResourceRef{held, ixIntent("t1")}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.Acquire(context.Background(), f.blocker.rootID, f.blocker.scopeID, []ResourceRef{held, ixIntent("t1")}); err != nil {
			t.Fatal(err)
		}

		park := parkWaiters(t, 2)
		creatorDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		summary, found := f.m.DetectDeadlock()
		if !found || summary.VictimRootID != f.rootID {
			t.Fatalf("deadlock detection = %+v found=%v, want %s as the victim", summary, found, f.rootID)
		}
		if summary.DetectedOrdinal == 0 {
			t.Fatal("the deadlock summary has no ordinal")
		}
		park.release()

		for i, done := range []<-chan waitOutcome{creatorDone, joinerDone} {
			outcome := awaitOutcome(t, done, "a caller of the victim's request")
			if !errors.Is(outcome.err, ErrDeadlock) {
				t.Fatalf("caller %d returned %v, want ErrDeadlock", i, outcome.err)
			}
		}
		requireNoResidue(t, f)
	})

	t.Run("epoch reset", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()
		park := parkWaiters(t, 2)
		creatorDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		if affected := f.m.ResetEpoch(2, 1); affected == 0 {
			t.Fatal("the epoch reset affected no owner")
		}
		park.release()

		for i, done := range []<-chan waitOutcome{creatorDone, joinerDone} {
			outcome := awaitOutcome(t, done, "a caller of the invalidated request")
			if !errors.Is(outcome.err, ErrLockInvalidated) {
				t.Fatalf("caller %d returned %v, want ErrLockInvalidated", i, outcome.err)
			}
		}
		requireNoResidue(t, f)
	})
}

// TestB02ScopeAndRootLifecyclesAroundASharedRequest pins the four lifecycles the contract
// distinguishes: the owning scope closing, a joiner's scope closing, the root ending, and an epoch
// change (covered above).
func TestB02ScopeAndRootLifecyclesAroundASharedRequest(t *testing.T) {
	t.Run("owning scope close withdraws the shared request", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()
		park := parkWaiters(t, 2)
		creatorDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		if _, err := f.m.CloseScope(f.rootID, f.creator); err != nil {
			t.Fatalf("close the owning scope: %v", err)
		}
		park.release()

		for i, done := range []<-chan waitOutcome{creatorDone, joinerDone} {
			outcome := awaitOutcome(t, done, "a caller of the withdrawn request")
			if !errors.Is(outcome.err, ErrConflict) {
				t.Fatalf("caller %d returned %v, want ErrConflict", i, outcome.err)
			}
		}
		if pending := f.m.PendingRequests(f.rootID); pending != 0 {
			t.Fatalf("pending requests = %d after the owning scope closed, want 0", pending)
		}
		requireNoResidue(t, f)
	})

	t.Run("joiner scope close ends the joiner's wait and leaves the request queued", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()
		park := parkWaiters(t, 2)
		creatorDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		if _, err := f.m.CloseScope(f.rootID, f.joiner); err != nil {
			t.Fatalf("close the joining scope: %v", err)
		}
		park.release()

		// The calling scope is gone, so its caller must stop waiting: a closed statement scope that
		// kept waiting could still be handed a grant it is no longer allowed to act on.
		joined := awaitOutcome(t, joinerDone, "the joined caller after its scope closed")
		if !errors.Is(joined.err, ErrScopeUnknown) {
			t.Fatalf("the joined caller returned %v, want ErrScopeUnknown for a closed scope", joined.err)
		}
		if status := joiner.Status(); status != RequestPending {
			t.Fatalf("the joined handle reports %s, want the shared request to stay PENDING", status)
		}
		// The request belongs to the owning scope, so it is still queued and its owner still gets it.
		if pending := f.m.PendingRequests(f.rootID); pending != 1 {
			t.Fatalf("pending requests = %d after a joining scope closed, want the shared request", pending)
		}
		f.releaseBlocker()
		owner := awaitOutcome(t, creatorDone, "the owning caller")
		requireOneGrantForAll(t, []waitOutcome{owner}, "the owning caller")
		requireNoResidue(t, f)
	})

	t.Run("root end withdraws the shared request", func(t *testing.T) {
		f := newSharedWaitFixture(t)
		joiner := f.join()
		park := parkWaiters(t, 2)
		creatorDone := f.watch(context.Background(), f.first)
		joinerDone := f.watch(context.Background(), joiner)
		park.awaitAll(t, 2)

		f.m.ReleaseOwner(f.rootID)
		park.release()

		for i, done := range []<-chan waitOutcome{creatorDone, joinerDone} {
			outcome := awaitOutcome(t, done, "a caller of the ended root's request")
			if !errors.Is(outcome.err, ErrConflict) {
				t.Fatalf("caller %d returned %v, want ErrConflict", i, outcome.err)
			}
		}
		requireNoResidue(t, f)
	})
}
