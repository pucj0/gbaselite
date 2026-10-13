package mvccadapter

import (
	"context"
	"errors"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/mvcc"
	"gbaselite/storageengine"
)

// locking.go implements the neutral storageengine.LockingTxn capability over the B02 lock
// manager.
//
// This is the layer that keeps the SQL executor free of both bbolt and the concrete lock
// manager: the executor speaks storageengine.LockComponent, and this file is the only place that
// knows how a neutral component becomes a lockmanager.ResourceRef. The architecture test forbids
// storageengine itself from importing lockmanager, so the conversion has to live here.
//
// Identity rule: Space, Key and the bounds are passed through untouched, because they are already
// the encoded bytes the write path uses. TableID and IndexID travel as opaque labels. Nothing in
// this file parses, re-encodes or normalises a key.

var _ storageengine.LockingTxn = (*txn)(nil)

// LockingTxn reports the neutral locking capability of this transaction, if the engine has one.
//
// The capability is optional by contract: a backend without a lock manager returns false and a
// caller that requires locking must then fail closed rather than run unprotected.
//
// This method answers "does this engine have a lock manager", and it is the authoritative check: it
// returns false for a transaction whose engine was opened without one. The neutral helper
// `storageengine.LockingTxnOf` is a type assertion instead, so for this backend it reports the
// capability as present even then, and every operation on it fails closed with ErrUnsupported (see
// `lockManager()` below, which each entry point checks). A caller that must distinguish the two
// states asks this method, or the engine's `Locking()`, rather than relying on the assertion.
func (t *txn) LockingTxn() (storageengine.LockingTxn, bool) {
	if t.lockManager() == nil {
		return nil, false
	}
	return t, true
}

// lockManager returns the engine's lock manager, or nil when the engine runs without locking.
func (t *txn) lockManager() *lockmanager.Manager {
	if t.engine == nil {
		return nil
	}
	return t.engine.locks
}

// LockManagerOf returns the engine's underlying lock manager, if available.
func LockManagerOf(e storageengine.Engine) *lockmanager.Manager {
	if eng, ok := e.(*engine); ok {
		return eng.locks
	}
	return nil
}

// BeginLockScope opens one request scope owned by this transaction's root.
// BeginLockScope opens one request scope owned by this transaction's root.
//
// The scope's wait deadline is taken from the caller's context when it has one. That is what makes
// the session's `innodb_lock_wait_timeout` effective in both directions: the executor derives the
// scope context as the earlier of the query context and the session budget, so a session asking for
// 300 seconds reaches the lock manager as a 300-second scope deadline instead of being silently cut
// off at the contract default. A context without a deadline leaves the scope's deadline unset and the
// manager applies its own default.
func (t *txn) BeginLockScope(ctx context.Context, kind storageengine.LockScopeKind) (uint64, error) {
	if _, ok := t.LockingTxn(); !ok {
		return 0, storageengine.ErrUnsupported
	}
	options := mvcc.LockScopeOptions{Kind: kind}
	if deadline, ok := ctx.Deadline(); ok {
		options.Deadline = deadline
	}
	return t.inner.BeginLockScope(ctx, options)
}

// BeginLockScopeBudget opens a scope whose waits each carry the given budget.
//
// The budget is a duration and is measured per wait, so a statement that waits twice for less than the
// session's bound each time stays inside it. The context deadline, when there is one, still caps every
// wait of the statement.
func (t *txn) BeginLockScopeBudget(ctx context.Context, kind storageengine.LockScopeKind, budget time.Duration) (uint64, error) {
	if _, ok := t.LockingTxn(); !ok {
		return 0, storageengine.ErrUnsupported
	}
	options := mvcc.LockScopeOptions{Kind: kind, WaitBudget: budget}
	if deadline, ok := ctx.Deadline(); ok {
		options.Deadline = deadline
	}
	return t.inner.BeginLockScope(ctx, options)
}

// CloseLockScope cancels this scope's pending requests and keeps every grant.
func (t *txn) CloseLockScope(ctx context.Context, scope uint64) error {
	if _, ok := t.LockingTxn(); !ok {
		return storageengine.ErrUnsupported
	}
	_, err := t.inner.CloseLockScope(ctx, scope)
	return err
}

