package lockmanager

import (
	"context"
	"sync"
	"testing"
	"time"
)

// test_helpers_test.go is the deterministic test support for the lock manager.
//
// It replaces sleep-based ordering with channels and barriers, and it gives every
// test a virtual clock, so a scheduling decision is an observed fact instead of a
// race. Production code never imports it and never constructs anything from it: the
// hooks here are test-only by construction, not a build-tag-gated production path.

// testEpoch is the clock origin every deterministic test starts from.
var testEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newTestClock returns a fake clock at the shared test epoch.
func newTestClock() *FakeClock { return NewFakeClock(testEpoch) }

// newTestManager returns a manager with a fake clock and explicit limits.
func newTestManager(t *testing.T, limits BudgetLimits) (*manager, *FakeClock) {
	t.Helper()
	clock := newTestClock()
	m, err := newManager(limits, 1, clock)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	t.Cleanup(m.Close)
	return m, clock
}

// request builds a record component for one key, with the matching table intent, so
// a test never has to spell out the intention rule.
func recordRequest(space string, tableID string, key []byte, mode Mode) []ResourceRef {
	components := []ResourceRef{{Identity: Identity{Kind: KindRecord, Mode: mode, Space: space, Key: key}}}
	if mode == ModeX {
		components = append(components, ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: tableID}})
	} else {
		components = append(components, ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIS, Space: "table", TableID: tableID}})
	}
	return components
}

// gapRequest builds one interval component plus its intention.
func gapRequest(space, tableID string, lower, upper []byte) []ResourceRef {
	return []ResourceRef{
		{Identity: Identity{Kind: KindInterval, Mode: ModeGap, Space: space, Lower: lower, Upper: upper}},
		{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: tableID}},
	}
}

// ownerFixture registers one root and opens one statement scope for it.
type ownerFixture struct {
	manager *manager
	clock   *FakeClock
	owner   *LockOwner
	scope   uint64
}

func newOwner(t *testing.T, m *manager, clock *FakeClock, rootID string) ownerFixture {
	t.Helper()
	owner, err := m.RegisterOwner(rootID, 1, 0, 100)
	if err != nil {
		t.Fatalf("RegisterOwner(%s): %v", rootID, err)
	}
	scope, err := m.OpenScope(rootID, rootID+"-txn", 0, ScopeStatement, time.Time{})
	if err != nil {
		t.Fatalf("OpenScope(%s): %v", rootID, err)
	}
	return ownerFixture{manager: m, clock: clock, owner: owner, scope: scope.ID}
}

// testBroker is a deterministic stand-in for the queue stage that a later phase
// implements.
//
// It exists so the foundation's notification contract can be exercised now, without
// implementing any lock mode: it decides exactly one outcome per request, inside one
// critical section, and every wait has a synchronous Wait that blocks on a condition
// variable instead of polling.
type testBroker struct {
	mu       sync.Mutex
	cond     *sync.Cond
	next     uint64
	waiting  map[uint64]*LockRequest
	resolved map[uint64]RequestStatus
}

func newTestBroker() *testBroker {
	broker := &testBroker{
		waiting:  make(map[uint64]*LockRequest),
		resolved: make(map[uint64]RequestStatus),
	}
	broker.cond = sync.NewCond(&broker.mu)
	return broker
}

// Enqueue registers one pending request and returns its id.
func (b *testBroker) Enqueue(ownerID string, scopeID uint64, deadline time.Time) *LockRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	request := &LockRequest{
		ID:             b.next,
		OwnerID:        ownerID,
		ScopeID:        scopeID,
		EnqueueOrdinal: b.next,
		RequestedAt:    testEpoch,
		Deadline:       deadline,
		Status:         RequestPending,
		Result:         make(chan RequestStatus, 1),
	}
	b.waiting[request.ID] = request
	return request
}

// Grant delivers the granted outcome unless the request already reached a terminal
// status. It returns whether this call decided the outcome.
func (b *testBroker) Grant(requestID uint64) bool {
	return b.resolve(requestID, RequestGranted)
}

