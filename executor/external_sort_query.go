package executor

import (
	"errors"
	"fmt"
	"strings"

	"context"
	"gbaselite/physical"
	"gbaselite/storage"
)

var errBudgetedRowsDone = errors.New("requested sorted rows emitted")

func querySortEnabled(session *Session) bool {
	return session != nil && session.query != nil && session.query.options.SortMemoryBytes > 0
}

// visitQueryTable checks even rows rejected by WHERE, so an unselective or
// always-false predicate cannot bypass the query deadline.
func visitQueryTable(q *queryControl, table *storage.Table, predicate storage.Predicate, visit func(storage.Row) error) error {
	return table.Visit(nil, func(row storage.Row) error {
		if err := q.check(); err != nil {
			return err
		}
		if predicate != nil && !predicate(row) {
			return nil
		}
		return visit(row)
	})
}

// executeBudgetedOrder receives projected values followed by any hidden ORDER
// BY keys. A lazy streaming result owns no run files until it is consumed.
func executeBudgetedOrder(session *Session, columns []Column, compare func([]any, []any) int, visit func(func([]any) error) error, offset, limit int) (*Result, error) {
	return executeBudgetedOrderWithInput(session, columns, compare, physical.Source[[]any](func(_ context.Context, y physical.Yield[[]any]) error { return visit(y) }), offset, limit)
}
func executeBudgetedOrderWithInput(session *Session, columns []Column, compare func([]any, []any) int, input physical.Operator[[]any], offset, limit int) (*Result, error) {
	q := session.query
	run := func(yield func([]any) error) error {
		sorter := physical.Sort[[]any]{Input: input, New: func() (physical.Sorter[[]any], error) { return newExternalRowSorter(q, compare) }}
		var op physical.Operator[[]any] = physical.Limit[[]any]{Input: sorter, Offset: offset, Count: limit}
		if limit >= 0 {
			op = physical.TopN[[]any]{Sort: sorter, Offset: offset, Count: limit}
		}
		project := physical.Projection[[]any, []any]{Input: op, Project: func(row []any) ([]any, error) {
			if len(row) < len(columns) {
				return nil, errors.New("sort projection has fewer values than result columns")
			}
			return row[:len(columns)], nil
		}}
		return project.Run(operatorContext(session), yield)
	}
	result := &Result{Columns: columns}
	if session.StreamResults {
		result.StreamRows = run
		return result, nil
	}
	used := int64(0)
	err := run(func(row []any) error {
		var err error
		used, err = checkResultMemory(q.options.ResultMemoryBytes, used, row)
		if err != nil {
			return err
		}
		// Dropping hidden keys also prevents them from keeping large payloads alive.
		result.Rows = append(result.Rows, append([]any(nil), row...))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func visitQueryResult(q *queryControl, result *Result, yield func([]any) error) error {
	emit := func(row []any) error {
		if err := q.check(); err != nil {
			return err
		}
		copied := false
		for i, column := range result.Columns {
			if value, changed := resultSemanticValue(column, row[i]); changed {
				if !copied {
					row = append([]any(nil), row...)
					copied = true
				}
				row[i] = value
			}
		}
		return yield(row)
	}
	for _, row := range result.Rows {
		if err := emit(row); err != nil {
			return err
		}
	}
	if result.StreamRows != nil {
		if err := result.StreamRows(emit); err != nil {
			return err
		}
	}
	if result.StreamValues != nil {
		return result.StreamValues(func(row storage.Row) error {
			values := make([]any, len(row))
			for i, value := range row {
				values[i] = value.Interface()
			}
			return emit(values)
		})
	}
	return q.check()
}

// DISTINCT first groups identical collation keys, keeping the earliest source
// row. A second bounded sort restores source ORDER BY/tie order before LIMIT.
// Both sorters share the query's temporary disk accounting.
func executeBudgetedDistinct(session *Session, source *Result, offset, limit int) (*Result, error) {
	q := session.query
	run := func(yield func([]any) error) error {
		split := *q
		split.options.SortMemoryBytes = q.options.SortMemoryBytes / 2
		if q.options.SortMemoryBytes == 0 {
			split.options.SortMemoryBytes = 2 << 20
		}
		if split.options.SortMemoryBytes < 64<<10 {
			return fmt.Errorf("%w: DISTINCT requires at least 131072 bytes of sort memory", ErrQueryResourceLimit)
		}
		op := physical.Distinct[[]any]{Input: resultOperator(q, source), Key: func(row []any) (string, error) { return groupedRowKey(row, session), nil }, NewSort: func(byKey bool) (physical.Sorter[physical.DistinctRow[[]any]], error) {
			compare := func(a, b []any) int {
				if byKey {
					if c := strings.Compare(a[0].(string), b[0].(string)); c != 0 {
						return c
					}
				}
				index := 0
				if byKey {
					index = 1
				}
				left, right := a[index].(uint64), b[index].(uint64)
				if left < right {
					return -1
				}
				if left > right {
					return 1
				}
				return 0
			}
			sorter, err := newExternalRowSorter(&split, compare)
			if err != nil {
				return nil, err
			}
			return &distinctSorter{sorter, byKey}, nil
		}}

		seen, emitted := 0, 0
		err := op.Run(operatorContext(session), func(row []any) error {
			if seen < offset {
				seen++
				return nil
			}
			if limit >= 0 && emitted >= limit {
				return errBudgetedRowsDone
			}
			emitted++
			return yield(row)
		})
		if errors.Is(err, errBudgetedRowsDone) {
			return nil
		}
		return err
	}
	result := &Result{Columns: source.Columns}
	if session.StreamResults {
		result.StreamRows = run
		return result, nil
	}
	used := int64(0)
	err := run(func(row []any) error {
		var err error
		used, err = checkResultMemory(q.options.ResultMemoryBytes, used, row)
		if err != nil {
			return err
		}
		result.Rows = append(result.Rows, append([]any(nil), row...))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// queryMemoryAccount is for materializing operators whose states cannot yet be
// spilled (hash aggregation/windows). Reserve before retaining a new group or
// row, and call Resize for variable-sized aggregate MIN/MAX values.
type queryMemoryAccount struct {
	control     *queryControl
	used, limit int64
	operation   string
}

func newQueryMemoryAccount(session *Session, operation string) *queryMemoryAccount {
	account := &queryMemoryAccount{operation: operation}
	if session != nil {
		account.control = session.query
	}
	if account.control != nil {
		account.limit = account.control.options.ResultMemoryBytes
	}
	return account
}
func (a *queryMemoryAccount) Reserve(bytes int64) error {
	if err := a.control.check(); err != nil {
		return err
	}
	if bytes < 0 {
		return errors.New("negative query memory reservation")
	}
	if a.used > int64(^uint64(0)>>1)-bytes || a.limit > 0 && bytes > a.limit-a.used {
		return fmt.Errorf("%w: %s exceeds %d retained bytes", ErrQueryResourceLimit, a.operation, a.limit)
	}
	a.used += bytes
	return nil
}
func (a *queryMemoryAccount) Resize(previous, next int64) error {
	if next <= previous {
		a.used -= previous - next
		if a.used < 0 {
			a.used = 0
		}
		return a.control.check()
	}
	return a.Reserve(next - previous)
}
func aggregateBucketBytes(groupValues []any, key string, itemCount, nodeCount int) int64 {
	return queryRowBytes(groupValues) + int64(len(key)) + 128 + int64(itemCount+nodeCount)*160
}

// The Result conversion belongs only at protocol/materialized result boundaries.
func resultOperator(q *queryControl, result *Result) physical.Operator[[]any] {
	return physical.Source[[]any](func(_ context.Context, y physical.Yield[[]any]) error { return visitQueryResult(q, result, y) })
}

type distinctSorter struct {
	sorter *externalRowSorter
	byKey  bool
}

func (s *distinctSorter) Add(r physical.DistinctRow[[]any]) error {
	prefix := []any{r.Ordinal}
	if s.byKey {
		prefix = []any{r.Key, r.Ordinal}
	}
	return s.sorter.Add(append(prefix, r.Row...))
}
func (s *distinctSorter) Finish(y func(physical.DistinctRow[[]any]) error) error {
	return s.sorter.Finish(func(r []any) error {
		if s.byKey {
			return y(physical.DistinctRow[[]any]{Key: r[0].(string), Ordinal: r[1].(uint64), Row: r[2:]})
		}
		return y(physical.DistinctRow[[]any]{Ordinal: r[0].(uint64), Row: r[1:]})
	})
}
func (s *distinctSorter) Close() error { return s.sorter.Close() }
