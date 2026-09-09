package executor

import (
	"gbaselite/parser"
	"gbaselite/storage"
	"strconv"
)

func qualifySchema(source *storage.Table, qualifier string) (*storage.Table, error) {
	columns := append([]storage.Column(nil), source.ColumnsView()...)
	for i := range columns {
		columns[i].Name = qualifier + "." + stripQualifier(columns[i].Name)
	}
	return storage.NewTransientTable("mutation_schema", columns)
}
func mutationIndexScan(table *storage.Table, session *Session, where parser.Expr, schema *storage.Table) *storage.IndexScan {
	if !safeMutationIndexExpression(where, schema) {
		return nil
	}
	plan := planIndexAccess(parser.Select{Where: where}, table, session)
	if plan == nil {
		return nil
	}
	return &plan.Scan
}

// Keep the general scan for expressions requiring runtime evaluation, and
// for unknown/incorrectly qualified columns so an empty index interval cannot
// hide an error that the original scan would report.
func safeMutationIndexExpression(expr parser.Expr, schema *storage.Table) bool {
	switch value := expr.(type) {
	case parser.LiteralExpr:
		return true
	case parser.Identifier:
		_, ok := queryColumnIndex(schema, value.Name)
		return ok
	case parser.BinaryExpr:
		if value.Operator == "AND" {
			return safeMutationIndexExpression(value.Left, schema) && safeMutationIndexExpression(value.Right, schema)
		}
		switch value.Operator {
		case "=", "<=>", "<", ">", "<=", ">=":
		default:
			return false
		}
		id, idOK := value.Left.(parser.Identifier)
		literal, literalOK := value.Right.(parser.LiteralExpr)
		if !idOK || !literalOK {
			id, idOK = value.Right.(parser.Identifier)
			literal, literalOK = value.Left.(parser.LiteralExpr)
		}
		if !idOK || !literalOK {
			return false
		}
		position, ok := queryColumnIndex(schema, id.Name)
		if !ok {
			return false
		}
		switch schema.ColumnsView()[position].Type {
		case storage.TypeInt, storage.TypeBigInt:
			if literal.Value.Kind != parser.LiteralNumber {
				return false
			}
			number, err := strconv.ParseInt(literal.Value.Text, 10, 64)
			// Existing SQL comparison promotes numbers to float64. Near/above 2^53,
			// its equality classes differ from exact integer index ordering.
			return err == nil && number > -(1<<53) && number < (1<<53)
		case storage.TypeFloat, storage.TypeDouble:
			return literal.Value.Kind == parser.LiteralNumber
		case storage.TypeText, storage.TypeVarchar:
			return literal.Value.Kind == parser.LiteralString
		}
	}
	return false
}