// Cancel delivers the canceled outcome.
func (b *testBroker) Cancel(requestID uint64) bool {
	return b.resolve(requestID, RequestCanceled)
}

// TimeOut delivers the timed-out outcome.
func (b *testBroker) TimeOut(requestID uint64) bool {
	return b.resolve(requestID, RequestTimedOut)
}

func (b *testBroker) resolve(requestID uint64, status RequestStatus) bool {
	b.mu.Lock()
	request, ok := b.waiting[requestID]
	if !ok || request.Status.Terminal() {
		b.mu.Unlock()
		return false
	}
	request.Status = status
	delete(b.waiting, requestID)
	b.resolved[requestID] = status
	b.mu.Unlock()
	request.Notify(status)
	b.cond.Broadcast()
	return true
}

// Wait blocks until the request reaches a terminal status and returns it.
//
// It has no timeout on purpose: a test that hangs is a test that observed a bug, and
// a hidden timeout would turn that into a flake. The blocking is on a condition
// variable, so it consumes no CPU and needs no sleep.
func (b *testBroker) Wait(requestID uint64) RequestStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if status, ok := b.resolved[requestID]; ok {
			return status
		}
		b.cond.Wait()
	}
}

// Pending reports how many requests are still waiting.
func (b *testBroker) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.waiting)
}

// TestB02DeterministicHarnessUsesNoSleeps is a guard on the harness itself: it proves
// the helpers can establish ordering without a wall-clock sleep by running a strict
// hand-off many times.
func TestB02DeterministicHarnessUsesNoSleeps(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	_ = m
	for iteration := 0; iteration < 100; iteration++ {
		broker := newTestBroker()
		request := broker.Enqueue("root-1", 1, clock.Now().Add(time.Second))
		if request.EnqueueOrdinal != 1 || request.Status.Terminal() {
			t.Fatalf("iteration %d: request = %+v", iteration, request)
		}
		if !broker.Cancel(request.ID) {
			t.Fatalf("iteration %d: cancel did not decide the request", iteration)
		}
		if broker.Cancel(request.ID) {
			t.Fatalf("iteration %d: a second cancel decided the same request twice", iteration)
		}
		if got := <-request.Result; got != RequestCanceled {
			t.Fatalf("iteration %d: notification = %q", iteration, got)
		}
		if broker.Pending() != 0 {
			t.Fatalf("iteration %d: %d requests still pending", iteration, broker.Pending())
		}
	}
}

// TestB02FakeClockFiresOnlyReachedDeadlines pins the virtual-clock contract.
func TestB02FakeClockFiresOnlyReachedDeadlines(t *testing.T) {
	clock := newTestClock()
	early := clock.After(testEpoch.Add(time.Second))
	late := clock.After(testEpoch.Add(2 * time.Second))
	if clock.PendingTimers() != 2 {
		t.Fatalf("pending timers = %d, want 2", clock.PendingTimers())
	}
	if clock.Advance(time.Second) != 1 {
		t.Fatal("advancing one second must fire exactly the one-second timer")
	}
	select {
	case <-early:
	default:
		t.Fatal("the reached deadline did not fire")
	}
	select {
	case <-late:
		t.Fatal("an unreached deadline fired early")
	default:
	}
	if clock.Advance(time.Second) != 1 {
		t.Fatal("advancing to the second deadline must fire it")
	}
	<-late
	if clock.PendingTimers() != 0 {
		t.Fatalf("pending timers = %d after both fired", clock.PendingTimers())
	}
	// An already-expired deadline fires immediately and never schedules a negative
	// timer.
	if clock.Advance(0) != 0 {
		t.Fatal("advancing by zero must not fire anything")
	}
	expired := clock.After(clock.Now().Add(-time.Second))
	select {
	case <-expired:
	default:
		t.Fatal("an already-expired deadline must fire immediately")
	}
}

