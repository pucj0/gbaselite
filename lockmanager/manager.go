package lockmanager

import (
	"context"
	"sort"
	"sync"
	"time"
)

// manager.go holds the owner registry, the scope attachment, the namespace binding and
// the granted locks of one engine's lock manager.
//
// What it owns is the part every later stage depends on and must not get wrong:
//
//   - one LockOwner per B01 root transaction, resolved through an ancestor chain;
//   - every scope belongs to exactly one root, so a grant can never be attributed to a
//     child or savepoint layer;
//   - a manager never holds a transaction pointer, so a detection or diagnostics
//     goroutine can never call a transaction method;
//   - every registration, grant and charge is bounded and fails closed;
//   - a request either acquires its whole bundle or becomes exactly one queued entry,
//     and it reaches exactly one terminal status.
//
// Locking discipline: one mutex guards the owner registry, the scope trees, the binding
// registry and the lock table. Every method takes it for one short critical section,
// never performs I/O under it, and never calls back into caller code under it. That is
// what makes "no internal lock is held across a wait or across external work"
// structural rather than a rule a caller has to remember.
//
// Waiting happens in the caller's goroutine on a channel. Acquire performs the whole
// decision while holding the mutex, records the final status (granted, queued with a
// handle, or refused) and returns; a queued caller then blocks on its own handle. The
// manager never blocks a caller while holding the mutex, so an unrelated resource can
// always make progress.
type manager struct {
	mu sync.Mutex

	bindings *Bindings
	ledger   *Ledger
	clock    Clock
	locks    *lockTable

	owners map[string]*LockOwner
	scopes map[string]*ScopeTree

	// nextOrdinal is the root registration ordinal. It is independent of the commit
	// sequence and never advances the head; a deadlock victim is the largest ordinal
	// in a cycle, and ordinal exhaustion fails closed rather than wrapping.
	nextOrdinal uint64
	nextGrantID uint64
	// nextRequestID and nextEnqueueOrdinal are the monotonic request identity and the
	// queue order. The queue order is what makes the fairness rule deterministic.
	nextRequestID      uint64
	nextEnqueueOrdinal uint64
	// nextResourceID is the counter behind every anonymous resource identity. It is a
	// manager-local sequence, not a hash of the key, so the number carries no information
	// about the table, index or key it stands for.
	nextResourceID uint64

	// pending indexes the live waiting requests by root, so a scope close, an epoch
	// reset or an owner release can cancel exactly that root's waits. A terminal
	// request is never left in this index.
	pending map[string]map[*lockRequest]bool
	// waiting holds every request that is still incomplete, so a state change on one
	// resource can retry the bundles it did not directly touch. A resource-local pass
	// alone cannot complete a bundle whose components live on different resources.
	waiting map[*requestBundle]bool

	// abortRequested records why a detector or an epoch change asked a root to stop. It is
	// separate from the owner state so the detector only ever marks an intent: the owner's own
	// goroutine finishes the root.
	abortRequested map[string]AbortReason
	// deadlocked is the set of roots a detector already chose as a victim and whose abort has
	// not been cleaned up yet. A cycle containing such a root is skipped, so one victim is
	// never reported twice and a second victim is not chosen for a cycle that is already
	// being resolved.
	deadlocked map[string]bool

	// graph is the derived wait-for graph. It is rebuilt from the live queues on demand rather
	// than after every change, and graphDirty records that a queue changed since the last
	// rebuild.
	//
	// Deriving it lazily is what keeps acquisition linear. Every mutation would otherwise have to
	// rescan every queued component to bring the graph up to date, which turns N queued requests
	// into N rebuilds of N components each. The graph is read by detection and diagnostics, which
	// are rare compared with a lock request, so paying for it there is the right trade.
	graph      *waitGraph
	graphDirty bool
	// deadlockRing is the bounded history of detected deadlocks, oldest first. It is a ring so
	// an engine that deadlocks repeatedly cannot grow without bound.
	deadlockRing []DeadlockSummary

	// detectRuns, timeouts, deadlocks and invalidations are the lifetime counters of the
	// bounded work this manager performed.
	detectRuns  uint64
	timeouts    uint64
	deadlocks   uint64
	invalidated uint64

	closed bool
}

// LockOwner is one registered root transaction's lock identity.
//
// It stores the root ID, the registration ordinal, the engine generation and leader
// term it was created under, its state, its scopes and its usage. It deliberately
// holds no *Tx: a lock owner must never be able to call Rollback or touch a write
// set from another goroutine.
type LockOwner struct {
	RootID     string
	Ordinal    uint64
	Generation uint64
	Term       uint64
	// ReadTS is a copy of the root's read timestamp for diagnostics only. The lock
	// manager never modifies it and never uses it to decide visibility.
	ReadTS    uint64
	State     OwnerState
	StartedAt time.Time
	EndedAt   time.Time

	scopes   *ScopeTree
	usage    BudgetUsage
	epochSet bool
}

// Scopes returns the owner's scope tree.
func (o *LockOwner) Scopes() *ScopeTree { return o.scopes }

// Usage returns the last accounting snapshot recorded for this owner.
func (o *LockOwner) Usage() BudgetUsage { return o.usage }

// LockOp is the outcome class of one Acquire.
type LockOp string

const (
	// LockOpGranted means the whole bundle is held now.
	LockOpGranted LockOp = "GRANTED"
	// LockOpReentrant means the owner already held every requested component, so
	// nothing new was taken and nothing was charged.
	LockOpReentrant LockOp = "REENTRANT"
	// LockOpWait means the bundle became exactly one queued request. The caller waits
	// on the returned handle.
	LockOpWait LockOp = "WAIT"
)

// AcquireResult is one acquisition attempt's outcome.
//
// Exactly one of Grant and Wait is set for a successful call. A refused call returns an
// error and never a partial result, so a caller cannot observe half a bundle.
type AcquireResult struct {
	Op    LockOp
	Grant LockGrant
	Wait  *LockHandle
}

// LockHandle is the caller-side view of a queued lock request.
//
// It is a value, but its decision field is shared by every copy: the manager records the
// single terminal status there through requestState.record, so asking for the status is
// idempotent and never consumes anything. That is what lets a caller learn whether a grant
// beat its cancellation without racing a notification.
//
// Several handles can refer to one request: the caller that queued it and every caller that
// joined it. They all observe the same decision through the same broadcast, which is why the
// request can be shared safely 鈥?a wake-up is not a value handed to one receiver but a channel
// closed for all of them.
//
// Synchronization: the shared decision guards itself with its own small mutex, so Status
// is safe to call from any number of goroutines at the very moment the manager decides the
// outcome. Status deliberately does not take the manager mutex: a caller that only asks
// what happened must never be able to block behind a manager that is waiting on something
// else, and the handle must not become a latch held across a wait.
type LockHandle struct {
	// RequestID identifies the queued request.
	RequestID uint64
	// EnqueueOrdinal is the request's queue order, exposed so a test can assert the
	// fairness order instead of inferring it.
	EnqueueOrdinal uint64

	decision *requestState
	// origin reports whether this handle belongs to the caller that created the request, as
	// opposed to one that joined it. Only the origin caller owns the wait: its context ending
	// withdraws the shared request, while a joiner's context ending stops its own wait and leaves
	// the request to the callers still waiting for it.
	origin bool
	// scopeDone is closed when the scope this caller joined from closes. It is nil for the origin
	// handle, and selecting on a nil channel simply never fires, so the owning caller is unaffected
	// by the mechanism.
	//
	// A joined caller waits for a request it does not own, so its own scope closing is the one
	// event that must end its wait without touching the request: a statement that has been
	// discarded must not keep blocking, and must never be handed a grant it can no longer use.
	scopeDone <-chan struct{}
}

// requestState is the shared decision of one queued request.
//
// It is the one place "grant", "cancel", "timeout", "deadlock victim", "invalidated" and
// "resource rejected" are funnelled through, and it enforces the contract the handle
// exists for: the first terminal status recorded is the only status ever observable, so a
// grant racing a cancel or a deadline still yields exactly one outcome.
//
// Synchronization: one sync.Mutex guards the status together with the decided flag, because
// recording the outcome is a compare-and-set rather than a plain store. The first writer
// must win and every later writer must observe the winner instead of overwriting it; a
// single atomic word would have to pack the status and the decided bit into one value and
// update them in a CAS loop, which is harder to read and no faster here. A requestState is
// written once, when the manager decides the outcome, and afterwards only read by a caller
// polling its handle, so the mutex is effectively uncontended.
//
// Lock order: the manager mutex is always the outer lock. Every writer of the decision
// (requestBundle.setStatus) is called from a *Locked path that already holds the manager
// mutex, and takes the state mutex only for the store; a reader (Status, Wait) takes the
// state mutex alone and never touches the manager mutex. There is therefore no inverted
// order and no path on which a status read can wait for a manager critical section.
type requestState struct {
	mu     sync.Mutex
	status RequestStatus
	// decided records that the single terminal status has been stored. It is a separate
	// field because the zero value of RequestStatus is not RequestPending and
	// RequestStatus.Terminal reports "anything but PENDING": an unwritten status must never
	// be mistaken for a decided one, or a fresh decision would reject its own first grant.
	decided bool
	// settled is closed exactly once, when the first terminal status is stored. It is the
	// request's broadcast: every caller waiting for the request selects on it, so any number of
	// callers wake up together and each of them reads the same recorded status afterwards.
	//
	// A channel that carried the status itself could only be received once, which is precisely
	// what a shared request must not do: one of several waiting callers would wake and the rest
	// would stay blocked for an outcome that had already been decided. Closing a channel is the
	// broadcast primitive; the status is read from this state under the same mutex, so the two can
	// never disagree.
	settled chan struct{}
	// grant is the capability this request was granted, recorded at the same moment as the
	// terminal status. It is stored with the request rather than rebuilt later because only this
	// request's own bundle knows what the request actually acquired or legitimately adopted: the
	// owner's resource registry also contains everything its *other* requests hold, so rebuilding
	// a grant from that registry would answer a waiter with locks its root happened to take
	// elsewhere. See grantForHandle.
	grant    LockGrant
	hasGrant bool
}

// newRequestState returns the shared, still-pending decision of one queued request.
//
// It is the only constructor, so no requestState can be created with a zero status that
// the accessors would have to reinterpret, and every decision owns the broadcast channel its
// waiters select on.
func newRequestState() *requestState {
	return &requestState{status: RequestPending, settled: make(chan struct{})}
}

// record stores the first terminal status and never lets a later write replace it.
//
// It returns the status that is recorded now, so a writer that lost the race can notify the
// winner rather than its own attempt: a cancel that lost to a grant must deliver GRANTED,
// not CANCELED, or a blocked waiter would be told to discard a lock it actually holds. A
// non-terminal status is stored only while nothing terminal is recorded, which is what
// makes "a terminal request never regresses to PENDING" structural instead of a rule the
// callers have to remember.
func (s *requestState) record(status RequestStatus) RequestStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decided {
		return s.status
	}
	s.status = status
	if status.Terminal() {
		s.decided = true
		// The broadcast happens while the state mutex is held, after the status is stored: a
		// waiter woken by the close cannot observe a pending status, and a second terminal write
		// can never close the channel twice.
		close(s.settled)
	}
	return s.status
}

// wait returns the channel that is closed when the request reaches its terminal status.
//
// It never blocks and can be selected on by any number of callers.
func (s *requestState) wait() <-chan struct{} { return s.settled }

// current reads the recorded status.
//
// It is non-blocking apart from the tiny state mutex and never touches the notification
// channel, so polling a handle for diagnostics or for a test can neither block on the
// manager nor consume the outcome some other waiter is entitled to receive.
func (s *requestState) current() RequestStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == "" {
		// An unwritten decision is still pending. Mapping the zero value to PENDING keeps a
		// defensively constructed state from ever reporting an unknown status.
		return RequestPending
	}
	return s.status
}

// recordGrant stores the capability this request was granted.
//
// It is called before the terminal status is recorded, so a caller that observes GRANTED always
// finds the capability that answers it, and a request that already settled keeps the outcome it
// has: a late record cannot rewrite a decided request's identity.
func (s *requestState) recordGrant(grant LockGrant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decided {
		return
	}
	s.grant = grant
	s.hasGrant = true
}

// resolvedGrant returns the recorded capability of a granted request.
func (s *requestState) resolvedGrant() (LockGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasGrant {
		return LockGrant{}, false
	}
	return s.grant, true
}

