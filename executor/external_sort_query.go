package executor

import (
	"errors"
	"fmt"
	"strings"

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
	q := session.query
	run := func(yield func([]any) error) error {
		sorter, err := newExternalRowSorter(q, compare)
		if err != nil {
			return err
		}
		defer sorter.Close()
		if err = visit(sorter.Add); err != nil {
			return err
		}
		seen, emitted := 0, 0
		err = sorter.Finish(func(row []any) error {
			if seen < offset {
				seen++
				return nil
			}
			if limit >= 0 && emitted >= limit {
				return errBudgetedRowsDone
			}
			emitted++
			if len(row) < len(columns) {
				return errors.New("sort projection has fewer values than result columns")
			}
			return yield(row[:len(columns)])
		})
		if errors.Is(err, errBudgetedRowsDone) {
			return nil
		}
		return err
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
		if split.options.SortMemoryBytes < 64<<10 {
			return fmt.Errorf("%w: DISTINCT requires at least 131072 bytes of sort memory", ErrQueryResourceLimit)
		}
		first, err := newExternalRowSorter(&split, func(a, b []any) int { return strings.Compare(a[0].(string), b[0].(string)) })
		if err != nil {
			return err
		}
		defer first.Close()
		second, err := newExternalRowSorter(&split, func(a, b []any) int {
			left, right := a[0].(uint64), b[0].(uint64)
			if left < right {
				return -1
			}
			if left > right {
				return 1
			}
			return 0
		})
		if err != nil {
			return err
		}
		defer second.Close()
		ordinal := uint64(0)
		err = visitQueryResult(q, source, func(row []any) error {
			values := make([]any, 2, len(row)+2)
			values[0] = groupedRowKey(row, session)
			values[1] = ordinal
			ordinal++
			return first.Add(append(values, row...))
		})
		if err != nil {
			return err
		}
		previous := ""
		hasPrevious := false
		err = first.Finish(func(row []any) error {
			key := row[0].(string)
			if hasPrevious && previous == key {
				return nil
			}
			previous, hasPrevious = key, true
			return second.Add(row[1:])
		})
		if err != nil {
			return err
		}
		seen, emitted := 0, 0
		err = second.Finish(func(row []any) error {
			if seen < offset {
				seen++
				return nil
			}
			if limit >= 0 && emitted >= limit {
				return errBudgetedRowsDone
			}
			emitted++
			return yield(row[1:])
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
