package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"gbaselite/mvcc"
	"gbaselite/parser"
	"gbaselite/storage"
	"math"
	"strconv"
	"strings"
)

type integerBatchPredicate struct {
	position int
	operator string
	value    int64
}
type integerBatchOutput struct {
	position int
	kind     aggregateKind
}
type integerBatchPlan struct {
	predicates []integerBatchPredicate
	outputs    []integerBatchOutput
	columns    []Column
	positions  []int // schema position -> packed vector ordinal, -1 means skip
	width      int
	aggregate  bool
}

func planIntegerBatch(s parser.Select, table versionedTable, schema *storage.Table, access mvccAccessPlan) *integerBatchPlan {
	if len(s.GroupBy) > 0 || s.Having != nil || table.RowEncoding != mvccCompactRowEncoding || s.Distinct || (access.kind == mvccAccessPoint || access.kind == mvccAccessUnique) || len(s.OrderBy) > 0 && !access.ordered || len(schema.ColumnsView()) > 512 {
		return nil
	}
	p := &integerBatchPlan{positions: make([]int, len(schema.ColumnsView())), aggregate: selectHasAggregate(s.Items)}
	for i, c := range schema.ColumnsView() {
		p.positions[i] = -1
		switch c.Type {
		case storage.TypeInt, storage.TypeBigInt, storage.TypeText, storage.TypeVarchar, storage.TypeDecimal:
		default:
			return nil
		}
	}
	add := func(name string) int {
		pos, ok := queryColumnIndex(schema, name)
		if !ok {
			return -1
		}
		c := schema.ColumnsView()[pos]
		if c.Type != storage.TypeInt && c.Type != storage.TypeBigInt {
			return -1
		}
		if p.positions[pos] < 0 {
			p.positions[pos] = p.width
			p.width++
		}
		return pos
	}
	var predicate func(parser.Expr) bool
	predicate = func(e parser.Expr) bool {
		if e == nil {
			return true
		}
		v, ok := e.(parser.BinaryExpr)
		if !ok {
			return false
		}
		if v.Operator == "AND" {
			return predicate(v.Left) && predicate(v.Right)
		}
		switch v.Operator {
		case "=", "<=>", "<", ">", "<=", ">=":
		default:
			return false
		}
		id, ok := v.Left.(parser.Identifier)
		lit, lok := v.Right.(parser.LiteralExpr)
		op := v.Operator
		if !ok || !lok {
			id, ok = v.Right.(parser.Identifier)
			lit, lok = v.Left.(parser.LiteralExpr)
			switch op {
			case "<":
				op = ">"
			case ">":
				op = "<"
			case "<=":
				op = ">="
			case ">=":
				op = "<="
			}
		}
		if !ok || !lok || lit.Value.Kind != parser.LiteralNumber {
			return false
		}
		n, err := strconv.ParseInt(lit.Value.Text, 10, 64)
		if err != nil || n <= -(1<<53) || n >= 1<<53 {
			return false
		}
		pos := add(id.Name)
		if pos < 0 {
			return false
		}
		p.predicates = append(p.predicates, integerBatchPredicate{pos, op, n})
		return true
	}
	if !predicate(s.Where) {
		return nil
	}
	for _, item := range s.Items {
		name := item.Alias
		if name == "" {
			name = item.Expression
		}
		if p.aggregate {
			kind, arg, ok := parseAggregateExpression(item.Expression)
			if !ok {
				return nil
			}
			pos := -1
			typ := storage.TypeBigInt
			if arg != "*" {
				expr, err := parser.ParseExpression(arg)
				if err != nil {
					return nil
				}
				id, ok := expr.(parser.Identifier)
				if !ok {
					return nil
				}
				pos = add(id.Name)
				if pos < 0 {
					return nil
				}
				typ = schema.ColumnsView()[pos].Type
			} else if kind != aggregateCount {
				return nil
			}
			if kind == aggregateCount || kind == aggregateSum {
				typ = storage.TypeBigInt
			}
			if kind == aggregateAvg {
				typ = storage.TypeDouble
			}
			p.outputs = append(p.outputs, integerBatchOutput{pos, kind})
			p.columns = append(p.columns, Column{Name: name, Type: typ})
			continue
		}
		var ids []string
		if item.Expression == "*" {
			for _, c := range schema.ColumnsView() {
				ids = append(ids, c.Name)
			}
		} else {
			expr, err := parser.ParseExpression(item.Expression)
			if err != nil {
				return nil
			}
			id, ok := expr.(parser.Identifier)
			if !ok {
				return nil
			}
			ids = []string{id.Name}
		}
		for _, id := range ids {
			pos := add(id)
			if pos < 0 {
				return nil
			}
			c := schema.ColumnsView()[pos]
			column := Column{Name: name, Type: c.Type}
			if item.Expression == "*" {
				column = Column{Name: stripQualifier(c.Name), Type: c.Type, SQLType: c.SQLType, Collation: c.Collation, Nullable: storage.ColumnNullable(c), jsonValue: strings.EqualFold(c.SQLType, "JSON")}
			} else {
				inheritExpressionColumn(&column, parser.Identifier{Name: id}, schema)
			}
			p.outputs = append(p.outputs, integerBatchOutput{position: pos})
			p.columns = append(p.columns, column)
		}
	}
	if p.width > 64 {
		return nil
	}
	return p
}

