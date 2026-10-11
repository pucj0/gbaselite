// Package lockmanager provides the neutral, in-process lock coordination model for
// B02: owners, scopes, resources, requests, grants and observations.
//
// It is deliberately dependency-free apart from the Go standard library. It must
// not import the MVCC store, the SQL executor, a storage backend or bbolt, so the
// SQL layer can keep using only storageengine DTOs while the backend adapter is
// the single place that composes this package with MVCC.
//
// Nothing here is persisted. No lock, owner, scope, grant or wait edge may reach a
// stage codec, WAL frame, Raft command, backup or restored image.
//
// Concurrency contract: the manager that owns these values is safe for concurrent
// use, and every exported value type here is safe to copy. The one documented
// exception is the mutable manager state, which is guarded inside manager.go. A
// single Txn remains a single caller; nothing in this package calls a transaction
// method, so a detection or diagnostics goroutine can never touch a write set.
package lockmanager

import (
	"errors"
	"time"
)

// Sentinel errors. The adapter maps each to the matching neutral
// storageengine error; this package must not import that one.
var (
	// ErrOwnerExists reports a duplicate root registration.
	ErrOwnerExists = errors.New("lock owner already registered")
	// ErrOwnerUnknown reports an operation on an unregistered or ended owner.
	ErrOwnerUnknown = errors.New("lock owner is not registered")
	// ErrScopeUnknown reports an operation on an unregistered or closed scope.
	ErrScopeUnknown = errors.New("lock scope is not open")
	// ErrScopeOwnerMismatch reports a scope that belongs to another owner.
	ErrScopeOwnerMismatch = errors.New("lock scope belongs to another owner")
	// ErrOrdinalExhausted reports that the registration ordinal or an ID counter
	// cannot advance further. It fails closed instead of wrapping.
	ErrOrdinalExhausted = errors.New("lock ordinal exhausted")
	// ErrInvalidResource reports a resource identity that is not usable: an unknown
	// mode or kind, a missing or non-canonical space, a table intent without a table
	// ID, a record without a key, or an inverted interval.
	ErrInvalidResource = errors.New("invalid lock resource")
	// ErrBudgetMemory reports that copied lock metadata would exceed its memory
	// budget.
	ErrBudgetMemory = errors.New("lock memory budget exceeded")
	// ErrBudgetRootRequests reports that one root would exceed its component budget.
	ErrBudgetRootRequests = errors.New("lock request budget exceeded for this transaction")
	// ErrBudgetWaiters reports that the engine waiter budget is full.
	ErrBudgetWaiters = errors.New("lock waiter budget exceeded")
	// ErrBudgetGraph reports that the wait-graph edge or traversal budget is full.
	ErrBudgetGraph = errors.New("lock graph budget exceeded")
	// ErrInvalidOptions reports a manager option outside its legal range.
	ErrInvalidOptions = errors.New("invalid lock manager options")
	// ErrTableIntentRequired reports a record or interval component whose bundle does
	// not carry the table intention the contract requires. It is separate from
	// ErrInvalidResource because the component itself can be well formed: what is
	// missing is the containing bundle's intention.
	ErrTableIntentRequired = errors.New("record lock requires a table intention in the same bundle")
	// ErrInsertIntentionRequiresRecord reports an Insert Intention whose bundle does not also
	// take the exclusive record lock of the same key. The two are one insertion claim: the
	// intention is what a covering Gap blocks and the record lock is what decides same-key
	// contention, so a bundle carrying only half of it is refused rather than granted with a
	// bypass.
	ErrInsertIntentionRequiresRecord = errors.New("insert intention requires the record lock of its own key in the same bundle")
	// ErrUnsupportedMode reports a mode outside the B02 set, such as a table S/X or
	// SIX. It fails closed instead of being approximated by an intention lock.
	ErrUnsupportedMode = errors.New("lock mode is not supported by B02")
	// ErrOwnerEnding reports a request from a root whose outcome is already decided.
	ErrOwnerEnding = errors.New("lock owner may no longer request locks")
	// ErrConflict reports that a request conflicts with another owner's grant and its
	// caller chose not to wait. It is the fail-fast outcome; the adapter maps it to the
	// neutral storageengine conflict error.
	ErrConflict = errors.New("lock request conflicts with a held lock")
	// ErrLockWaitTimeout reports that a queued request exceeded its wait deadline. The
	// adapter maps it to the neutral lock-timeout error (MySQL 1205).
	ErrLockWaitTimeout = errors.New("lock wait timeout exceeded")
	// ErrDeadlock reports that the request was chosen as a deadlock victim. The adapter
	// maps it to the neutral deadlock error (MySQL 1213).
	ErrDeadlock = errors.New("deadlock found when trying to get lock; retry transaction")
	// ErrLockResourceLimit reports that a lock budget, ordinal or graph traversal bound was
	// exhausted. It is a resource failure and is never reported as a deadlock.
	ErrLockResourceLimit = errors.New("lock resource limit exceeded")
	// ErrLockInvalidated reports that a grant or pending request became unusable because
	// the owning engine generation or leader term ended.
	ErrLockInvalidated = errors.New("lock owner or grant invalidated")
)