// Status reports the current status of the request.
//
// A status is PENDING until the manager decides the single outcome, and terminal
// afterwards; the value never goes back to PENDING. It is safe to call concurrently from
// any number of goroutines, including while the manager decides the outcome, and it never
// consumes the notification channel.
func (h *LockHandle) Status() RequestStatus {
	if h == nil || h.decision == nil {
		return RequestCanceled
	}
	return h.decision.current()
}

// resolvedGrant returns the capability recorded for this handle's request.
//
// It is the grant *of that request*: its own identity, its own origin scope and exactly the
// components it acquired or legitimately adopted. It is never the root's whole lock inventory.
func (h *LockHandle) resolvedGrant() (LockGrant, bool) {
	if h == nil || h.decision == nil {
		return LockGrant{}, false
	}
	return h.decision.resolvedGrant()
}

// newHandle returns the caller-side view of the request this caller created.
//
// Every path that hands a caller a queued request goes through here, so a request that a caller
// started and a request it joined share one identity: same request id, same decision, same
// broadcast. The origin handle owns the withdrawal right and has no scope-close channel, because
// its scope closing cancels the request itself.
func newHandle(bundle *requestBundle, origin bool) *LockHandle {
	return &LockHandle{
		RequestID:      bundle.req.ID,
		EnqueueOrdinal: bundle.req.EnqueueOrdinal,
		decision:       bundle.decision,
		origin:         origin,
	}
}

// newJoinedHandle returns the caller-side view of a request this caller joined.
//
// It differs from newHandle in the one way that matters for ownership: the handle carries the
// channel that the joining scope's close closes, so the caller stops waiting when the scope it
// came from is discarded, while the request keeps waiting for its owner and its other joiners.
func newJoinedHandle(bundle *requestBundle, join *scopeJoin) *LockHandle {
	handle := newHandle(bundle, false)
	if join != nil {
		handle.scopeDone = join.closed
	}
	return handle
}

// settled returns the broadcast channel this handle's request closes when it ends.
//
// A nil handle or a decision-less handle reports a channel that is already closed, so a caller
// selecting on it wakes immediately instead of blocking for ever on a handle it cannot use.
func (h *LockHandle) settled() <-chan struct{} {
	if h == nil || h.decision == nil {
		return closedWait
	}
	return h.decision.wait()
}

// closedWait is the already-closed channel an unusable handle reports.
var closedWait = func() <-chan struct{} {
	closed := make(chan struct{})
	close(closed)
	return closed
}()

// Wait blocks until the request reaches a terminal status and returns it.
//
// It reports only the status; the caller resolves a GRANTED status into its grant through the
// AcquireResult it already holds, or through WaitForGrantResult. It never blocks after a terminal
// status, because the terminal status closes the broadcast every waiter selects on.
//
// Any number of callers may wait on one request: the broadcast wakes all of them and each one then
// reads the same recorded status, so no caller can consume another caller's outcome.
func (h *LockHandle) Wait(ctx context.Context) (RequestStatus, error) {
	if h == nil || h.decision == nil {
		return RequestCanceled, ErrScopeUnknown
	}
	// A request that already reached a terminal status is reported immediately. Without this the
	// select below could observe the context ending first even though the grant was decided, and a
	// caller would be told "cancelled" for a lock it actually holds.
	if status := h.Status(); status.Terminal() {
		return status, nil
	}
	parkWaiter(h)
	select {
	case <-h.settled():
		return h.Status(), nil
	case <-h.scopeDone:
		// The scope this caller joined from was discarded. A request that was already decided in
		// the same instant still wins, exactly as it does when the context ends: a caller must not
		// discard an outcome the manager has already recorded. It is reported so the caller knows
		// the root holds what it waited for, even though its own statement is gone.
		if status := h.Status(); status.Terminal() {
			return status, nil
		}
		return RequestCanceled, ErrScopeUnknown
	case <-ctx.Done():
		// The context ended, but a grant may have been decided at the same moment. Consult
		// the recorded decision before reporting the cancellation: reporting a cancel for a
		// granted request would invite a caller to discard a lock it actually holds.
		if status := h.Status(); status.Terminal() {
			return status, nil
		}
		return RequestCanceled, ctx.Err()
	}
}

// LockEdge is one directed wait edge: the root a waiting request is blocked by.
//
// It is the wait graph's own record. ResourceID is the anonymous identity of the resource the
// block happens on, and Revision is the graph revision the edge belongs to, so a consumer can
// tell whether the edges it holds still describe the current graph.
type LockEdge struct {
	FromRootID string
	ToRootID   string
	RequestID  uint64
	Reason     WaitEdgeReason
	ResourceID uint64
	Revision   uint64
}

// LockSnapshot is one resource's observable lock state, for tests and diagnostics.
type LockSnapshot struct {
	Resource    ResourceRef
	State       LockState
	HolderMode  Mode
	Holders     int
	Waiters     int
	HolderRoots []string
}

// newManager builds a manager with resolved limits.
func newManager(limits BudgetLimits, generation uint64, clock Clock) (*manager, error) {
	ledger, err := NewLedger(limits)
	if err != nil {
		return nil, err
	}
	if clock == nil {
		clock = SystemClock{}
	}
	return &manager{
		bindings:       NewBindings(generation),
		ledger:         ledger,
		clock:          clock,
		locks:          newLockTable(),
		owners:         make(map[string]*LockOwner),
		scopes:         make(map[string]*ScopeTree),
		pending:        make(map[string]map[*lockRequest]bool),
		waiting:        make(map[*requestBundle]bool),
		abortRequested: make(map[string]AbortReason),
		deadlocked:     make(map[string]bool),
		graph:          newWaitGraph(),
	}, nil
}

// Bindings exposes the namespace binding registry.
func (m *manager) Bindings() *Bindings { return m.bindings }

// Ledger exposes the accounting ledger.
func (m *manager) Ledger() *Ledger { return m.ledger }

// Clock exposes the injectable clock.
func (m *manager) Clock() Clock { return m.clock }

// Limits reports the resolved budget limits.
func (m *manager) Limits() BudgetLimits { return m.ledger.Limits() }

// LockTableSize reports how many resources currently have a holder or a waiter.
//
// It is the bounded-growth check: an untouched resource must leave no entry behind, so
// a released table returns to zero.
func (m *manager) LockTableSize() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks.entries)
}

// DetectDeadlock searches for a wait cycle and marks the contract's victim.
//
// It is the exported entry point for whatever drives detection, so a caller can drive it from
// its own clock or after each acquisition without the manager owning a goroutine. It reports
// whether a deadlock was found and what was decided.
func (m *Manager) DetectDeadlock() (DeadlockSummary, bool) { return m.inner.DetectDeadlock() }

// WaitEdges reports the current wait-for graph.
func (m *Manager) WaitEdges() []LockEdge { return m.inner.WaitEdges() }

// Diagnostics returns a bounded, fully copied snapshot of the manager's observable state.
//
// It is read-only and never waits for a lock owner to make progress: it takes one short
// critical section, copies plain values and returns. Nothing in the returned page aliases the
// manager's mutable state, so a caller may keep and iterate it freely.
func (m *Manager) Diagnostics(options DiagnosticsOptions) LockDiagnosticsPage {
	return m.inner.Diagnostics(options)
}

// CheckTimeouts completes every queued request whose wait deadline has passed.
//
// It exists so a caller can drive the sweep from its own clock instead of relying on a
// goroutine inside the manager, which keeps the manager free of background work and makes the
// timeout path testable with a virtual clock.
func (m *Manager) CheckTimeouts() int { return m.inner.CheckTimeouts() }

// ResetEpoch invalidates the current generation or leader term.
func (m *Manager) ResetEpoch(generation, term uint64) int {
	return m.inner.ResetEpoch(generation, term)
}

// Deadlocks reports the retained deadlock summaries, newest first.
func (m *Manager) Deadlocks(limit int) ([]DeadlockSummary, bool) {
	return m.inner.Deadlocks(limit)
}

// Manager is the exported fa莽ade of one engine's lock manager.
//
// It exists so the backend adapter has one documented entry point and tests have one type
// to hold. It deliberately exposes no way to observe or mutate a transaction: the adapter
// owns that composition, and the lock manager never holds a transaction pointer.
type Manager struct {
	inner *manager
}

// NewManager builds the lock manager of one engine generation.
//
// A zero BudgetLimits field takes its documented default, so an engine that configures
// nothing still gets bounded lock metadata rather than unlimited state.
func NewManager(limits BudgetLimits, generation uint64, clock Clock) (*Manager, error) {
	inner, err := newManager(limits, generation, clock)
	if err != nil {
		return nil, err
	}
	return &Manager{inner: inner}, nil
}

// RegisterOwner registers one root transaction as a lock owner.
func (m *Manager) RegisterOwner(rootID string, generation, term, readTS uint64) (*LockOwner, error) {
	return m.inner.RegisterOwner(rootID, generation, term, readTS)
}

// Owner reports the registered lock owner of one root.
//
// A caller uses it to attribute a transaction to the owner its grants belong to, which for a
// child is the root's owner rather than its own identity.
func (m *Manager) Owner(rootID string) (*LockOwner, bool) { return m.inner.Owner(rootID) }

// AbortRequested reports whether a detector or an early conflict asked one root to stop.
//
// It is the whole of what the detector communicates to a transaction: an intent to abort. The
// transaction's own goroutine is what ends the root.
func (m *Manager) AbortRequested(rootID string) bool { return m.inner.AbortRequested(rootID) }

// AbortReasonOf reports why one root was asked to stop, or the empty reason when it was not.
func (m *Manager) AbortReasonOf(rootID string) AbortReason { return m.inner.AbortReasonOf(rootID) }

// Scope reports one owner's lock scope by id.
//
// A caller uses it to confirm that a scope it opened belongs to the owner it expected and nests
// where it intended.
func (m *Manager) Scope(rootID string, scopeID uint64) (Scope, bool) {
	return m.inner.Scope(rootID, scopeID)
}

// Clock exposes the injectable time source of this manager.
//
// A caller that owns both the store and the manager drives wait deadlines through it, which is
// what lets a lifecycle test establish "the deadline fired before the release" as a fact rather
// than as a race.
func (m *Manager) Clock() Clock { return m.inner.Clock() }

// OpenScope opens one statement, child or savepoint scope for a registered owner.
func (m *Manager) OpenScope(rootID, originTxnID string, parentScopeID uint64, kind ScopeKind, deadline time.Time) (*Scope, error) {
	return m.inner.OpenScope(rootID, originTxnID, parentScopeID, kind, deadline)
}

// CloseScope closes one scope, cancelling its pending requests and keeping every grant.
func (m *Manager) CloseScope(rootID string, scopeID uint64) ([]uint64, error) {
	return m.inner.CloseScope(rootID, scopeID)
}

// Acquire requests one atomic bundle and waits when it conflicts.
func (m *Manager) Acquire(ctx context.Context, rootID string, scopeID uint64, components []ResourceRef) (AcquireResult, error) {
	return m.inner.Acquire(ctx, rootID, scopeID, components)
}

// AcquireWith requests one atomic bundle with a caller-supplied wait decision.
func (m *Manager) AcquireWith(ctx context.Context, rootID string, scopeID uint64, components []ResourceRef, mayWait func() bool) (AcquireResult, error) {
	return m.inner.AcquireWith(ctx, rootID, scopeID, components, mayWait)
}

// TryAcquire requests one atomic bundle and refuses to queue.
func (m *Manager) TryAcquire(ctx context.Context, rootID string, scopeID uint64, components []ResourceRef) (AcquireResult, error) {
	return m.inner.TryAcquire(ctx, rootID, scopeID, components)
}

// WaitForGrant blocks until the queued request reaches a terminal status and returns the
// grant when it was granted.
//
// The waiting happens here, in the caller's goroutine, so the manager never holds its
// mutex across a wait. A grant that arrives at the same moment as the context's end is
// still reported as a grant, so a caller cannot discard a lock it actually holds.
func (m *Manager) WaitForGrant(ctx context.Context, rootID string, handle *LockHandle) (LockGrant, error) {
	result, err := m.WaitForGrantResult(ctx, rootID, handle)
	if err != nil {
		return LockGrant{}, err
	}
	return result.Grant, nil
}

// waitHook, when non-nil, is consulted by the blocking wait after the caller has established that
// the request is still unsettled and immediately before it blocks on the request's decision.
//
// It is the deterministic synchronization point of the "several callers wait for one request"
// tests: a hook that blocks parks every caller at exactly the moment before the outcome is
// awaited, so a test can decide the outcome 鈥?grant, cancel, timeout, deadlock victim, epoch reset
// 鈥?and then release the callers, and observe which of them learns it. Parking inside the blocking
// path (and not before it) is what makes the test meaningful: a caller parked earlier would take
// the "already decided" fast path instead of exercising the wake-up under test.
//
// It is nil in production, so the default path carries no callback and no coordination state.
var waitHook func(handle *LockHandle)

