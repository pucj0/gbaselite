package storageengine

import (
	"context"
	"errors"
	"time"
)

// This file defines the neutral, optional capability surface for blocking row and
// range locks plus locked current reads. It is additive only:
//
//   - Engine and Txn keep their existing minimal method sets, so every existing
//     backend, wrapper and test fixture stays valid without a lock implementation.
//   - Absence of a capability is reported as ErrUnsupported, never as an empty
//     result: an empty lock list means "no locks", not "locks are unsupported".
//   - No type here references a concrete lock manager, MVCC store or SQL encoding.
//     SQL composes these DTOs; the backend adapter translates them.
//
// Lock state is in-memory coordination state. Nothing in this file is persisted,
// so no lock field may reach a stage codec, WAL frame, Raft command or backup.
var (
	// ErrLockWaitTimeout reports that a lock request exceeded its wait deadline.
	// MySQL maps it to 1205 / HY000.
	ErrLockWaitTimeout = errors.New("lock wait timeout exceeded")
	// ErrDeadlock reports that the request was chosen as a deadlock victim.
	// MySQL maps it to 1213 / 40001.
	ErrDeadlock = errors.New("deadlock found when trying to get lock; retry transaction")
	// ErrLockResourceLimit reports that a lock budget, ordinal or graph traversal
	// bound was exhausted. It is a resource failure and is never reported as a
	// deadlock, so callers cannot mistake a budget for a detected cycle.
	ErrLockResourceLimit = errors.New("lock resource limit exceeded")
	// ErrLockInvalidated reports that a grant or capability became unusable
	// because the owning engine generation, leader term or engine lifetime ended.
	// Stale handles fail with this error instead of observing a newer generation.
	ErrLockInvalidated = errors.New("lock owner or grant invalidated")
)

// LockMode is the requested or held mode of one lock component.
//
// The set is closed and deliberately small: B02 has no table S/X, no SIX and no
// automatic table escalation, so a mode outside this list is a programming error
// rather than a capability that silently degrades.
type LockMode string

const (
	// LockModeIS is a shared table-level intention. Record S requires it.
	LockModeIS LockMode = "IS"
	// LockModeIX is an exclusive table-level intention. Record X and an insert
	// intention require it; IX also satisfies this owner's need for IS.
	LockModeIX LockMode = "IX"
	// LockModeS is a shared record lock.
	LockModeS LockMode = "S"
	// LockModeX is an exclusive record lock.
	LockModeX LockMode = "X"
	// LockModeGap is a stable interval lock. It has no S/X strength: a gap blocks
	// the creation of a key inside its bounds and does not protect an existing
	// value.
	LockModeGap LockMode = "GAP"
	// LockModeInsertIntention is the intention to insert at one exact key.
	LockModeInsertIntention LockMode = "INSERT_INTENTION"
)

// Valid reports whether the mode is one this contract defines.
func (m LockMode) Valid() bool {
	switch m {
	case LockModeIS, LockModeIX, LockModeS, LockModeX, LockModeGap, LockModeInsertIntention:
		return true
	default:
		return false
	}
}

// Intention reports whether the mode is a table-level intention.
func (m LockMode) Intention() bool { return m == LockModeIS || m == LockModeIX }

// Record reports whether the mode protects one record.
func (m LockMode) Record() bool { return m == LockModeS || m == LockModeX }

// Interval reports whether the mode protects an interval or an insertion point.
func (m LockMode) Interval() bool { return m == LockModeGap || m == LockModeInsertIntention }

// LockComponentKind classifies one neutral lock identity.
type LockComponentKind string

const (
	// LockComponentTableIntent identifies one table by an immutable table ID.
	LockComponentTableIntent LockComponentKind = "TABLE_INTENT"
	// LockComponentRecord identifies one point key inside a byte namespace.
	LockComponentRecord LockComponentKind = "RECORD"
	// LockComponentInterval identifies one byte interval inside a namespace.
	LockComponentInterval LockComponentKind = "INTERVAL"
)

// Valid reports whether the kind is one this contract defines.
func (k LockComponentKind) Valid() bool {
	switch k {
	case LockComponentTableIntent, LockComponentRecord, LockComponentInterval:
		return true
	default:
		return false
	}
}