// LockState is the observable state of one lock resource.
//
// It is a projection of the holders and the waiters, not independent state: a resource
// is SHARED when every holder is shared, EXCLUSIVE when any holder is exclusive, and
// WAITING when at least one request is queued for it. It exists so a test, EXPLAIN or
// a diagnostics page can name what is happening on a resource in one value.
type LockState string

const (
	// LockStateFree has no holder and no waiter.
	LockStateFree LockState = "FREE"
	// LockStateShared is held by shared holders only.
	LockStateShared LockState = "SHARED"
	// LockStateExclusive is held exclusively.
	LockStateExclusive LockState = "EXCLUSIVE"
	// LockStateWaiting has at least one queued request. A resource can be WAITING and
	// held at the same time, so this state is reported in preference to the held state
	// whenever a request is queued.
	LockStateWaiting LockState = "WAITING"
)

// Describe projects a resource's holders and waiter count onto one state.
//
// A held resource with a queue is reported as WAITING, because a caller that only sees
// the held state would miss that a request is blocked behind it.
func Describe(holders int, holderMode Mode, waiters int) LockState {
	if waiters > 0 {
		return LockStateWaiting
	}
	if holders == 0 {
		return LockStateFree
	}
	if holderMode == ModeX {
		return LockStateExclusive
	}
	return LockStateShared
}

// OwnerState is the lifecycle state of one lock owner.
//
// It is intentionally a distinct type from the neutral transaction state: the lock
// owner tracks only whether the root may still request locks, whether its abort was
// requested, and whether its grants were released. It makes no claim about
// visibility or durability, and it never substitutes for the transaction state.
type OwnerState string

// AbortReason records why a root was asked to stop.
//
// The two causes are not interchangeable: a deadlock victim retries the transaction, while an
// invalidated epoch means the handle belongs to a generation that no longer exists. A caller that
// could not tell them apart would report the wrong retry instruction.
type AbortReason string

const (
	// AbortDeadlock means a detector chose this root as a cycle victim.
	AbortDeadlock AbortReason = "DEADLOCK"
	// AbortEpoch means a generation reset or a term change invalidated this root.
	AbortEpoch AbortReason = "EPOCH"
)

const (
	// OwnerActive accepts new lock requests and grant upgrades.
	OwnerActive OwnerState = "ACTIVE"
	// OwnerAbortRequested means a detector or an early conflict asked this root to
	// stop. New requests are refused; existing grants are kept until release.
	OwnerAbortRequested OwnerState = "ABORT_REQUESTED"
	// OwnerFinishing means the root's outcome is decided and no further write or
	// merge may use this owner.
	OwnerFinishing OwnerState = "FINISHING"
	// OwnerEnded means every grant and edge was released. Release is idempotent.
	OwnerEnded OwnerState = "ENDED"
)

// CanRequest reports whether the owner may still request a new lock.
func (s OwnerState) CanRequest() bool { return s == OwnerActive }

// ScopeKind classifies why a lock scope exists. A scope records where a request
// came from; it never owns a grant.
type ScopeKind string

const (
	// ScopeStatement is one SQL statement, including an autocommit statement.
	ScopeStatement ScopeKind = "statement"
	// ScopeChild is one child or savepoint layer transaction.
	ScopeChild ScopeKind = "child"
	// ScopeSavepoint is one named savepoint layer.
	ScopeSavepoint ScopeKind = "savepoint"
)

// Valid reports whether the kind is one this model defines.
func (k ScopeKind) Valid() bool {
	switch k {
	case ScopeStatement, ScopeChild, ScopeSavepoint:
		return true
	default:
		return false
	}
}