// Acquire requests one atomic bundle and returns the opaque grant.
//
// The bundle is granted as a whole or becomes exactly one pending request. A request that has to
// wait is waited for here, in the caller's goroutine, and the terminal status is mapped to the
// neutral error set so the executor never sees a lock-manager type.
func (t *txn) Acquire(ctx context.Context, scope uint64, request storageengine.LockRequest) (storageengine.LockGrant, error) {
	manager := t.lockManager()
	if manager == nil {
		return storageengine.LockGrant{}, storageengine.ErrUnsupported
	}
	components, err := lockComponents(request.Components)
	if err != nil {
		return storageengine.LockGrant{}, err
	}
	shouldWait := func() bool { return true }
	if request.ShouldWait != nil {
		// The neutral decision takes a context; the manager consults it once, without a context
		// of its own, so it is bound to the call's context here.
		decision := request.ShouldWait
		shouldWait = func() bool { return decision(ctx) }
	}
	grant, op, wait, err := t.inner.AcquireLock(ctx, scope, components, shouldWait)
	if err != nil {
		return storageengine.LockGrant{}, neutralLockError(err)
	}
	switch op {
	case lockmanager.LockOpGranted:
		return t.grant(grant), nil
	case lockmanager.LockOpReentrant:
		// A re-entrant acquisition took no new lock, so the manager reports the owner's existing
		// protection instead of a fresh grant identity. It is converted exactly like a new grant,
		// components included: a locked current read must be able to prove that this capability
		// covers the key being read, and a reduced owner-only grant would authorise nothing.
		return t.grant(grant), nil
	case lockmanager.LockOpWait:
		if wait == nil {
			return storageengine.LockGrant{}, storageengine.ErrConflict
		}
		outcome, waitErr := t.inner.WaitForLock(ctx, wait)
		if waitErr != nil {
			return storageengine.LockGrant{}, neutralLockError(waitErr)
		}
		return t.grant(outcome), nil
	default:
		return storageengine.LockGrant{}, storageengine.ErrConflict
	}
}

// ReadCurrent returns the latest published state for one key after the caller holds a covering
// grant.
//
// The whole grant is forwarded, not just its owner identity: the read re-validates the owner, the
// generation, the covering resource and the lock mode against the live lock registry, so a
// fabricated or reduced capability fails closed instead of recording a write baseline for a row
// nothing protected. A value observed from a generation that ended mid-read is discarded rather
// than returned as current.
func (t *txn) ReadCurrent(ctx context.Context, grant storageengine.LockGrant, space string, key []byte) (storageengine.CurrentObservation, error) {
	if _, ok := t.LockingTxn(); !ok {
		return storageengine.CurrentObservation{}, storageengine.ErrUnsupported
	}
	if t.engine.node != nil {
		if err := t.engine.node.Barrier(ctx); err != nil {
			return storageengine.CurrentObservation{}, err
		}
		if currentTerm := t.engine.node.Term(); grant.Term != 0 && currentTerm != 0 && grant.Term != currentTerm {
			return storageengine.CurrentObservation{}, storageengine.ErrLockInvalidated
		}
	}
	components, err := lockComponents(grant.Components)
	if err != nil {
		// A capability whose components cannot be translated is not a capability this backend can
		// honour: refusing is the only safe answer, because guessing the coverage would authorise
		// a read of an unknown resource.
		return storageengine.CurrentObservation{}, neutralLockError(err)
	}
	observation, err := t.inner.ReadCurrentLocked(ctx, space, key, lockmanager.LockGrant{
		GrantID:       grant.ID,
		OwnerID:       grant.OwnerID,
		OriginScopeID: grant.ScopeID,
		Generation:    grant.Generation,
		Term:          grant.Term,
		Components:    components,
	})
	if err != nil {
		return storageengine.CurrentObservation{}, neutralLockError(err)
	}
	return neutralObservation(observation), nil
}

// NewCurrentIterator opens a bounded locked scan.
//
// The scan protects the caller's access interval first, then walks the range in bounded pages: each
// page releases its storage view before the iterator claims the keys of that page, and every claimed
// key is re-read so a row that was deleted or rewritten while the iterator waited is never returned
// as it was listed. See `currentIterator` for the two rules that make that structural.
//
// A backend without the locking capability, or a request without a usable record mode, fails closed
// instead of falling back to an unlocked snapshot: a caller that asked for a locked scan and
// silently received an unlocked one would take a decision on data nothing protected.
func (t *txn) NewCurrentIterator(ctx context.Context, scope uint64, request storageengine.CurrentScanRequest) (storageengine.Iterator, error) {
	if _, ok := t.LockingTxn(); !ok {
		return nil, storageengine.ErrUnsupported
	}
	var termFence func() error
	if t.engine.node != nil {
		if err := t.engine.node.Barrier(ctx); err != nil {
			return nil, err
		}
		initialTerm := t.engine.node.Term()
		termFence = func() error {
			if t.engine.node.Status().State != "Leader" || t.engine.node.Term() != initialTerm {
				return storageengine.ErrLockInvalidated
			}
			return nil
		}
	}
	it, err := newCurrentIterator(ctx, t.inner, scope, request, termFence)
	if err != nil {
		return nil, neutralLockError(err)
	}
	return it, nil
}