// LockComponent is one neutral lock identity: a mode applied to either a table, a
// point key or an interval in a byte namespace.
//
// Identity must match how the write path already addresses bytes, so a raw write
// and a SQL write to the same row converge on one resource instead of two:
//
//   - Space is the real logical namespace the backend uses (for example the row
//     or index keyspace). A SQL table or index name is never a substitute.
//   - Key, Lower and Upper are bytewise bounds in that namespace, compared with
//     the existing unsigned lexicographic order.
//   - TableID and IndexID are immutable SQL metadata labels. The lock manager
//     treats them as opaque tags; it never parses or re-encodes a key.
//
// A nil bound means infinity. An empty non-nil slice is a valid byte bound, so 0
// and "" are never used to mean unbounded.
//
// Every byte slice in a component is owned by the request: the lock manager copies
// what it retains and the caller must not mutate a slice it handed over.
type LockComponent struct {
	Mode  LockMode
	Kind  LockComponentKind
	Space string

	// Key is the exact point key of a RECORD component and the exact insertion key
	// of an INSERT_INTENTION component.
	Key []byte
	// Lower and Upper bound an INTERVAL component. A nil bound is infinite.
	Lower, Upper                   []byte
	LowerInclusive, UpperInclusive bool

	// TableID and IndexID are optional SQL metadata labels for diagnostics and
	// namespace binding. They never change Space, Key or the bounds.
	TableID string
	IndexID string
}

// LockRequest is one atomic bundle of lock components.
//
// Atomicity: the whole bundle is either granted, or it becomes exactly one pending
// request. A partially granted bundle that already granted some of its components
// is an implementation error, not a state a caller has to handle.
//
// Wait-or-fail is the caller's decision, not the manager's. ShouldWait is consulted
// once for a conflict with another owner: returning false turns the request into an
// immediate ErrConflict instead of a wait. A request never calls ShouldWait for a
// conflict with its own owner, because re-entrancy by an ancestor or child of the
// same root is always compatible.
type LockRequest struct {
	Components []LockComponent
	ShouldWait func(context.Context) bool
}

// LockGrant is the opaque handle of one granted bundle.
//
// It is a capability, not a durable receipt: it becomes unusable once its owner
// root finishes or the engine generation or leader term changes. OwnerID is the
// root that owns every grant in the bundle, and ScopeID records the scope the
// request came from without transferring ownership to it.
//
// Components and the generation/term pair are part of the capability rather than
// decoration. `ReadCurrent` must prove that the grant covers the key being read,
// that it belongs to the reading transaction's root and that it was issued for the
// generation the root still lives in; a reduced grant carrying only an owner ID
// would let a caller authorise a current read of a row it never locked, and the
// observation that read records would then become that transaction's write
// baseline. A caller must therefore pass back the grant it received, unchanged.
//
// It carries no expiry of its own: the wait bound belongs to the request and its
// scope (see the lock manager's diagnostics), and a grant lives until its owner
// root ends. An earlier revision of this DTO carried a Deadline that was filled
// with the instant of granting, which read as an expiry that had already passed.
type LockGrant struct {
	ID      uint64
	OwnerID string
	ScopeID uint64
	Granted time.Time
	// Generation is the engine generation the grant was issued under. A grant from
	// an ended generation is refused rather than honoured.
	Generation uint64
	// Term is the leader term the grant was issued under, so a term change
	// invalidates old grants instead of leaving them usable across leaders.
	Term uint64
	// Components are the resources the grant covers, each with the mode the owner
	// holds on it. A locked current read succeeds only when one record component
	// covers the exact space and key it is about to read.
	Components []LockComponent
}

// CurrentObservation is one locked current read result.
//
// It is deliberately separate from the snapshot read path: observing a newer
// committed version never changes the transaction's read timestamp, never creates
// a commit dependency and never relaxes the existing write conflict rule.
//
// ChangeTS is the sequence of the committed change that produced Value or the
// tombstone, and HasChange disambiguates the legal ChangeTS=0. Source records where
// the returned state came from, which keeps "never existed" distinguishable from
// "deleted".
type CurrentObservation struct {
	Value     []byte
	Exists    bool
	ChangeTS  uint64
	HasChange bool
	Source    CurrentObservationSource
	Tombstone bool
}

// CurrentObservationSource is the origin of a locked current observation.
type CurrentObservationSource string

