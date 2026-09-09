package executor

import (
	"gbaselite/parser"
	"strings"
)

// scalarExpressionSupported excludes subqueries and aggregates from row evaluation.
func scalarExpressionSupported(expression parser.Expr) bool {
	switch value := expression.(type) {
	case nil, parser.Identifier, parser.LiteralExpr:
		return true
	case parser.BinaryExpr:
		return scalarExpressionSupported(value.Left) && scalarExpressionSupported(value.Right)
	case parser.UnaryExpr:
		return scalarExpressionSupported(value.Value)
	case parser.InExpr:
		if value.Subquery != nil || !scalarExpressionSupported(value.Value) {
			return false
		}
		for _, item := range value.Values {
			if !scalarExpressionSupported(item) {
				return false
			}
		}
		return true
	case parser.BetweenExpr:
		return scalarExpressionSupported(value.Value) && scalarExpressionSupported(value.Lower) && scalarExpressionSupported(value.Upper)
	case parser.IsExpr:
		return scalarExpressionSupported(value.Value) && scalarExpressionSupported(value.Target)
	case parser.FunctionExpr:
		switch strings.ToUpper(value.Name) {
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
			return false
		}
		for _, argument := range value.Args {
			if !scalarExpressionSupported(argument) {
				return false
			}
		}
		return true
	case parser.IntervalExpr:
		return scalarExpressionSupported(value.Value)
	case parser.RowExpr:
		for _, item := range value.Values {
			if !scalarExpressionSupported(item) {
				return false
			}
		}
		return true
	case parser.CaseExpr:
		if !scalarExpressionSupported(value.Operand) || !scalarExpressionSupported(value.Else) {
			return false
		}
		for _, branch := range value.Whens {
			if !scalarExpressionSupported(branch.When) || !scalarExpressionSupported(branch.Then) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