// ScopeState is the lifecycle state of one lock scope.
type ScopeState string

const (
	// ScopeOpen accepts new requests.
	ScopeOpen ScopeState = "OPEN"
	// ScopeClosing is closing: new requests are refused and pending requests are
	// being cancelled. Grants are never touched.
	ScopeClosing ScopeState = "CLOSING"
	// ScopeClosed is closed. A closed scope keeps its identity for diagnostics, so a
	// historical origin scope is never mistaken for a live registry entry.
	ScopeClosed ScopeState = "CLOSED"
)

// RequestStatus is the single terminal or pending status of one lock request.
//
// The transition is one-way: PENDING becomes exactly one terminal status, and a
// terminal request never returns to PENDING. That is what makes "grant, cancel and
// deadline all fired together" observable as one outcome.
type RequestStatus string

const (
	// RequestPending is waiting for its blockers.
	RequestPending RequestStatus = "PENDING"
	// RequestGranted holds its components.
	RequestGranted RequestStatus = "GRANTED"
	// RequestTimedOut exceeded its wait deadline.
	RequestTimedOut RequestStatus = "TIMED_OUT"
	// RequestContextCanceled is a request whose caller's context ended.
	//
	// It is separate from RequestCanceled because the two outcomes have different owners: an
	// explicit cancellation is the statement giving up, while a context cancellation is the caller
	// going away. Both must withdraw the request from its queue, and a caller that later asks what
	// happened has to be able to tell them apart.
	RequestContextCanceled RequestStatus = "CONTEXT_CANCELED"
	// RequestCanceled was cancelled by its owner, scope or caller.
	RequestCanceled RequestStatus = "CANCELED"
	// RequestDeadlockVictim was chosen as a deadlock victim.
	RequestDeadlockVictim RequestStatus = "DEADLOCK_VICTIM"
	// RequestInvalidated lost its engine generation or leader term.
	RequestInvalidated RequestStatus = "INVALIDATED"
	// RequestResourceRejected exceeded a lock budget.
	RequestResourceRejected RequestStatus = "RESOURCE_REJECTED"
)

// Terminal reports whether no further status change is allowed.
func (s RequestStatus) Terminal() bool { return s != RequestPending }

// ResourceKind classifies one lock identity.
type ResourceKind string

const (
	// KindTableIntent identifies one table by an immutable table ID.
	KindTableIntent ResourceKind = "TABLE_INTENT"
	// KindRecord identifies one point key in a byte namespace.
	KindRecord ResourceKind = "RECORD"
	// KindInterval identifies one byte interval in a byte namespace.
	KindInterval ResourceKind = "INTERVAL"
)

// Valid reports whether the kind is one this model defines.
func (k ResourceKind) Valid() bool {
	switch k {
	case KindTableIntent, KindRecord, KindInterval:
		return true
	default:
		return false
	}
}

// Mode is the requested or held mode of one lock component.
type Mode string

const (
	// ModeIS is a shared table-level intention; record S requires it.
	ModeIS Mode = "IS"
	// ModeIX is an exclusive table-level intention; record X and an insert
	// intention require it, and IX also satisfies this owner's need for IS.
	ModeIX Mode = "IX"
	// ModeS is a shared record lock.
	ModeS Mode = "S"
	// ModeX is an exclusive record lock.
	ModeX Mode = "X"
	// ModeGap is a stable interval lock. It has no S/X strength.
	ModeGap Mode = "GAP"
	// ModeInsertIntention is the intention to insert at one exact key.
	ModeInsertIntention Mode = "INSERT_INTENTION"
)

// Valid reports whether the mode is one this model defines.
func (m Mode) Valid() bool {
	switch m {
	case ModeIS, ModeIX, ModeS, ModeX, ModeGap, ModeInsertIntention:
		return true
	default:
		return false
	}
}

// Intention reports whether the mode is a table-level intention.
func (m Mode) Intention() bool { return m == ModeIS || m == ModeIX }

// Record reports whether the mode protects one record.
func (m Mode) Record() bool { return m == ModeS || m == ModeX }

// Interval reports whether the mode protects an interval or insertion point.
func (m Mode) Interval() bool { return m == ModeGap || m == ModeInsertIntention }

