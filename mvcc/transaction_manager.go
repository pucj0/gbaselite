package mvcc

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Registry errors. They are defensive invariants rather than SQL-visible
// failures: a caller that sees one either has a bug or hit a TxnID collision
// (FR-002), and the transaction it describes must not be silently replaced.
var (
	ErrTransactionIDCollision = errors.New("mvcc: transaction ID is already registered")
	ErrUnknownTransaction     = errors.New("mvcc: transaction is not registered")
	ErrIllegalTransactionMove = errors.New("mvcc: illegal transaction state transition")
	ErrInvalidRegistration    = errors.New("mvcc: invalid transaction registration")
)

// RootRegistration is the metadata captured when a root transaction registers.
//
// StartTS is not a field: FR-003 fixes StartTS == ReadTS for the whole lifetime,
// so the manager derives StartTS from ReadTS instead of accepting two values that
// could disagree.
type RootRegistration struct {
	ID         string
	ReadTS     uint64
	Generation uint64
	StartedAt  time.Time
}

// ChildRegistration is the metadata captured when a child or savepoint
// transaction registers for diagnostics.
//
// ReadTS is not a field either: FR-007 makes a child inherit its root's read
// timestamp, so the manager copies it from the registered parent. That makes it
// structurally impossible for a child to pin a second snapshot.
type ChildRegistration struct {
	ID         string
	ParentID   string
	Generation uint64
	StartedAt  time.Time
}

// TransitionOptions carries the optional durable outcome of a lifecycle move.
type TransitionOptions struct {
	// CommitTS is the published commit/version sequence. It is accepted only for
	// a root move into COMMITTED, which is the sole durable commit point (D5).
	CommitTS    uint64
	HasCommitTS bool
	// Conflict marks an ABORTED move caused by a commit-time conflict.
	Conflict bool
	// Reason is a short abort category. It must never carry write-set content
	// (contracts/transaction-diagnostics.md).
	Reason string
}

// TransactionInfo is the diagnostics snapshot of one registered transaction. It
// is a plain value: handing one out never exposes a manager-owned object.
//
// The counters are bounded summaries. A transaction's read activity is recorded as
// counts only -- never as a set of keys -- so this value has a fixed size no matter
// how many rows the transaction read.
type TransactionInfo struct {
	ID       string
	ParentID string

	StartTS uint64
	ReadTS  uint64

	CommitTS    uint64
	HasCommitTS bool

	State      TransactionState
	Generation uint64
	StartedAt  time.Time

	// Read observation: ordinary Get/Scan/ScanRange activity.
	PointReads    uint64
	RangeReads    uint64
	RowsObserved  uint64
	BytesObserved uint64

	// Write set: the current logical write set, not a call history.
	Writes     uint64
	WriteBytes int64

	// Dependency set: declared Guard / GuardRange dependencies.
	PointDependencies uint64
	RangeDependencies uint64

	AbortReason string
}

// TransactionStats are the manager's aggregate counters plus the current GC
// horizon. The monotonic counters are process-lifetime only and are never
// persisted.
type TransactionStats struct {
	ActiveRoot     uint64
	ActiveChildren uint64
	Committed      uint64
	Aborted        uint64
	Conflicts      uint64

	OldestReadTS    uint64
	HasOldestReadTS bool
}

// transactionCounters is the per-transaction diagnostics block.
//
// It holds bounded counters only: no keys, no values, and above all no read set.
// The transaction's own goroutine writes it and diagnostics read it from another
// goroutine, so every field is atomic and the block can be shared by the
// transaction and its registry entry without a lock on the read path.
type transactionCounters struct {
	pointReads    atomic.Uint64
	rangeReads    atomic.Uint64
	rowsObserved  atomic.Uint64
	bytesObserved atomic.Uint64

	writes            atomic.Uint64
	writeBytes        atomic.Int64
	pointDependencies atomic.Uint64
	rangeDependencies atomic.Uint64
}

func newTransactionCounters() *transactionCounters { return &transactionCounters{} }

// applyTo copies the counters into a diagnostics snapshot. It is the single reader
// of the block, shared by the registry entry and by a finished transaction's own
// snapshot.
func (c *transactionCounters) applyTo(info *TransactionInfo) {
	if c == nil {
		return
	}
	info.PointReads = c.pointReads.Load()
	info.RangeReads = c.rangeReads.Load()
	info.RowsObserved = c.rowsObserved.Load()
	info.BytesObserved = c.bytesObserved.Load()
	info.Writes = c.writes.Load()
	info.WriteBytes = c.writeBytes.Load()
	info.PointDependencies = c.pointDependencies.Load()
	info.RangeDependencies = c.rangeDependencies.Load()
}

