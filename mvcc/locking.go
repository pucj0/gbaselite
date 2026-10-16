package mvcc

import (
	"context"
	"sync"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking.go binds the B02 lock manager to the B01 transaction lifecycle.
//
// The contract it implements is narrow and load-bearing: a grant belongs to the *root*
// transaction and to nothing else, so the root is the only thing that ever releases one. A
// child, a savepoint and a statement scope are request origins; closing one withdraws the
// requests it was still waiting for and keeps every grant. That is what makes "child commit
// must not release the root's record X" true by construction rather than by a rule someone has
// to remember.
//
// Lock order (the rule that keeps this free of inversion):
//
//	Store.views -> Store.gate / TransactionManager.mu -> lockmanager.Manager.mu
//
// The store's own locks are always taken first and the lock manager is always the innermost
// lock. Nothing inside the lock manager calls back into a transaction, and no transaction path
// holds the lock manager's mutex while taking a store lock, so the order is total. A test pins
// it, and the detector's "never call Txn" rule is the same invariant stated for the deadlock
// path.

// LockBinding attaches one lock manager to one MVCC store.
//
// It exists so the transaction lifecycle has exactly one place that knows how a lock owner, a
// scope and a store transaction correspond. A store without a binding runs with no locking at
// all, which is the default and keeps B01 behaviour unchanged.
type LockBinding struct {
	manager *lockmanager.Manager
	epochMu sync.Mutex
	// storeGeneration records the B01 generation the binding was last synchronised with, so a
	// generation change is recognisable as an epoch change.
	storeGeneration uint64
	// storeTerm records the Raft leader term the binding was last synchronised with.
	storeTerm uint64
}

// NewLockBinding validates and returns a binding for one manager.
func NewLockBinding(manager *lockmanager.Manager) (*LockBinding, error) {
	if manager == nil {
		return nil, storageengine.ErrUnsupported
	}
	return &LockBinding{manager: manager}, nil
}

// Manager exposes the underlying lock manager.
//
// A caller that owns both the store and the manager needs it to run detection or read
// diagnostics; nothing in the transaction lifecycle does.
func (b *LockBinding) Manager() *lockmanager.Manager { return b.manager }

// AttachLocks attaches one lock manager to the store.
//
// It must be called before the first Begin. Attaching a different manager twice is refused,
// because two managers over one store would put the same key on two resources and lose mutual
// exclusion silently.
func (s *Store) AttachLocks(manager *lockmanager.Manager) (*LockBinding, error) {
	binding, err := NewLockBinding(manager)
	if err != nil {
		return nil, err
	}
	s.views.Lock()
	defer s.views.Unlock()
	if s.locks != nil && s.locks.manager != binding.manager {
		return nil, ErrLocksAttached
	}
	binding.epochMu.Lock()
	if s.generation.Load() == 0 {
		s.generation.Store(manager.Generation())
	}
	binding.storeGeneration = s.generation.Load()
	binding.storeTerm = manager.Term()
	binding.epochMu.Unlock()
	s.locks = binding
	return binding, nil
}

// LockBinding returns the store's lock binding, if one is attached.
func (s *Store) LockBinding() *LockBinding { return s.locks }

// CloseLocks invalidates and then closes the lock manager attached to the store.
//
// The order is the contract's: old handles are refused first and only then are the pending
// waiters woken. A waiter woken before its handle was refused could observe a grant from a
// closed manager.
func (s *Store) CloseLocks() {
	binding := s.locks
	if binding == nil {
		return
	}
	binding.manager.Close()
}

// ResetLockEpoch invalidates the lock manager's epoch for the store's current generation and
// term, waking every pending waiter, and reports how many owners were affected.
//
// Store.Restore and a leader-term change both take this path. It is called while the store's
// restore locks are held, which is what makes the invalidated set exact: a transaction created
// under the old generation is already invalid because newTx registers under Store.views, and the
// lock manager now agrees with the registry instead of keeping an owner the registry dropped.
//
// It is protected by binding.epochMu so concurrent epoch resets are strictly serialized and
// monotonic: a stale term cannot overwrite a newer term, and identical (generation, term)
// transitions are idempotent.
func (s *Store) ResetLockEpoch(term uint64) int {
	generation := s.generation.Load()
	binding := s.locks
	if binding == nil {
		if s.dirtyRegistry != nil {
			s.dirtyRegistry.Reset(generation, term)
		}
		return 0
	}
	binding.epochMu.Lock()
	defer binding.epochMu.Unlock()

	// Monotonicity: an epoch reset for an older generation or older term cannot roll back epoch state.
	if generation < binding.storeGeneration {
		return 0
	}
	if generation == binding.storeGeneration && term < binding.storeTerm {
		return 0
	}
	// Idempotency: an identical (generation, term) transition is a strict no-op.
	if generation == binding.storeGeneration && term == binding.storeTerm {
		return 0
	}
	binding.storeGeneration = generation
	binding.storeTerm = term
	if s.dirtyRegistry != nil {
		s.dirtyRegistry.Reset(generation, term)
	}
	return binding.manager.ResetEpoch(generation, term)
}

var forceResetLockEpochHook func(stage string, generation, term uint64)

// ForceResetLockEpoch unconditionally invalidates all lock owners, releases grants,
// cancels pending requests, and resets the dirty registry for the expected generation and term,
// provided that the store has not already advanced to a newer generation or higher term.
// If the store is already in a newer generation or higher term, the stale reset is rejected
// and returns 0 without touching newer lock owners or dirty versions.
func (s *Store) ForceResetLockEpoch(generation, term uint64) int {
	if forceResetLockEpochHook != nil {
		forceResetLockEpochHook("before_acquire_epoch_mu", generation, term)
	}
	binding := s.locks
	if binding == nil {
		if s.dirtyRegistry != nil {
			s.dirtyRegistry.ForceReset(generation, term)
		}
		return 0
	}
	binding.epochMu.Lock()
	defer binding.epochMu.Unlock()

	if forceResetLockEpochHook != nil {
		forceResetLockEpochHook("inside_epoch_mu", generation, term)
	}

	// Monotonicity & Stale Event Protection:
	// A force reset from an older generation or older term must NOT roll back or
	// disrupt a newer generation or term that has already been established.
	currentGen := s.generation.Load()
	if generation < currentGen || generation < binding.storeGeneration {
		return 0
	}
	if generation == binding.storeGeneration && term < binding.storeTerm {
		return 0
	}
	if generation > currentGen {
		return 0
	}

	binding.storeGeneration = generation
	binding.storeTerm = term
	if s.dirtyRegistry != nil {
		s.dirtyRegistry.ForceReset(generation, term)
	}
	return binding.manager.ResetEpoch(generation, term)
}

// SyncLockEpoch synchronises the lock epoch with the store's generation and Raft leader term
// when either has drifted, and reports whether it had to act.
//
// It is protected by binding.epochMu so concurrent sync calls for the same term transition
// execute ResetEpoch at most once without a TOCTOU race window. A second concurrent call sees
// the updated state and returns false without performing a redundant reset that could kill
// newly registered roots.
func (s *Store) SyncLockEpoch(term uint64) bool {
	generation := s.generation.Load()
	binding := s.locks
	if binding == nil {
		if s.dirtyRegistry != nil {
			return s.dirtyRegistry.Reset(generation, term)
		}
		return false
	}
	binding.epochMu.Lock()
	defer binding.epochMu.Unlock()

	if generation < binding.storeGeneration {
		return false
	}
	if generation == binding.storeGeneration && term <= binding.storeTerm {
		return false
	}
	binding.storeGeneration = generation
	binding.storeTerm = term
	if s.dirtyRegistry != nil {
		s.dirtyRegistry.Reset(generation, term)
	}
	binding.manager.ResetEpoch(generation, term)
	return true
}

// beginLockOwner registers the root lock owner for one store transaction.
//
// A child does not get its own owner: it inherits the root's, so every grant a child takes is
// attributed to the root and survives the child's merge. That is the lifecycle contract's
// "child locks belong to the root" expressed as a single fact about registration.
func (s *Store) beginLockOwner(tx *Tx) error {
	if s.locks == nil {
		return nil
	}
	if tx.parent != nil {
		owner, ok := s.locks.manager.Owner(tx.rootTx().ID)
		if !ok {
			// The root's owner is gone, which means the root was already released while this child
			// still existed. Refusing is the safe answer: a child that cannot name its owner could
			// acquire a grant nobody would ever release.
			return storageengine.ErrLockInvalidated
		}
		tx.lockOwnerID = owner.RootID
		return nil
	}
	if _, err := s.locks.manager.RegisterOwner(tx.ID, s.generation.Load(), tx.term, tx.Snapshot); err != nil {
		return err
	}
	tx.lockOwnerID = tx.ID
	return nil
}

// rootTx returns the root of this transaction's ancestor chain.
func (t *Tx) rootTx() *Tx {
	root := t
	for root.parent != nil {
		root = root.parent
	}
	return root
}

// LockOwnerID reports the root lock owner this transaction's grants belong to.
//
// It is the transaction's own id when no lock manager is attached, so a caller can still ask the
// question in the B01-only configuration; use LockManager to find out whether locking is
// actually active.
func (t *Tx) LockOwnerID() string {
	if t.lockOwnerID != "" {
		return t.lockOwnerID
	}
	return t.rootTx().ID
}

// LockManager exposes the lock manager behind this transaction's store.
//
// It reports false when no lock manager is attached, so a caller that requires locking can fail
// closed instead of silently running unprotected.
func (t *Tx) LockManager() (*lockmanager.Manager, bool) {
	if t.store.locks == nil {
		return nil, false
	}
	return t.store.locks.manager, true
}

// beginTransactionScope opens the transaction's own lock scope.
//
// A root gets a statement scope, because a root is the origin of a top-level statement; a child
// gets a child scope, which is what SQL opens for a savepoint or statement layer. The scope is a
// request origin for diagnostics only: it never owns a grant.
//
// A child's scope is nested inside the scope its parent transaction opened. That is what makes the
// scope tree real rather than decorative: discarding an enclosing layer closes its whole subtree,
// so the requests a savepoint's scope issued are cancelled by rolling back the layer that owns
// them. A root-level sibling scope would survive its savepoint's rollback and keep a request alive
// for a layer that no longer exists.
func (s *Store) beginTransactionScope(tx *Tx) error {
	if s.locks == nil {
		return nil
	}
	kind := lockmanager.ScopeChild
	parentScopeID := uint64(0)
	if tx.parent == nil {
		kind = lockmanager.ScopeStatement
	} else {
		parentScopeID = tx.parent.lockScopeID
	}
	return s.openLockScope(tx, kind, parentScopeID, tx.lockDeadline)
}

// openLockScope opens one lock scope for a transaction and records it as the transaction's own.
//
// parentScopeID is passed in rather than read from the transaction, because a transaction's own
// scope does not exist yet while it is being opened and because a caller may deliberately nest a
// new scope under an older one.
func (s *Store) openLockScope(tx *Tx, kind lockmanager.ScopeKind, parentScopeID uint64, deadline time.Time) error {
	ownerID := tx.LockOwnerID()
	scope, err := s.locks.manager.OpenScope(ownerID, tx.ID, parentScopeID, kind, deadline)
	if err != nil {
		return err
	}
	tx.lockScopeID = scope.ID
	return nil
}

// LockScopeOptions selects the origin and the wait bound of one lock scope.
type LockScopeOptions struct {
	// Kind classifies the scope. An empty kind is refused rather than guessed.
	Kind storageengine.LockScopeKind
	// ParentScopeID nests the scope inside an enclosing one when it is non-zero. It defaults to
	// the transaction's own scope.
	ParentScopeID uint64
	// Deadline caps the whole scope: it is the statement's or query's own bound. Zero means "no
	// context bound". It is not the session's lock-wait budget, which bounds one wait each.
	Deadline time.Time
	// WaitBudget is the bound of one wait issued from this scope, as a duration: the session's
	// `innodb_lock_wait_timeout`. Each request measures it from the moment it starts waiting, so a
	// statement that waits twice for less than the budget stays inside it. Zero means the manager's
	// default budget (50 seconds per the contract).
	WaitBudget time.Duration
}

// lockScopeKind maps one neutral scope kind onto the lock manager's.
func lockScopeKind(kind storageengine.LockScopeKind) (lockmanager.ScopeKind, bool) {
	switch kind {
	case storageengine.LockScopeStatement:
		return lockmanager.ScopeStatement, true
	case storageengine.LockScopeChild:
		return lockmanager.ScopeChild, true
	case storageengine.LockScopeSavepoint:
		return lockmanager.ScopeSavepoint, true
	default:
		return "", false
	}
}

// BeginLockScope opens one additional lock scope for this transaction.
//
// It is the entry point a statement or a savepoint uses when it needs a request origin of its
// own. The returned scope id is passed to Acquire; closing it withdraws the requests that scope
// was still waiting for and keeps every grant the root holds.
func (t *Tx) BeginLockScope(ctx context.Context, options LockScopeOptions) (uint64, error) {
	if err := t.checkOpen(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if t.store.locks == nil {
		return 0, storageengine.ErrUnsupported
	}
	kind, ok := lockScopeKind(options.Kind)
	if !ok {
		return 0, storageengine.ErrUnsupported
	}
	// ParentScopeID defaults to this transaction's own scope, so a statement scope is reported as
	// belonging to the transaction it came from.
	parentScopeID := options.ParentScopeID
	if parentScopeID == 0 {
		parentScopeID = t.lockScopeID
	}
	scope, err := t.store.locks.manager.OpenScopeWithBudget(t.LockOwnerID(), t.ID, parentScopeID, kind, options.Deadline, options.WaitBudget)
	if err != nil {
		return 0, err
	}
	return scope.ID, nil
}

// CloseLockScope closes one lock scope of this transaction.
//
// Closing a scope cancels its pending requests with one definite status and releases nothing. A
// statement that failed therefore leaves the root holding exactly the locks it had already
// taken, which is the rule the contract states and the reason a failed statement may not hand
// back locks another statement of the same transaction still relies on.
func (t *Tx) CloseLockScope(ctx context.Context, scopeID uint64) ([]uint64, error) {
	if t.store.locks == nil {
		return nil, storageengine.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cancelled, err := t.store.locks.manager.CloseScope(t.LockOwnerID(), scopeID)
	if err != nil {
		return nil, err
	}
	if scopeID == t.lockScopeID {
		t.lockScopeID = 0
	}
	return cancelled, nil
}

// AcquireLock requests one atomic lock bundle and returns the grant, waiting when it must.
//
// It is the transaction-level entry point a caller uses when it has already resolved the lock
// components it needs. It does not build components itself: a caller that guessed an interval
// would be inventing a lock range it cannot prove, which is exactly what the access-plan rule
// forbids.
func (t *Tx) AcquireLock(ctx context.Context, scopeID uint64, components []lockmanager.ResourceRef, mayWait func() bool) (lockmanager.LockGrant, lockmanager.LockOp, *lockmanager.LockHandle, error) {
	manager, ok := t.LockManager()
	if !ok {
		return lockmanager.LockGrant{}, "", nil, storageengine.ErrUnsupported
	}
	if err := t.checkOpen(); err != nil {
		return lockmanager.LockGrant{}, "", nil, err
	}
	result, err := manager.AcquireWith(ctx, t.LockOwnerID(), scopeID, components, mayWait)
	if err != nil {
		return lockmanager.LockGrant{}, "", nil, err
	}
	return result.Grant, result.Op, result.Wait, nil
}

// WaitForLock blocks until a queued request reaches a terminal status and returns its grant.
func (t *Tx) WaitForLock(ctx context.Context, handle *lockmanager.LockHandle) (lockmanager.LockGrant, error) {
	manager, ok := t.LockManager()
	if !ok {
		return lockmanager.LockGrant{}, storageengine.ErrUnsupported
	}
	manager.DetectDeadlock()
	result, err := manager.WaitForGrantResult(ctx, t.LockOwnerID(), handle)
	if err != nil {
		return lockmanager.LockGrant{}, err
	}
	return result.Grant, nil
}

// AcquireRawWriteLock takes the locks one raw write to one key needs.
//
// It exists so the raw client cannot bypass SQL and SQL cannot bypass the raw client: the raw
// write entry points (`Tx.Put`, `Tx.Delete`) take the same resources a SQL write of the same
// namespace and key takes, because the identity is the one the storage write path uses — the
// encoded namespace and the key bytes travel unchanged, and the manager never re-encodes them.
// Record identity is (Generation, Space, Key), so the SQL layer's table and index labels do not
// split the resource: a raw client that has never heard of a table label addresses the same row.
//
// Two cases, and both halves of an insertion are one request:
//
//   - the key exists in this transaction's own view: the exclusive record lock of that key, which
//     is what decides contention with another writer of the same row;
//   - the key is missing or is a tombstone in this transaction's own view (including a key this
//     transaction deleted earlier and is now re-inserting): an insert intention at the insertion
//     position **and** the same exclusive record lock, in one bundle. The intention is what a
//     covering Gap of another root blocks (FR-003/FR-005) — a gap deliberately does not conflict
//     with a plain record lock, because the interval rule is independent of the record rule, so
//     without the intention a raw insert would walk straight through another transaction's gap
//     protection. The record lock is what decides same-key contention (FR-005), and the manager
//     refuses an intention that is not paired with it (`ErrInsertIntentionRequiresRecord`).
//
// The existence test is this transaction's own view, not the published state: `read` already layers
// the transaction's own staged writes and its parent's, so a key this transaction deleted and is
// re-inserting is classified as an insertion, and a key another transaction committed after this
// snapshot is classified as missing (which is what makes our insert conflict with whatever that
// transaction is holding).
//
// The raw path has no per-statement context — neither `storageengine.Txn.Put` nor `mvcc.Tx.Put`
// carries one — so the wait is bounded by the manager's own wait deadline (the session default,
// 50 seconds) rather than by a caller deadline. Cancellation of a raw wait is therefore reached
// through the manager's own lifecycle (rollback, root end, Close, epoch reset), not through a
// context.
//
// It must be called before anything is staged: no bbolt write transaction may be open while a
// caller waits, which is why `Tx.write` acquires here and only then buffers the op. The existence
// probe above is a read (a snapshot read plus this transaction's own overlay), never a write, and it
// finishes before the wait starts.
func (t *Tx) AcquireRawWriteLock(op Op) error {
	if op.Check {
		// A guard is an optimistic dependency, not a write: it protects nothing and takes no lock.
		return nil
	}
	if _, ok := t.LockManager(); !ok {
		// The engine runs without a lock manager. That is a legitimate configuration (the neutral
		// capability is optional), and there is no lock to take.
		return nil
	}
	ownerID := t.LockOwnerID()
	if ownerID == "" {
		// The transaction no longer participates in locking, so it must not write either: failing
		// closed here is what keeps "every raw write is protected" true after a close.
		return ErrClosed
	}
	if t.lockScopeID == 0 {
		// A transaction without its own scope cannot attribute a request. Begin always opens one
		// while a manager is attached, so this is a closed or partially built transaction.
		return ErrClosed
	}
	if err := t.checkOpen(); err != nil {
		return err
	}
	components, err := t.rawWriteLockComponents(op)
	if err != nil {
		return err
	}
	// context.Background is deliberate: Put/Delete carry no context, and inventing one would
	// silently drop the caller's deadline. The manager bounds the wait itself.
	_, acquired, wait, err := t.AcquireLock(context.Background(), t.lockScopeID, components, nil)
	if err != nil {
		return err
	}
	if acquired == lockmanager.LockOpWait {
		// A conflicting acquisition is not a grant. The write must block here — before anything is
		// staged — and continue only once the manager has decided this request; returning on WAIT
		// would let the caller write while another root still holds the row, which is exactly the
		// bypass this function exists to prevent.
		if wait == nil {
			return storageengine.ErrConflict
		}
		if _, err := t.WaitForLock(context.Background(), wait); err != nil {
			return err
		}
		// The wait is where a root can end under us: the transaction may have been closed, or the
		// generation may have moved on, while this caller was queued. Staging the write anyway would
		// let an invalidated transaction keep writing, so the liveness of the transaction is
		// re-established after the wait and before the op is buffered.
		if err := t.checkOpen(); err != nil {
			return err
		}
	}
	return nil
}

// rawWriteLockComponents builds the components one raw write needs.
//
// The classification comes from this transaction's own view of the key (`read`, which layers the
// transaction's staged writes and its ancestors over the snapshot), so "missing" means missing for
// the caller that is about to write, not for the publication.
func (t *Tx) rawWriteLockComponents(op Op) ([]lockmanager.ResourceRef, error) {
	_, found, err := t.read(op.Space, op.Key)
	if err != nil {
		return nil, err
	}
	record := lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:  lockmanager.KindRecord,
		Mode:  lockmanager.ModeX,
		Space: op.Space,
		Key:   op.Key,
	}}
	if found {
		return []lockmanager.ResourceRef{record}, nil
	}
	intention := lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:  lockmanager.KindRecord,
		Mode:  lockmanager.ModeInsertIntention,
		Space: op.Space,
		Key:   op.Key,
	}}
	// The pair travels as one atomic request: the manager refuses an intention without its record
	// lock, so the two answers of an insertion can never be taken separately.
	return []lockmanager.ResourceRef{intention, record}, nil
}