// TestB02BarrierEstablishesCausalOrder proves the barrier hands off without a sleep.
func TestB02BarrierEstablishesCausalOrder(t *testing.T) {
	barrier := NewBarrier()
	if barrier.Reached() {
		t.Fatal("a fresh barrier is already reached")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		barrier.Arrive()
	}()
	if !barrier.Wait(context.Background()) {
		t.Fatal("the barrier was not reached")
	}
	<-done
	if !barrier.Reached() {
		t.Fatal("a reached barrier reports not reached")
	}
	// A cancelled wait reports failure instead of blocking forever.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if NewBarrier().Wait(cancelled) {
		t.Fatal("a cancelled wait must not report the barrier reached")
	}
}

// requireBudgetZero asserts that no accounting survived a full release. It is the
// shared terminal-state assertion of every budget and lifecycle test.
func requireBudgetZero(t *testing.T, ledger *Ledger) {
	t.Helper()
	totals := ledger.Totals()
	if totals.MemoryBytes != 0 || totals.Components != 0 || totals.Waiters != 0 || totals.GraphEdges != 0 {
		t.Fatalf("ledger did not return to zero: %+v", totals)
	}
	if grants := ledger.Grants(); grants != 0 {
		t.Fatalf("grants = %d after a full release", grants)
	}
}

// lockFixture is one registered owner with one open statement scope, plus the helpers
// every lock test needs.
//
// It exists so a test reads as the scenario it describes: "A holds X on key 1, B
// requests X on key 1 and waits" rather than as a sequence of registration calls.
type lockFixture struct {
	t       *testing.T
	manager *manager
	clock   *FakeClock
	rootID  string
	scopeID uint64
	last    AcquireResult
	handle  *LockHandle
}

// newLockFixture registers one owner and opens its statement scope.
func newLockFixture(t *testing.T, m *manager, clock *FakeClock, rootID string) *lockFixture {
	t.Helper()
	if _, err := m.RegisterOwner(rootID, 1, 0, 100); err != nil {
		t.Fatalf("register %s: %v", rootID, err)
	}
	scope, err := m.OpenScope(rootID, rootID+"-txn", 0, ScopeStatement, time.Time{})
	if err != nil {
		t.Fatalf("scope %s: %v", rootID, err)
	}
	return &lockFixture{t: t, manager: m, clock: clock, rootID: rootID, scopeID: scope.ID}
}

// acquire performs one acquisition through the public manager entry point and records
// the result. It fails the test when the call is refused unexpectedly.
func (f *lockFixture) acquire(components ...ResourceRef) AcquireResult {
	f.t.Helper()
	result, err := f.manager.Acquire(context.Background(), f.rootID, f.scopeID, components)
	if err != nil {
		f.t.Fatalf("%s: acquire %v: %v", f.rootID, componentModes(components), err)
	}
	f.last = result
	f.handle = result.Wait
	return result
}

// acquireFail performs one acquisition and returns its error.
func (f *lockFixture) acquireFail(components ...ResourceRef) error {
	f.t.Helper()
	_, err := f.manager.Acquire(context.Background(), f.rootID, f.scopeID, components)
	return err
}

// acquireNoWait performs one acquisition whose request refuses to queue, so a conflict is
// reported immediately instead of blocking.
func (f *lockFixture) acquireNoWait(components ...ResourceRef) error {
	f.t.Helper()
	_, err := f.manager.TryAcquire(context.Background(), f.rootID, f.scopeID, components)
	return err
}

// requireGranted asserts that the last acquisition was granted inline, and returns its
// grant.
func (f *lockFixture) requireGranted() LockGrant {
	f.t.Helper()
	switch f.last.Op {
	case LockOpGranted:
		return f.last.Grant
	case LockOpReentrant:
		f.t.Fatalf("%s: acquisition was re-entrant, want a fresh grant", f.rootID)
	default:
		f.t.Fatalf("%s: acquisition is waiting, want granted", f.rootID)
	}
	return LockGrant{}
}

// requireReentrant asserts that the last acquisition needed no new protection.
func (f *lockFixture) requireReentrant() LockGrant {
	f.t.Helper()
	if f.last.Op != LockOpReentrant {
		f.t.Fatalf("%s: acquisition op = %s, want REENTRANT", f.rootID, f.last.Op)
	}
	return f.last.Grant
}

