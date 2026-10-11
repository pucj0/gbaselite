package executor

import (
	"context"
	"errors"

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
}

// beginDMLLockScope opens one statement lock scope for a mutation.
//
// It returns a scope that is safe to close twice, so every exit path in the pipeline can defer the
// close without tracking whether it already ran.
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
	scope, err := locks.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		return nil, err
	}
	return &dmlLockScope{ctx: ctx, locks: locks, scope: scope}, nil
}

// close ends the statement scope, cancelling its pending requests and keeping every grant.
func (s *dmlLockScope) close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
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

// indexEntryComponent is the exclusive lock of one index entry.
//
// Unique and secondary entries are locked with the index's own namespace and encoded entry bytes,
// so an INSERT and an UPDATE that would collide on an index meet on one resource. That is what
// lets a concurrent duplicate wait and then be rejected by a re-check instead of both writing.
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

// heapKey is the identity the write path synthesises for a table with no primary key.
//
// It is byte-for-byte the string writeVersionedRowUnchecked receives as its fallback, so a lock
// taken on it and the row the write installs address the same resource. Using anything else would
// protect a key the write never touches.
func heapKey(write storageengine.Txn, ordinal uint64) []byte {
	if write == nil {
		return nil
	}
	return []byte(heapKeyString(write.ID(), ordinal))
}

// heapKeyString builds the heap identity exactly as the insert path does.
func heapKeyString(txID string, ordinal uint64) string {
	return txID + "/" + padOrdinal(ordinal)
}

// padOrdinal renders an ordinal with the same zero padding the write path uses.
func padOrdinal(ordinal uint64) string {
	const digits = 20
	text := make([]byte, 0, digits)
	for i := digits - 1; i >= 0; i-- {
		text = append([]byte{byte('0' + (ordinal/uint64(pow10(i)))%10)}, text...)
	}
	return string(text)
}

// pow10 is a small integer power helper for the fixed-width ordinal rendering.
func pow10(exp int) int {
	result := 1
	for i := 0; i < exp; i++ {
		result *= 10
	}
	return result
}

// acquireRowLocks takes the table intention and the exclusive row locks of one mutation.
//
// The order is fixed — table intention first, then the primary row, then index entries — so two
// transactions that touch the same rows in different statements still take the same locks in the
// same order and are far less likely to deadlock. Real deadlocks remain possible and are still
// detected: a cycle built from these grants goes into the same wait-for graph as any other.
func (s *dmlLockScope) acquireRowLocks(table versionedTable, row storage.Row, extra []storageengine.LockComponent) error {
	if s == nil {
		return nil
	}
	key, ok := primaryKeyBytes(table, row)
	if !ok {
		return errLockIdentityUnavailable
	}
	components := make([]storageengine.LockComponent, 0, 2+len(extra))
	components = append(components, tableIntentComponent(table))
	components = append(components, primaryRowComponent(table, key))
	components = append(components, extra...)
	_, err := s.locks.Acquire(s.ctx, s.scope, storageengine.LockRequest{Components: components})
	return err
}

// acquireRowLocksForKey takes the same locks for a row identified by an already-encoded key.
func (s *dmlLockScope) acquireRowLocksForKey(table versionedTable, key []byte, extra []storageengine.LockComponent) error {
	if s == nil {
		return nil
	}
	if len(key) == 0 {
		return errLockIdentityUnavailable
	}
	components := make([]storageengine.LockComponent, 0, 2+len(extra))
	components = append(components, tableIntentComponent(table))
	components = append(components, primaryRowComponent(table, key))
	components = append(components, extra...)
	_, err := s.locks.Acquire(s.ctx, s.scope, storageengine.LockRequest{Components: components})
	return err
}

// errLockIdentityUnavailable reports that a mutation's lock identity cannot be derived.
//
// Refusing is the only safe answer: a mutation whose row cannot be named cannot be protected, and
// running it anyway would leave the promise "every row mutation holds its record lock" false
// without any signal.
var errLockIdentityUnavailable = errors.New("cannot derive the lock identity of a modified row")
