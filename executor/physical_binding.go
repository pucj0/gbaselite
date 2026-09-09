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
func bindScan(tx storageengine.Txn, table versionedTable, access mvccAccessPlan, decode func([]byte) (storage.Row, error)) physical.Operator[storage.Row] {
	return physical.Scan[storage.Row]{Open: func(ctx context.Context) (storageengine.Iterator, error) {
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
	}, Decode: func(_, v []byte) (storage.Row, error) { return decode(v) }}
}
func rowSource(ctx context.Context, op physical.Operator[storage.Row]) func(func(storage.Row) error) error {
	return func(y func(storage.Row) error) error { return op.Run(ctx, y) }
}
func sourceOperator(source func(func(storage.Row) error) error) physical.Operator[storage.Row] {
	return physical.Source[storage.Row](func(_ context.Context, y physical.Yield[storage.Row]) error { return source(y) })
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

func runRowModification(ctx context.Context, tx storageengine.Txn, table versionedTable, schema *storage.Table, session *Session, where parser.Expr, limit int, apply func([]byte, storage.Row) error) error {
	access := planMVCCAccess(parser.Select{Where: where}, table, schema, session)
	input := physical.Source[mutationRow](func(ctx context.Context, y physical.Yield[mutationRow]) error {
		return access.scan(ctx, tx, table, func(key, value []byte) error {
			if err := checkQuery(session); err != nil {
				return err
			}
			row, err := decodeMVCCRow(table, value)
			if err != nil {
				return err
			}
			return y(mutationRow{key, row})
		})
	})
	filter := physical.Filter[mutationRow]{Input: input, Predicate: func(r mutationRow) (bool, error) {
		if where == nil {
			return true, nil
		}
		v, err := evaluateExprWithContext(where, schema, r.row, session, nil)
		return truthy(v), err
	}}
	op := physical.Modify[mutationRow, struct{}]{Input: physical.Limit[mutationRow]{Input: filter, Count: limit}, Apply: func(_ context.Context, r mutationRow) (struct{}, error) { return struct{}{}, apply(r.key, r.row) }}
	return op.Run(ctx, func(struct{}) error { return nil })
}