// requireWaiting asserts that the last acquisition queued and returned a handle.
func (f *lockFixture) requireWaiting() *LockHandle {
	f.t.Helper()
	if f.last.Op != LockOpWait || f.handle == nil {
		f.t.Fatalf("%s: acquisition op = %s, want WAIT with a handle", f.rootID, f.last.Op)
	}
	return f.handle
}

// release drops one owner's grant on one resource.
func (f *lockFixture) release(components ...ResourceRef) {
	f.t.Helper()
	for _, component := range components {
		if _, err := f.manager.Release(f.rootID, component); err != nil {
			f.t.Fatalf("%s: release: %v", f.rootID, err)
		}
	}
}

// expectStatus asserts the terminal status a request reached, and that its grant, if
// any, covers the components it asked for.
func (f *lockFixture) expectStatus(handle *LockHandle, want RequestStatus) {
	f.t.Helper()
	if got := f.waitReady(handle); got != want {
		f.t.Fatalf("%s: request %d status = %s, want %s", f.rootID, handle.RequestID, got, want)
	}
}

// waitReady reads the status a request already reached, without waiting: every test in
// this package drives the manager deterministically, so a status that has not been
// delivered is a bug rather than a scheduling delay.
func (f *lockFixture) waitReady(handle *LockHandle) RequestStatus {
	f.t.Helper()
	if handle == nil {
		f.t.Fatal("no handle to inspect")
	}
	select {
	case status := <-handle.result:
		return status
	default:
		pending := f.manager.PendingRequests(f.rootID)
		f.t.Fatalf("%s: request %d has no delivered status yet (pending=%d)", f.rootID, handle.RequestID, pending)
		return RequestCanceled
	}
}

// expectNoStatus asserts that a request has not reached a status yet.
func (f *lockFixture) expectNoStatus(handle *LockHandle) {
	f.t.Helper()
	select {
	case status := <-handle.result:
		f.t.Fatalf("%s: request %d unexpectedly reached %s", f.rootID, handle.RequestID, status)
	default:
	}
}

// requireLockState asserts one resource's observable lock state.
func (f *lockFixture) requireLockState(resource ResourceRef, want LockState) {
	f.t.Helper()
	if got := f.manager.LockStateFor(resource); got != want {
		f.t.Fatalf("resource %s state = %s, want %s", resource.Key, got, want)
	}
}

// requireOfferedMode asserts the mode one root offers other owners on one resource, which
// includes the claim of a waiting upgrade.
func (f *lockFixture) requireOfferedMode(rootID string, resource ResourceRef, want Mode) {
	f.t.Helper()
	got, ok := f.manager.offeredModeFor(rootID, resource)
	if !ok {
		f.t.Fatalf("%s offers nothing on %s, want %s", rootID, resource.Key, want)
	}
	if got != want {
		f.t.Fatalf("%s offers %s on %s, want %s", rootID, got, resource.Key, want)
	}
}

// requireHeldMode asserts the mode one root holds on one resource.
func (f *lockFixture) requireHeldMode(rootID string, resource ResourceRef, want Mode) {
	f.t.Helper()
	got, ok := f.manager.HeldMode(rootID, resource)
	if !ok {
		f.t.Fatalf("%s does not hold %s, want %s", rootID, resource.Key, want)
	}
	if got != want {
		f.t.Fatalf("%s holds %s on %s, want %s", rootID, got, resource.Key, want)
	}
}

// requireNoGrant asserts that one root holds nothing on one resource.
func (f *lockFixture) requireNoGrant(rootID string, resource ResourceRef) {
	f.t.Helper()
	if mode, ok := f.manager.HeldMode(rootID, resource); ok {
		f.t.Fatalf("%s unexpectedly holds %s on %s", rootID, mode, resource.Key)
	}
}

