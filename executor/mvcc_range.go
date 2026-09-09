package executor

import (
	"encoding/binary"
	"errors"
	"gbaselite/parser"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strconv"
	"strings"
)

// Only newly created single signed-integer primary keys opt into this layout.
// Encoding 0 remains the existing IndexValueKey layout; existing tables are
// never rewritten implicitly. Unique-index keys retain their existing format.
const mvccIntegerKeyEncoding = 1

func mvccIntegerPrimary(table versionedTable) (int, bool) {
	for _, idx := range table.Definition.Indexes {
		if !idx.Primary || len(idx.Columns) != 1 {
			continue
		}
		for i, c := range table.Definition.Columns {
			if strings.EqualFold(c.Name, idx.Columns[0]) && (c.Type == storage.TypeInt || c.Type == storage.TypeBigInt) {
				return i, true
			}
		}
	}
	return 0, false
}
func validateMVCCKeyEncoding(table versionedTable) error {
	if table.SecondaryEncoding > 1 {
		return errors.New("unsupported secondary index encoding")
	}
	if table.RowEncoding != 0 && table.RowEncoding != mvccCompactRowEncoding {
		return errors.New("unsupported MVCC row encoding; use a compatible database binary")
	}
	if table.KeyEncoding == 0 {
		return nil
	}
	if table.KeyEncoding == mvccIntegerKeyEncoding {
		if _, ok := mvccIntegerPrimary(table); ok {
			return nil
		}
	}
	return errors.New("unsupported MVCC primary key encoding/schema; use a compatible database binary")
}
func mvccIntegerKey(n int64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(n)^(uint64(1)<<63))
	return key
}
func mvccPrimaryKey(table versionedTable, index storage.Index, row storage.Row) ([]byte, bool) {
	if table.KeyEncoding == mvccIntegerKeyEncoding {
		p, ok := mvccIntegerPrimary(table)
		if !ok || p >= len(row) || row[p].Null {
			return nil, false
		}
		return mvccIntegerKey(row[p].Int64), true
	}
	k, ok := storage.IndexValueKey(index, table.Definition.Columns, row)
	return []byte(k), ok
}

func mvccSafeRangeExpression(expr parser.Expr, schema *storage.Table) bool {
	if expr == nil {
		return true
	}
	switch v := expr.(type) {
	case parser.BinaryExpr:
		if v.Operator == "AND" {
			return mvccSafeRangeExpression(v.Left, schema) && mvccSafeRangeExpression(v.Right, schema)
		}
	case parser.BetweenExpr:
		if v.Not {
			return false
		}
		return safeMutationIndexExpression(parser.BinaryExpr{Left: v.Value, Operator: ">=", Right: v.Lower}, schema) && safeMutationIndexExpression(parser.BinaryExpr{Left: v.Value, Operator: "<=", Right: v.Upper}, schema)
	}
	return safeMutationIndexExpression(expr, schema)
}

// Reject ranges with unsafe conversions/unknown columns. Residual predicates
// are still evaluated on every candidate; these bounds do not replace WHERE.
func mvccPrimaryRange(where parser.Expr, table versionedTable, schema *storage.Table) (storageengine.KeyRange, bool) {
	var r storageengine.KeyRange
	if table.KeyEncoding != mvccIntegerKeyEncoding || !mvccSafeRangeExpression(where, schema) {
		return r, false
	}
	position, ok := mvccIntegerPrimary(table)
	if !ok {
		return r, false
	}
	var lower, upper int64
	bounded := false
	add := func(op string, n int64) {
		bounded = true
		if op == ">" || op == ">=" || op == "=" || op == "<=>" {
			inclusive := op != ">"
			if r.Lower == nil || n > lower {
				lower = n
				r.Lower = mvccIntegerKey(n)
				r.LowerInclusive = inclusive
			} else if n == lower {
				r.LowerInclusive = r.LowerInclusive && inclusive
			}
		}
		if op == "<" || op == "<=" || op == "=" || op == "<=>" {
			inclusive := op != "<"
			if r.Upper == nil || n < upper {
				upper = n
				r.Upper = mvccIntegerKey(n)
				r.UpperInclusive = inclusive
			} else if n == upper {
				r.UpperInclusive = r.UpperInclusive && inclusive
			}
		}
	}
	var visit func(parser.Expr)
	visit = func(expr parser.Expr) {
		if between, ok := expr.(parser.BetweenExpr); ok {
			visit(parser.BinaryExpr{Left: between.Value, Operator: ">=", Right: between.Lower})
			visit(parser.BinaryExpr{Left: between.Value, Operator: "<=", Right: between.Upper})
			return
		}
		c, ok := expr.(parser.BinaryExpr)
		if !ok {
			return
		}
		if c.Operator == "AND" {
			visit(c.Left)
			visit(c.Right)
			return
		}
		id, idOK := c.Left.(parser.Identifier)
		lit, litOK := c.Right.(parser.LiteralExpr)
		op := c.Operator
		if !idOK || !litOK {
			id, idOK = c.Right.(parser.Identifier)
			lit, litOK = c.Left.(parser.LiteralExpr)
			switch op {
			case "<":
				op = ">"
			case "<=":
				op = ">="
			case ">":
				op = "<"
			case ">=":
				op = "<="
			}
		}
		if !idOK || !litOK || lit.Value.Kind != parser.LiteralNumber {
			return
		}
		p, ok := queryColumnIndex(schema, id.Name)
		if !ok || p != position {
			return
		}
		n, err := strconv.ParseInt(lit.Value.Text, 10, 64)
		if err != nil || n <= -(1<<53) || n >= 1<<53 {
			return
		}
		add(op, n)
	}
	visit(where)
	return r, bounded
}

// An ORDER BY projection alias can shadow the input primary key. Only bypass
// sorting when its meaning is unambiguously the physical integer primary key.
func mvccPrimaryOrder(statement parser.Select, table versionedTable, schema *storage.Table) bool {
	if table.KeyEncoding != mvccIntegerKeyEncoding || len(statement.OrderBy) != 1 {
		return false
	}
	p, ok := mvccIntegerPrimary(table)
	if !ok {
		return false
	}
	order := statement.OrderBy[0].Column
	pos, ok := queryColumnIndex(schema, order)
	if !ok || pos != p {
		return false
	}
	for _, item := range statement.Items {
		if item.Alias != "" && strings.EqualFold(item.Alias, stripQualifier(order)) {
			expr, err := parser.ParseExpression(item.Expression)
			if err != nil {
				return false
			}
			id, ok := expr.(parser.Identifier)
			if !ok {
				return false
			}
			position, ok := queryColumnIndex(schema, id.Name)
			if !ok || position != p {
				return false
			}
		}
	}
	return true
}
