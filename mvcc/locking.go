package mvcc

import (
	"context"
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
	// storeGeneration records the B01 generation the binding was last synchronised with, so a
	// generation change is recognisable as an epoch change.
	storeGeneration uint64
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
	binding.storeGeneration = s.generation.Load()
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
func (s *Store) ResetLockEpoch(term uint64) int {
	binding := s.locks
	if binding == nil {
		return 0
	}
	generation := s.generation.Load()
	binding.storeGeneration = generation
	return binding.manager.ResetEpoch(generation, term)
}

// SyncLockEpoch synchronises the lock epoch with the store's generation when the two have
// drifted, and reports whether it had to act.
//
// It is the safety net for a generation change that did not go through Restore: a caller that
// bumped the generation through some other path still cannot leave lock owners behind, because
// the drift is detected from the generation rather than assumed.
func (s *Store) SyncLockEpoch(term uint64) bool {
	binding := s.locks
	if binding == nil {
		return false
	}
	if binding.storeGeneration == s.generation.Load() {
		return false
	}
	s.ResetLockEpoch(term)
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
func (s *Store) beginTransactionScope(tx *Tx) error {
	if s.locks == nil {
		return nil
	}
	kind := lockmanager.ScopeChild
	if tx.parent == nil {
		kind = lockmanager.ScopeStatement
	}
	return s.openLockScope(tx, kind, tx.lockDeadline)
}

// openLockScope opens one lock scope for a transaction and records it as the transaction's own.
func (s *Store) openLockScope(tx *Tx, kind lockmanager.ScopeKind, deadline time.Time) error {
	ownerID := tx.LockOwnerID()
	scope, err := s.locks.manager.OpenScope(ownerID, tx.ID, tx.lockScopeID, kind, deadline)
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
	// Deadline bounds how long a request from this scope may wait. Zero means the manager's
	// session default, which the contract fixes at 50 seconds.
	Deadline time.Time
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
	scope, err := t.store.locks.manager.OpenScope(t.LockOwnerID(), t.ID, parentScopeID, kind, options.Deadline)
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
	result, err := manager.WaitForGrantResult(ctx, t.LockOwnerID(), handle)
	if err != nil {
		return lockmanager.LockGrant{}, err
	}
	return result.Grant, nil
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