// parkWaiter consults the test hook, if one is installed. It is a no-op in production.
func parkWaiter(handle *LockHandle) {
	if hook := waitHook; hook != nil {
		hook(handle)
	}
}

// WaitForGrantResult blocks for the outcome of a queued request.
//
// It returns a result carrying the grant when the request was granted, or an error that
// names the outcome: ErrLockWaitTimeout for a deadline, ErrDeadlock for a victim, ErrConflict
// for a cancellation, ErrLockInvalidated for an epoch change and ctx.Err() for a caller
// cancellation.
func (m *Manager) WaitForGrantResult(ctx context.Context, rootID string, handle *LockHandle) (AcquireResult, error) {
	if handle == nil {
		return AcquireResult{}, ErrScopeUnknown
	}
	// The wait deadline is decided by the manager's clock, not by the wall clock, so a test can
	// drive the timeout deterministically. The timer is only armed when the request is
	// genuinely waiting: a request that already reached a terminal status must never be
	// converted into a timeout by a deadline that happened to pass.
	status, err := m.waitWithDeadline(ctx, rootID, handle)
	if err != nil {
		// The context ended. If the request nevertheless reached a terminal status, that
		// status is the answer: a grant must not be discarded because a deadline fired in
		// the same instant.
		if status = handle.Status(); !status.Terminal() {
			return AcquireResult{}, err
		}
	}
	switch status {
	case RequestGranted:
		grant, grantErr := m.grantForHandle(rootID, handle)
		if grantErr != nil {
			return AcquireResult{}, grantErr
		}
		return AcquireResult{Op: LockOpGranted, Grant: grant}, nil
	case RequestTimedOut:
		return AcquireResult{}, ErrLockWaitTimeout
	case RequestDeadlockVictim:
		return AcquireResult{}, ErrDeadlock
	case RequestInvalidated:
		return AcquireResult{}, ErrLockInvalidated
	case RequestResourceRejected:
		return AcquireResult{}, ErrLockResourceLimit
	case RequestContextCanceled:
		// The caller's own context ended, so the context's error is the answer rather than the
		// generic conflict a deliberate cancellation reports.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return AcquireResult{}, ctxErr
		}
		return AcquireResult{}, ErrConflict
	case RequestCanceled:
		return AcquireResult{}, ErrConflict
	default:
		return AcquireResult{}, ErrConflict
	}
}

// waitWithDeadline blocks until the request settles, its context ends or its wait deadline
// passes.
//
// The three outcomes are funnelled through the same recorded decision: whichever of "granted",
// "cancelled" and "timed out" reaches the manager first wins and the others observe it, so a
// release and a timeout arriving together still produce exactly one result (FR-007).
func (m *Manager) waitWithDeadline(ctx context.Context, rootID string, handle *LockHandle) (RequestStatus, error) {
	// A deadline that has already passed is decided before arming anything.
	if m.timeOutIfExpired(rootID, handle) {
		return handle.Status(), nil
	}
	deadline, ok := m.handleDeadline(rootID, handle)
	if !ok {
		return handle.Wait(ctx)
	}
	// The wait is bounded by the manager's clock, not by the wall clock. With the production
	// clock that is an ordinary timer; with the virtual clock a test decides when the deadline is
	// reached, so "the timeout fired before the grant" is a fact rather than a race.
	expired := m.inner.clock.After(deadline)
	parkWaiter(handle)
	select {
	case <-handle.settled():
		// The broadcast is closed for every waiter at once; the status is read from the recorded
		// decision, so two waiters cannot observe different outcomes and none of them consumes
		// another's.
		return handle.Status(), nil
	case <-handle.scopeDone:
		// The joining scope was discarded while the request is still queued. This caller's wait is
		// over and the request is not: a decided outcome in the same instant still wins, so a grant
		// is never silently dropped by a scope close.
		if status := handle.Status(); status.Terminal() {
			return status, nil
		}
		return RequestCanceled, ErrScopeUnknown
	case <-ctx.Done():
		if status := handle.Status(); status.Terminal() {
			return status, nil
		}
		if !handle.origin {
			// The caller joined a request that belongs to another caller of the same root. Its
			// own context ending stops *its* wait and nothing else: withdrawing the shared
			// request would cancel protection the owning caller and every other joiner is still
			// waiting for. There is no request of its own to clean up.
			return RequestCanceled, ctx.Err()
		}
		// The owning caller's context ended while the request was still queued. The request is
		// withdrawn from its queues under the manager's lock, because a queued entry whose caller
		// has gone away would keep blocking every later request on that resource while nobody
		// waits for it. Every joiner is woken by the recorded terminal status.
		m.withdrawOnContextEnd(rootID, handle)
		if status := handle.Status(); status.Terminal() {
			return status, ctx.Err()
		}
		return RequestCanceled, ctx.Err()
	case <-expired:
		// The wall-clock timer fired. The authoritative decision is still made under the
		// manager lock, so a grant that landed in the same instant is reported as a grant.
		if m.timeOutIfExpired(rootID, handle) {
			return handle.Status(), nil
		}
		// The deadline moved or was reached but not passed. Report the current decision rather
		// than looping, because the caller's context is the remaining bound.
		if status := handle.Status(); status.Terminal() {
			return status, nil
		}
		return handle.Wait(ctx)
	}
}

// withdrawOnContextEnd completes a queued request whose caller's context ended.
//
// It is the context-cancellation half of the single-terminal-status rule: whichever of "granted",
// "cancelled", "timed out" and "caller went away" reaches the manager first wins, and the rest
// observe it. The withdrawal is why a cancelled statement cannot leave a waiter behind that blocks
// the resource for everyone else.
func (m *Manager) withdrawOnContextEnd(rootID string, handle *LockHandle) {
	m.inner.mu.Lock()
	defer m.inner.mu.Unlock()
	bundle := m.inner.findPendingLocked(rootID, handle.RequestID)
	if bundle == nil {
		return
	}
	m.inner.finishRequestLocked(bundle, RequestContextCanceled)
}

// handleDeadline reports the wait deadline recorded for one queued request.
func (m *Manager) handleDeadline(rootID string, handle *LockHandle) (time.Time, bool) {
	m.inner.mu.Lock()
	defer m.inner.mu.Unlock()
	bundle := m.inner.findPendingLocked(rootID, handle.RequestID)
	if bundle == nil || bundle.req.Deadline.IsZero() {
		return time.Time{}, false
	}
	return bundle.req.Deadline, true
}

// grantForHandle returns the capability of the request behind one settled handle.
//
// The grant is read from the request's own recorded decision, never rebuilt from the owner's
// registry. The registry lists every resource the root holds 鈥?including locks taken by its other,
// unrelated requests 鈥?so rebuilding a grant from it would answer a waiter with components its
// request never acquired, lose the grant identity, and put the request id where the origin scope
// belongs. Reading it back also keeps the answer stable: two callers that wait on one request get
// one identity, not two reconstructions that drift apart.
//
// The owner must still be live, and it must still be the same generation and leader term the grant
// was issued under: a handle from an owner whose root ended, or from a replaced epoch, is a stale
// capability rather than a grant.
func (m *Manager) grantForHandle(rootID string, handle *LockHandle) (LockGrant, error) {
	m.inner.mu.Lock()
	owner, live := m.inner.owners[rootID]
	var generation, term uint64
	if live {
		generation, term = owner.Generation, owner.Term
	}
	m.inner.mu.Unlock()
	if !live {
		return LockGrant{}, ErrOwnerUnknown
	}
	resolved, ok := handle.resolvedGrant()
	if !ok {
		// A GRANTED request with no recorded capability would be an internal inconsistency.
		// Failing closed is the only safe answer: inventing a grant here is exactly the defect this
		// path used to have.
		return LockGrant{}, ErrConflict
	}
	if resolved.Generation != generation || resolved.Term != term {
		return LockGrant{}, ErrLockInvalidated
	}
	out := resolved
	out.Components = CloneResourceRefs(resolved.Components)
	return out, nil
}

// Release drops one owner's grant on one resource.
func (m *Manager) Release(rootID string, resource ResourceRef) (bool, error) {
	return m.inner.Release(rootID, resource)
}

// ReleaseOwner releases every grant and pending request of one root. It is idempotent.
func (m *Manager) ReleaseOwner(rootID string) BudgetUsage { return m.inner.ReleaseOwner(rootID) }

// Cancel withdraws a queued request and reports the single status that won.
func (m *Manager) Cancel(rootID string, handle *LockHandle) RequestStatus {
	return m.inner.Cancel(rootID, handle)
}

// WaitStatus reports a queued request's current status without waiting for it.
//
// It is a read: it never advances a queue and never consumes the notification, so a caller can
// poll a handle for reporting without changing the outcome it will observe.
func (m *Manager) WaitStatus(rootID string, handle *LockHandle) RequestStatus {
	return m.inner.WaitStatus(rootID, handle)
}

// LockStateFor reports one resource's observable state.
func (m *Manager) LockStateFor(resource ResourceRef) LockState {
	return m.inner.LockStateFor(resource)
}

// Snapshot returns every resource that has a holder or a waiter.
func (m *Manager) Snapshot() []LockSnapshot { return m.inner.Snapshot() }

// Edges returns the initial blocker footprint of every queued request.
func (m *Manager) Edges() []LockEdge { return m.inner.Edges() }

// PendingRequests reports how many distinct requests one root is waiting for.
func (m *Manager) PendingRequests(rootID string) int { return m.inner.PendingRequests(rootID) }

// Stats returns the aggregate diagnostics view.
func (m *Manager) Stats() Stats { return m.inner.Stats() }

// Closed reports whether this manager was closed.
//
// It is part of the diagnostics surface because "the engine closed and the lock manager went with
// it" is an observable contract, not an implementation detail: a caller holding the manager after
// the engine ended must be able to see that it can no longer serve requests, instead of inferring it
// from a refused registration.
func (m *Manager) Closed() bool { return m.inner.Closed() }

// Ledger exposes the accounting ledger.
func (m *Manager) Ledger() *Ledger { return m.inner.Ledger() }

// Bindings exposes the namespace binding registry.
func (m *Manager) Bindings() *Bindings { return m.inner.Bindings() }

// LockTableSize reports how many resources currently have a holder or a waiter.
func (m *Manager) LockTableSize() int { return m.inner.LockTableSize() }

// ReleaseAll drops every grant and pending request of every owner.
func (m *Manager) ReleaseAll() {
	m.inner.mu.Lock()
	owners := make([]string, 0, len(m.inner.owners))
	for rootID := range m.inner.owners {
		owners = append(owners, rootID)
	}
	m.inner.mu.Unlock()
	for _, rootID := range owners {
		m.inner.ReleaseOwner(rootID)
	}
}

// Close ends the manager and wakes every blocked caller.
func (m *Manager) Close() { m.inner.Close() }

// RegisterOwner registers one root transaction as a lock owner.
//
// The call is bounded and fails closed: a duplicate root ID, an empty ID, an ended
// manager and an exhausted ordinal are all refused without registering anything.
// The returned owner is the only handle the caller keeps; the manager itself never
// hands out a transaction pointer.
func (m *manager) RegisterOwner(rootID string, generation, term, readTS uint64) (*LockOwner, error) {
	if rootID == "" {
		return nil, ErrInvalidOptions
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrOwnerUnknown
	}
	if _, exists := m.owners[rootID]; exists {
		return nil, ErrOwnerExists
	}
	if m.nextOrdinal == ^uint64(0) {
		return nil, ErrOrdinalExhausted
	}
	if err := m.ledger.ChargeFixed(ownerFixedBytes, rootID); err != nil {
		return nil, err
	}
	m.nextOrdinal++
	owner := &LockOwner{
		RootID:     rootID,
		Ordinal:    m.nextOrdinal,
		Generation: generation,
		Term:       term,
		ReadTS:     readTS,
		State:      OwnerActive,
		StartedAt:  m.clock.Now(),
		scopes:     NewScopeTree(),
		epochSet:   true,
	}
	m.owners[rootID] = owner
	m.scopes[rootID] = owner.scopes
	return owner, nil
}

// Owner returns one registered owner.
func (m *manager) Owner(rootID string) (*LockOwner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.owners[rootID]
	return owner, ok
}

// OrdinalOf reports the registration ordinal of one owner.
func (m *manager) OrdinalOf(rootID string) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.owners[rootID]
	if !ok {
		return 0, false
	}
	return owner.Ordinal, true
}

