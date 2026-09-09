package executor

import (
	"context"
	"fmt"
	"gbaselite/mvcc"
	"gbaselite/parser"
	"gbaselite/storage"
	"strconv"
)

type mvccJoinInput struct {
	definition versionedTable
	schema     *storage.Table
	combined   *storage.Table
	join       parser.Join
}

func bindMVCCJoins(tx *mvcc.Tx, session *Session, s parser.Select) ([]mvccJoinInput, error) {
	if len(s.Joins) > 16 {
		return nil, fmt.Errorf("MVCC join exceeds 16 inputs")
	}
	inputs := make([]mvccJoinInput, 0, len(s.Joins)+1)
	joins := append([]parser.Join{{Table: s.Table, TableAlias: s.TableAlias}}, s.Joins...)
	var columns []storage.Column
	for i, j := range joins {
		if j.Subquery != nil {
			return nil, fmt.Errorf("MVCC derived join input is not supported")
		}
		if i > 0 && j.Type != "INNER" && j.Type != "LEFT" {
			return nil, fmt.Errorf("MVCC supports INNER and LEFT JOIN")
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
		inputs = append(inputs, mvccJoinInput{table, schema, combined, j})
	}
	return inputs, nil
}
func mvccJoinedSource(ctx context.Context, tx *mvcc.Tx, session *Session, s parser.Select) (*storage.Table, func(func(storage.Row) error) error, error) {
	inputs, err := bindMVCCJoins(tx, session, s)
	if err != nil {
		return nil, nil, err
	}
	schema := inputs[len(inputs)-1].combined
	if err = bindMVCCExplainExpr(s.Where, schema); err != nil {
		return nil, nil, err
	}
	source := func(yield func(storage.Row) error) error {
		var next func(int, storage.Row) error
		next = func(level int, left storage.Row) error {
			if err := checkQuery(session); err != nil {
				return err
			}
			if level == len(inputs) {
				if s.Where != nil {
					v, err := evaluateExprWithContext(s.Where, schema, left, session, nil)
					if err != nil {
						return err
					}
					if !truthy(v) {
						return nil
					}
				}
				return yield(left)
			}
			input := inputs[level]
			access := mvccAccessPlan{kind: mvccAccessAll}
			if level > 0 {
				if where, ok := mvccJoinLookup(input.join.On, inputs[level-1].combined, input.schema, left); ok {
					access = planMVCCAccess(parser.Select{Where: where}, input.definition, input.schema, session)
				}
			}
			matched := false
			err := access.scanBatches(ctx, tx, input.definition, 1, func(batch []mvccBatchEntry) error {
				for _, entry := range batch {
					right, err := decodeMVCCRow(input.definition, entry.value)
					if err != nil {
						return err
					}
					row := make(storage.Row, len(left)+len(right))
					copy(row, left)
					copy(row[len(left):], right)
					if level > 0 && input.join.On != nil {
						v, err := evaluateExprWithContext(input.join.On, input.combined, row, session, nil)
						if err != nil {
							return err
						}
						if !truthy(v) {
							continue
						}
					}
					matched = true
					if err = next(level+1, row); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if !matched && input.join.Type == "LEFT" {
				row := make(storage.Row, len(left)+len(input.schema.ColumnsView()))
				copy(row, left)
				for i, c := range input.schema.ColumnsView() {
					row[len(left)+i] = storage.NullValue(c.Type)
				}
				return next(level+1, row)
			}
			return nil
		}
		return next(0, nil)
	}
	return schema, source, nil
}
func mvccJoinLookup(on parser.Expr, left, right *storage.Table, row storage.Row) (parser.Expr, bool) {
	expr, ok := on.(parser.BinaryExpr)
	if !ok {
		return nil, false
	}
	if expr.Operator == "AND" {
		if p, ok := mvccJoinLookup(expr.Left, left, right, row); ok {
			return p, true
		}
		return mvccJoinLookup(expr.Right, left, right, row)
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
