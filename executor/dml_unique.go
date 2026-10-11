package executor

import (
	"bytes"
	"context"
	"errors"

	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// dml_unique.go owns the "lock, wait, then re-check" rule of the unique constraint.
//
// A uniqueness decision is a statement about published state, and every decision this path can take
// before it acquires the row's reservation is a statement about the snapshot the statement started
// with. Those two differ exactly when another transaction commits the same unique value while this
// one waits, which is the case the lock exists for. Re-running the check under the reservation is
// therefore not a second opinion on the same data: it is the only check that can see the row the
// waiter was blocked by.

// errSkipInsertRow reports that the locked re-check found the row's unique value taken by a row this
// statement cannot observe, and the statement's IGNORE policy says to skip the row. It is an
// internal control signal, never an error the caller sees: the row is simply not written.
var errSkipInsertRow = errors.New("insert row skipped by the IGNORE duplicate policy")

// verifyUniqueValuesLocked re-runs the uniqueness check against the latest published index state
// while the statement holds the row's unique-value reservations.
//
// The decision depends on whether the conflicting row is observable *in this statement's snapshot*:
//
//   - It is observable: the statement already sees the conflict, so the existing REPLACE,
//     ON DUPLICATE KEY UPDATE and IGNORE machinery owns the case and will act on the row it can
//     read. Nothing is decided here.
//   - It is not observable: the duplicate was published after this statement's snapshot. A plain
//     INSERT reports the permanent duplicate; IGNORE skips the row; REPLACE and ON DUPLICATE KEY
//     UPDATE report a retryable conflict, because rewriting or updating a row the statement never
//     observed would rebase it onto post-readTS state, which the B01 invariant forbids.
//
// Without a lock capability the function does nothing, and deliberately so: an unlocked current
// read is exactly the unsound baseline the current-read contract refuses, so this path keeps the
// backend's own concurrency control and the snapshot-level check it always had.
func (o *InsertOperator) verifyUniqueValuesLocked(ctx context.Context, candidate InsertCandidate, grant storageengine.LockGrant) error {
	if o.Locks == nil || len(grant.Components) == 0 {
		return nil
	}
	table := o.Target.definition
	row := candidate.Values
	ownKey, hasOwnKey := primaryKeyBytes(table, row)
	for _, index := range table.Definition.Indexes {
		if index.Primary || !index.Unique {
			continue
		}
		valueKey, ok := storage.IndexValueKey(index, table.Definition.Columns, row)
		if !ok {
			// A value that does not constrain (a NULL part, an unresolvable column) reserves
			// nothing and therefore cannot conflict.
			continue
		}
		observation, err := o.Locks.readLockedValue(ctx, grant, sqllayout.UniqueIndex(table.ID, index.Name), []byte(valueKey))
		if err != nil {
			return err
		}
		if !observation.Exists {
			continue
		}
		if hasOwnKey && bytes.Equal(observation.Value, ownKey) {
			// The reservation is held by this very row: an in-place rewrite of the row being
			// written, which is not a conflict with another row.
			continue
		}
		visible, err := o.conflictRowVisible(observation.Value)
		if err != nil {
			return err
		}
		if visible {
			continue
		}
		switch {
		case o.Mode.ignore:
			return errSkipInsertRow
		case o.Mode.replace || len(o.Mode.onDuplicate) > 0:
			return storageengine.ErrConflict
		default:
			return storage.ErrDuplicateKey
		}
	}
	return nil
}

// conflictRowVisible reports whether this statement's own transaction can observe the row that owns
// a conflicting unique value.
//
// The entry's value is the owning primary key, which is what the writer stores there. Reading it
// through the statement transaction is what distinguishes "the statement already sees this
// duplicate" from "the duplicate appeared while this statement waited".
func (o *InsertOperator) conflictRowVisible(owner []byte) (bool, error) {
	if len(owner) == 0 {
		return false, nil
	}
	_, exists, err := o.Write.Table(o.Target.definition.ID).Get(owner)
	return exists, err
}