// Owners returns a copy of the registered owners in registration order.
//
// The result is a value snapshot: no manager-owned object crosses the boundary, so a
// diagnostics or detection caller cannot mutate the registry.
func (m *manager) Owners() []LockOwner {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LockOwner, 0, len(m.owners))
	for _, owner := range m.owners {
		out = append(out, *owner)
	}
	sortOwnersByOrdinal(out)
	return out
}

func sortOwnersByOrdinal(owners []LockOwner) {
	// Insertion sort keeps this dependency-free and is fine for the bounded owner
	// count; the ordering exists so diagnostics and victim selection are
	// deterministic.
	for i := 1; i < len(owners); i++ {
		for j := i; j > 0 && owners[j].Ordinal < owners[j-1].Ordinal; j-- {
			owners[j], owners[j-1] = owners[j-1], owners[j]
		}
	}
}

// OpenScope opens one request scope for a registered owner.
//
// parentScopeID must be a scope of the same owner, which is what makes "every scope
// belongs to exactly one root" structural rather than a convention. A scope is not a
// lock owner: closing it cancels its own pending requests and never releases a grant.
func (m *manager) OpenScope(rootID, originTxnID string, parentScopeID uint64, kind ScopeKind, deadline time.Time) (*Scope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.owners[rootID]
	if !ok {
		return nil, ErrOwnerUnknown
	}
	if owner.State == OwnerEnded {
		return nil, ErrOwnerUnknown
	}
	scopes := m.scopes[rootID]
	if scopes == nil {
		scopes = NewScopeTree()
		m.scopes[rootID] = scopes
	}
	scope, err := scopes.Open(rootID, originTxnID, parentScopeID, kind, m.clock.Now(), deadline)
	if err != nil {
		return nil, err
	}
	if err := m.ledger.ChargeFixed(scopeFixedBytes, rootID); err != nil {
		// The scope itself must be charged before it exists, so a rejected scope leaves
		// nothing behind: not a half-registered entry and not a dangling ordinal.
		scopes.Discard(scope.ID)
		return nil, err
	}
	return scope, nil
}

// Scope returns one scope of one owner. An empty rootID searches every owner, which
// exists for diagnostics only.
func (m *manager) Scope(rootID string, scopeID uint64) (Scope, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rootID != "" {
		scopes, ok := m.scopes[rootID]
		if !ok {
			return Scope{}, false
		}
		return scopes.Get(scopeID)
	}
	for _, scopes := range m.scopes {
		if scope, ok := scopes.Get(scopeID); ok {
			return scope, true
		}
	}
	return Scope{}, false
}

// ownerOfScope resolves the root that owns a scope.
func (m *manager) ownerOfScope(scopeID uint64) (string, error) {
	for rootID, scopes := range m.scopes {
		if _, ok := scopes.Get(scopeID); ok {
			return rootID, nil
		}
	}
	return "", ErrScopeUnknown
}

// verifyScope checks that a scope is open, belongs to the given owner and may issue a
// new request. It is the single validation point every request path uses, so a
// scope from another owner can never be used to acquire a lock.
func (m *manager) verifyScope(rootID string, scopeID uint64) error {
	scopes, ok := m.scopes[rootID]
	if !ok {
		return ErrOwnerUnknown
	}
	scope, ok := scopes.Get(scopeID)
	if !ok {
		return ErrScopeUnknown
	}
	if scope.OwnerID != rootID {
		return ErrScopeOwnerMismatch
	}
	if !scope.CanRequest() {
		return ErrScopeUnknown
	}
	return nil
}

// BeginCloseScope marks one scope as closing and returns the ids whose pending
// requests must be cancelled, deepest first. It never releases a grant.
func (m *manager) BeginCloseScope(rootID string, scopeID uint64) ([]uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scopes, ok := m.scopes[rootID]
	if !ok {
		return nil, ErrOwnerUnknown
	}
	if _, err := scopes.BeginClose(scopeID); err != nil {
		return nil, err
	}
	closed := scopes.CloseSubtree(scopeID)
	discarded := make(map[uint64]bool, len(closed))
	for _, id := range closed {
		discarded[id] = true
	}
	// A scope becomes unable to issue requests here, so the callers that joined a request from it
	// must stop waiting now rather than at the later close.
	m.closeJoinsLocked(discarded)
	return closed, nil
}

// CloseScope closes one scope and returns the ids it closed. The caller cancels the
// pending requests of those ids and keeps every grant: grants belong to the root.
//
// Cancelling the pending requests is done here, under the same critical section that
// closes the scope, so a request from a discarded savepoint layer can never be granted
// after its scope is gone.
func (m *manager) CloseScope(rootID string, scopeID uint64) ([]uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scopes, ok := m.scopes[rootID]
	if !ok {
		return nil, ErrOwnerUnknown
	}
	if _, ok := scopes.Get(scopeID); !ok {
		return nil, ErrScopeUnknown
	}
	closed := scopes.CloseSubtree(scopeID)
	discarded := make(map[uint64]bool, len(closed))
	for _, id := range closed {
		discarded[id] = true
	}
	m.cancelRequestsLocked(func(entry *lockRequest) bool {
		return entry.req.OwnerID == rootID && discarded[entry.req.ScopeID]
	}, RequestCanceled)
	m.closeJoinsLocked(discarded)
	return closed, nil
}

// closeJoinsLocked ends the waits of the callers that joined a request from one of the discarded
// scopes.
//
// A join is a caller waiting for a request it did not create, so discarding the scope the join came
// from has to end that caller's wait: a closed statement scope must not keep blocking, and it must
// never be handed a grant for a statement that no longer exists. The request itself is deliberately
// untouched 鈥?it belongs to the scope that created it, to the root and to every other caller still
// waiting for it 鈥?which is why this is not part of cancelRequestsLocked.
//
// The scan runs over the queued requests, which are bounded by the waiter budget, and each scope's
// join record is removed as it is closed, so a repeated scope close finds nothing left to do.
func (m *manager) closeJoinsLocked(discarded map[uint64]bool) {
	if len(discarded) == 0 || len(m.waiting) == 0 {
		return
	}
	for bundle := range m.waiting {
		for scopeID, join := range bundle.joins {
			if !discarded[scopeID] {
				continue
			}
			join.close()
			delete(bundle.joins, scopeID)
		}
	}
}

// Ancestors returns the scope chain of one open scope, nearest first. A read layer
// uses it to decide which ancestors' overlays it may observe: its own chain only,
// never a sibling's.
func (m *manager) Ancestors(rootID string, scopeID uint64) []Scope {
	m.mu.Lock()
	defer m.mu.Unlock()
	scopes, ok := m.scopes[rootID]
	if !ok {
		return nil
	}
	return scopes.Ancestors(scopeID)
}

// Sibling reports whether two scopes of one owner are different branches, which is
// exactly the case a read layer must refuse.
func (m *manager) Sibling(rootID string, a, b uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	scopes, ok := m.scopes[rootID]
	if !ok || a == b {
		return false
	}
	return !scopes.IsAncestor(a, b) && !scopes.IsAncestor(b, a)
}

// RequestAbort records that a root should stop.
//
// The manager only records the intent and reports whether this call was the first
// one; it never rolls a transaction back. Ending the root stays the job of the owner's
// own goroutine, which is what keeps a detector from touching a write set.
//
// The cause is part of the intent, because the caller that observes it has to know whether to
// retry the transaction (a deadlock) or to treat its handles as belonging to a generation that no
// longer exists (an epoch change).
func (m *manager) RequestAbort(rootID string, reason AbortReason) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.owners[rootID]
	if !ok {
		return false, ErrOwnerUnknown
	}
	if owner.State == OwnerEnded {
		return false, ErrOwnerUnknown
	}
	if m.abortRequested[rootID] != "" {
		// The first cause wins. An epoch change that follows a deadlock verdict must not relabel a
		// victim as an invalidated handle, or the retry instruction would change under the caller.
		return false, nil
	}
	m.abortRequested[rootID] = reason
	if owner.State == OwnerActive {
		owner.State = OwnerAbortRequested
	}
	return true, nil
}

// AbortRequested reports whether a root was asked to stop.
func (m *manager) AbortRequested(rootID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.abortRequested[rootID] != ""
}

// AbortReasonOf reports why one root was asked to stop.
func (m *manager) AbortReasonOf(rootID string) AbortReason {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.abortRequested[rootID]
}

// MarkFinishing moves an owner to FINISHING. From here the owner may no longer
// request a lock, which is what keeps a decided root from acquiring new protection.
func (m *manager) MarkFinishing(rootID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.owners[rootID]
	if !ok {
		return ErrOwnerUnknown
	}
	if owner.State == OwnerEnded {
		return nil
	}
	owner.State = OwnerFinishing
	return nil
}

// Acquire requests one atomic bundle of lock components and waits when it conflicts.
//
// It is AcquireWith defaulting the wait decision to "wait", which is the behaviour a
// transaction wants: it queues and is woken by the holder's release.
func (m *manager) Acquire(ctx context.Context, rootID string, scopeID uint64, components []ResourceRef) (AcquireResult, error) {
	return m.AcquireWith(ctx, rootID, scopeID, components, nil)
}

// TryAcquire requests one atomic bundle and refuses to queue.
//
// It exists for a caller that must not block, such as a fail-fast lock probe: a conflict
// is reported as ErrConflict immediately, with no queue entry and no charge.
func (m *manager) TryAcquire(ctx context.Context, rootID string, scopeID uint64, components []ResourceRef) (AcquireResult, error) {
	return m.AcquireWith(ctx, rootID, scopeID, components, func() bool { return false })
}

