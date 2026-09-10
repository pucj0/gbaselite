package executor

import (
	"gbaselite/parser"
	"gbaselite/storage"
	"strings"
)

// A nil mask means decode all columns. Fall back for expressions whose column
// dependencies are not known here; never infer dependencies from SQL text.
func sqlProjectionMask(statement parser.Select, schema *storage.Table) []bool {
	needed := make([]bool, len(schema.ColumnsView()))
	var visit func(parser.Expr) bool
	visit = func(expr parser.Expr) bool {
		switch v := expr.(type) {
		case nil, parser.LiteralExpr:
			return true
		case parser.Identifier:
			p, ok := queryColumnIndex(schema, v.Name)
			if !ok {
				return false
			}
			needed[p] = true
			return true
		case parser.BinaryExpr:
			return visit(v.Left) && visit(v.Right)
		case parser.UnaryExpr:
			return visit(v.Value)
		case parser.BetweenExpr:
			return visit(v.Value) && visit(v.Lower) && visit(v.Upper)
		case parser.IsExpr:
			return visit(v.Value) && visit(v.Target)
		case parser.InExpr:
			if v.Subquery != nil || !visit(v.Value) {
				return false
			}
			for _, item := range v.Values {
				if !visit(item) {
					return false
				}
			}
			return true
		case parser.FunctionExpr:
			for _, arg := range v.Args {
				if !visit(arg) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	if !visit(statement.Where) {
		return nil
	}
	if !visit(statement.Having) {
		return nil
	}
	for _, group := range statement.GroupBy {
		resolved := group
		for _, item := range statement.Items {
			if item.Alias != "" && strings.EqualFold(item.Alias, group) {
				resolved = item.Expression
				break
			}
		}
		expr, err := parser.ParseExpression(resolved)
		if err != nil || !visit(expr) {
			return nil
		}
	}
	for _, item := range statement.Items {
		if item.Expression == "*" {
			return nil
		}
		expr, err := parser.ParseExpression(item.Expression)
		if err != nil || !visit(expr) {
			return nil
		}
	}
	for _, order := range statement.OrderBy {
		alias := false
		for _, item := range statement.Items {
			if item.Alias != "" && strings.EqualFold(item.Alias, order.Column) {
				alias = true
				break
			}
		}
		if !alias && !visit(parser.Identifier{Name: order.Column}) {
			return nil
		}
	}
	return needed
}