// KeysAround returns the published neighbours of one key in a namespace.
//
// Both lookups are one-key probes over the latest published state plus this transaction's own overlay
// (`mvcc.Tx.ScanLatestRange`), and each stops at its first row: the view of a page is released before
// the consumer sees a key, so no storage view outlives the call and none is held across a wait. A
// neighbour another transaction committed after this transaction's snapshot still counts, because the
// index the gap has to bound is the current index, not the statement's view of it.
func (t *txn) KeysAround(ctx context.Context, space string, key []byte) ([]byte, []byte, error) {
	if _, ok := t.LockingTxn(); !ok {
		return nil, nil, storageengine.ErrUnsupported
	}
	if space == "" || len(key) == 0 {
		return nil, nil, storageengine.ErrUnsupported
	}
	next, err := t.probeKey(ctx, space, storageengine.KeyRange{Lower: key})
	if err != nil {
		return nil, nil, neutralLockError(err)
	}
	previous, err := t.probeKey(ctx, space, storageengine.KeyRange{Upper: key, Reverse: true})
	if err != nil {
		return nil, nil, neutralLockError(err)
	}
	return previous, next, nil
}

// probeKey returns the first key one bounded probe yields, or nil when it yields none.
func (t *txn) probeKey(ctx context.Context, space string, bounds storageengine.KeyRange) ([]byte, error) {
	var found []byte
	err := t.inner.ScanLatestRange(ctx, space, bounds, func(k, v []byte) error {
		found = append([]byte(nil), k...)
		return errStopNeighbourProbe
	})
	if err != nil && err != errStopNeighbourProbe {
		return nil, err
	}
	return found, nil
}

// errStopNeighbourProbe ends a one-key probe after its first row. It never escapes the adapter.
var errStopNeighbourProbe = errors.New("neighbour probe complete")

// grant converts one lock-manager grant into the neutral capability.
//
// Every field of the capability is mapped, because the neutral grant is what a later
// ReadCurrent re-validates and what a caller relies on to know what it acquired: dropping the
// components, the generation or the term would leave the caller with a capability that can only be
// trusted, never checked, and mapping the scope or the id to something else would name a request
// that is not this one.
func (t *txn) grant(g lockmanager.LockGrant) storageengine.LockGrant {
	return storageengine.LockGrant{
		ID:         g.GrantID,
		OwnerID:    g.OwnerID,
		ScopeID:    g.OriginScopeID,
		Generation: g.Generation,
		Term:       g.Term,
		Components: neutralComponents(g.Components),
		Granted:    g.GrantedAt,
	}
}

// neutralComponents converts one granted component list into the neutral DTOs.
func neutralComponents(components []lockmanager.ResourceRef) []storageengine.LockComponent {
	if len(components) == 0 {
		return nil
	}
	out := make([]storageengine.LockComponent, 0, len(components))
	for _, component := range components {
		out = append(out, storageengine.LockComponent{
			Mode:           neutralLockMode(component.Mode),
			Kind:           neutralLockKind(component.Kind),
			Space:          component.Space,
			Key:            component.Key,
			Lower:          component.Lower,
			Upper:          component.Upper,
			LowerInclusive: component.LowerInclusive,
			UpperInclusive: component.UpperInclusive,
			TableID:        component.TableID,
			IndexID:        component.IndexID,
		})
	}
	return out
}

// neutralLockMode maps one lock-manager mode onto the neutral set.
func neutralLockMode(mode lockmanager.Mode) storageengine.LockMode {
	switch mode {
	case lockmanager.ModeIS:
		return storageengine.LockModeIS
	case lockmanager.ModeIX:
		return storageengine.LockModeIX
	case lockmanager.ModeS:
		return storageengine.LockModeS
	case lockmanager.ModeX:
		return storageengine.LockModeX
	case lockmanager.ModeGap:
		return storageengine.LockModeGap
	case lockmanager.ModeInsertIntention:
		return storageengine.LockModeInsertIntention
	default:
		return ""
	}
}

// neutralLockKind maps one lock-manager resource kind onto the neutral set.
func neutralLockKind(kind lockmanager.ResourceKind) storageengine.LockComponentKind {
	switch kind {
	case lockmanager.KindTableIntent:
		return storageengine.LockComponentTableIntent
	case lockmanager.KindRecord:
		return storageengine.LockComponentRecord
	case lockmanager.KindInterval:
		return storageengine.LockComponentInterval
	default:
		return ""
	}
}

