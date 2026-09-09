package executor

import (
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
	"strconv"
	"strings"
)

func executeBudgetedExpressionOrder(store *storage.Store, session *Session, statement parser.Select, table *storage.Table, predicate storage.Predicate, columns []Column, project func(storage.Row) ([]any, error)) (*Result, error) {
	q := session.query
	return executeBudgetedExpressionOrderWithSource(store, session, statement, table, columns, project, func(yield func(storage.Row) error) error {
		return visitQueryTable(q, table, predicate, yield)
	})
}

func executeBudgetedExpressionOrderWithSource(store *storage.Store, session *Session, statement parser.Select, table *storage.Table, columns []Column, project func(storage.Row) ([]any, error), source func(func(storage.Row) error) error) (*Result, error) {
	expressions := make([]parser.Expr, len(statement.OrderBy))
	positions := make([]int, len(expressions))
	for i, order := range statement.OrderBy {
		positions[i] = -1
		if ordinal, err := strconv.Atoi(strings.TrimSpace(order.Column)); err == nil {
			if ordinal < 1 || ordinal > len(columns) {
				return nil, fmt.Errorf("invalid ORDER BY position %d", ordinal)
			}
			positions[i] = ordinal - 1
			continue
		}
		for j, column := range columns {
			if strings.EqualFold(column.Name, stripQualifier(order.Column)) {
				positions[i] = j
				break
			}
		}
		if positions[i] < 0 {
			var err error
			expressions[i], err = parser.ParseExpression(order.Column)
			if err != nil {
				return nil, err
			}
		}
	}
	compare := func(left, right []any) int {
		for i, order := range statement.OrderBy {
			cmp := session.Compare(left[len(columns)+i], right[len(columns)+i])
			if cmp != 0 {
				if order.Desc {
					return -cmp
				}
				return cmp
			}
		}
		return 0
	}
	visit := func(yield func([]any) error) error {
		return source(func(row storage.Row) error {
			values, err := project(row)
			if err != nil {
				return err
			}
			all := make([]any, len(values)+len(expressions))
			copy(all, values)
			for i, expr := range expressions {
				if positions[i] >= 0 {
					all[len(values)+i] = values[positions[i]]
				} else {
					all[len(values)+i], err = evaluateExprWithContext(expr, table, row, session, store)
					if err != nil {
						return err
					}
				}
			}
			return yield(all)
		})
	}
	limit := -1
	if statement.HasLimit {
		limit = statement.Limit
	}
	return executeBudgetedOrder(session, columns, compare, visit, statement.Offset, limit)
}