// AcquireWith requests one atomic bundle of lock components and optionally lets the caller
// decide whether a conflict may be waited for.
//
// The whole decision happens under the manager mutex:
//
//  1. The scope must be open and belong to the owner, and the owner must still be
//     allowed to request locks.
//  2. The bundle is validated as a whole, including its table-intention prerequisite.
//  3. If the owner already holds every component, the call is a re-entrant no-op: it
//     returns LockOpReentrant and charges nothing, so repeating a lock is free.
//  4. The bundle's budget is reserved in one all-or-nothing step, so a rejected bundle
//     leaves nothing behind.
//  5. Every component is examined against the other owners' grants and against the
//     older waiters on that resource. If the request may not wait, the reservation is
//     refunded and ErrConflict is returned instead of a queue entry.
//
// The whole bundle is granted together or queued as exactly one request per component:
// a partially granted bundle is never observable. A queued request is returned as a
// LockHandle for the caller to wait on, so no manager lock is ever held across a wait.
func (m *manager) AcquireWith(ctx context.Context, rootID string, scopeID uint64, components []ResourceRef, mayWait func() bool) (AcquireResult, error) {
	if err := ctx.Err(); err != nil {
		return AcquireResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	owner, ok := m.owners[rootID]
	if !ok {
		return AcquireResult{}, ErrOwnerUnknown
	}
	if !owner.State.CanRequest() {
		// An owner that is ending or that a detector asked to stop must not start new work. Letting
		// a victim take one more lock would contradict "a chosen victim cannot continue", and the
		// grant would then have to be undone by the abort anyway.
		return AcquireResult{}, ErrOwnerEnding
	}
	if err := m.verifyScope(rootID, scopeID); err != nil {
		return AcquireResult{}, err
	}
	if err := ValidateBundle(components, m.bindings); err != nil {
		return AcquireResult{}, err
	}
	deduped := DedupeResourceRefs(components)

	// Re-entrancy: the owner already *holds* every component at an equal or stronger mode, so
	// there is nothing to take and nothing to charge for.
	//
	// "Holds" means an applied claim and nothing else. A claim that belongs to a request which is
	// still queued is protection the owner has *asked* for, not protection it was given: treating
	// it as held would answer an upgrade with LockOpReentrant while the owner still holds only the
	// shared lock it started with, and every later decision based on that answer would be wrong.
	allHeld := true
	for _, component := range deduped {
		if m.claimSatisfiedByOwner(rootID, component) {
			continue
		}
		allHeld = false
		break
	}
	if allHeld {
		// A re-entrant request reports the owner's existing protection. It carries no
		// fresh grant ID, because it took no new lock.
		return AcquireResult{Op: LockOpReentrant, Grant: LockGrant{
			OwnerID:       rootID,
			OriginScopeID: scopeID,
			Generation:    owner.Generation,
			Term:          owner.Term,
			Components:    CloneResourceRefs(deduped),
			GrantedAt:     m.clock.Now(),
		}}, nil
	}

	// A component this owner has already queued is not held, but it is also not a new need:
	// starting a second request for it would take a second claim for one protection and give the
	// caller a second terminal status to reconcile. When one of the owner's queued requests
	// already covers this whole request, the acquisition joins it, so both callers wait for the
	// same request and observe its single outcome.
	if queued := m.joinableRequestLocked(rootID, deduped); queued != nil {
		if mayWait != nil && !mayWait() {
			// A caller that refuses to wait cannot join a request that has not settled. Refusing is
			// the fail-fast answer; reporting the queued request as held would be a lie.
			return AcquireResult{}, ErrConflict
		}
		return AcquireResult{Op: LockOpWait, Wait: newJoinedHandle(queued, queued.joinFor(scopeID))}, nil
	}

	reservation, err := m.ledger.Reserve(rootID, deduped, true)
	if err != nil {
		return AcquireResult{}, err
	}

	request, err := m.newRequestLocked(rootID, scopeID, deduped, reservation, mayWait, nil)
	if err != nil {
		m.ledger.Release(reservation)
		return AcquireResult{}, err
	}

	blocked, blockers := m.inspectLocked(request)

	if len(blocked) > 0 {
		if !request.req.MayWait() {
			m.ledger.Release(reservation)
			return AcquireResult{}, ErrConflict
		}
		// A queued bundle may repeat protection its owner already holds. Those components are
		// adopted instead of claimed again, so the request adds exactly one claim per genuinely
		// new component and its release retracts exactly those.
		//
		// The snapshot is taken *before* the bundle records its own claims: adopting after that
		// would match the request's own pending claims and mark every component as adopted,
		// which would make the bundle look complete without holding anything. Marking the
		// adopted entries must equally happen *before* the bundle is queued, because queueing
		// is what records a claim: an adopted component marked afterwards would keep a pending
		// claim that nothing ever retracts, so releasing the lock the owner actually took
		// would leave the resource held by a ghost.
		adopted := m.adoptedClaims(rootID, request.components())
		m.markAdoptedLocked(request, adopted)
		// The whole bundle is queued, including the components that are already grantable.
		// Registering all of them is what keeps the bundle atomic: a component left out of its
		// resource's queue would only be granted by a pass triggered from some *other*
		// resource, so a bundle touching two contended resources could report granted with one
		// component still unheld. The grantable components are applied immediately.
		if request.req.BlockedAt.IsZero() {
			request.req.BlockedAt = m.clock.Now()
		}
		m.enqueueLocked(request, blocked)
		request.req.BlockerIDs = blockers
		m.advanceLocked(request)
		if request.complete() {
			return AcquireResult{Op: LockOpGranted, Grant: m.grantOfLocked(request)}, nil
		}
		return AcquireResult{Op: LockOpWait, Wait: newHandle(request, true)}, nil
	}

	// Nothing blocks the bundle, so it is granted as a whole in this critical section.
	m.grantLocked(request)
	if !request.complete() {
		// Granting could not commit the reservation, which only happens when the budget cannot
		// hold even the grant overhead. Fail closed rather than reporting a lock that is not
		// charged for.
		m.ledger.Release(reservation)
		return AcquireResult{}, ErrBudgetMemory
	}
	return AcquireResult{Op: LockOpGranted, Grant: m.grantOfLocked(request)}, nil
}

// grantOfLocked returns the capability of a bundle that has completed.
//
// The bundle recorded its grant when it completed, so this is the same value every other caller of
// the same request resolves. The fallback exists only for a bundle that completed before it could
// record one, which would be an internal inconsistency rather than a normal path.
func (m *manager) grantOfLocked(bundle *requestBundle) LockGrant {
	if grant, ok := bundle.recordedGrant(); ok {
		grant.Components = CloneResourceRefs(grant.Components)
		return grant
	}
	return m.buildGrant(bundle)
}

// claimSatisfiedByOwner reports whether this owner already *holds* one component at a mode equal
// to or stronger than the one requested.
//
// Only an applied claim counts. A claim recorded by a request that is still queued is not a grant:
// the owner has asked for that mode, it has not been given it, and another owner's incompatible
// grant may still be standing in the way. Re-entrancy answers "you already have this", so
// answering it from a queued claim would report protection that does not exist. A request for a
// *stronger* mode is deliberately not satisfied either 鈥?that is an upgrade, which must be taken.
//
// The claim must be the *same lock*, not merely a claim of the same conflict domain. Two gaps of
// one namespace live in one entry because they have to be compared with each other, and holding
// `Gap(a,m)` says nothing about a request for `Gap(n,z)`: that is a second protection, and
// answering it with REENTRANT would leave the second range unprotected while telling the caller it
// holds it. Same for two insertion positions.
func (m *manager) claimSatisfiedByOwner(rootID string, component ResourceRef) bool {
	entry, ok := m.locks.get(component.ResourceKey())
	if !ok {
		return false
	}
	for _, claim := range entry.claims[rootID] {
		if claim.pending {
			continue
		}
		if !SameClaim(claim.ref, component) {
			continue
		}
		if !upgradeNeeded(component.Mode, claim.mode) {
			return true
		}
	}
	return false
}

// joinableRequestLocked returns one of the owner's queued requests that already covers every
// component of a new request, or nil when there is none.
//
// It is how a repeated acquisition is kept idempotent without lying about what is held. The
// earliest such request wins, so the queue position and the terminal status the caller observes do
// not depend on map iteration order. A request that has already settled is skipped: its outcome is
// final, and joining it would not deliver the protection the caller is asking for.
func (m *manager) joinableRequestLocked(rootID string, components []ResourceRef) *requestBundle {
	var best *requestBundle
	for entry := range m.pending[rootID] {
		bundle := entry.bundle
		if bundle == nil || bundle.req.Status.Terminal() {
			continue
		}
		if best != nil && bundle.req.EnqueueOrdinal >= best.req.EnqueueOrdinal {
			continue
		}
		if !bundleCoversRequest(bundle, components) {
			continue
		}
		best = bundle
	}
	return best
}

// bundleCoversRequest reports whether one queued request asked for every component of a bundle, at
// a mode that covers the requested one.
//
// The component is matched by its exact claim identity (`ClaimKey`), which is what makes the match
// independent of the order the caller listed its components in 鈥?and what keeps a queued
// `Gap(a,m)` from being treated as already covering a new request for `Gap(n,z)`. Sharing a
// conflict domain is not covering: the two gaps are different ranges, so joining the queued request
// would leave the new range protected only by accident.
func bundleCoversRequest(bundle *requestBundle, components []ResourceRef) bool {
	for _, component := range components {
		covered := false
		for _, entry := range bundle.entries {
			if !SameClaim(entry.ref, component) {
				continue
			}
			if upgradeNeeded(component.Mode, entry.ref.Mode) {
				continue
			}
			covered = true
			break
		}
		if !covered {
			return false
		}
	}
	return true
}

// adoptedClaims maps a bundle component to the request whose claim already covers it.
//
// A component the owner already holds is adopted rather than claimed again, so the queued
// request adds exactly one claim per genuinely new component and its release retracts exactly
// those.
//
// Only *applied* claims are adopted: they are protection the owner already holds, so the
// component is satisfied without a second claim. A pending claim belongs to a request that is
// still waiting and must not be adopted, because that would report a grant for a mode the
// owner does not hold yet.
//
// Adoption matches the exact claim identity for the same reason re-entrancy does: an applied
// `Gap(a,m)` is not the protection a request for `Gap(n,z)` asked for.
func (m *manager) adoptedClaims(rootID string, components []ResourceRef) map[string]*LockRequest {
	adopted := make(map[string]*LockRequest, len(components))
	for _, component := range components {
		entry, ok := m.locks.get(component.ResourceKey())
		if !ok {
			continue
		}
		for _, claim := range entry.claims[rootID] {
			if claim.pending || claim.request == nil {
				// Only an applied claim is adopted. A pending claim belongs to a request that is
				// still waiting, so adopting it would let this request report a grant for a mode
				// the owner does not hold yet.
				continue
			}
			if !SameClaim(claim.ref, component) {
				continue
			}
			if upgradeNeeded(component.Mode, claim.mode) {
				continue
			}
			adopted[component.ClaimKey()] = claim.request
			break
		}
	}
	return adopted
}

// markAdoptedLocked records which entries an earlier claim of the same owner already covers.
//
// It runs while the bundle is being built, before the bundle records any claim of its own, so the
// adoption cannot match the request's own pending claims and mark every component adopted.
func (m *manager) markAdoptedLocked(bundle *requestBundle, adopted map[string]*LockRequest) {
	for _, entry := range bundle.entries {
		if _, ok := adopted[entry.ref.ClaimKey()]; ok {
			entry.state = entryAdopted
		}
	}
}

// newRequestLocked builds the request record for one validated bundle.
//
// The request obtains its identity and queue ordinal here, under the mutex, so the
// fairness order is total and monotonic independently of how many goroutines arrive at
// once. A request whose per-component entries could not be created fails closed and is
// refunded by the caller.
func (m *manager) newRequestLocked(rootID string, scopeID uint64, deduped []ResourceRef, reservation *Reservation, mayWait func() bool, adopted map[string]*LockRequest) (*requestBundle, error) {
	if m.nextRequestID == ^uint64(0) || m.nextEnqueueOrdinal == ^uint64(0) {
		return nil, ErrOrdinalExhausted
	}
	m.nextRequestID++
	m.nextEnqueueOrdinal++
	now := m.clock.Now()
	request := &LockRequest{
		ID:             m.nextRequestID,
		OwnerID:        rootID,
		ScopeID:        scopeID,
		EnqueueOrdinal: m.nextEnqueueOrdinal,
		RequestedAt:    now,
		Status:         RequestPending,
		ReservedBytes:  reservation.Bytes(),
		ShouldWait:     mayWait,
		Deadline:       m.requestDeadlineLocked(scopeID),
	}
	bundle := &requestBundle{req: request, reservation: reservation, decision: newRequestState()}
	bundle.entries = make([]*lockRequest, 0, len(deduped))
	for _, component := range deduped {
		bundle.entries = append(bundle.entries, &lockRequest{req: request, ref: component, bundle: bundle, adopted: adopted[component.ClaimKey()]})
	}
	return bundle, nil
}

// inspectLocked reports which of a bundle's components are blocked, and by which roots.
//
// The returned entries are exactly the components the bundle cannot take now, so the
// caller can queue only those. Two rules are checked per component:
//
//   - a holder of another owner with an incompatible mode, and
//   - an older waiter on that component whose mode would be hindered by this request.
//
// The second rule is what makes the queue fair, and it is checked on *every component of
// the bundle*, not only on the ones this request shares with another request: a bundle
// carries its own table intention, so a request for a new row still has to respect the
// waiters that already hold or want the same table. Checking only an entry's own queue
// would let a young request overtake an older one on the table and break the order the
// contract requires.
func (m *manager) inspectLocked(bundle *requestBundle) ([]*lockRequest, []string) {
	var blocked []*lockRequest
	seen := make(map[string]bool)
	for _, entry := range bundle.entries {
		lockEntry := m.locks.entryFor(entry.ref)
		conflict := false
		// The mode that matters is *this* component's mode. A bundle carries components of
		// different kinds 鈥?a table intention beside a record lock 鈥?so testing every component
		// with one mode would let a record mode be compared against a table intention and the
		// conflict would be missed or invented.
		for _, holder := range lockEntry.conflictingHolders(entry.ref, entry.req.OwnerID) {
			conflict = true
			if !seen[holder] {
				seen[holder] = true
				bundle.blockers = append(bundle.blockers, holder)
			}
		}
		if lockEntry.immediateConflict(entry) {
			conflict = true
			if lockEntry.waitersAhead(entry, entry.ref) {
				bundle.blockedByOlderWaiter = true
			}
		}
		if conflict {
			blocked = append(blocked, entry)
		}
	}
	sort.Strings(bundle.blockers)

	return blocked, bundle.blockers
}

// waiterAheadAnywhere reports whether an older waiter on this entry's own resource would
// be hindered by the entry's request.
func (m *manager) waiterAheadAnywhere(entry *lockRequest) bool {
	lockEntry, ok := m.locks.get(entry.ref.ResourceKey())
	if !ok {
		return false
	}
	return lockEntry.waitersAhead(entry, entry.ref)
}

// enqueueLocked records every component of the bundle in its resource's queue and indexes
// the request so a scope close, an owner release or an epoch reset can cancel it.
//
// Every component is queued, not only the blocked ones. Registering all of them is what
// makes the bundle atomic and is also what keeps the wait graph complete: a component that
// is already grantable is applied in the same critical section by the fair-order pass, and
// a component that is not yet grantable is visible to every later request on that resource
// so it can be respected while this request waits.
func (m *manager) enqueueLocked(bundle *requestBundle, blocking []*lockRequest) {
	blocked := make(map[*lockRequest]bool, len(blocking))
	for _, entry := range blocking {
		blocked[entry] = true
	}
	for _, entry := range bundle.entries {
		entry.blocked = blocked[entry]
		lockEntry := m.locks.entryFor(entry.ref)
		if entry.state != entryAdopted {
			// A queued component records its claim as *pending*: the claim is real and must count
			// against other owners 鈥?that is what makes a waiting upgrade protect the row and what
			// lets a repeated request be recognised as covered 鈥?but it does not yet apply to the
			// owner's granted mode. An adopted component already has the owner's claim and records
			// nothing.
			lockEntry.addClaim(entry.req, entry.ref, true)
		}
		// Every component joins its resource's queue, including one that is already grantable.
		// The bundle is granted as a whole by bundleReadyLocked, which looks each component up in
		// its resource's queue, so a component left out of the queue could never be found and the
		// bundle would wait for ever on a component that was free from the start.
		lockEntry.enqueue(entry)
	}
	if m.pending[bundle.req.OwnerID] == nil {
		m.pending[bundle.req.OwnerID] = make(map[*lockRequest]bool)
	}
	for _, entry := range bundle.entries {
		m.pending[bundle.req.OwnerID][entry] = true
	}
	m.waiting[bundle] = true
	bundle.req.ReservationToken = bundle.reservation.Token()
}

// advanceEntryLocked runs the fair-order pass over one resource and then retries every
// incomplete bundle.
//
// A state change on one resource can unblock a bundle whose other components live on other
// resources, so a resource-local pass is not enough on its own. The retry pass is what
// completes such a bundle, and it is bounded because only registered incomplete bundles are
// visited.
func (m *manager) advanceEntryLocked(entry *lockEntry) {
	m.retryWaitingLocked()
}

// advanceLocked runs the fair-order pass after a bundle has been queued.
//
// The components that are already grantable are applied in the same critical section, so
// only the genuinely blocked components keep the caller waiting.
func (m *manager) advanceLocked(bundle *requestBundle) {
	m.retryWaitingLocked()
}

// anonIDLocked returns the anonymous identity of one resource entry, assigning one on first
// use. The id is a manager-local counter, so it stands for a resource without revealing
// anything about it.
func (m *manager) anonIDLocked(entry *lockEntry) uint64 {
	if entry.anonID == 0 {
		if m.nextResourceID < ^uint64(0) {
			m.nextResourceID++
		}
		entry.anonID = m.nextResourceID
	}
	return entry.anonID
}

// recordDeadlockLocked appends one summary to the bounded deadlock ring.
func (m *manager) recordDeadlockLocked(summary DeadlockSummary) {
	if len(m.deadlockRing) >= maxDeadlockSummaries {
		copy(m.deadlockRing, m.deadlockRing[1:])
		m.deadlockRing[len(m.deadlockRing)-1] = summary
		return
	}
	m.deadlockRing = append(m.deadlockRing, summary)
}

// deadlockSummariesLocked returns the retained summaries, newest first, with each entry copied
// so a caller cannot reach the manager's own storage.
func (m *manager) deadlockSummariesLocked(limit int) ([]DeadlockSummary, bool) {
	out := make([]DeadlockSummary, 0, len(m.deadlockRing))
	for i := len(m.deadlockRing) - 1; i >= 0; i-- {
		if len(out) >= limit {
			return out, true
		}
		out = append(out, cloneDeadlockSummary(m.deadlockRing[i]))
	}
	return out, false
}

// cloneDeadlockSummary deep-copies one summary's slices.
func cloneDeadlockSummary(summary DeadlockSummary) DeadlockSummary {
	out := summary
	out.CycleRootIDs = append([]string(nil), summary.CycleRootIDs...)
	out.AnonymousIDs = append([]uint64(nil), summary.AnonymousIDs...)
	out.Modes = append([]Mode(nil), summary.Modes...)
	return out
}

// rebuildGraphLocked replaces the derived wait-for graph with the edges the live queues imply.
//
// It is called after any change to a queue, so the graph is never a stale copy: the edges are
// exactly the blockers the fair pass would see. Deriving them here rather than maintaining them
// incrementally is what keeps a grant, a cancellation, a scope close, an owner release and an
// epoch reset from each needing their own edge bookkeeping.
// rebuildGraphLocked marks the derived graph as stale.
//
// It deliberately does no work. The graph is rebuilt by ensureGraphLocked when a reader needs it,
// which is what keeps the acquisition path from rescanning every queued component.
func (m *manager) rebuildGraphLocked() { m.graphDirty = true }

// ensureGraphLocked brings the derived graph up to date when a queue changed.
//
// The flag stays set after a rebuild. Rebuilding an unchanged graph leaves the revision where it
// was, so the work is cheap and bounded, and a diagnostics reader refreshing a page therefore
// keeps the same cursor revision for as long as the graph really is unchanged. Clearing the flag
// instead would advance the revision on every read of an idle manager, which would make the cursor
// useless for detecting a genuine change.
func (m *manager) ensureGraphLocked() {
	if !m.graphDirty {
		return
	}
	m.rebuildGraphNowLocked()
}

// rebuildGraphNowLocked replaces the derived wait-for graph with the edges the live queues imply.
func (m *manager) rebuildGraphNowLocked() {
	// Group the edges by the waiting root so setEdges can replace a root's whole set, which is
	// what removes an edge whose blocker is gone.
	byRoot := make(map[string][]LockEdge)
	rootsSeen := make(map[string]bool)
	// The blocker buffer is reused across components: the rebuild visits every queued component,
	// so allocating one slice per component would make it allocation-bound.
	scratch := make([]string, 0, 8)
	for _, requests := range m.pending {
		for entry := range requests {
			rootsSeen[entry.req.OwnerID] = true
			lockEntry, ok := m.locks.get(entry.ref.ResourceKey())
			if !ok {
				continue
			}
			// HOLDER edges: every other owner whose claim conflicts with this component.
			anonID := m.anonIDLocked(lockEntry)
			scratch = lockEntry.appendBlockingClaims(scratch[:0], entry)
			for _, ownerID := range scratch {
				byRoot[entry.req.OwnerID] = append(byRoot[entry.req.OwnerID], LockEdge{
					FromRootID: entry.req.OwnerID,
					ToRootID:   ownerID,
					RequestID:  entry.req.ID,
					Reason:     EdgeHolder,
					ResourceID: anonID,
				})
			}
			// OLDER_WAITER edges: every older waiter whose mode this component would hinder.
			for _, older := range lockEntry.olderBlockers(entry, entry.ref) {
				if older.req.OwnerID == entry.req.OwnerID {
					// The owner's own earlier request is not a deadlock participant.
					continue
				}
				byRoot[entry.req.OwnerID] = append(byRoot[entry.req.OwnerID], LockEdge{
					FromRootID: entry.req.OwnerID,
					ToRootID:   older.req.OwnerID,
					RequestID:  entry.req.ID,
					Reason:     EdgeOlderWaiter,
					ResourceID: anonID,
				})
			}
		}
	}
	for from, edges := range byRoot {
		m.graph.setEdges(from, dedupeEdges(edges))
	}
	// A root that no longer waits must lose its edges, which is the dangling-edge half of the
	// contract's "no edge survives the condition that produced it".
	for from := range m.graph.out {
		if !rootsSeen[from] {
			m.graph.removeRoot(from)
		}
	}
}

// dedupeEdges collapses repeated edges while keeping the first reason seen.
func dedupeEdges(edges []LockEdge) []LockEdge {
	seen := make(map[graphEdgeKey]bool, len(edges))
	out := make([]LockEdge, 0, len(edges))
	for _, edge := range edges {
		key := graphEdgeKey{to: edge.ToRootID, request: edge.RequestID, reason: edge.Reason}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, edge)
	}
	return out
}