// Identity is the stable identity of one lock resource inside one engine
// generation.
//
// It matches how the write path addresses bytes, so a raw write and a SQL write to
// the same row converge on one resource:
//
//   - Space is the real logical namespace. A SQL table or index name is never a
//     substitute, and TableID is an opaque label it must not change.
//   - Key, Lower and Upper are compared with the existing unsigned byte
//     lexicographic order. A nil bound is infinite; an empty non-nil slice is a
//     valid bound. Zero and "" never mean unbounded.
//
// The zero Identity is invalid; Validate reports why.
type Identity struct {
	Generation uint64
	Space      string
	TableID    string
	IndexID    string
	Kind       ResourceKind

	// Key is the point key of a RECORD and the insertion key of an
	// INSERT_INTENTION.
	Key []byte
	// Lower and Upper bound an INTERVAL.
	Lower, Upper                   []byte
	LowerInclusive, UpperInclusive bool

	// Mode is not part of the identity. It is carried alongside so a caller can
	// request and compare modes for the same resource.
	Mode Mode
}

// ResourceRef is one listed component of a request, grant or wait edge.
//
// It embeds Identity by value and owns its byte slices: reading a ResourceRef never
// exposes a mutable internal slice.
type ResourceRef struct {
	Identity
}

// OwnedBytes reports the accounted size of this reference: the copied identity
// bytes the lock manager retains for it. It is the exact quantity the memory
// budget charges, so accounting never depends on a platform struct size.
func (r ResourceRef) OwnedBytes() int { return int(r.OwnedBytes64()) }

// OwnedBytes64 is OwnedBytes in the int64 the budget arithmetic uses.
func (r ResourceRef) OwnedBytes64() int64 {
	n := int64(len(r.Space) + len(r.TableID) + len(r.IndexID) + len(r.Key) + len(r.Lower) + len(r.Upper))
	return n + resourceFixedBytes
}

const (
	// resourceFixedBytes is the fixed per-reference overhead the memory budget
	// charges. It stands in for the identity fields, the containing struct and one
	// map or ordered-set node, and it is deliberately a named constant so the
	// budget test can assert an exact total.
	resourceFixedBytes = 64
	// requestFixedBytes is the fixed per-request overhead.
	requestFixedBytes = 128
	// waiterFixedBytes is the fixed per-pending-request overhead.
	waiterFixedBytes = 64
	// grantFixedBytes is the fixed per-grant overhead.
	grantFixedBytes = 64
	// edgeFixedBytes is the fixed per-wait-edge overhead.
	edgeFixedBytes = 48
	// scopeFixedBytes is the fixed per-scope overhead.
	scopeFixedBytes = 96
	// ownerFixedBytes is the fixed per-owner overhead.
	ownerFixedBytes = 128
)

// Scope is one request origin inside an owner. A scope is never a lock owner: it
// records where a request came from and whether it may still issue one.
//
// ParentScopeID links a child or savepoint scope to the scope it nests in. A
// zero ParentScopeID is a root scope of its owner.
type Scope struct {
	ID              uint64
	OwnerID         string
	OriginTxnID     string
	ParentScopeID   uint64
	Kind            ScopeKind
	State           ScopeState
	OpenedAt        time.Time
	ContextDeadline time.Time
	// WaitBudget is the bound of *one* wait issued from this scope, as a duration. It is the
	// session's `innodb_lock_wait_timeout`, and it is deliberately a duration rather than an
	// absolute deadline: the bound applies to each request when it starts waiting, so a statement
	// that waits twice for less than the budget each time stays inside it. ContextDeadline caps the
	// whole scope instead, so an earlier query deadline still ends every wait.
	//
	// Zero means "use the manager's default budget".
	WaitBudget time.Duration
}

// CanRequest reports whether the scope may issue a new request.
func (s Scope) CanRequest() bool { return s.State == ScopeOpen }