// addObservations folds another transaction's read counters into this block.
// Merging a child into its parent means the child was reading on the parent's
// behalf.
func (c *transactionCounters) addObservations(other *transactionCounters) {
	if other == nil {
		return
	}
	c.pointReads.Add(other.pointReads.Load())
	c.rangeReads.Add(other.rangeReads.Load())
	c.rowsObserved.Add(other.rowsObserved.Load())
	c.bytesObserved.Add(other.bytesObserved.Load())
}

// addDependencies folds another transaction's declared dependencies into this
// block. Callers must not combine it with re-buffering the same operations, which
// would count the dependency twice.
func (c *transactionCounters) addDependencies(other *transactionCounters) {
	if other == nil {
		return
	}
	c.pointDependencies.Add(other.pointDependencies.Load())
	c.rangeDependencies.Add(other.rangeDependencies.Load())
}

// addWriteSet folds another transaction's write-set counters into this block. It
// is only valid when the two logical write sets are disjoint, which is exactly
// what the empty-parent merge fast path guarantees.
func (c *transactionCounters) addWriteSet(other *transactionCounters) {
	if other == nil {
		return
	}
	c.writes.Add(other.writes.Load())
	c.writeBytes.Add(other.writeBytes.Load())
}

// transactionEntry is the mutable registry record. It never leaves the manager:
// only TransactionInfo copies cross the boundary.
type transactionEntry struct {
	id       string
	parentID string
	root     bool

	startTS     uint64
	readTS      uint64
	commitTS    uint64
	hasCommitTS bool
	state       TransactionState
	generation  uint64
	startedAt   time.Time

	// counters is shared with the transaction that registered the entry. It is a
	// fixed-size block of atomics, never a collection.
	counters *transactionCounters

	abortReason string

	// pinned reports whether this entry currently owns exactly one snapshotRefs
	// reference. It flips to false exactly once, which is what makes a repeated
	// unregister or a repeated terminal move unable to underflow a reference.
	pinned bool
}

func (e *transactionEntry) info() TransactionInfo {
	info := TransactionInfo{
		ID:          e.id,
		ParentID:    e.parentID,
		StartTS:     e.startTS,
		ReadTS:      e.readTS,
		CommitTS:    e.commitTS,
		HasCommitTS: e.hasCommitTS,
		State:       e.state,
		Generation:  e.generation,
		StartedAt:   e.startedAt,
		AbortReason: e.abortReason,
	}
	if e.counters != nil {
		e.counters.applyTo(&info)
	}
	return info
}

// TransactionManager owns the B01 transaction registry: which transactions are
// registered, their lifecycle state, and the snapshot retention references that
// keep history alive for root transactions.
//
// It deliberately does not touch the MVCC version layout, the visibility rules
// or any commit path. This is the standalone foundation: the Tx lifecycle is
// wired to it in a later phase, so nothing here changes Begin/Commit/Rollback.
//
// Locking: every method holds the manager mutex for one short critical section
// and never calls back into Store. Callers may therefore use it while holding
// store locks, as long as they keep the store order apply -> views -> manager.mu.
type TransactionManager struct {
	mu           sync.RWMutex
	activeByID   map[string]*transactionEntry
	snapshotRefs map[uint64]uint64

	committed uint64
	aborted   uint64
	conflicts uint64
}

// NewTransactionManager returns an empty registry.
func NewTransactionManager() *TransactionManager {
	return &TransactionManager{
		activeByID:   make(map[string]*transactionEntry),
		snapshotRefs: make(map[uint64]uint64),
	}
}