// retryWaitingLocked grants every queued bundle whose components are all grantable now.
//
// A bundle is granted as a whole or not at all. A per-component grant would make a partially
// granted bundle observable 鈥?its owner would appear to hold the record but not the table
// intention 鈥?and would let an unrelated queue pass complete a bundle on the wrong trigger.
// Checking readiness for the whole bundle and then applying it keeps the grant atomic.
//
// The pass repeats until it makes no progress, because completing one bundle changes the
// holder set the next bundle is evaluated against. Each iteration grants at least one bundle
// or stops, so the loop is bounded by the number of waiting bundles.
func (m *manager) retryWaitingLocked() {
	for progress := true; progress; {
		progress = false
		for bundle := range m.waiting {
			if bundle.req.Status.Terminal() {
				delete(m.waiting, bundle)
				continue
			}
			if bundle.complete() {
				m.completeBundleLocked(bundle)
				continue
			}
			if !m.bundleReadyLocked(bundle) {
				continue
			}
			m.grantLocked(bundle)
			progress = true
		}
	}
	m.rebuildGraphLocked()
}

// bundleReadyLocked reports whether every component of a queued bundle may be granted now.
func (m *manager) bundleReadyLocked(bundle *requestBundle) bool {
	for _, entry := range bundle.entries {
		if entry.dead || entry.settled() {
			// An adopted component is already covered by the owner's earlier claim, and an
			// already-held one is settled.
			continue
		}
		lockEntry, ok := m.locks.get(entry.ref.ResourceKey())
		if !ok {
			return false
		}
		if !lockEntry.grantable(entry) {
			return false
		}
	}
	return true
}

// grantLocked applies a whole bundle's claims, records its grant and notifies the caller once.
func (m *manager) grantLocked(bundle *requestBundle) {
	if bundle.req.Status.Terminal() {
		return
	}
	for _, entry := range bundle.entries {
		lockEntry, ok := m.locks.get(entry.ref.ResourceKey())
		if !ok {
			continue
		}
		switch entry.state {
		case entryHeld:
			continue
		case entryAdopted:
			// An earlier request of the same owner already *applied* the claim this component
			// needs, so this bundle is satisfied without a second one. The claim this bundle
			// recorded for the component when it was queued must be retracted: leaving it behind
			// would keep the resource held by a claim whose request is already settled, so
			// releasing the lock the owner really took would not release it. The claim is pending
			// (it was never applied for this request), so there is no grant to refund.
			lockEntry.dropClaim(entry.req, entry.ref)
			entry.state = entryHeld
			bundle.held++
			lockEntry.remove(entry)
			continue
		}
		if !lockEntry.applyClaim(entry.req, entry.ref) {
			lockEntry.addClaim(entry.req, entry.ref, false)
		}
		entry.state = entryHeld
		bundle.held++
		lockEntry.remove(entry)
	}
	for _, key := range bundle.resourceKeys() {
		if lockEntry, ok := m.locks.get(key); ok {
			m.pruneEntryLocked(lockEntry)
			m.locks.release(key, lockEntry)
		}
	}
	m.completeBundleLocked(bundle)
}

// completeBundleLocked commits the reservation of a bundle whose every component is held,
// records its grant and notifies the caller exactly once.
func (m *manager) completeBundleLocked(bundle *requestBundle) {
	if bundle.req.Status.Terminal() || !bundle.complete() {
		return
	}
	// The reservation was charged at Acquire time, so committing it is a transition rather
	// than a second charge.
	if err := m.ledger.Commit(bundle.reservation); err != nil {
		// A budget that cannot hold even the grant overhead leaves the request queued, which
		// the caller observes as still waiting instead of as a false grant.
		return
	}
	m.untrackPendingLocked(bundle)
	delete(m.waiting, bundle)
	if m.nextGrantID < ^uint64(0) {
		m.nextGrantID++
		bundle.grantID = m.nextGrantID
	}
	// The capability is recorded with the request before its terminal status is published, so
	// every caller of this request 鈥?the one that started it and every caller that joined it 鈥?	// resolves the same identity, the same origin scope and the same component list.
	bundle.setGrant(m.buildGrant(bundle))
	bundle.setStatus(RequestGranted)
}

// defaultLockWaitTimeout is the wait deadline used when neither the request's context nor its
// scope supplies one.
//
// The contract fixes 50 seconds as the session lock wait timeout (FR-010), and the manager
// needs its own bound so a caller that passes a context with no deadline is still guaranteed to
// be woken rather than queued for ever.
const defaultLockWaitTimeout = 50 * time.Second

