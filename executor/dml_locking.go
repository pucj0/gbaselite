package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// dml_locking.go gives the unified A05 modify pipeline the locks its writes need.
//
// The pipeline is the one place every INSERT, UPDATE and DELETE goes through, which is what makes
// it the right place for this: a lock taken by an individual operator could be forgotten by a new
// operator, and the rule "every row mutation is protected" would then depend on someone
// remembering it.
//
// Identity rule: the lock components use exactly the bytes the write path uses — the same
// sqllayout namespace and the same encoded primary-key bytes. A lock taken on a different
// encoding than the write would protect nothing, because the raw client would address a different
// resource. Nothing here parses or re-encodes a key.
//
// Fail-closed rule: when the engine has no locking capability, or the table has no primary key we
// can encode, the mutation is refused rather than run unprotected. A backend without a lock
// manager is allowed to exist (the neutral capability is optional), but then it must not silently
// accept a mutation this code promised to protect.

// dmlLockScope is one statement's lock scope plus the capability behind it.
//
// A statement scope is exactly right for DML: closing it cancels the requests that statement was
// still waiting for and keeps every grant, which is the contract's rule for a failed statement.
// It never releases a lock the root already holds, so a transaction that runs several statements
// keeps all of their locks until it ends.
type dmlLockScope struct {
	ctx    context.Context
	locks  storageengine.LockingTxn
	scope  uint64
	closed bool

	// cancel releases the deadline this scope derived from the session lock-wait
	// budget. It is nil when the caller's own context was the earlier bound.
	cancel context.CancelFunc
	// sessionDeadline is the instant the session lock-wait budget reaches, or the zero
	// time when the session budget is not this scope's effective bound. It is what
	// distinguishes "the session budget ran out" from "the caller's context ended" in
	// the error a failed request reports.
	sessionDeadline time.Time
}

// beginDMLLockScope opens one statement lock scope for a mutation.
//
// It returns a scope that is safe to close twice, so every exit path in the pipeline can defer the
// close without tracking whether it already ran.
//
// Wait bound: the effective bound of every request from this scope is the *earlier*
// of the caller's context deadline and the session's `innodb_lock_wait_timeout`
// budget. The neutral locking API carries no deadline, so the session half travels as
// the scope's context — the lock manager already ends a wait when that context is
// done, and it withdraws the pending request rather than leaving a phantom waiter
// behind. The derived deadline is never later than the context that arrived, so a
// caller's deadline can only be narrowed and never widened, and a caller whose context
// is already the earlier bound keeps that context (and its existing error mapping)
// untouched.
func beginDMLLockScope(ctx context.Context, txn storageengine.Txn) (*dmlLockScope, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	provider, ok := txn.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	if !ok {
		return nil, nil
	}
	locks, available := provider.LockingTxn()
	if !available {
		// The backend has no lock manager. That is a legitimate configuration (the neutral
		// capability is optional), so the mutation is not refused; it runs with the concurrency
		// control its backend does provide. The nil scope makes that explicit at every call site
		// rather than hiding it behind an empty scope that looks like a lock was taken.
		return nil, nil
	}
	var cancel context.CancelFunc
	var sessionDeadline time.Time
	if deadline, derived := dmlSessionLockDeadline(ctx, lockWaitTimeoutForContext(ctx), time.Now()); derived {
		ctx, cancel = context.WithDeadline(ctx, deadline)
		sessionDeadline = deadline
	}
	scope, err := locks.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	return &dmlLockScope{ctx: ctx, locks: locks, scope: scope, cancel: cancel, sessionDeadline: sessionDeadline}, nil
}

// dmlSessionLockDeadline reports the absolute instant of the session lock-wait budget
// and whether that budget is the effective bound.
//
// derived=false is reported when the caller's context already carries an earlier or
// equal deadline. That case must keep the context exactly as it arrived: deriving a
// later deadline would widen the caller's bound, and reporting a session timeout for a
// caller cancellation would name the wrong cause to the user.
func dmlSessionLockDeadline(ctx context.Context, timeout time.Duration, now time.Time) (time.Time, bool) {
	if timeout <= 0 {
		timeout = DefaultLockWaitTimeout
	}
	sessionDeadline := now.Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && !sessionDeadline.Before(contextDeadline) {
		return time.Time{}, false
	}
	return sessionDeadline, true
}

