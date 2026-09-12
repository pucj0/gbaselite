package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// subqueryRunner evaluates subqueries for one statement against the statement's
// parent snapshot transaction. A subquery therefore observes exactly the same
// snapshot as its statement and never the statement's uncommitted child writes,
// and it creates no transaction of its own.
type subqueryRunner struct {
	engine  *Engine
	session *Session
	ctx     context.Context
	tx      storageengine.Txn
	// ctes holds the statement-local WITH relations currently in scope.
	ctes map[string]cteRelation
}

func (r *subqueryRunner) resultMemoryLimit() int64 {
	if r.session != nil && r.session.query != nil && r.session.query.options.ResultMemoryBytes > 0 {
		return r.session.query.options.ResultMemoryBytes
	}
	return 16 << 20
}

// rows binds and executes one subquery, materializing at most limit rows
// (limit <= 0 means unlimited).
func (r *subqueryRunner) rows(query parser.Query, limit int) ([]Column, [][]any, error) {
	bound, err := bindSubqueryQuery(r.ctx, r.tx, r.session, query)
	if err != nil {
		return nil, nil, err
	}
	input := bound.Input
	if limit > 0 {
		input = physical.Limit[[]any]{Input: input, Count: limit}
	}
	rows := make([][]any, 0, 1)
	used := int64(0)
	limitBytes := r.resultMemoryLimit()
	err = input.Run(r.ctx, func(row []any) error {
		var err error
		used, err = checkResultMemory(limitBytes, used, row)
		if err != nil {
			return err
		}
		rows = append(rows, append([]any(nil), row...))
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return bound.Columns, rows, nil
}

// materializeSubqueries replaces every subquery in expression with the value it
// produced for the current row. The caller published the correlation scope, so a
// subquery that references an outer column binds and runs against that row.
func (r *subqueryRunner) materializeSubqueries(expression parser.Expr) (parser.Expr, error) {
	if expression == nil || !expressionHasSubquery(expression) {
		return expression, nil
	}
	switch value := expression.(type) {
	case parser.ScalarSubquery:
		if value.Query == nil {
			return nil, errors.New("scalar subquery is missing its SELECT")
		}
		columns, rows, err := r.rows(value.Query, 0)
		if err != nil {
			return nil, err
		}
		if len(columns) != 1 {
			return nil, errors.New("scalar subquery must return exactly one column")
		}
		if len(rows) > 1 {
			return nil, errors.New("scalar subquery returned more than one row")
		}
		if len(rows) == 0 {
			return queryResultLiteral(nil), nil
		}
		return queryResultLiteral(rows[0][0]), nil
	case parser.ExistsExpr:
		if value.Query == nil {
			return nil, errors.New("EXISTS is missing its SELECT")
		}
		columns, rows, err := r.rows(value.Query, 1)
		if err != nil {
			return nil, err
		}
		if len(columns) == 0 {
			return nil, errors.New("EXISTS subquery must return at least one column")
		}
		return queryResultLiteral(len(rows) > 0), nil
	case parser.InExpr:
		value.Values = append([]parser.Expr(nil), value.Values...)
		var err error
		value.Value, err = r.materializeSubqueries(value.Value)
		if err != nil {
			return nil, err
		}
		for index := range value.Values {
			value.Values[index], err = r.materializeSubqueries(value.Values[index])
			if err != nil {
				return nil, err
			}
		}
		if value.Subquery == nil {
			return value, nil
		}
		columns, rows, err := r.rows(value.Subquery, 0)
		if err != nil {
			return nil, err
		}
		if len(columns) == 0 {
			return nil, errors.New("IN subquery must return at least one column")
		}
		value.Values = make([]parser.Expr, 0, len(rows))
		for _, row := range rows {
			value.Values = append(value.Values, queryResultExpression(row))
		}
		value.Subquery = nil
		return value, nil
	case parser.BinaryExpr:
		var err error
		value.Left, err = r.materializeSubqueries(value.Left)
		if err != nil {
			return nil, err
		}
		value.Right, err = r.materializeSubqueries(value.Right)
		return value, err
	case parser.UnaryExpr:
		inner, err := r.materializeSubqueries(value.Value)
		value.Value = inner
		return value, err
	case parser.BetweenExpr:
		var err error
		value.Value, err = r.materializeSubqueries(value.Value)
		if err != nil {
			return nil, err
		}
		value.Lower, err = r.materializeSubqueries(value.Lower)
		if err != nil {
			return nil, err
		}
		value.Upper, err = r.materializeSubqueries(value.Upper)
		return value, err
	case parser.IsExpr:
		var err error
		value.Value, err = r.materializeSubqueries(value.Value)
		if err != nil {
			return nil, err
		}
		value.Target, err = r.materializeSubqueries(value.Target)
		return value, err
	case parser.FunctionExpr:
		value.Args = append([]parser.Expr(nil), value.Args...)
		for index := range value.Args {
			inner, err := r.materializeSubqueries(value.Args[index])
			if err != nil {
				return nil, err
			}
			value.Args[index] = inner
		}
		return value, nil
	case parser.RowExpr:
		value.Values = append([]parser.Expr(nil), value.Values...)
		for index := range value.Values {
			inner, err := r.materializeSubqueries(value.Values[index])
			if err != nil {
				return nil, err
			}
			value.Values[index] = inner
		}
		return value, nil
	case parser.IntervalExpr:
		inner, err := r.materializeSubqueries(value.Value)
		value.Value = inner
		return value, err
	case parser.CaseExpr:
		value.Whens = append([]parser.CaseWhen(nil), value.Whens...)
		var err error
		value.Operand, err = r.materializeSubqueries(value.Operand)
		if err != nil {
			return nil, err
		}
		for index := range value.Whens {
			value.Whens[index].When, err = r.materializeSubqueries(value.Whens[index].When)
			if err != nil {
				return nil, err
			}
			value.Whens[index].Then, err = r.materializeSubqueries(value.Whens[index].Then)
			if err != nil {
				return nil, err
			}
		}
		value.Else, err = r.materializeSubqueries(value.Else)
		return value, err
	default:
		return expression, nil
	}
}

// evaluateExprWithSubqueries is the Txn-backed counterpart of the legacy
// store-based evaluator: it publishes the current row as a correlation scope,
// rewrites the subqueries for that row, then evaluates the expression.
func (r *subqueryRunner) evaluateExprWithSubqueries(expression parser.Expr, table *storage.Table, row storage.Row, session *Session) (any, error) {
	scope := make(map[string]any)
	if table != nil {
		for index, column := range table.ColumnsView() {
			if index >= len(row) {
				break
			}
			value := jsonColumnValue(column, row[index])
			scope[column.Name] = value
			scope[stripQualifier(column.Name)] = value
		}
	}
	session.correlationScopes = append(session.correlationScopes, scope)
	defer func() { session.correlationScopes = session.correlationScopes[:len(session.correlationScopes)-1] }()
	materialized, err := r.materializeSubqueries(expression)
	if err != nil {
		return nil, err
	}
	return evaluateExprWithLookup(materialized, expressionLookup(session, table, row))
}

// expressionLookup is the shared identifier resolver for expression evaluation.
func expressionLookup(session *Session, table *storage.Table, row storage.Row) func(string) (any, error) {
	return func(name string) (any, error) {
		if name == sessionLookupIdentifier {
			return session, nil
		}
		if session != nil && strings.EqualFold(name, "LAST_INSERT_ID()") {
			return int64(session.LastInsertID), nil
		}
		if table != nil {
			if index, ok := queryColumnIndex(table, name); ok {
				return jsonColumnValue(table.ColumnsView()[index], row[index]), nil
			}
		}
		if correlated, exists := correlationScopeValue(session, name); exists {
			return correlated, nil
		}
		return nil, fmt.Errorf("unknown column %s", name)
	}
}