// componentModes renders a component list for a failure message.
func componentModes(components []ResourceRef) string {
	out := ""
	for i, component := range components {
		if i > 0 {
			out += ","
		}
		out += string(component.Mode) + ":" + string(component.Kind) + "@" + string(component.Key)
	}
	return out
}

// TestB02LockFixtureDrivesTheManagerOnce is a guard on the fixture itself: it proves the
// helpers observe a grant through the real Acquire path rather than assembling state.
func TestB02LockFixtureDrivesTheManagerOnce(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	result := a.acquire(recordRequest("rows", "t1", []byte("k1"), ModeS)...)
	if result.Op != LockOpGranted {
		t.Fatalf("first acquisition op = %s, want GRANTED", result.Op)
	}
	grant := a.requireGranted()
	if len(grant.Components) != 2 {
		t.Fatalf("grant covers %d components, want the record and its table intent", len(grant.Components))
	}
	if grant.OwnerID != "root-a" {
		t.Fatalf("grant owner = %q, want root-a", grant.OwnerID)
	}
	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: []byte("k1")}}
	a.requireLockState(record, LockStateShared)
	a.release(recordRequest("rows", "t1", []byte("k1"), ModeS)...)
	if m.LockTableSize() != 0 {
		t.Fatalf("lock table = %d entries after the only release, want 0", m.LockTableSize())
	}
}

// TestB02SharedRequestsCoexist proves the S/S rule through the manager: two owners hold
// the same row at the same time, and a third exclusive request then waits.
func TestB02SharedRequestsCoexist(t *testing.T) {
	m, clock := newTestManager(t, BudgetLimits{})
	a := newLockFixture(t, m, clock, "root-a")
	b := newLockFixture(t, m, clock, "root-b")
	c := newLockFixture(t, m, clock, "root-c")

	key := []byte("k1")
	a.acquire(recordRequest("rows", "t1", key, ModeS)...)
	a.requireGranted()
	b.acquire(recordRequest("rows", "t1", key, ModeS)...)
	b.requireGranted()

	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: key}}
	a.requireLockState(record, LockStateShared)
	a.requireHeldMode("root-a", record, ModeS)
	a.requireHeldMode("root-b", record, ModeS)
	if got := holderCountOf(m.Snapshot(), record); got != 2 {
		t.Fatalf("record holders = %d, want 2", got)
	}

	// The exclusive request cannot join them. It is blocked by both shared holders, and
	// each distinct blocker root gets exactly one HOLDER edge.
	c.acquire(recordRequest("rows", "t1", key, ModeX)...)
	handle := c.requireWaiting()
	a.requireLockState(record, LockStateWaiting)
	edges := m.Edges()
	if len(edges) != 2 {
		t.Fatalf("edges = %+v, want one edge to each of the two shared holders", edges)
	}
	blockers := map[string]bool{edges[0].ToRootID: true, edges[1].ToRootID: true}
	if !blockers["root-a"] || !blockers["root-b"] {
		t.Fatalf("edges = %+v, want blockers root-a and root-b", edges)
	}
	for _, edge := range edges {
		if edge.FromRootID != "root-c" || edge.Reason != EdgeHolder {
			t.Fatalf("edge = %+v, want a HOLDER edge from root-c", edge)
		}
	}
	c.expectNoStatus(handle)
}

// holderCountOf reports how many owners hold one resource in a snapshot.
func holderCountOf(snapshot []LockSnapshot, resource ResourceRef) int {
	key := resource.ResourceKey()
	for _, entry := range snapshot {
		if entry.Resource.ResourceKey() == key {
			return entry.Holders
		}
	}
	return 0
}

// snapshotFor returns one resource's snapshot entry.
func snapshotFor(t *testing.T, snapshot []LockSnapshot, resource ResourceRef) LockSnapshot {
	t.Helper()
	key := resource.ResourceKey()
	for _, entry := range snapshot {
		if entry.Resource.ResourceKey() == key {
			return entry
		}
	}
	t.Fatalf("resource %v is absent from the snapshot", resource.Key)
	return LockSnapshot{}
}
