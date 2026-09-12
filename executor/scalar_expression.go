package executor

import (
	"gbaselite/parser"
	"strings"
)

// scalarExpressionSupported excludes subqueries and aggregates from row
// evaluation. The legacy cold-read guard relies on subqueries being rejected
// here, so the MVCC projection binder uses rowExpressionSupported instead.
func scalarExpressionSupported(expression parser.Expr) bool {
	return expressionSupported(expression, false)
}

// rowExpressionSupported additionally allows subqueries, which the statement
// evaluator resolves per row against the statement snapshot transaction.
func rowExpressionSupported(expression parser.Expr) bool {
	return expressionSupported(expression, true)
}

func expressionSupported(expression parser.Expr, subqueries bool) bool {
	switch value := expression.(type) {
	case nil, parser.Identifier, parser.LiteralExpr:
		return true
	case parser.ScalarSubquery, parser.ExistsExpr:
		return subqueries
	case parser.BinaryExpr:
		return expressionSupported(value.Left, subqueries) && expressionSupported(value.Right, subqueries)
	case parser.UnaryExpr:
		return expressionSupported(value.Value, subqueries)
	case parser.InExpr:
		if value.Subquery != nil && !subqueries {
			return false
		}
		if !expressionSupported(value.Value, subqueries) {
			return false
		}
		for _, item := range value.Values {
			if !expressionSupported(item, subqueries) {
				return false
			}
		}
		return true
	case parser.BetweenExpr:
		return expressionSupported(value.Value, subqueries) && expressionSupported(value.Lower, subqueries) && expressionSupported(value.Upper, subqueries)
	case parser.IsExpr:
		return expressionSupported(value.Value, subqueries) && expressionSupported(value.Target, subqueries)
	case parser.FunctionExpr:
		switch strings.ToUpper(value.Name) {
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
			return false
		}
		for _, argument := range value.Args {
			if !expressionSupported(argument, subqueries) {
				return false
			}
		}
		return true
	case parser.IntervalExpr:
		return expressionSupported(value.Value, subqueries)
	case parser.RowExpr:
		for _, item := range value.Values {
			if !expressionSupported(item, subqueries) {
				return false
			}
		}
		return true
	case parser.CaseExpr:
		if !expressionSupported(value.Operand, subqueries) || !expressionSupported(value.Else, subqueries) {
			return false
		}
		for _, branch := range value.Whens {
			if !expressionSupported(branch.When, subqueries) || !expressionSupported(branch.Then, subqueries) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
