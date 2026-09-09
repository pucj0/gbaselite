package executor

import (
	"gbaselite/parser"
	"gbaselite/storage"
)

// Common numeric metadata must describe every branch, including branches that
// are skipped during evaluation and values sent through prepared result packets.
func commonExpressionType(expressions []parser.Expr, table *storage.Table, columns []storage.Column, session *Session) (storage.DataType, error) {
	var result storage.DataType
	for _, expression := range expressions {
		if literal, ok := expression.(parser.LiteralExpr); ok && literal.Value.Kind == parser.LiteralNull {
			continue
		}
		typ, err := expressionTypeWithSession(expression, table, columns, session)
		if err != nil {
			return "", err
		}
		if result == "" {
			result = typ
			continue
		}
		if typ == result {
			continue
		}
		if !isNumericResultType(result) || !isNumericResultType(typ) {
			result = storage.TypeVarchar
			continue
		}
		switch {
		case result == storage.TypeDouble || result == storage.TypeFloat || typ == storage.TypeDouble || typ == storage.TypeFloat:
			result = storage.TypeDouble
		case result == storage.TypeDecimal || typ == storage.TypeDecimal:
			result = storage.TypeDecimal
		default:
			result = storage.TypeBigInt
		}
	}
	if result == "" {
		return storage.TypeVarchar, nil
	}
	return result, nil
}

func isNumericResultType(typ storage.DataType) bool {
	switch typ {
	case storage.TypeInt, storage.TypeBigInt, storage.TypeBoolean, storage.TypeDecimal, storage.TypeFloat, storage.TypeDouble:
		return true
	}
	return false
}
