package executor

import (
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
	// rows carries a derived table or CTE input. It is nil for base tables, which
	// are scanned (and probed) per outer row instead.
	rows []storage.Row
	// identityColumn is set by mutation joins: every scanned base-table row gets a
	// monotonic value in this column, and identityKeys maps it back to the storage
	// row key so DELETE can use a stable physical identity.
	identityColumn string
	identityKeys   map[int64][]byte
	identityNext   int64
}

func bindJoins(tx storageengine.Txn, session *Session, s parser.Select) ([]joinInput, error) {
	return bindJoinsMode(tx, session, s, false)
}

// bindJoinsIdentified is bindJoins for mutation statements: every base-table row
// carries its storage row key as a hidden identity column so joined DELETE targets
// can be identified without a primary key.
func bindJoinsIdentified(tx storageengine.Txn, session *Session, s parser.Select) ([]joinInput, error) {
	return bindJoinsMode(tx, session, s, true)
}

func bindJoinsMode(tx storageengine.Txn, session *Session, s parser.Select, identified bool) ([]joinInput, error) {
	if len(s.Joins) > 16 {
		return nil, fmt.Errorf("join exceeds 16 inputs")
	}
	ctx := operatorContext(session)
	inputs := make([]joinInput, 0, len(s.Joins)+1)
	joins := append([]parser.Join{{Table: s.Table, TableAlias: s.TableAlias, Subquery: s.Subquery}}, s.Joins...)
	var columns []storage.Column
	for i, j := range joins {
		if i > 0 && j.Type != "INNER" && j.Type != "LEFT" && j.Type != "RIGHT" && j.Type != "CROSS" {
			return nil, fmt.Errorf("supports INNER, LEFT, RIGHT and CROSS JOIN")
		}
		alias := j.TableAlias
		if alias == "" && j.Subquery == nil {
			_, alias = splitTableName(j.Table)
		}
		var (
			table  versionedTable
			schema *storage.Table
			rows   []storage.Row
			err    error
		)
		relation, isCTE := cteFor(session, j.Table)
		switch {
		case j.Subquery != nil:
			if alias == "" {
				return nil, fmt.Errorf("every derived table must have its own alias")
			}
			schema, rows, err = derivedRelation(ctx, tx, session, j.Subquery, alias, nil)
		case isCTE:
			schema = relation.schema
			rows = relation.rows
			if alias != "" {
				schema, err = qualifySchema(schema, alias)
			} else {
				_, alias = splitTableName(j.Table)
			}
		default:
			var isView bool
			schema, rows, isView, err = viewRelation(ctx, tx, session, j.Table, alias)
			if err == nil && !isView {
				table, schema, _, err = loadVersionedTable(tx, session, j.Table)
				if err == nil {
					schema, err = qualifySchema(schema, alias)
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if j.Type == "RIGHT" {
			for k := range columns {
				columns[k].Nullable = true
				columns[k].MetadataVersion = 1
			}
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
			if err = bindSQLExplainExprSession(j.On, combined, session); err != nil {
				return nil, err
			}
		}
		entry := joinInput{definition: table, schema: schema, combined: combined, join: j, rows: rows}
		if identified && rows == nil && table.ID != "" {
			entry.identityColumn = alias + ".__rowid"
			entry.identityKeys = make(map[int64][]byte)
			// The identity column is part of the input schema, so it flows through
			// the join and is null-extended with the rest of that input.
			identitySchema, identityErr := storage.NewTransientTable("join_identity", append(append([]storage.Column(nil), schema.ColumnsView()...), storage.Column{Name: entry.identityColumn, Type: storage.TypeBigInt, MetadataVersion: 1, Nullable: true}))
			if identityErr != nil {
				return nil, identityErr
			}
			entry.schema = identitySchema
			combinedColumns := append(append([]storage.Column(nil), columns...), storage.Column{Name: entry.identityColumn, Type: storage.TypeBigInt, MetadataVersion: 1, Nullable: j.Type == "LEFT" || j.Type == "RIGHT"})
			entry.combined, identityErr = storage.NewTransientTable("join", combinedColumns)
			if identityErr != nil {
				return nil, identityErr
			}
			columns = combinedColumns
		}
		inputs = append(inputs, entry)
	}
	return inputs, nil
}

func joinedInput(tx storageengine.Txn, session *Session, s parser.Select) (*storage.Table, physical.Operator[storage.Row], error) {
	inputs, err := bindJoins(tx, session, s)
	if err != nil {
		return nil, nil, err
	}
	schema := inputs[len(inputs)-1].combined
	if err = bindSQLExplainExprSession(s.Where, schema, session); err != nil {
		return nil, nil, err
	}
	left := physical.Operator[storage.Row](bindScan(tx, inputs[0].definition, sqlAccessPlan{kind: sqlAccessAll}, func(v []byte) (storage.Row, error) { return decodeSQLRow(inputs[0].definition, v) }))
	if inputs[0].rows != nil {
		left = derivedRows(inputs[0].rows)
	}
	op := chainJoinInputs(tx, session, inputs, left, false)
	if s.Where != nil {
		op = physical.Filter[storage.Row]{Input: op, Predicate: func(row storage.Row) (bool, error) {
			v, err := evaluateExprWithContext(s.Where, schema, row, session, nil)
			return truthy(v), err
		}}
	}
	return schema, op, nil
}

// chainJoinInputs extends the driving relation with the bound join inputs. When targetDriven is
// set (UPDATE JOIN and multi-table DELETE) the target must stay the driving side, so a RIGHT
// JOIN is evaluated as its matched subset: every target row matching at least one right row is
// still visited once, while null-extended right rows contribute no target identity.
func chainJoinInputs(tx storageengine.Txn, session *Session, inputs []joinInput, driving physical.Operator[storage.Row], targetDriven bool) physical.Operator[storage.Row] {
	op := driving
	for level := 1; level < len(inputs); level++ {
		input := &inputs[level]
		leftSchema := inputs[level-1].combined
		leftColumns := leftSchema.ColumnsView()
		// inputSource opens one source row set for the join input. Base tables can be
		// probed with an index when the ON clause allows it; derived and CTE inputs
		// replay their materialized rows.
		inputSource := func(left storage.Row, probe bool) physical.Operator[storage.Row] {
			if input.rows != nil {
				return derivedRows(input.rows)
			}
			access := sqlAccessPlan{kind: sqlAccessAll}
			if probe {
				if where, ok := joinLookup(input.join.On, leftSchema, input.schema, left); ok {
					access = planSQLAccess(parser.Select{Where: where}, input.definition, input.schema, session)
				}
			}
			if input.identityColumn != "" {
				return identityScan(tx, input, access, session)
			}
			return bindScan(tx, input.definition, access, func(v []byte) (storage.Row, error) {
				if err := checkQuery(session); err != nil {
					return nil, err
				}
				return decodeSQLRow(input.definition, v)
			})
		}
		combine := func(left, right storage.Row) storage.Row {
			row := make(storage.Row, len(left)+len(right))
			copy(row, left)
			copy(row[len(left):], right)
			return row
		}
		predicate := func(row storage.Row) (bool, error) {
			if input.join.On == nil || input.join.Type == "CROSS" {
				return true, checkQuery(session)
			}
			value, err := evaluateExprWithContext(input.join.On, input.combined, row, session, nil)
			return truthy(value), err
		}
		rightPlan := &physical.PlanNode{Kind: "DynamicScan", Attributes: map[string]string{"table": input.definition.CatalogName, "access": "integer equality lookup or scan, chosen per outer row"}}
		if input.rows != nil {
			rightPlan = &physical.PlanNode{Kind: "MaterializedDerived", Attributes: map[string]string{"rows": strconv.Itoa(len(input.rows))}}
		}
		if input.join.Type == "RIGHT" && !targetDriven {
			// RIGHT JOIN swaps the driving side: the right input drives and the already
			// bound left relation is replayed per driving row, keeping the combined row in
			// [left..., right...] order like the legacy join does.
			drivingSource := inputSource(nil, false)
			leftInput := op
			join := physical.Join3[storage.Row, storage.Row, storage.Row]{RightPlan: rightPlan, Left: drivingSource, Right: func(storage.Row) (physical.Operator[storage.Row], error) {
				return bufferedRows(session, leftInput), nil
			}, Combine: func(rightRow, leftRow storage.Row) storage.Row {
				return combine(leftRow, rightRow)
			}, Predicate: predicate, NullRight: func(rightRow storage.Row) storage.Row {
				row := make(storage.Row, len(leftColumns)+len(rightRow))
				for index, column := range leftColumns {
					row[index] = storage.NullValue(column.Type)
				}
				copy(row[len(leftColumns):], rightRow)
				return row
			}}
			op = join
			continue
		}
		join := physical.Join3[storage.Row, storage.Row, storage.Row]{RightPlan: rightPlan, Left: op, Right: func(left storage.Row) (physical.Operator[storage.Row], error) {
			return inputSource(left, true), nil
		}, Combine: combine, Predicate: predicate}
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
	return op
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