// requestDeadlineLocked computes the wait deadline of one request as the earlier of the
// manager's default budget and the scope's context deadline.
//
// The comparison uses the injected clock, so a test with a virtual clock drives the deadline
// deterministically and no assertion depends on wall-clock elapsed time.
// requestDeadlineLocked returns the wait deadline of a request issued from one scope.
//
// The scope's own deadline *is* the session's lock-wait bound when one was configured: a scope is
// opened with `now + innodb_lock_wait_timeout`, and the caller may legitimately ask for more than the
// contract default (the SQL variable accepts up to 86400 seconds). It is therefore not clamped to the
// default — clamping would silently cut a 300-second session off at 50 seconds, which is exactly the
// kind of hidden limit a configuration surface must not have. A caller whose query context ends
// earlier still stops waiting earlier, because the waiting caller observes its own context
// separately and a cancelled context withdraws the request.
func (m *manager) requestDeadlineLocked(scopeID uint64) time.Time {
	for _, scopes := range m.scopes {
		if scope, ok := scopes.Get(scopeID); ok && !scope.ContextDeadline.IsZero() {
			return scope.ContextDeadline
		}
	}
	return m.clock.Now().Add(defaultLockWaitTimeout)
}

// DetectDeadlock searches the wait-for graph for a cycle and, when it finds one, chooses the
// victim the contract prescribes.
//
// The detector only ever *marks* the victim. It withdraws the victim's pending requests with one
// definite status, which wakes any caller blocked on them, and it records the root so the same
// cycle is not reported twice. It deliberately does not roll back a transaction, release a grant
// or touch a write set: ending the root is the victim's own job, which is what keeps a detector
// from calling a transaction method from its own goroutine (FR-008/009/020/029).
func (m *manager) DetectDeadlock() (DeadlockSummary, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.detectRuns++
	m.ensureGraphLocked()
	cycleRoots, cycleEdges, exhausted := m.graph.detectCycle(m.OrdinalOfLocked, int(m.ledger.Limits().GraphWork))
	if exhausted {
		// An exhausted traversal budget is a resource failure and must never be reported as a
		// deadlock, because a false victim would abort an innocent transaction.
		return DeadlockSummary{}, false
	}
	if len(cycleRoots) == 0 {
		return DeadlockSummary{}, false
	}
	// A cycle that already contains a chosen victim is being resolved; reporting it again would
	// pick a second victim for one deadlock.
	for _, root := range cycleRoots {
		if m.deadlocked[root] {
			return DeadlockSummary{}, false
		}
	}
	victim, _ := pickVictim(cycleRoots, m.OrdinalOfLocked)
	if victim == "" {
		return DeadlockSummary{}, false
	}
	m.deadlocks++
	m.deadlocked[victim] = true
	m.markAbortLocked(victim, AbortDeadlock)
	m.cancelRequestsLocked(func(entry *lockRequest) bool {
		return entry.req.OwnerID == victim
	}, RequestDeadlockVictim)

	summary := DeadlockSummary{
		DetectedOrdinal: m.deadlocks,
		GraphRevision:   m.graph.Revision(),
		VictimRootID:    victim,
		CycleRootIDs:    truncateStrings(cycleRoots, maxDeadlockNodes),
		Truncated:       len(cycleRoots) > maxDeadlockNodes,
	}
	for _, edge := range cycleEdges {
		if len(summary.AnonymousIDs) >= maxDeadlockNodes {
			break
		}
		summary.AnonymousIDs = append(summary.AnonymousIDs, edge.ResourceID)
		summary.Modes = append(summary.Modes, m.modeOfRequestLocked(edge.RequestID, edge.FromRootID))
	}
	m.recordDeadlockLocked(summary)
	return summary, true
}

// OrdinalOfLocked reports a root's registration ordinal while the manager lock is held.
func (m *manager) OrdinalOfLocked(rootID string) (uint64, bool) {
	owner, ok := m.owners[rootID]
	if !ok {
		return 0, false
	}
	return owner.Ordinal, true
}

// markAbortLocked records why one root was asked to stop, while the manager lock is held.
func (m *manager) markAbortLocked(rootID string, reason AbortReason) {
	owner, ok := m.owners[rootID]
	if !ok || owner.State == OwnerEnded {
		return
	}
	m.abortRequested[rootID] = reason
	if owner.State == OwnerActive {
		owner.State = OwnerAbortRequested
	}
}

// modeOfRequestLocked reports the mode one request asked for, for a deadlock summary.
func (m *manager) modeOfRequestLocked(requestID uint64, rootID string) Mode {
	for entry := range m.pending[rootID] {
		if entry.req.ID == requestID {
			return entry.ref.Mode
		}
	}
	return ""
}

// truncateStrings caps a root list at one summary's node budget.
func truncateStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return append([]string(nil), values...)
	}
	return append([]string(nil), values[:limit]...)
}

// checkDeadlinesLocked completes every queued request whose wait deadline has passed.
//
// It is the manager-side half of the wait timeout: the caller's own timer wakes it, and this
// sweep decides the single terminal outcome under the lock so a deadline, a grant and a
// cancellation arriving together still produce exactly one result (FR-007).
func (m *manager) checkDeadlinesLocked() int {
	now := m.clock.Now()
	var expired []*requestBundle
	for _, requests := range m.pending {
		for entry := range requests {
			bundle := entry.bundle
			if bundle.req.Deadline.IsZero() || !now.After(bundle.req.Deadline) {
				continue
			}
			if bundle.req.Status.Terminal() {
				continue
			}
			expired = append(expired, bundle)
		}
	}
	count := 0
	for _, bundle := range expired {
		if bundle.req.Status.Terminal() {
			continue
		}
		m.timeouts++
		m.finishRequestLocked(bundle, RequestTimedOut)
		count++
	}
	return count
}

// timeOutIfExpired completes a request whose deadline has passed and reports whether it did.
func (m *Manager) timeOutIfExpired(rootID string, handle *LockHandle) bool {
	return m.inner.timeOutIfExpired(rootID, handle)
}

// checkDeadlines completes every queued request whose deadline has passed.
func (m *Manager) checkDeadlines() int { return m.inner.CheckTimeouts() }

// timeOutIfExpired completes a request whose deadline has passed and reports whether it did.
func (m *manager) timeOutIfExpired(rootID string, handle *LockHandle) bool {
	if handle == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bundle := m.findPendingLocked(rootID, handle.RequestID)
	if bundle == nil {
		return false
	}
	if bundle.req.Deadline.IsZero() || !m.clock.Now().After(bundle.req.Deadline) {
		return false
	}
	m.timeouts++
	m.finishRequestLocked(bundle, RequestTimedOut)
	return true
}

// WaitEdges reports the current wait-for graph in a stable order.
func (m *manager) WaitEdges() []LockEdge {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureGraphLocked()
	return m.graph.edges()
}

// CheckTimeouts completes every queued request whose wait deadline has passed.
func (m *manager) CheckTimeouts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkDeadlinesLocked()
}