// LockRequest is one atomic bundle request.
//
// A request is either granted as a whole or becomes exactly one pending request; a
// partially granted bundle is an implementation error, not a state a caller has to
// handle. BlockerIDs is the initial blocker footprint; the graph may add or remove
// edges as the wait progresses.
type LockRequest struct {
	ID             uint64
	OwnerID        string
	ScopeID        uint64
	Components     []ResourceRef
	EnqueueOrdinal uint64
	RequestedAt    time.Time
	Deadline       time.Time
	// BlockedAt is when the request started waiting, which is when it was queued. It is the
	// origin of the reported wait duration and is set once, from the manager's clock, so the
	// duration a diagnostics page reports is derived from the same clock that decides timeouts.
	BlockedAt time.Time
	// Status is the request's lifecycle status. The authoritative copy lives in the request's
	// shared decision (requestState), which is what waiters observe; this field is kept for
	// diagnostics and is only ever written under the manager mutex.
	Status RequestStatus
	// ShouldWait is consulted once for a conflict with another owner. It is never
	// consulted for a conflict inside the same owner, because an ancestor or child
	// of one root always re-enters compatibly.
	ShouldWait    func() bool
	BlockerIDs    []string
	ReservedBytes int64
	// ReservationToken identifies the budget reservation that currently owns this
	// request's accounting. Zero means nothing is reserved.
	ReservationToken uint64
}

// There is deliberately no per-request result channel here.
//
// A request can have several waiting callers (the one that queued it and every caller that joined
// it), and a channel can hand its single value to exactly one receiver: the others would stay
// blocked for an outcome that was already decided. Wake-up is therefore a broadcast owned by the
// request's shared decision — a channel that is closed once, when the terminal status is stored —
// and every caller reads the same status and the same grant from that decision afterwards.

// MayWait reports whether the request is allowed to queue.
//
// A nil decision means "yes", so the default is to wait; a caller that wants a
// fail-fast lock supplies a decision that returns false. The decision is consulted
// exactly once, when a conflict is first found, and never for a conflict inside the
// request's own owner: an ancestor or child of one root always re-enters compatibly.
func (r *LockRequest) MayWait() bool {
	if r.ShouldWait == nil {
		return true
	}
	return r.ShouldWait()
}

// LockGrant is one granted bundle. Grants belong to the owner root; the origin
// scope is recorded for diagnostics only and never owns a grant.
type LockGrant struct {
	GrantID       uint64
	OwnerID       string
	OriginScopeID uint64
	Generation    uint64
	Term          uint64
	Components    []ResourceRef
	GrantedAt     time.Time
}

// Observation is one locked current read result.
//
// It never changes the reading transaction's read timestamp, never creates a commit
// dependency and never relaxes the write conflict rule. ChangeTS is the sequence of
// the committed change behind the value or tombstone, and HasChange disambiguates
// the legal ChangeTS=0, so "never published" and "deleted" stay distinguishable.
type Observation struct {
	Value     []byte
	Exists    bool
	ChangeTS  uint64
	HasChange bool
	Source    ObservationSource
	Tombstone bool
}

// ObservationSource is the origin of a locked current observation.
type ObservationSource string

const (
	// SourceCommitted is the latest published version in the storage layer.
	SourceCommitted ObservationSource = "COMMITTED"
	// SourceOverlay is a write of the reading layer or one of its ancestors. It has
	// no durable commit sequence of its own.
	SourceOverlay ObservationSource = "OWN_OVERLAY"
	// SourceAbsent means neither a version nor an overlay write was found.
	SourceAbsent ObservationSource = "ABSENT"
)

// WaitEdgeReason explains why one root waits for another.
type WaitEdgeReason string

const (
	// EdgeHolder means the target holds an incompatible grant.
	EdgeHolder WaitEdgeReason = "HOLDER"
	// EdgeOlderWaiter means the target is an older incompatible waiter: a younger
	// compatible request may not overtake it.
	EdgeOlderWaiter WaitEdgeReason = "OLDER_WAITER"
)

// WaitEdge is one directed wait-graph edge, the wait-for graph's public unit.
//
// ResourceID is an *anonymous* resource identity: it is a small manager-local counter, never a
// table name, index name or encoded key, so a page built from edges cannot leak a raw key
// (FR-023).
type WaitEdge struct {
	FromRootID string
	ToRootID   string
	RequestID  uint64
	Reason     WaitEdgeReason
	ResourceID uint64
	Revision   uint64
}

// DeadlockSummary is one retained deadlock record. It is bounded: at most
// maxDeadlockSummaries are kept and at most maxDeadlockNodes nodes are recorded, and
// detection itself never depends on the summary being complete.
type DeadlockSummary struct {
	DetectedOrdinal uint64
	GraphRevision   uint64
	VictimRootID    string
	CycleRootIDs    []string
	AnonymousIDs    []uint64
	Modes           []Mode
	Truncated       bool
}