// RegisterRoot registers a root transaction and pins its read timestamp so that
// history compaction cannot drop versions the transaction may still need
// (INV-007). A duplicate ID is rejected instead of replacing the registered
// transaction (FR-002).
//
// It returns the transaction's counters block: the caller owns the writes to that
// block, and diagnostics read it through the registry.
func (m *TransactionManager) RegisterRoot(registration RootRegistration) (*transactionCounters, error) {
	if registration.ID == "" {
		return nil, fmt.Errorf("%w: empty transaction ID", ErrInvalidRegistration)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.activeByID[registration.ID]; exists {
		return nil, fmt.Errorf("%w: %s", ErrTransactionIDCollision, registration.ID)
	}
	counters := newTransactionCounters()
	m.activeByID[registration.ID] = &transactionEntry{
		id:         registration.ID,
		root:       true,
		startTS:    registration.ReadTS,
		readTS:     registration.ReadTS,
		state:      TransactionActive,
		generation: registration.Generation,
		startedAt:  registration.StartedAt,
		counters:   counters,
		pinned:     true,
	}
	m.snapshotRefs[registration.ReadTS]++
	return counters, nil
}

// RegisterChild registers a child or savepoint transaction for diagnostics only.
// It inherits the parent's read timestamp and never pins a snapshot: the root
// already owns retention for the whole family (FR-007).
func (m *TransactionManager) RegisterChild(registration ChildRegistration) (*transactionCounters, error) {
	if registration.ID == "" {
		return nil, fmt.Errorf("%w: empty transaction ID", ErrInvalidRegistration)
	}
	if registration.ParentID == "" {
		return nil, fmt.Errorf("%w: child registration requires a parent ID", ErrInvalidRegistration)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.activeByID[registration.ID]; exists {
		return nil, fmt.Errorf("%w: %s", ErrTransactionIDCollision, registration.ID)
	}
	parent := m.activeByID[registration.ParentID]
	if parent == nil {
		return nil, fmt.Errorf("%w: parent %s", ErrUnknownTransaction, registration.ParentID)
	}
	if parent.state != TransactionActive {
		return nil, fmt.Errorf("%w: parent %s is %s, not ACTIVE", ErrInvalidRegistration, registration.ParentID, parent.state)
	}
	counters := newTransactionCounters()
	m.activeByID[registration.ID] = &transactionEntry{
		id:         registration.ID,
		parentID:   registration.ParentID,
		startTS:    parent.readTS,
		readTS:     parent.readTS,
		state:      TransactionActive,
		generation: registration.Generation,
		startedAt:  registration.StartedAt,
		counters:   counters,
	}
	return counters, nil
}

// Unregister removes a transaction from the registry and releases the snapshot
// reference it still owns. Removing a transaction also removes every registered
// descendant, because nothing built on top of a finished transaction can still be
// meaningful: a savepoint layer abandoned by ROLLBACK TO or by a statement
// boundary must not survive as an orphan.
//
// It is idempotent by design. An unknown, already released, or already removed ID
// is a no-op, so a double rollback, a failed merge that already rolled the parent
// back, and a duplicate disconnect cleanup can all call it safely without moving
// a reference below zero.
func (m *TransactionManager) Unregister(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.activeByID[id]
	if entry == nil {
		return
	}
	m.removeLocked(entry)
	m.dropDescendantsLocked(id)
}

// removeLocked deletes one entry and releases its retention reference at most
// once, so no reference count can ever go negative.
func (m *TransactionManager) removeLocked(entry *transactionEntry) {
	delete(m.activeByID, entry.id)
	m.releaseRefLocked(entry)
}

func (m *TransactionManager) releaseRefLocked(entry *transactionEntry) {
	if !entry.pinned {
		return
	}
	entry.pinned = false
	if refs := m.snapshotRefs[entry.readTS]; refs > 1 {
		m.snapshotRefs[entry.readTS] = refs - 1
		return
	}
	// Last reference, or a defensive zero: drop the key so a count can never
	// become negative or linger as a phantom pin.
	delete(m.snapshotRefs, entry.readTS)
}

// dropDescendantsLocked removes every registered descendant of the transaction
// id. Descendants never pin a snapshot themselves, so this normally reclaims
// diagnostics entries only; it still goes through removeLocked so the reference
// accounting stays correct even if a pinned entry were ever reachable from here.
func (m *TransactionManager) dropDescendantsLocked(rootID string) {
	removed := map[string]bool{rootID: true}
	for {
		grew := false
		for _, entry := range m.activeByID {
			if !removed[entry.parentID] {
				continue
			}
			removed[entry.id] = true
			m.removeLocked(entry)
			grew = true
		}
		if !grew {
			return
		}
	}
}

// Transition moves a registered transaction to another lifecycle state.
//
// The edge must exist in transactionTransitionKind and must be legal for the
// transaction's kind: roots commit, children merge. Repeating a terminal state is
// a no-op, which keeps a double rollback idempotent and never re-records an
// outcome.
func (m *TransactionManager) Transition(id string, to TransactionState, options TransitionOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.activeByID[id]
	if entry == nil {
		return fmt.Errorf("%w: %s", ErrUnknownTransaction, id)
	}
	kind := transactionTransitionKind(entry.state, to)
	if kind == transitionIllegal {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransactionMove, entry.state, to)
	}
	if kind == transitionRootOnly && !entry.root {
		return fmt.Errorf("%w: %s -> %s is root-only", ErrIllegalTransactionMove, entry.state, to)
	}
	if kind == transitionChildOnly && entry.root {
		return fmt.Errorf("%w: %s -> %s is child-only", ErrIllegalTransactionMove, entry.state, to)
	}
	if options.HasCommitTS && (!entry.root || to != TransactionCommitted) {
		return fmt.Errorf("%w: a durable commit timestamp requires a root COMMITTED move", ErrIllegalTransactionMove)
	}
	if options.Conflict && to != TransactionAborted {
		return fmt.Errorf("%w: a conflict marker requires an ABORTED move", ErrIllegalTransactionMove)
	}
	if options.Reason != "" && to != TransactionAborted {
		return fmt.Errorf("%w: an abort reason requires an ABORTED move", ErrIllegalTransactionMove)
	}
	if entry.state == to {
		// Idempotent confirmation of a terminal state. The state, commit
		// timestamp, abort reason and counters were recorded by the first move.
		return nil
	}
	entry.state = to
	if options.HasCommitTS {
		entry.commitTS, entry.hasCommitTS = options.CommitTS, true
	}
	if to == TransactionAborted {
		entry.abortReason = options.Reason
		if options.Conflict {
			m.conflicts++
		}
	}
	switch to {
	case TransactionCommitted:
		m.committed++
	case TransactionAborted:
		m.aborted++
	}
	if to.Terminal() {
		// A terminal transaction never reads again, so history retention for its
		// read timestamp ends here. Unregister then finds nothing left to
		// release, which keeps the release exactly-once.
		m.releaseRefLocked(entry)
	}
	return nil
}

// Reset drops every registered transaction and every retention reference.
//
// It is the generation-change hook: after Store.Restore replaces the database,
// transactions registered against the previous generation are invalid, and none
// of them may keep pinning history for the new one. Clearing the registry outright
// is what makes that guarantee independent of whether a stale transaction is ever
// unregistered. Process-lifetime counters are deliberately kept, because they are
// diagnostics for this process rather than state of the replaced database.
func (m *TransactionManager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeByID = make(map[string]*transactionEntry)
	m.snapshotRefs = make(map[uint64]uint64)
}

// ActiveTransactions returns a copy of the registered transactions. Entries that
// reached a terminal state but were not unregistered yet are included, because
// they are still registered; callers that want live transactions only must filter
// on State. Iteration order is unspecified and is not part of the contract.
func (m *TransactionManager) ActiveTransactions() []TransactionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	transactions := make([]TransactionInfo, 0, len(m.activeByID))
	for _, entry := range m.activeByID {
		transactions = append(transactions, entry.info())
	}
	return transactions
}