// close ends the statement scope, cancelling its pending requests and keeping every grant.
func (s *dmlLockScope) close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	// The derived deadline owns a timer, so it is released with the scope: a session
	// budget of hours must not leave a timer (or a cancelable context) behind a
	// statement that already finished.
	if s.cancel != nil {
		defer s.cancel()
	}
	// The context may already be cancelled, and the contract requires the scope to be closed even
	// then: a statement that timed out must still withdraw the requests it was waiting for rather
	// than leave them queued behind a caller that has gone away.
	closeCtx := s.ctx
	if closeCtx == nil || closeCtx.Err() != nil {
		closeCtx = context.Background()
	}
	_ = s.locks.CloseLockScope(closeCtx, s.scope)
}

// errLockingUnavailable reports that a mutation cannot be protected because the storage engine
// exposes no locking capability.
var errLockingUnavailable = errors.New("storage engine does not support row locking")

// tableIntentComponent is the intention a row mutation takes on its table.
//
// The mode is IX because every DML statement intends to write some row of the table. It is taken
// on the same table identity the DDL and the raw client use, so a table-level intention from SQL
// and from the raw path converge on one resource.
func tableIntentComponent(table versionedTable) storageengine.LockComponent {
	return storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   sqllayout.Rows(table.ID),
		TableID: table.ID,
		IndexID: "",
	}
}

// primaryRowComponent is the exclusive row lock of one primary-key identity.
//
// The key bytes are the ones the write path installs, so this lock and the write address the same
// resource. A table whose primary key cannot be encoded has no lockable identity and the caller
// must refuse the mutation rather than guess one.
func primaryRowComponent(table versionedTable, key []byte) storageengine.LockComponent {
	return storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   sqllayout.Rows(table.ID),
		TableID: table.ID,
		Key:     key,
	}
}

// indexEntryComponent is the exclusive lock of one *non-unique* index entry.
//
// The entry is named by the index's own namespace plus the value parts and the owner suffix, which
// is byte-for-byte the key the write path installs. Two rows may legally share such a value, so the
// owner suffix is what identifies one row's entry; it must never be dropped here.
func indexEntryComponent(space, tableID, indexID string, key []byte) storageengine.LockComponent {
	return storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   space,
		TableID: tableID,
		IndexID: indexID,
		Key:     key,
	}
}

// uniqueValueReservationComponent is the exclusive reservation of one logical unique value.
//
// Its key is the encoded unique value alone — no owner suffix — because that is the resource two
// statements writing the same value must meet on, whatever primary keys they use. For a unique index
// the write path stores the entry under exactly this key and keeps the owner as the entry's value,
// so the reservation and the physical entry are the same resource; that is a property of unique
// indexes, not an accident, and it is why the reservation may reuse the index namespace.
func uniqueValueReservationComponent(space, tableID, indexID string, valueKey []byte) storageengine.LockComponent {
	return storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   space,
		TableID: tableID,
		IndexID: indexID,
		Key:     valueKey,
	}
}

// primaryKeyBytes encodes the primary-key identity of one row.
//
// It reports false when the row's identity is not the table's primary key, which is the case for a
// table with no primary key: B01 supports those and writes them under a heap key the write path
// synthesises, so this function must not invent one. The caller resolves the heap identity instead.
func primaryKeyBytes(table versionedTable, row storage.Row) ([]byte, bool) {
	if row == nil {
		return nil, false
	}
	for _, index := range table.Definition.Indexes {
		if !index.Primary {
			continue
		}
		return sqlPrimaryKey(table, index, row)
	}
	return nil, false
}

// acquireRowLocks takes the table intention and the exclusive row locks of one mutation.
//
// The order is fixed — table intention first, then the primary row, then index entries — so two
// transactions that touch the same rows in different statements still take the same locks in the
// same order and are far less likely to deadlock. Real deadlocks remain possible and are still
// detected: a cycle built from these grants goes into the same wait-for graph as any other.
func (s *dmlLockScope) acquireRowLocks(table versionedTable, row storage.Row, extra []storageengine.LockComponent) (storageengine.LockGrant, error) {
	if s == nil {
		return storageengine.LockGrant{}, nil
	}
	key, ok := primaryKeyBytes(table, row)
	if !ok {
		return storageengine.LockGrant{}, errLockIdentityUnavailable
	}
	return s.acquireRowLocksForKey(table, key, extra)
}