// lockComponents converts a neutral bundle.
func lockComponents(components []storageengine.LockComponent) ([]lockmanager.ResourceRef, error) {
	out := make([]lockmanager.ResourceRef, 0, len(components))
	for _, component := range components {
		kind, err := lockKind(component.Kind)
		if err != nil {
			return nil, err
		}
		mode, err := lockMode(component.Mode)
		if err != nil {
			return nil, err
		}
		out = append(out, lockmanager.ResourceRef{Identity: lockmanager.Identity{
			Kind:           kind,
			Mode:           mode,
			Space:          component.Space,
			TableID:        component.TableID,
			IndexID:        component.IndexID,
			Key:            component.Key,
			Lower:          component.Lower,
			Upper:          component.Upper,
			LowerInclusive: component.LowerInclusive,
			UpperInclusive: component.UpperInclusive,
		}})
	}
	return out, nil
}

// lockKind maps one neutral component kind.
func lockKind(kind storageengine.LockComponentKind) (lockmanager.ResourceKind, error) {
	switch kind {
	case storageengine.LockComponentTableIntent:
		return lockmanager.KindTableIntent, nil
	case storageengine.LockComponentRecord:
		return lockmanager.KindRecord, nil
	case storageengine.LockComponentInterval:
		return lockmanager.KindInterval, nil
	default:
		return "", storageengine.ErrUnsupported
	}
}

// lockMode maps one neutral lock mode.
func lockMode(mode storageengine.LockMode) (lockmanager.Mode, error) {
	switch mode {
	case storageengine.LockModeIS:
		return lockmanager.ModeIS, nil
	case storageengine.LockModeIX:
		return lockmanager.ModeIX, nil
	case storageengine.LockModeS:
		return lockmanager.ModeS, nil
	case storageengine.LockModeX:
		return lockmanager.ModeX, nil
	case storageengine.LockModeGap:
		return lockmanager.ModeGap, nil
	case storageengine.LockModeInsertIntention:
		return lockmanager.ModeInsertIntention, nil
	default:
		return "", storageengine.ErrUnsupported
	}
}

// neutralLockError maps a lock-manager error onto the neutral set.
//
// The mapping is what makes the executor backend-agnostic: it reacts to ErrDeadlock,
// ErrLockWaitTimeout and ErrConflict without knowing which lock manager produced them. A
// conflict that is not mapped would leak a lock-manager type through the neutral boundary, so
// every sentinel the manager can return for a refused request has an explicit answer here.
func neutralLockError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, lockmanager.ErrDeadlock):
		return storageengine.ErrDeadlock
	case errors.Is(err, lockmanager.ErrLockWaitTimeout):
		return storageengine.ErrLockWaitTimeout
	case errors.Is(err, lockmanager.ErrLockResourceLimit):
		return storageengine.ErrLockResourceLimit
	case errors.Is(err, lockmanager.ErrLockInvalidated), errors.Is(err, lockmanager.ErrOwnerUnknown):
		return storageengine.ErrLockInvalidated
	case errors.Is(err, lockmanager.ErrUnsupportedMode):
		return storageengine.ErrUnsupported
	case errors.Is(err, lockmanager.ErrOwnerEnding), errors.Is(err, lockmanager.ErrConflict):
		return storageengine.ErrConflict
	case errors.Is(err, lockmanager.ErrScopeUnknown):
		// The scope this request came from no longer exists: a statement whose scope was closed
		// while it waited for a request it had joined, or a caller using a scope after its close.
		// That is a statement-level cancellation, not an internal fault, so it maps onto the
		// retryable conflict the storage contract already uses for "this attempt is over, start
		// again" instead of leaking a lock-manager sentinel through the neutral boundary.
		return storageengine.ErrConflict
	default:
		// Anything else is a programming error (a malformed component, an unknown scope) or an
		// error this adapter does not classify. Passing it through leaves it visible instead of
		// disguising it as a retryable conflict, which would invite an endless retry loop.
		return err
	}
}

// neutralObservation maps one current observation.
func neutralObservation(o mvcc.CurrentObservation) storageengine.CurrentObservation {
	return storageengine.CurrentObservation{
		Value:     o.Value,
		Exists:    o.Exists,
		ChangeTS:  o.ChangeTS,
		HasChange: o.HasChange,
		Source:    neutralObservationSource(o.Source),
		Tombstone: o.Tombstone,
	}
}

// neutralObservationSource maps where one observation came from.
func neutralObservationSource(source mvcc.CurrentObservationSource) storageengine.CurrentObservationSource {
	switch source {
	case mvcc.CurrentCommitted:
		return storageengine.CurrentSourceCommitted
	case mvcc.CurrentOwnOverlay:
		return storageengine.CurrentSourceOverlay
	default:
		return storageengine.CurrentSourceAbsent
	}
}