// Decode directly into packed integer vectors. Other supported fields are
// structurally validated and skipped without allocating strings or storage.Rows.
func decodeIntegerBatch(table versionedTable, p *integerBatchPlan, batch []mvccBatchEntry, stride int, values []int64, nulls []bool) error {
	for row, entry := range batch {
		encoded := entry.value
		if !bytes.HasPrefix(encoded, mvccRowMagic) || len(encoded) > mvcc.MaxValueBytes {
			return errMVCCRowEncoding
		}
		n, k := binary.Uvarint(encoded[4:])
		if k <= 0 || n != uint64(len(p.positions)) {
			return errMVCCRowEncoding
		}
		offset := 4 + k
		nb := (len(p.positions) + 7) / 8
		if len(encoded)-offset < nb {
			return errMVCCRowEncoding
		}
		mask := encoded[offset : offset+nb]
		reader := mvccRowReader{data: encoded, offset: offset + nb}
		for col, c := range table.Definition.Columns {
			isNull := mask[col/8]&(1<<uint(col%8)) != 0
			vector := p.positions[col]
			if vector >= 0 {
				nulls[vector*stride+row] = isNull
			}
			if isNull {
				continue
			}
			switch c.Type {
			case storage.TypeInt, storage.TypeBigInt:
				n, k := binary.Varint(encoded[reader.offset:])
				if k <= 0 {
					return errMVCCRowEncoding
				}
				reader.offset += k
				if vector >= 0 {
					values[vector*stride+row] = n
				}
			case storage.TypeText, storage.TypeVarchar, storage.TypeDecimal:
				if _, err := reader.blob(); err != nil {
					return err
				}
			default:
				return errMVCCRowEncoding
			}
		}
		if reader.offset != len(encoded) {
			return errMVCCRowEncoding
		}
	}
	return nil
}
func (p *integerBatchPlan) selectRows(n, stride int, values []int64, nulls []bool, selection []uint16) []uint16 {
	selection = selection[:n]
	for i := range selection {
		selection[i] = uint16(i)
	}
	for _, pred := range p.predicates {
		kept := 0
		base := p.positions[pred.position] * stride
		for _, r := range selection {
			if nulls[base+int(r)] {
				continue
			}
			v := values[base+int(r)]
			ok := false
			switch pred.operator {
			case "=", "<=>":
				ok = v == pred.value
			case "<":
				ok = v < pred.value
			case ">":
				ok = v > pred.value
			case "<=":
				ok = v <= pred.value
			case ">=":
				ok = v >= pred.value
			}
			if ok {
				selection[kept] = r
				kept++
			}
		}
		selection = selection[:kept]
	}
	return selection
}
func executeIntegerBatch(ctx context.Context, tx *mvcc.Tx, session *Session, s parser.Select, table versionedTable, access mvccAccessPlan, p *integerBatchPlan) (*Result, error) {
	rows := min(mvccBatchRows, mvccBatchBytes/max(1, p.width*9+2))
	if !p.aggregate && s.HasLimit {
		rows = min(rows, max(1, s.Limit))
	}
	result := &Result{Columns: p.columns}
	if !p.aggregate && s.HasLimit && s.Limit == 0 {
		return result, nil
	}
	values := make([]int64, p.width*rows)
	nulls := make([]bool, len(values))
	selection := make([]uint16, rows)
	states := make([]aggregateState, len(p.outputs))
	offset := s.Offset
	used := int64(0)
	err := access.scanBatches(ctx, tx, table, rows, func(batch []mvccBatchEntry) error {
		if err := checkQuery(session); err != nil {
			return err
		}
		if err := decodeIntegerBatch(table, p, batch, rows, values, nulls); err != nil {
			return err
		}
		selected := p.selectRows(len(batch), rows, values, nulls, selection)
		if p.aggregate {
			for i, out := range p.outputs {
				if out.position < 0 {
					states[i].count += int64(len(selected))
					continue
				}
				base := p.positions[out.position] * rows
				for _, r := range selected {
					at := base + int(r)
					if nulls[at] {
						continue
					}
					v := values[at]
					state := &states[i]
					if out.kind == aggregateCount {
						state.count++
						continue
					}
					if (out.kind == aggregateSum || out.kind == aggregateAvg) && !state.decimal.has && !(v > 0 && state.integerSum > math.MaxInt64-v) && !(v < 0 && state.integerSum < math.MinInt64-v) {
						state.integerSum += v
						state.count++
						state.has = true
						continue
					}
					// Preserve overflow promotion and existing comparison semantics.
					if err := updateAggregate(state, out.kind, v, false, session); err != nil {
						return err
					}
				}
			}
			return nil
		}
		for _, r := range selected {
			if offset > 0 {
				offset--
				continue
			}
			record := make([]any, len(p.outputs))
			for i, out := range p.outputs {
				at := p.positions[out.position]*rows + int(r)
				if !nulls[at] {
					record[i] = values[at]
				}
			}
			var err error
			used, err = checkResultMemory(session.query.options.ResultMemoryBytes, used, record)
			if err != nil {
				return err
			}
			result.Rows = append(result.Rows, record)
			if s.HasLimit && len(result.Rows) >= s.Limit {
				return errBudgetedRowsDone
			}
		}
		return nil
	})
	if errors.Is(err, errBudgetedRowsDone) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if p.aggregate && s.Offset == 0 && (!s.HasLimit || s.Limit > 0) {
		record := make([]any, len(p.outputs))
		for i, out := range p.outputs {
			record[i], err = finishAggregate(states[i], out.kind, p.columns[i].Type)
			if err != nil {
				return nil, err
			}
		}
		result.Rows = [][]any{record}
	}
	return result, nil
}