const (
	// CurrentSourceCommitted is the latest published version in the storage layer.
	CurrentSourceCommitted CurrentObservationSource = "COMMITTED"
	// CurrentSourceOverlay is a write of the reading transaction's own layer or an
	// ancestor layer. It has no durable commit sequence of its own.
	CurrentSourceOverlay CurrentObservationSource = "OWN_OVERLAY"
	// CurrentSourceAbsent means no version and no overlay write was found.
	CurrentSourceAbsent CurrentObservationSource = "ABSENT"
)

// CurrentScanRequest selects the locked current-read range of one namespace.
//
// The lock plan is supplied by the caller: a backend must not invent an interval
// it cannot prove, and it must not silently fall back to an unlocked snapshot
// scan. A nil plan means the caller asked for a plain current read of the range.
type CurrentScanRequest struct {
	Space string
	Range KeyRange
	Limit int
	// Plan is an opaque, caller-owned description of the intended lock coverage.
	// The neutral layer carries it unchanged so a backend or adapter can interpret
	// it without this package depending on SQL planning types.
	Plan LockPlan
}

// LockPlan describes the coverage a locked read or write intends to protect.
//
// It exists so EXPLAIN and diagnostics can distinguish an exact record plan from a
// conservative fallback without the neutral layer having to re-derive SQL bounds.
type LockPlan struct {
	// Kind is a short stable label such as "record", "range" or "fallback".
	Kind string
	// Index is the SQL index label the plan was proved against, if any.
	Index string
	// Proven reports whether the bounds were proved to match the stored byte order.
	// A false value means the coverage is a documented conservative fallback.
	Proven bool
	// Components are the lock components the plan intends to acquire.
	Components []LockComponent
}

// LockingTxn is the optional per-transaction lock capability.
//
// The receiver of ReadCurrent is the read layer: a child may observe its own and
// its ancestors' writes, while a parent must never observe an unmerged child.
// CloseLockScope cancels the scope's pending requests and never releases a grant,
// because every grant belongs to the root.
//
// All methods must be safe to call from the single goroutine that owns the
// transaction. Implementations must not hold a storage view, cursor or commit gate
// while a request waits.
type LockingTxn interface {
	// BeginLockScope opens a request scope owned by this transaction's root.
	BeginLockScope(ctx context.Context, kind LockScopeKind) (uint64, error)
	// Acquire requests one atomic bundle. It returns a grant, or a neutral error
	// such as ErrLockWaitTimeout, ErrDeadlock, ErrLockResourceLimit, ErrConflict,
	// ErrLockInvalidated or ErrUnsupported.
	Acquire(ctx context.Context, scope uint64, request LockRequest) (LockGrant, error)
	// ReadCurrent returns the latest published state for one key after the caller
	// holds a covering grant.
	ReadCurrent(ctx context.Context, grant LockGrant, space string, key []byte) (CurrentObservation, error)
	// NewCurrentIterator opens a bounded locked scan. The iterator follows the
	// existing borrowed-bytes and Close contract.
	NewCurrentIterator(ctx context.Context, scope uint64, request CurrentScanRequest) (Iterator, error)
	// CloseLockScope cancels this scope's pending requests and keeps every grant.
	CloseLockScope(ctx context.Context, scope uint64) error
}

// LockScopeKind classifies why a scope exists. It is diagnostics and request
// origin only: a scope never owns a grant.
type LockScopeKind string

const (
	// LockScopeStatement is one SQL statement or one autocommit statement.
	LockScopeStatement LockScopeKind = "statement"
	// LockScopeChild is one child or savepoint layer.
	LockScopeChild LockScopeKind = "child"
	// LockScopeSavepoint is one named savepoint layer.
	LockScopeSavepoint LockScopeKind = "savepoint"
)

// Valid reports whether the scope kind is one this contract defines.
func (k LockScopeKind) Valid() bool {
	switch k {
	case LockScopeStatement, LockScopeChild, LockScopeSavepoint:
		return true
	default:
		return false
	}
}

// OpaqueResourceID identifies one lock resource in diagnostics without exposing
// the key. It must not be a guessable hash of a primary key, and it is only
// meaningful for the engine generation that produced it.
type OpaqueResourceID uint64

// LockRow is one granted lock of the diagnostics snapshot.
type LockRow struct {
	GrantID     uint64
	RootID      string
	ScopeID     uint64
	OriginTxnID string
	Generation  uint64
	Term        uint64
	Mode        LockMode
	Kind        LockComponentKind
	Resource    OpaqueResourceID
	GrantedAt   time.Time
}

