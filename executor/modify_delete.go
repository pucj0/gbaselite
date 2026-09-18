package executor

import (
	"context"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// This file owns the unified single-table DELETE mutation. A plain DELETE lowers its
// query pipeline into physical.Operator[DeleteCandidate] and hands it to DeleteOperator:
//
//	planSQLAccess -> Scan -> WHERE -> DeleteCandidate -> DeleteOperator -> writeVersionedRow(old -> nil)
//
// What stays outside this operator, deliberately:
//
//   - the access plan and scan (the binding layer keeps planSQLAccess, so an indexed
//     DELETE does not degrade to an unconditional full scan);
//   - the FK action, index and constraint logic, reached through the existing
//     applyForeignKeyActions and writeVersionedRow;
//   - statement transaction commit.
//
// The operator writes only through the statement child transaction it is given, while the
// scan that feeds it reads the parent statement snapshot. That split is what keeps a row
// deleted during the scan from being revisited by the same statement, and it makes a
// mid-statement failure roll the whole statement back.

// DeleteCandidate is one physical target row waiting to be deleted.
//
// Identity is the stable physical row identity the scan observed, which is what makes two
// value-identical heap rows distinct targets and lets a deleted row be addressed by where
// it was read rather than by its values.
type DeleteCandidate struct {
	// Identity is the physical row identity of the target.
	Identity RowIdentity
	// OldRow holds the target table values from the statement snapshot. It must contain
	// the table's own columns only: a multi-table DELETE projects its target window out of
	// a joined row and has to strip the hidden join identity column before the row reaches
	// the delete write, which is what removes secondary index entries for the old values.
	OldRow storage.Row
}

// Target exposes the mutation target identity for dedup and operator input.
func (c DeleteCandidate) Target() (RowIdentity, bool) {
	return c.Identity, c.Identity.IsTarget()
}

// deleteApplyHook, when non-nil, observes every candidate the single-table DELETE path
// deletes. The A05 regression tests install it to prove that a plain DELETE really does
// reach this operator. It is nil in production and carries no behaviour of its own.
var deleteApplyHook func(candidate DeleteCandidate, engine string)

// DeleteOperator deletes every DeleteCandidate produced by its source.
type DeleteOperator struct {
	// Input yields candidates for the rows the statement must delete, in access-plan order.
	Input physical.Operator[DeleteCandidate]

	// Target is the table the rows are deleted from.
	Target versionedTable
	// Write is the statement child transaction. The operator never commits it.
	Write storageengine.Txn
	// Session carries the FK check policy the action path reads.
	Session *Session

	// Result accumulates the statement's affected rows.
	Result ModifyResult
}

// Run deletes every candidate and discards the operator's own output, which is what a
// terminal mutation operator does.
func (o *DeleteOperator) Run(ctx context.Context) error {
	modify := physical.Modify[DeleteCandidate, struct{}]{Input: o.Input, Apply: func(ctx context.Context, candidate DeleteCandidate) (struct{}, error) {
		return struct{}{}, o.Apply(ctx, candidate)
	}}
	return modify.Run(ctx, func(struct{}) error { return nil })
}

// Apply deletes one candidate.
//
// It reuses the existing FK action path and then writes the row away by handing
// writeVersionedRow an old row and no new row, which is exactly what a DELETE means to the
// storage layer: secondary and unique index entries for the old values are removed and the
// row is dropped.
func (o *DeleteOperator) Apply(ctx context.Context, candidate DeleteCandidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deleteApplyHook != nil {
		deleteApplyHook(candidate, o.Target.CatalogName)
	}
	if err := applyForeignKeyActions(ctx, o.Write, o.Session, o.Target, candidate.OldRow, nil, nil, 0); err != nil {
		return err
	}
	if err := writeVersionedRow(ctx, o.Write, o.Target, candidate.Identity.Key, candidate.OldRow, nil, ""); err != nil {
		return err
	}
	o.Result.AffectedRows++
	return nil
}
