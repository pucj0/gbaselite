package executor

import (
	"gbaselite/physical"
)

// boundQuery separates binding from execution. Intermediate relational results
// stay in the pipeline; only the ownership boundary constructs Result rows.
type boundQuery struct {
	Columns []Column
	Input   physical.Operator[[]any]
}

func collectBoundQuery(session *Session, q *boundQuery, stream bool) (*Result, error) {
	result := &Result{Columns: q.Columns}
	run := func(y func([]any) error) error { return q.Input.Run(operatorContext(session), y) }
	if stream {
		result.StreamRows = run
		return result, nil
	}
	used := int64(0)
	err := run(func(row []any) error {
		var err error
		used, err = checkResultMemory(session.query.options.ResultMemoryBytes, used, row)
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
func semanticInput(columns []Column, input physical.Operator[[]any]) physical.Operator[[]any] {
	return physical.Projection[[]any, []any]{Input: input, Project: func(row []any) ([]any, error) {
		copied := false
		for i, c := range columns {
			if value, changed := resultSemanticValue(c, row[i]); changed {
				if !copied {
					row = append([]any(nil), row...)
					copied = true
				}
				row[i] = value
			}
		}
		return row, nil
	}}
}

// discardOutput preserves expression/error evaluation for blocking LIMIT 0.
func discardOutput(input physical.Operator[[]any]) physical.Operator[[]any] {
	return physical.Filter[[]any]{Input: input, Predicate: func([]any) (bool, error) { return false, nil }}
}

// Used only at scalar and external materialized-result boundaries.
func boundResult(session *Session, r *Result) *boundQuery {
	return &boundQuery{r.Columns, resultOperator(session.query, r)}
}
