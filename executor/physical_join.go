package executor

import (
	"context"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strconv"
)

type joinInput struct {
	definition versionedTable
	schema     *storage.Table
	combined   *storage.Table
	join       parser.Join
}

func bindJoins(tx storageengine.Txn, session *Session, s parser.Select) ([]joinInput, error) {
	if len(s.Joins) > 16 {
		return nil, fmt.Errorf("join exceeds 16 inputs")
	}
	inputs := make([]joinInput, 0, len(s.Joins)+1)
	joins := append([]parser.Join{{Table: s.Table, TableAlias: s.TableAlias}}, s.Joins...)
	var columns []storage.Column
	for i, j := range joins {
		if j.Subquery != nil {
			return nil, fmt.Errorf("derived join input is not supported")
		}
		if i > 0 && j.Type != "INNER" && j.Type != "LEFT" {
			return nil, fmt.Errorf("supports INNER and LEFT JOIN")
		}
		table, schema, _, err := loadVersionedTable(tx, session, j.Table)
		if err != nil {
			return nil, err
		}
		alias := j.TableAlias
		if alias == "" {
			_, alias = splitTableName(j.Table)
		}
		schema, err = qualifySchema(schema, alias)
		if err != nil {
			return nil, err
		}
		rightColumns := append([]storage.Column(nil), schema.ColumnsView()...)
		if j.Type == "LEFT" {
			for k := range rightColumns {
				rightColumns[k].Nullable = true
				rightColumns[k].MetadataVersion = 1
			}
		}
		columns = append(columns, rightColumns...)
		combined, err := storage.NewTransientTable("join", append([]storage.Column(nil), columns...))
		if err != nil {
			return nil, err
		}
		if i > 0 {
			if err = bindMVCCExplainExpr(j.On, combined); err != nil {
				return nil, err
			}
		}
		inputs = append(inputs, joinInput{table, schema, combined, j})
	}
	return inputs, nil
}
func joinedSource(ctx context.Context, tx storageengine.Txn, session *Session, s parser.Select) (*storage.Table, func(func(storage.Row) error) error, error) {
	inputs, err := bindJoins(tx, session, s)
	if err != nil {
		return nil, nil, err
	}
	schema := inputs[len(inputs)-1].combined
	if err = bindMVCCExplainExpr(s.Where, schema); err != nil {
		return nil, nil, err
	}
	op := bindScan(tx, inputs[0].definition, mvccAccessPlan{kind: mvccAccessAll}, func(v []byte) (storage.Row, error) { return decodeMVCCRow(inputs[0].definition, v) })
	for level := 1; level < len(inputs); level++ {
		input := inputs[level]
		leftSchema := inputs[level-1].combined
		join := physical.Join[storage.Row]{Left: op, Right: func(left storage.Row) (physical.Operator[storage.Row], error) {
			access := mvccAccessPlan{kind: mvccAccessAll}
			if where, ok := joinLookup(input.join.On, leftSchema, input.schema, left); ok {
				access = planMVCCAccess(parser.Select{Where: where}, input.definition, input.schema, session)
			}
			return bindScan(tx, input.definition, access, func(v []byte) (storage.Row, error) {
				if err := checkQuery(session); err != nil {
					return nil, err
				}
				return decodeMVCCRow(input.definition, v)
			}), nil
		}, Combine: func(left, right storage.Row) storage.Row {
			row := make(storage.Row, len(left)+len(right))
			copy(row, left)
			copy(row[len(left):], right)
			return row
		}, Predicate: func(row storage.Row) (bool, error) {
			if input.join.On == nil {
				return true, checkQuery(session)
			}
			value, err := evaluateExprWithContext(input.join.On, input.combined, row, session, nil)
			return truthy(value), err
		}}
		if input.join.Type == "LEFT" {
			join.NullRight = func(left storage.Row) storage.Row {
				row := make(storage.Row, len(left)+len(input.schema.ColumnsView()))
				copy(row, left)
				for i, c := range input.schema.ColumnsView() {
					row[len(left)+i] = storage.NullValue(c.Type)
				}
				return row
			}
		}
		op = join
	}
	if s.Where != nil {
		op = physical.Filter[storage.Row]{Input: op, Predicate: func(row storage.Row) (bool, error) {
			v, err := evaluateExprWithContext(s.Where, schema, row, session, nil)
			return truthy(v), err
		}}
	}
	source := rowSource(ctx, op)
	return schema, source, nil
}
func joinLookup(on parser.Expr, left, right *storage.Table, row storage.Row) (parser.Expr, bool) {
	expr, ok := on.(parser.BinaryExpr)
	if !ok {
		return nil, false
	}
	if expr.Operator == "AND" {
		if p, ok := joinLookup(expr.Left, left, right, row); ok {
			return p, true
		}
		return joinLookup(expr.Right, left, right, row)
	}
	if expr.Operator != "=" {
		return nil, false
	}
	a, ok := expr.Left.(parser.Identifier)
	b, bok := expr.Right.(parser.Identifier)
	if !ok || !bok {
		return nil, false
	}
	lp, lok := queryColumnIndex(left, a.Name)
	rp, rok := queryColumnIndex(right, b.Name)
	if !lok || !rok {
		a, b = b, a
		lp, lok = queryColumnIndex(left, a.Name)
		rp, rok = queryColumnIndex(right, b.Name)
	}
	if !lok || !rok {
		return nil, false
	}
	value := row[lp]
	kind := right.ColumnsView()[rp].Type
	if value.Null || (kind != storage.TypeInt && kind != storage.TypeBigInt) || (value.Type != storage.TypeInt && value.Type != storage.TypeBigInt) || value.Int64 <= -(1<<53) || value.Int64 >= 1<<53 {
		return nil, false
	}
	return parser.BinaryExpr{Operator: "=", Left: parser.Identifier{Name: b.Name}, Right: parser.LiteralExpr{Value: parser.Literal{Kind: parser.LiteralNumber, Text: strconv.FormatInt(value.Int64, 10)}}}, true
}