// Deadlocks reports the retained deadlock summaries, newest first, up to limit.
func (m *manager) Deadlocks(limit int) ([]DeadlockSummary, bool) {
	if limit <= 0 {
		limit = maxDeadlockSummaries
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deadlockSummariesLocked(limit)
}

// buildGrant assembles the grant view a caller receives for one bundle.
//
// It is the single description of a granted request: the bundle's own grant id, its own origin
// scope, and exactly the components the bundle asked for. `completeBundleLocked` records it with
// the request so a waiter gets the same value the inline path returns.
func (m *manager) buildGrant(bundle *requestBundle) LockGrant {
	owner := m.owners[bundle.req.OwnerID]
	grant := LockGrant{
		GrantID:       bundle.grantID,
		OwnerID:       bundle.req.OwnerID,
		OriginScopeID: bundle.req.ScopeID,
		Components:    CloneResourceRefs(bundle.components()),
		GrantedAt:     m.clock.Now(),
	}
	if owner != nil {
		grant.Generation = owner.Generation
		grant.Term = owner.Term
	}
	return grant
}

// WaitStatus reports the current status of a queued request.
//
// A caller that is blocked on a handle calls this after its wait reports something, and a
// caller that wants to know whether its request is still queued calls it to read the
// current status without waiting. A terminal status is always taken from the delivered
// notification, so the answer is the outcome the manager actually sent rather than a
// value reconstructed here.
func (m *manager) WaitStatus(rootID string, handle *LockHandle) RequestStatus {
	if handle == nil {
		return RequestCanceled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if bundle := m.findPendingLocked(rootID, handle.RequestID); bundle != nil {
		return bundle.req.Status
	}
	// The request is not queued any more, so its decision is already recorded: read it
	// rather than the notification channel, which the caller may have consumed.
	return handle.Status()
}

// Cancel withdraws a queued request.
//
// It returns the single terminal status that won. If the request was already granted,
// that status is GRANTED and the grant is kept: the contract requires that a
// cancellation racing a grant cannot silently drop a held lock, so the caller learns it
// must release the grant instead.
//
// Only the caller that created the request owns it. A handle from a caller that joined the request
// has nothing of its own to withdraw, so cancelling through it reports the request's current
// status and leaves the request queued for its owner and for every other caller: one participant
// of a shared wait cannot destroy protection the others are still waiting for. A joiner stops
// waiting through its own context instead.
func (m *manager) Cancel(rootID string, handle *LockHandle) RequestStatus {
	if handle == nil {
		return RequestCanceled
	}
	if !handle.origin {
		return handle.Status()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bundle := m.findPendingLocked(rootID, handle.RequestID)
	if bundle == nil {
		return handle.Status()
	}
	return m.finishRequestLocked(bundle, RequestCanceled)
}

// ReleaseOwner releases every charge of one root and removes it from the registry.
//
// It is idempotent: a second call for an unknown or already released root is a no-op,
// so a double root cleanup, a failed merge and a disconnect cannot underflow the
// accounting. Every grant the root held is dropped and every request it was waiting for
// is cancelled with one definite status, which is what wakes a blocked caller instead of
// leaving it stuck on a handle nobody will complete.
func (m *manager) ReleaseOwner(rootID string) BudgetUsage {
	m.mu.Lock()
	owner, ok := m.owners[rootID]
	if ok {
		owner.State = OwnerEnded
		owner.EndedAt = m.clock.Now()
		delete(m.owners, rootID)
	}
	delete(m.scopes, rootID)
	delete(m.abortRequested, rootID)
	delete(m.deadlocked, rootID)
	m.releaseGrantsLocked(rootID, "")
	m.cancelRequestsLocked(func(entry *lockRequest) bool {
		return entry.req.OwnerID == rootID
	}, RequestCanceled)
	m.graph.removeRoot(rootID)
	// Rebuilding after the removal drops every edge that pointed at the released root, so no
	// waiting root keeps a blocker that no longer exists.
	m.rebuildGraphLocked()
	m.mu.Unlock()

	// The ledger takes its own lock, so the manager mutex is released first: the two
	// registries are independent and must never be nested.
	usage := m.ledger.ReleaseOwner(rootID)
	if ok {
		owner.usage = usage
	}
	return usage
}

// releaseGrantsLocked drops every grant one owner holds, or only those on one resource
// when resourceKey is not empty.
//
// A dropped grant makes the resource's queue try again, so the fair order decides which
// waiter moves next. An entry that is left with neither holder nor waiter is removed, so
// a released table grows no further.
//
// It drops claims, not charges: both of its callers refund the owner's whole ledger entry
// afterwards (ReleaseOwner and ResetEpoch), which is why neither of them can leave accounting
// behind, and why a caller must not use this function on its own.
func (m *manager) releaseGrantsLocked(ownerID string, resourceKey string) {
	for key, entry := range m.locks.entries {
		if resourceKey != "" && key != resourceKey {
			continue
		}
		if !entry.dropAllClaims(ownerID) {
			continue
		}
		m.advanceEntryLocked(entry)
		m.locks.release(key, entry)
	}
}

// Release drops one owner's grant on one resource.
//
// It exists for the caller that releases a single lock it acquired, and it is
// idempotent: releasing a resource the owner does not hold reports false and changes
// nothing, so a repeated release cannot corrupt the accounting or steal another owner's
// grant.
func (m *manager) Release(rootID string, resource ResourceRef) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owners[rootID]; !ok {
		return false, ErrOwnerUnknown
	}
	key := resource.ResourceKey()
	entry, ok := m.locks.get(key)
	if !ok {
		return false, nil
	}
	// The claim is retracted by its exact identity and refunded under the same identity: releasing
	// one gap of a namespace must not drop the other gaps that share its conflict domain, and the
	// ledger must not refund a charge that belongs to a different lock.
	if !entry.dropClaimForOwner(rootID, resource) {
		return false, nil
	}
	m.ledger.ReleaseGrant(rootID, resource.ClaimKey())
	m.advanceEntryLocked(entry)
	m.locks.release(key, entry)
	return true, nil
}

// pruneEntryLocked drops terminal entries and removes an empty resource entry.
func (m *manager) pruneEntryLocked(entry *lockEntry) {
	live := entry.waiting[:0]
	for _, candidate := range entry.waiting {
		if candidate.dead || candidate.req.Status.Terminal() {
			continue
		}
		live = append(live, candidate)
	}
	for i := len(live); i < len(entry.waiting); i++ {
		entry.waiting[i] = nil
	}
	entry.waiting = live
}

// cancelRequestsLocked completes every queued request matching a predicate with one
// definite status, and reports how many distinct requests it completed.
//
// It is the single terminal-transition helper for cancellation paths, so "scope closed",
// "owner released", "epoch invalidated", "deadline passed" and "deadlock victim" cannot disagree
// about how a queue entry is removed or how many notifications it produces.
//
// The count is of distinct requests, not of queue entries. The pending index is keyed by
// component entry, so a two-component bundle appears in it twice while still being one request; a
// caller that used the entry count as an invalidation total would over-report every multi-part
// request.
func (m *manager) cancelRequestsLocked(match func(*lockRequest) bool, status RequestStatus) int {
	cancelled := make(map[*requestBundle]bool)
	for _, requests := range m.pending {
		for entry := range requests {
			if match(entry) {
				cancelled[entry.bundle] = true
			}
		}
	}
	count := 0
	for bundle := range cancelled {
		if bundle.req.Status.Terminal() {
			continue
		}
		m.finishRequestLocked(bundle, status)
		count++
	}
	return count
}

// finishRequestLocked completes one queued request with one terminal status.
//
// It marks every component entry dead, removes them from their queues, refunds the
// reservation exactly once, removes the request from the pending index and notifies the
// waiting caller once. Removing the entries and waking the next waiter are the same
// critical section, so a cancellation can never leave a resource blocked by a request
// that no longer exists.
func (m *manager) finishRequestLocked(bundle *requestBundle, status RequestStatus) RequestStatus {
	if bundle.req.Status.Terminal() {
		return bundle.req.Status
	}
	for _, entry := range bundle.entries {
		lockEntry, ok := m.locks.get(entry.ref.ResourceKey())
		if !ok {
			continue
		}
		entry.dead = true
		// Retract exactly this request's claim, whether it was applied or still pending. A
		// pending claim is real protection offered to other owners, so leaving it behind would
		// keep blocking them after the request that made it is gone.
		if lockEntry.dropClaim(entry.req, entry.ref) && entry.state == entryHeld {
			m.ledger.ReleaseGrant(entry.req.OwnerID, entry.ref.ClaimKey())
		}
		lockEntry.remove(entry)
		m.pruneEntryLocked(lockEntry)
		m.locks.release(entry.ref.ResourceKey(), lockEntry)
	}
	m.ledger.Release(bundle.reservation)
	m.untrackPendingLocked(bundle)
	delete(m.waiting, bundle)
	bundle.setStatus(status)
	// A cancelled request frees its place in every queue it was in, so each of those
	// resources must re-run the fair pass. The bundle itself is terminal now, so the pass is
	// driven per resource rather than through the bundle.
	for _, key := range bundle.resourceKeys() {
		if lockEntry, ok := m.locks.get(key); ok {
			m.advanceEntryLocked(lockEntry)
			m.locks.release(key, lockEntry)
		}
	}
	m.rebuildGraphLocked()
	return status
}

// untrackPendingLocked removes one bundle from the per-root pending index.
//
// The index is keyed by entry because a bundle owns one entry per component, so
// removing the last entry is what removes the bundle and its root key.
func (m *manager) untrackPendingLocked(bundle *requestBundle) {
	bundle.untrack(m.pending)
}

// findPendingLocked resolves a queued request by its caller-visible identity.
func (m *manager) findPendingLocked(rootID string, requestID uint64) *requestBundle {
	for entry := range m.pending[rootID] {
		if entry.req.ID == requestID {
			return entry.bundle
		}
	}
	return nil
}

// PendingRequests reports how many distinct requests one root is currently waiting for.
//
// The pending index is keyed by component entry, so a bundle with several components has
// several entries; a caller asks about requests, so the bundles are counted.
func (m *manager) PendingRequests(rootID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	bundles := make(map[*requestBundle]bool, len(m.pending[rootID]))
	for entry := range m.pending[rootID] {
		bundles[entry.bundle] = true
	}
	return len(bundles)
}

// Edges reports the current wait-for graph.
//
// The graph is derived from the live queues, so it reports exactly the blockers the fair pass
// would see, both HOLDER and OLDER_WAITER. It is the same graph the detector searches, so a
// consumer observing a cycle is observing the cycle that would be detected.
func (m *manager) Edges() []LockEdge {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureGraphLocked()
	return m.graph.edges()
}

// Snapshot returns the observable state of every resource that has a holder or a waiter.
func (m *manager) Snapshot() []LockSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LockSnapshot, 0, len(m.locks.entries))
	for _, entry := range m.locks.entries {
		if entry.holderCount() == 0 && entry.waiterCount() == 0 {
			continue
		}
		roots := make([]string, 0, len(entry.claims))
		for rootID := range entry.claims {
			roots = append(roots, rootID)
		}
		sort.Strings(roots)
		out = append(out, LockSnapshot{
			Resource:    ResourceRef{Identity: entry.ref.Identity.Clone()},
			State:       entry.state(),
			HolderMode:  entry.holderMode(),
			Holders:     entry.holderCount(),
			Waiters:     entry.waiterCount(),
			HolderRoots: roots,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Resource.ResourceKey() < out[j].Resource.ResourceKey()
	})
	return out
}

// LockStateFor reports one resource's observable state.
func (m *manager) LockStateFor(resource ResourceRef) LockState {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.locks.get(resource.ResourceKey())
	if !ok {
		return LockStateFree
	}
	return entry.state()
}

// HeldMode reports the mode one owner actually holds on one resource.
//
// It counts applied claims only, so a transaction that is waiting to upgrade still reports the
// shared lock it holds.
//
// The answer is about the resource the caller named, not about its conflict domain: the domain
// entry holds every gap and intention of one namespace, so a domain-only lookup would report a
// `Gap(a,m)` as held when the caller asked whether it holds `Gap(n,z)`.
func (m *manager) HeldMode(rootID string, resource ResourceRef) (Mode, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.locks.get(resource.ResourceKey())
	if !ok {
		return "", false
	}
	return entry.heldModeOf(rootID, resource)
}

// offeredModeFor reports the offered mode for the test helpers, which work on the unexported
// manager type.
func (m *manager) offeredModeFor(rootID string, resource ResourceRef) (Mode, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.locks.get(resource.ResourceKey())
	if !ok {
		return "", false
	}
	return entry.offeredModeOf(rootID, resource)
}

// It counts applied claims only, so a transaction that is waiting to upgrade still reports the
// shared lock it holds. Use OfferedMode for the mode other owners must respect, which includes
// the claim of a waiting upgrade.
func (m *Manager) HeldMode(rootID string, resource ResourceRef) (Mode, bool) {
	return m.inner.HeldMode(rootID, resource)
}

// OfferedMode reports the mode one owner offers other owners on one resource.
//
// It includes the claims of the owner's waiting requests, so a transaction that is waiting to
// upgrade already blocks a new shared request from another owner even though its own reported
// mode is still shared.
func (m *Manager) OfferedMode(rootID string, resource ResourceRef) (Mode, bool) {
	m.inner.mu.Lock()
	defer m.inner.mu.Unlock()
	entry, ok := m.inner.locks.get(resource.ResourceKey())
	if !ok {
		return "", false
	}
	return entry.offeredMode(rootID)
}

// ResetEpoch invalidates the current generation or leader term.
//
// Every owner is *ended*, not merely marked as finishing, and every request it was waiting for is
// invalidated with one definite status. Ending the owners is what makes the two registries agree:
// an epoch change means the generation those owners belonged to no longer exists, so leaving them
// registered would keep lock owners behind that the transaction registry has already dropped, and
// they would go on blocking resources in a database that no longer contains them.
//
// Old handles are refused from here on, which happens before the waiters are woken: a waiter
// woken before its handle was refused could observe a grant from an invalidated generation.
// Namespace bindings move to the new generation, and lifecycle reconciliation for an undecided
// publication stays with the transaction layer, not here.
func (m *manager) ResetEpoch(generation, term uint64) int {
	m.mu.Lock()
	invalidated := 0
	ended := make([]string, 0, len(m.owners))
	for rootID, owner := range m.owners {
		if owner.State == OwnerEnded {
			continue
		}
		owner.State = OwnerEnded
		owner.EndedAt = m.clock.Now()
		owner.Generation = generation
		owner.Term = term
		m.abortRequested[rootID] = AbortEpoch
		// The owner is removed from the registry in the same step, together with the scopes that
		// belonged to it. Keeping the entry would leave an owner that can never request again and
		// that nothing would ever release, so the lock table would grow one dead owner per
		// generation change.
		delete(m.owners, rootID)
		delete(m.scopes, rootID)
		ended = append(ended, rootID)
		invalidated++
	}
	// The waiters are invalidated *before* the grants are dropped. The order is what makes the
	// outcome correct: dropping a grant runs the fair pass, and a pass over a still-pending queue
	// would hand the freed resource to a waiter whose answer is already decided. Invalidating first
	// means every waiter reaches a terminal state before any resource becomes available.
	m.cancelRequestsLocked(func(*lockRequest) bool { return true }, RequestInvalidated)
	// Every grant of an ended owner is dropped, so a resource cannot stay held by an owner the
	// registry no longer has.
	for _, rootID := range ended {
		m.releaseGrantsLocked(rootID, "")
	}
	m.graph.clear()
	m.deadlocked = make(map[string]bool)
	// The counter is a count of reset *events*, not of the owners each event marked: a
	// diagnostics reader asking "how many epoch changes invalidated state" wants the number of
	// changes, while the return value reports how many owners this one change affected.
	m.invalidated++
	m.bindings.Reset(generation)
	m.mu.Unlock()

	// The ledger has its own lock, so the manager mutex is released first, exactly as ReleaseOwner
	// does: the two registries are independent and are never nested here. The refund is not
	// optional bookkeeping 鈥?the owners were deleted above, so a charge left behind would account a
	// lock that no owner holds and no generation can release, which is the "epoch change leaves no
	// lock resource behind" half of the contract.
	for _, rootID := range ended {
		m.ledger.ReleaseOwner(rootID)
	}
	return invalidated
}

// Close ends the manager. Every owner is released, so a closed manager reports zero
// usage and refuses further registrations, and every blocked caller is woken.
func (m *manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	for rootID, owner := range m.owners {
		owner.State = OwnerEnded
		owner.EndedAt = m.clock.Now()
		delete(m.owners, rootID)
		delete(m.scopes, rootID)
		delete(m.abortRequested, rootID)
	}
	m.cancelRequestsLocked(func(*lockRequest) bool { return true }, RequestCanceled)
	m.locks = newLockTable()
	m.waiting = make(map[*requestBundle]bool)
	m.graph.clear()
	m.deadlocked = make(map[string]bool)
	m.ledger.ReleaseAll()
}

// Closed reports whether the manager was closed.
func (m *manager) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Stats returns the aggregate diagnostics view.
//
// It reads the manager registry and the ledger through their own locks and never
// holds one while taking the other, so a diagnostics call can never block a lock
// operation or vice versa.
func (m *manager) Stats() Stats {
	m.mu.Lock()
	m.ensureGraphLocked()
	owners := len(m.owners)
	rootIDs := make([]string, 0, owners)
	for rootID := range m.owners {
		rootIDs = append(rootIDs, rootID)
	}
	m.ensureGraphLocked()
	revision := m.graph.Revision()
	// A "waiter" is one queued request, while the pending index is keyed by component
	// entry, so the distinct bundles are counted rather than the entries.
	bundles := make(map[*requestBundle]bool)
	for _, requests := range m.pending {
		for entry := range requests {
			bundles[entry.bundle] = true
		}
	}
	waiters := len(bundles)
	// The edge count is the graph's own size, so a statistics reader and a graph reader cannot
	// disagree about how many edges exist.
	edgeCount := m.graph.Size()
	timeouts := m.timeouts
	deadlocks := m.deadlocks
	invalidations := m.invalidated
	m.mu.Unlock()

	totals := m.ledger.Totals()
	granted := int64(0)
	for _, rootID := range rootIDs {
		granted += m.ledger.GrantedComponents(rootID)
	}
	return Stats{
		ActiveOwners:       uint64(owners),
		GrantedComponents:  uint64(granted),
		PendingRequests:    uint64(waiters),
		WaitEdges:          uint64(edgeCount),
		MemoryBytes:        uint64(totals.MemoryBytes),
		Timeouts:           timeouts,
		Deadlocks:          deadlocks,
		EpochInvalidations: invalidations,
		Revision:           revision,
		ResourceRejections: uint64(m.ledger.Rejections()),
	}
}
