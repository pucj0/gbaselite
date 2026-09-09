package executor

import "gbaselite/parser"

func expressionHasSubquery(expr parser.Expr) bool {
	switch v := expr.(type) {
	case parser.ScalarSubquery, parser.ExistsExpr:
		return true
	case parser.BinaryExpr:
		return expressionHasSubquery(v.Left) || expressionHasSubquery(v.Right)
	case parser.UnaryExpr:
		return expressionHasSubquery(v.Value)
	case parser.InExpr:
		if v.Subquery != nil || expressionHasSubquery(v.Value) {
			return true
		}
		for _, arg := range v.Values {
			if expressionHasSubquery(arg) {
				return true
			}
		}
	case parser.BetweenExpr:
		return expressionHasSubquery(v.Value) || expressionHasSubquery(v.Lower) || expressionHasSubquery(v.Upper)
	case parser.IsExpr:
		return expressionHasSubquery(v.Value) || expressionHasSubquery(v.Target)
	case parser.FunctionExpr:
		for _, arg := range v.Args {
			if expressionHasSubquery(arg) {
				return true
			}
		}
	case parser.RowExpr:
		for _, arg := range v.Values {
			if expressionHasSubquery(arg) {
				return true
			}
		}
	case parser.IntervalExpr:
		return expressionHasSubquery(v.Value)
	case parser.CaseExpr:
		if expressionHasSubquery(v.Operand) || expressionHasSubquery(v.Else) {
			return true
		}
		for _, branch := range v.Whens {
			if expressionHasSubquery(branch.When) || expressionHasSubquery(branch.Then) {
				return true
			}
		}
	}
	return false
}

func atomicInsert(statement parser.Insert) bool {
	return statement.Replace || statement.Select != nil || len(statement.Values) > 1
}
