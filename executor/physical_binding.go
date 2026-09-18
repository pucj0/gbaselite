package executor

import (
	"context"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"iter"
)

// accessIterator adapts index probes and table scans to the same Scan operator.
// Pull keeps borrowed buffers alive until Next and unwinds the scan on Close.
type accessRecord struct{ key, value []byte }
type accessIterator struct {
	next   func() (accessRecord, bool)
	stop   func()
	row    accessRecord
	err    error
	closed bool
}

func (i *accessIterator) Next() bool {
	if i.closed {
		return false
	}
	r, ok := i.next()
	i.row = r
	return ok
}
func (i *accessIterator) Key() []byte   { return i.row.key }
func (i *accessIterator) Value() []byte { return i.row.value }
func (i *accessIterator) Err() error    { return i.err }
func (i *accessIterator) Close() error {
	if !i.closed {
		i.closed = true
		i.stop()
		i.row = accessRecord{}
	}
	return nil
}

// openAccessIterator is the byte-access boundary. Common table/range scans
// pass the backend iterator directly; multi-step index probes retain one adapter.
func openAccessIterator(ctx context.Context, tx storageengine.Txn, table versionedTable, access sqlAccessPlan) (storageengine.Iterator, error) {
	switch access.kind {
	case sqlAccessAll:
		return tx.Table(table.ID).Scan(ctx, storageengine.ScanRequest{Unordered: true})
	case sqlAccessRange, sqlAccessOrdered:
		return tx.Table(table.ID).Scan(ctx, storageengine.ScanRequest{Range: access.bounds})
	}
	i := &accessIterator{}
	i.next, i.stop = iter.Pull(func(yield func(accessRecord) bool) {
		i.err = access.scan(ctx, tx, table, func(k, v []byte) error {
			if !yield(accessRecord{k, v}) {
				return errBudgetedRowsDone
			}
			return nil
		})
		if i.err == errBudgetedRowsDone {
			i.err = nil
		}
	})
	return i, nil
}
func bindScan(tx storageengine.Txn, table versionedTable, access sqlAccessPlan, decode func([]byte) (storage.Row, error)) physical.Operator[storage.Row] {
	return physical.Scan[storage.Row]{Plan: &physical.PlanNode{Kind: scanKind(access), Attributes: map[string]string{"table": table.CatalogName, "access": access.kind, "index": access.index}}, Open: func(ctx context.Context) (storageengine.Iterator, error) {
		return openAccessIterator(ctx, tx, table, access)
	}, Decode: func(_, v []byte) (storage.Row, error) { return decode(v) }}
}
func operatorContext(session *Session) context.Context {
	if session != nil && session.query != nil {
		return session.query.context
	}
	return context.Background()
}

type aggregateBinding[A, B any] struct {
	add    func(A) error
	finish func(physical.Yield[B]) error
}

func (a *aggregateBinding[A, B]) Add(r A) error                    { return a.add(r) }
func (a *aggregateBinding[A, B]) Finish(y physical.Yield[B]) error { return a.finish(y) }
func (a *aggregateBinding[A, B]) Close() error                     { return nil }

// Row identity travels with a mutation input, independently of its table codec.
type mutationRow struct {
	key []byte
	row storage.Row
}

// mutationScan is the shared scan/filter/limit stage of every single-table mutation.
//
// It keeps planSQLAccess: the access plan still decides whether the statement scans a
// range, probes an index or falls back to a full scan, so an indexed UPDATE or DELETE
// does not degrade into an unconditional table scan. It builds the operator directly
// rather than routing rows through a callback, and returns it for the caller to
// compose. The scan reads through the parent statement transaction, while the operator
// it feeds writes through the statement child transaction.
func mutationScan(ctx context.Context, tx storageengine.Txn, table versionedTable, schema *storage.Table, session *Session, where parser.Expr, limit int) physical.Operator[mutationRow] {
	access := planSQLAccess(parser.Select{Where: where}, table, schema, session)
	input := physical.Scan[mutationRow]{Plan: &physical.PlanNode{Kind: scanKind(access), Attributes: map[string]string{"table": table.CatalogName, "access": access.kind, "index": access.index}}, Open: func(ctx context.Context) (storageengine.Iterator, error) {
		return openAccessIterator(ctx, tx, table, access)
	}, Decode: func(key, value []byte) (mutationRow, error) {
		if err := checkQuery(session); err != nil {
			return mutationRow{}, err
		}
		row, err := decodeSQLRow(table, value)
		return mutationRow{key, row}, err
	}}
	filter := physical.Filter[mutationRow]{Input: input, Predicate: func(r mutationRow) (bool, error) {
		if where == nil {
			return true, nil
		}
		v, err := evaluateExprWithContext(where, schema, r.row, session, nil)
		return truthy(v), err
	}}
	return physical.Limit[mutationRow]{Input: filter, Count: limit}
}

// updateCandidates projects the shared row source into the unified UPDATE model.
//
// Identity carries the physical storage key the scan observed for the *old* row.
// OldRow is a private copy of the decoded row: a scan iterator may reuse its buffer for
// the next position, and the write path re-reads OldRow to remove the old secondary and
// unique index entries, so it must stay a valid snapshot of the row as it was read. The
// copy also keeps the old values consistent with the identity that addresses them.
func updateCandidates(source physical.Operator[mutationRow], table versionedTable) physical.Operator[UpdateCandidate] {
	return physical.Projection[mutationRow, UpdateCandidate]{Input: source, Project: func(r mutationRow) (UpdateCandidate, error) {
		oldRow := make(storage.Row, len(r.row))
		copy(oldRow, r.row)
		return UpdateCandidate{
			Identity: physical.NewRowIdentity(table.ID, r.key),
			OldRow:   oldRow,
			EvalRow:  oldRow,
		}, nil
	}}
}

// deleteCandidates projects the shared row source into the unified DELETE model.
//
// Identity carries the physical storage key the scan observed for the row, so a deleted row
// is addressed by where it was read rather than by its values. OldRow is a private copy of
// the decoded row for the same reason the UPDATE projection copies it: a scan iterator may
// reuse its buffer for the next position, and the write path re-reads OldRow to remove the
// old secondary and unique index entries.
func deleteCandidates(source physical.Operator[mutationRow], table versionedTable) physical.Operator[DeleteCandidate] {
	return physical.Projection[mutationRow, DeleteCandidate]{Input: source, Project: func(r mutationRow) (DeleteCandidate, error) {
		oldRow := make(storage.Row, len(r.row))
		copy(oldRow, r.row)
		return DeleteCandidate{
			Identity: physical.NewRowIdentity(table.ID, r.key),
			OldRow:   oldRow,
		}, nil
	}}
}

func scanKind(p sqlAccessPlan) string {
	switch p.kind {
	case sqlAccessRange, sqlAccessOrdered:
		return "IndexRangeScan"
	case sqlAccessPoint, sqlAccessUnique:
		return "IndexLookup"
	default:
		if p.index != "" {
			return "IndexScan"
		}
		return "Scan"
	}
}