// AbortRequested reports whether a detector asked this transaction's root to stop.
//
// A victim learns it was chosen through this check or through ErrDeadlock from a wait. The
// transaction layer never aborts on the detector's behalf: the flag is an intent, and the owning
// goroutine is the one that ends the root, which is what keeps a detector from calling a
// transaction method from its own goroutine.
func (t *Tx) AbortRequested() bool {
	manager, ok := t.LockManager()
	if !ok {
		return false
	}
	return manager.AbortRequested(t.LockOwnerID())
}

// AbortRequestReason reports why this transaction's root was asked to stop.
//
// It is empty when no abort was requested. The cause matters to the caller: a deadlock victim
// retries the transaction, while an invalidated epoch means the handle belongs to a generation
// that no longer exists and retrying the same handle cannot help.
func (t *Tx) AbortRequestReason() lockmanager.AbortReason {
	manager, ok := t.LockManager()
	if !ok {
		return ""
	}
	return manager.AbortReasonOf(t.LockOwnerID())
}

func (t *Tx) deadlockAborted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.abortReason == string(lockmanager.AbortDeadlock) || t.abortReason == "deadlock" {
		return true
	}
	return t.AbortRequestReason() == lockmanager.AbortDeadlock
}

// canStillCommit reports whether the transaction may still reach a durable publication.
//
// It is consulted before the COMMITTING transition, so a victim can never publish: the contract
// requires that a chosen victim cannot continue to write or commit, and refusing here is what
// makes that true independently of when the detector ran.
func (t *Tx) canStillCommit() error {
	switch t.AbortRequestReason() {
	case lockmanager.AbortDeadlock:
		return storageengine.ErrDeadlock
	case lockmanager.AbortEpoch:
		// An invalidated epoch is the same class as a generation change, which the commit path
		// already reports as a conflict. Reusing that error keeps one retry instruction per cause
		// instead of inventing a second one for the same condition.
		return ErrConflict
	default:
		return nil
	}
}

// closeTransactionLocks ends the lock participation of one finished transaction.
//
// The order inside it is the contract's cleanup order, and the caller has already done the first
// two steps:
//
//  1. the terminal outcome is recorded, so releasing locks can never rewrite a decided result;
//  2. the write set is unusable, so nothing can still publish from it;
//  3. the scope is withdrawn, and only then is the owner released.
//
// A child releases its own scope and nothing else. A root releases the whole owner, which drops
// every grant, every pending request and every graph edge it was part of. ReleaseOwner is
// idempotent, so a double rollback and a close race cannot underflow the accounting.
func (t *Tx) closeTransactionLocks() {
	s := t.store
	if s.locks == nil {
		return
	}
	ownerID := t.LockOwnerID()
	if t.lockScopeID != 0 {
		_, _ = s.locks.manager.CloseScope(ownerID, t.lockScopeID)
		t.lockScopeID = 0
	}
	if t.parent == nil {
		s.locks.manager.ReleaseOwner(ownerID)
	}
	// The transaction no longer participates in locking, so a later call through a stale reference
	// must not be able to name the released owner as if it were still live.
	t.lockOwnerID = ""
}