// WaitRow is one pending lock request of the diagnostics snapshot.
type WaitRow struct {
	RequestID    uint64
	RootID       string
	ScopeID      uint64
	Mode         LockMode
	Resource     OpaqueResourceID
	BlockerRoots []string
	Reason       string
	WaitMillis   uint64
	Deadline     time.Time
}

// DeadlockRow is one retained deadlock summary.
type DeadlockRow struct {
	DetectedOrdinal uint64
	GraphRevision   uint64
	VictimRootID    string
	CycleRootIDs    []string
	Resources       []OpaqueResourceID
	Modes           []LockMode
	Truncated       bool
}

// LockStats is the aggregate diagnostics view of one lock manager.
//
// The lifetime counters are process-local observations: they are never persisted
// and never carry keys, values or SQL text.
type LockStats struct {
	ActiveOwners       uint64
	GrantedComponents  uint64
	PendingRequests    uint64
	WaitEdges          uint64
	MemoryBytes        uint64
	Timeouts           uint64
	Deadlocks          uint64
	ResourceRejections uint64
	EpochInvalidations uint64
}

// LockDiagnosticsPage is one bounded diagnostics page.
//
// SnapshotRevision identifies the manager state the page was taken from. A cursor
// is only valid for the revision that returned it, so a caller that sees a
// different revision must re-read instead of mixing two versions of the graph.
type LockDiagnosticsPage struct {
	SnapshotRevision uint64
	Limit            int
	Truncated        bool
	NextCursor       string
}

// LockPage is one page of granted locks. A nil Rows slice is an empty page, which
// is only meaningful when the capability exists.
type LockPage struct {
	LockDiagnosticsPage
	Rows []LockRow
}

// WaitPage is one page of pending lock requests.
type WaitPage struct {
	LockDiagnosticsPage
	Rows []WaitRow
}

// DeadlockPage is one page of retained deadlock summaries.
type DeadlockPage struct {
	LockDiagnosticsPage
	Rows []DeadlockRow
}

// LockDiagnostics is the optional lock diagnostics capability.
//
// Implementations must not hold a lock-manager latch while producing a page, must
// not wait for a user transaction to progress, and must never return a raw space,
// key, bound, value or SQL text. Pages are bounded: MaxLockDiagnosticRows is the
// largest page a caller may receive, and a larger Limit is truncated and flagged
// rather than served.
//
// Absence of this capability is reported by the caller as ErrUnsupported. An empty
// page must never be used to mean the capability is missing.
type LockDiagnostics interface {
	LocksPage(cursor string, limit int) (LockPage, error)
	WaitPage(cursor string, limit int) (WaitPage, error)
	DeadlocksPage(cursor string, limit int) (DeadlockPage, error)
	LockStats() (LockStats, error)
}

// LockingEngine is the optional engine-level lock capability.
//
// It carries no locking method of its own: a lock belongs to a transaction's owner
// root, never to the engine. The interface exists so a caller can advertise or refuse
// "this backend can lock" without probing a transaction first.
type LockingEngine interface {
	// Locking reports whether this engine can serve lock requests. A backend that
	// returns false must return ErrUnsupported from every locking path instead of
	// degrading to an unlocked read.
	Locking() bool
}

// LockingTxnOf returns the optional lock capability of one transaction.
//
// It reports ErrUnsupported when the backend has no lock implementation at all, so a
// caller can never mistake "no capability" for "no locks currently held".
func LockingTxnOf(tx Txn) (LockingTxn, error) {
	if tx == nil {
		return nil, ErrUnsupported
	}
	locking, ok := tx.(LockingTxn)
	if !ok {
		return nil, ErrUnsupported
	}
	return locking, nil
}

// LockDiagnosticsOf returns the optional lock diagnostics capability of one engine.
func LockDiagnosticsOf(engine Engine) (LockDiagnostics, error) {
	if engine == nil {
		return nil, ErrUnsupported
	}
	diagnostics, ok := engine.(LockDiagnostics)
	if !ok {
		return nil, ErrUnsupported
	}
	return diagnostics, nil
}

// MaxLockDiagnosticRows is the largest diagnostics page any backend may return.
// A request for more is truncated to this and reported through Truncated.
const MaxLockDiagnosticRows = 256
