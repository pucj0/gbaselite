package executor

import (
	"gbaselite/parser"
	"gbaselite/storage"
	"strings"
)

// Bind names once, but retain the shared SQL evaluator for NULL, numeric and
// collation semantics. Plans are query-local and never survive schema changes.
func bindSQLFilter(expr parser.Expr, schema *storage.Table, session *Session) func(storage.Row) (any, error) {
	fallback := func(row storage.Row) (any, error) { return evaluateExprWithContext(expr, schema, row, session, nil) }
	if !safeMutationIndexExpression(expr, schema) {
		return fallback
	}
	positions := make(map[string]int)
	var bind func(parser.Expr) bool
	bind = func(e parser.Expr) bool {
		switch v := e.(type) {
		case parser.LiteralExpr:
			return true
		case parser.Identifier:
			if v.Name == sessionLookupIdentifier || strings.EqualFold(v.Name, "LAST_INSERT_ID()") {
				return false
			}
			p, ok := queryColumnIndex(schema, v.Name)
			if !ok {
				return false
			}
			positions[v.Name] = p
			return true
		case parser.BinaryExpr:
			return bind(v.Left) && bind(v.Right)
		}
		return false
	}
	if !bind(expr) {
		return fallback
	}
	columns := schema.ColumnsView()
	return func(row storage.Row) (any, error) {
		if err := checkQuery(session); err != nil {
			return nil, err
		}
		return evaluateExprWithLookup(expr, func(name string) (any, error) {
			if name == sessionLookupIdentifier {
				return session, nil
			}
			p := positions[name]
			return jsonColumnValue(columns[p], row[p]), nil
		})
	}
}