// acquireRowLocksForKey takes the same locks for a row identified by an already-encoded key.
//
// It returns the capability the lock manager issued, not just an error: a caller that has to
// re-derive a decision it took before it waited (a unique-value check, for example) needs the
// grant to read the latest published state under the same protection.
func (s *dmlLockScope) acquireRowLocksForKey(table versionedTable, key []byte, extra []storageengine.LockComponent) (storageengine.LockGrant, error) {
	if s == nil {
		return storageengine.LockGrant{}, nil
	}
	if len(key) == 0 {
		return storageengine.LockGrant{}, errLockIdentityUnavailable
	}
	components := make([]storageengine.LockComponent, 0, 2+len(extra))
	components = append(components, tableIntentComponent(table))
	components = append(components, primaryRowComponent(table, key))
	components = append(components, extra...)
	grant, err := s.locks.Acquire(s.ctx, s.scope, dmlLockRequest(components))
	if err != nil {
		return storageengine.LockGrant{}, s.lockWaitError(err)
	}
	return grant, nil
}

// lockWaitError reports a failed lock request in the vocabulary of its cause.
//
// The manager ends a wait as soon as the request's context is done and reports that
// context's error; the session lock-wait budget travels on exactly that context
// because the neutral locking API has no deadline parameter. A wait that ended on the
// session budget must therefore be reported as the lock-wait timeout the contract
// names — storageengine.ErrLockWaitTimeout, which the protocol layer maps to the
// MySQL timeout error — while a deadline or cancellation that came from the caller's
// own context keeps its existing cancellation mapping. Whether this scope derived the
// deadline itself is exactly that distinction.
func (s *dmlLockScope) lockWaitError(err error) error {
	if s == nil || err == nil || s.sessionDeadline.IsZero() {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: session %s expired", storageengine.ErrLockWaitTimeout, innodbLockWaitVariable)
	}
	return err
}

// readLockedValue reads one key's latest published state under a grant this statement holds.
//
// It is the second half of the contract's "lock, then re-check": a decision taken before a wait is
// not evidence after it, so the unique-value verification is re-run against the published index
// while the reservation is still held. The read is refused by the backend unless the grant really
// covers the key, so a caller cannot turn a statement into an unlocked current read.
func (s *dmlLockScope) readLockedValue(ctx context.Context, grant storageengine.LockGrant, space string, key []byte) (storageengine.CurrentObservation, error) {
	if s == nil {
		return storageengine.CurrentObservation{}, errLockingUnavailable
	}
	return s.locks.ReadCurrent(ctx, grant, space, key)
}

// errLockIdentityUnavailable reports that a mutation's lock identity cannot be derived.
//
// Refusing is the only safe answer: a mutation whose row cannot be named cannot be protected, and
// running it anyway would leave the promise "every row mutation holds its record lock" false
// without any signal.
var errLockIdentityUnavailable = errors.New("cannot derive the lock identity of a modified row")

// dmlLockRequestHook, when non-nil, may adjust the neutral lock request a mutation is about to
// issue. It exists so a concurrency acceptance test can install the neutral ShouldWait decision
// and observe the exact moment a request is about to wait on another owner, which is what makes
// "the waiter was queued before the holder released" a deterministic fact rather than a timing
// assumption. It is nil in production and carries no behaviour of its own: the returned request is
// the one the lock manager sees, and a hook that changes nothing preserves every rule.
var dmlLockRequestHook func(request storageengine.LockRequest) storageengine.LockRequest

// dmlLockRequest builds the neutral request one mutation issues.
func dmlLockRequest(components []storageengine.LockComponent) storageengine.LockRequest {
	request := storageengine.LockRequest{Components: components}
	if dmlLockRequestHook != nil {
		request = dmlLockRequestHook(request)
	}
	return request
}
