package executor

import (
	"errors"
	"gbaselite/storage"
)

func queryGuardsEnabled(session *Session) bool {
	if session == nil || session.query == nil {
		return false
	}
	q := session.query
	return q.hasDeadline() || q.options.ResultMemoryBytes > 0 || q.options.SortMemoryBytes > 0
}
func executeBudgetedProjection(session *Session, table *storage.Table, predicate storage.Predicate, selected []int, columns []Column, offset, limit int) (*Result, error) {
	q := session.query
	run := func(yield func([]any) error) error {
		seen, emitted := 0, 0
		err := visitQueryTable(q, table, predicate, func(row storage.Row) error {
			if seen < offset {
				seen++
				return nil
			}
			if limit >= 0 && emitted >= limit {
				return errBudgetedRowsDone
			}
			emitted++
			values := make([]any, len(selected))
			for i, p := range selected {
				values[i] = row[p].Interface()
			}
			return yield(values)
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
		result.Rows = append(result.Rows, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
