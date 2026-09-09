package executor

import (
	"fmt"
	"gbaselite/internal/mysqlcompat"
	"gbaselite/parser"
	"gbaselite/storage"
	"strings"
)

type collatedText struct{ Text, Collation string }

func (v collatedText) String() string { return v.Text }

func compareCollated(left, right any) (int, bool) {
	if left == nil || right == nil {
		return 0, false
	}
	collation := ""
	if v, ok := left.(collatedText); ok {
		collation = v.Collation
		left = v.Text
	}
	if v, ok := right.(collatedText); ok {
		if collation == "" {
			collation = v.Collation
		}
		right = v.Text
	}
	if collation == "" {
		return 0, false
	}
	a, aOK := left.(string)
	b, bOK := right.(string)
	if !aOK || !bOK {
		return 0, false
	}
	return mysqlcompat.CompareStrings(a, b, collation), true
}
func likeCollated(left, right any, session *Session) bool {
	collation := ""
	if v, ok := left.(collatedText); ok {
		collation = v.Collation
	}
	if v, ok := right.(collatedText); ok && collation == "" {
		collation = v.Collation
	}
	if collation == "" && session != nil {
		collation = session.CollationConnection
	}
	a, b := fmt.Sprint(left), fmt.Sprint(right)
	if collation == "" || strings.HasSuffix(strings.ToLower(collation), "_ci") {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	return likeMatchCaseSensitive(a, b)
}

func resultSemanticValue(column Column, value any) (any, bool) {
	if value == nil {
		return nil, false
	}
	if column.jsonValue {
		if _, ok := value.(jsonDocument); !ok {
			return jsonDocument(fmt.Sprint(value)), true
		}
	}
	if column.Collation != "" {
		if _, ok := value.(collatedText); !ok {
			return collatedText{fmt.Sprint(value), column.Collation}, true
		}
	}
	return value, false
}

func inheritExpressionColumn(result *Column, expression parser.Expr, table *storage.Table) {
	if identifier, ok := expression.(parser.Identifier); ok {
		if position, found := queryColumnIndex(table, identifier.Name); found {
			source := table.ColumnsView()[position]
			result.SQLType, result.Collation, result.Length = source.SQLType, source.Collation, source.Length
		}
	}
}