// Info returns a copy of one registered transaction.
func (m *TransactionManager) Info(id string) (TransactionInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry := m.activeByID[id]
	if entry == nil {
		return TransactionInfo{}, false
	}
	return entry.info(), true
}

// OldestReadTS reports the smallest read timestamp still pinned by a registered
// root transaction, which is the horizon history compaction must respect.
//
// The boolean is required because ReadTS=0 is legal: 0 can never act as an unset
// sentinel (spec.md section 7).
func (m *TransactionManager) OldestReadTS() (uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.oldestReadTSLocked()
}

func (m *TransactionManager) oldestReadTSLocked() (uint64, bool) {
	var oldest uint64
	found := false
	for readTS, refs := range m.snapshotRefs {
		if refs == 0 {
			// A defensive zero never pins anything.
			continue
		}
		if !found || readTS < oldest {
			oldest, found = readTS, true
		}
	}
	return oldest, found
}

// Stats returns the aggregate counters and the current GC horizon. Active counts
// cover registered transactions that have not reached a terminal state; the
// monotonic counters cover terminal outcomes for the process lifetime.
func (m *TransactionManager) Stats() TransactionStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stats := TransactionStats{Committed: m.committed, Aborted: m.aborted, Conflicts: m.conflicts}
	for _, entry := range m.activeByID {
		if entry.state.Terminal() {
			continue
		}
		if entry.root {
			stats.ActiveRoot++
			continue
		}
		stats.ActiveChildren++
	}
	stats.OldestReadTS, stats.HasOldestReadTS = m.oldestReadTSLocked()
	return stats
}