// BudgetLimits are the enforced bounds of one lock manager.
//
// A zero field means "use the package default" and never "unlimited". Negative
// values are rejected at construction.
type BudgetLimits struct {
	// MemoryBytes bounds the copied lock metadata: resource bytes, request and
	// waiter overhead, grants, edges and the bounded deadlock ring.
	MemoryBytes int64
	// RootRequests bounds the components one root may hold as grants plus pending.
	RootRequests int64
	// Waiters bounds the pending requests of one engine.
	Waiters int64
	// GraphWork bounds both the wait-edge capacity and one traversal's work.
	GraphWork int64
}

// Default budget values. The per-root component limit was calibrated against the
// existing large-transaction baseline: an UPDATE on a table with a primary key and
// one unique index needs three components per row, so 262144 keeps the existing
// 10000-row benchmark path (30000 components) and a 20000-row statement (60000) well
// inside the budget while the 64 MiB memory budget still caps the total.
const (
	DefaultMemoryBytes  int64 = 64 << 20
	DefaultRootRequests int64 = 262144
	DefaultWaiters      int64 = 4096
	DefaultGraphWork    int64 = 100000

	// MaxRootRequests and friends are the largest values a configuration may
	// request. They match the planned configuration table so a YAML, environment or
	// embedded option cannot overflow a platform int.
	MaxMemoryBytes  int64 = 1 << 40
	MaxRootRequests int64 = 1<<31 - 1
	MaxWaiters      int64 = 1<<31 - 1
	MaxGraphWork    int64 = 1<<31 - 1

	// MaxDiagnosticRows is the largest diagnostics page any manager returns.
	MaxDiagnosticRows = 256
	// maxDeadlockSummaries is the retained deadlock ring size.
	maxDeadlockSummaries = 32
	// maxDeadlockNodes is the node budget of one recorded summary.
	maxDeadlockNodes = 32
)

// Resolve fills every zero limit with its default and rejects the rest.
//
// It is the single place that decides what "0 means default" and which ranges are
// legal, so a YAML mapping, an environment override and an embedded option cannot
// disagree about it.
func (l BudgetLimits) Resolve() (BudgetLimits, error) {
	resolved := l
	if resolved.MemoryBytes == 0 {
		resolved.MemoryBytes = DefaultMemoryBytes
	}
	if resolved.RootRequests == 0 {
		resolved.RootRequests = DefaultRootRequests
	}
	if resolved.Waiters == 0 {
		resolved.Waiters = DefaultWaiters
	}
	if resolved.GraphWork == 0 {
		resolved.GraphWork = DefaultGraphWork
	}
	if resolved.MemoryBytes < 0 || resolved.RootRequests < 0 || resolved.Waiters < 0 || resolved.GraphWork < 0 {
		return BudgetLimits{}, ErrInvalidOptions
	}
	if resolved.MemoryBytes > MaxMemoryBytes || resolved.RootRequests > MaxRootRequests ||
		resolved.Waiters > MaxWaiters || resolved.GraphWork > MaxGraphWork {
		return BudgetLimits{}, ErrInvalidOptions
	}
	return resolved, nil
}

// BudgetUsage is the accounting snapshot of one owner or of the whole ledger.
type BudgetUsage struct {
	MemoryBytes int64
	Components  int64
	Waiters     int64
	GraphEdges  int64
}

// Page is the shared bound of one diagnostics page.
type Page struct {
	SnapshotRevision uint64
	Limit            int
	Truncated        bool
	NextCursor       string
}

// ResolvePageLimit clamps a requested page limit to the contract bound.
//
// A limit of zero or less means "the contract default"; anything larger than
// MaxDiagnosticRows is truncated to it and reported, never served.
func ResolvePageLimit(limit int) (int, bool) {
	if limit <= 0 {
		return MaxDiagnosticRows, false
	}
	if limit > MaxDiagnosticRows {
		return MaxDiagnosticRows, true
	}
	return limit, false
}

// Stats is the aggregate diagnostics view of one lock manager.
type Stats struct {
	ActiveOwners       uint64
	GrantedComponents  uint64
	PendingRequests    uint64
	WaitEdges          uint64
	MemoryBytes        uint64
	Timeouts           uint64
	Deadlocks          uint64
	ResourceRejections uint64
	EpochInvalidations uint64
	Revision           uint64
}
