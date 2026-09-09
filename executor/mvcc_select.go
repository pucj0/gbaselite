package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/mvcc"
	"gbaselite/parser"
	"gbaselite/storage"
	"strings"
)

func executeMVCCSelect(ctx context.Context, tx *mvcc.Tx, session *Session, statement parser.Select) (*Result, error) {
	if err := validateMVCCSelectShape(statement); err != nil {
		return nil, err
	}
	if statement.Table == "" && statement.Subquery == nil {
		return executeScalarSelect(session, statement)
	}
	if len(statement.Joins) > 0 {
		schema, source, err := mvccJoinedSource(ctx, tx, session, statement)
		if err != nil {
			return nil, err
		}
		return finishMVCCSelect(session, statement, schema, source, false)
	}
	definition, schema, _, err := loadVersionedTableForRead(tx, session, statement.Table)
	if err != nil {
		return nil, err
	}
	if statement.TableAlias != "" {
		schema, err = qualifySchema(schema, statement.TableAlias)
		if err != nil {
			return nil, err
		}
	}
	needed := mvccProjectionMask(statement, schema)
	plan := planMVCCAccess(statement, definition, schema, session)
	if batchPlan := planIntegerBatch(statement, definition, schema, plan); batchPlan != nil {
		return executeIntegerBatch(ctx, tx, session, statement, definition, plan, batchPlan)
	}
	point := plan.kind == mvccAccessPoint
	ordered := plan.ordered
	var filter func(storage.Row) (any, error)
	if !point && statement.Where != nil {
		filter = bindMVCCFilter(statement.Where, schema, session)
	}
	aggregate := selectHasAggregate(statement.Items)
	source := func(yield func(storage.Row) error) error {
		var scratch storage.Row
		visit := func(encoded []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := checkQuery(session); err != nil {
				return err
			}
			row, err := decodeMVCCRowInto(definition, encoded, needed, scratch)
			if aggregate {
				scratch = row
			}
			if err != nil {
				return err
			}
			if statement.Where != nil {
				var value any
				var err error
				if filter != nil {
					value, err = filter(row)
				} else {
					value, err = evaluateExprWithContext(statement.Where, schema, row, session, nil)
				}
				if err != nil {
					return err
				}
				if !truthy(value) {
					return nil
				}
			}
			return yield(row)
		}
		batchRows := mvccBatchRows
		// LIMIT can terminate within the first batch; avoid unnecessary lookups.
		if statement.HasLimit && !aggregate && len(statement.OrderBy) == 0 && statement.Limit < batchRows {
			batchRows = max(1, statement.Limit)
		}
		return plan.scanBatches(ctx, tx, definition, batchRows, func(batch []mvccBatchEntry) error {
			for _, entry := range batch {
				if err := visit(entry.value); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return finishMVCCSelect(session, statement, schema, source, ordered)
}
func finishMVCCSelect(session *Session, statement parser.Select, schema *storage.Table, source func(func(storage.Row) error) error, ordered bool) (*Result, error) {
	if len(statement.GroupBy) > 0 || statement.Having != nil {
		if statement.Distinct {
			base := statement
			base.Distinct = false
			base.HasLimit = false
			base.Offset = 0
			result, err := executeGroupedSelectWithSource(schema, base, schema.ColumnsView(), session, source)
			if err != nil {
				return nil, err
			}
			limit := -1
			if statement.HasLimit {
				limit = statement.Limit
			}
			local := *session
			local.StreamResults = false
			return executeBudgetedDistinct(&local, result, statement.Offset, limit)
		}
		return executeGroupedSelectWithSource(schema, statement, schema.ColumnsView(), session, source)
	}
	aggregate := selectHasAggregate(statement.Items)
	if aggregate {
		return mvccAggregate(session, statement, schema, source)
	}
	if statement.Distinct {
		base := statement
		base.Distinct = false
		base.HasLimit = false
		base.Offset = 0
		base.Limit = 0
		result, err := finishMVCCSelect(session, base, schema, source, ordered)
		if err != nil {
			return nil, err
		}
		limit := -1
		if statement.HasLimit {
			limit = statement.Limit
		}
		local := *session
		local.StreamResults = false
		return executeBudgetedDistinct(&local, result, statement.Offset, limit)
	}
	var plans []projectedExpression
	columns := schema.ColumnsView()
	for _, item := range statement.Items {
		if item.Expression == "*" {
			for _, column := range columns {
				plans = append(plans, projectedExpression{expression: parser.Identifier{Name: column.Name}, column: Column{Name: stripQualifier(column.Name), Type: column.Type, SQLType: column.SQLType, Collation: column.Collation, Nullable: storage.ColumnNullable(column), jsonValue: strings.EqualFold(column.SQLType, "JSON")}})
			}
			continue
		}
		expression, err := parser.ParseExpression(item.Expression)
		if err != nil {
			return nil, err
		}
		if !coldScalarExpression(expression) {
			return nil, errors.New("MVCC scalar subqueries are not supported")
		}
		kind, err := expressionTypeWithSession(expression, schema, columns, session)
		if err != nil {
			return nil, err
		}
		name := item.Alias
		if name == "" {
			name = item.Expression
		}
		column := Column{Name: name, Type: kind, jsonValue: expressionReturnsJSON(expression, schema)}
		inheritExpressionColumn(&column, expression, schema)
		plans = append(plans, projectedExpression{expression: expression, column: column})
	}
	result := &Result{Columns: make([]Column, len(plans))}
	for i, plan := range plans {
		result.Columns[i] = plan.column
	}
	project := func(row storage.Row) ([]any, error) {
		values := make([]any, len(plans))
		for i, plan := range plans {
			v, err := evaluateExprWithContext(plan.expression, schema, row, session, nil)
			if err != nil {
				return nil, err
			}
			values[i] = v
		}
		return values, nil
	}
	local := *session
	local.StreamResults = false
	if len(statement.OrderBy) > 0 && !ordered {
		return executeBudgetedExpressionOrderWithSource(nil, &local, statement, schema, result.Columns, project, source)
	}
	used := int64(0)
	offset, emitted := statement.Offset, 0
	if statement.HasLimit && statement.Limit == 0 {
		return result, nil
	}
	err := source(func(row storage.Row) error {
		if offset > 0 {
			offset--
			return nil
		}
		values, err := project(row)
		if err != nil {
			return err
		}
		used, err = checkResultMemory(session.query.options.ResultMemoryBytes, used, values)
		if err != nil {
			return err
		}
		result.Rows = append(result.Rows, values)
		emitted++
		if statement.HasLimit && emitted >= statement.Limit {
			return errBudgetedRowsDone
		}
		return nil
	})
	if errors.Is(err, errBudgetedRowsDone) {
		err = nil
	}
	return result, err
}
func mvccPointKey(expression parser.Expr, table versionedTable, schema *storage.Table, session *Session) ([]byte, bool) {
	if !safeMutationIndexExpression(expression, schema) {
		return nil, false
	}
	comparison, ok := expression.(parser.BinaryExpr)
	if !ok || comparison.Operator != "=" {
		return nil, false
	}
	identifier, ok := comparison.Left.(parser.Identifier)
	literal, literalOK := comparison.Right.(parser.LiteralExpr)
	if !ok || !literalOK {
		return nil, false
	}
	position, ok := queryColumnIndex(schema, identifier.Name)
	if !ok {
		return nil, false
	}
	column := table.Definition.Columns[position]
	if isTextColumn(column.Type) && column.Collation == "" && !strings.HasSuffix(session.CollationConnection, "_bin") && session.CollationConnection != "binary" {
		return nil, false
	}
	value, err := literalToValue(literal.Value, column)
	if err != nil || value.Null {
		return nil, false
	}
	for _, index := range table.Definition.Indexes {
		if index.Primary && len(index.Columns) == 1 && strings.EqualFold(stripQualifier(identifier.Name), index.Columns[0]) {
			row := make(storage.Row, len(table.Definition.Columns))
			row[position] = value
			k, valid := mvccPrimaryKey(table, index, row)
			return k, valid
		}
	}
	return nil, false
}
func mvccAggregate(session *Session, statement parser.Select, schema *storage.Table, source func(func(storage.Row) error) error) (*Result, error) {
	kinds := make([]aggregateKind, len(statement.Items))
	expressions := make([]parser.Expr, len(kinds))
	positions := make([]int, len(kinds))
	columns := schema.ColumnsView()
	for i := range positions {
		positions[i] = -1
	}
	states := make([]aggregateState, len(kinds))
	result := &Result{Columns: make([]Column, len(kinds))}
	for i, item := range statement.Items {
		kind, argument, ok := parseAggregateExpression(item.Expression)
		if !ok {
			return nil, errors.New("non-aggregate item without GROUP BY")
		}
		kinds[i] = kind
		typ := storage.TypeBigInt
		if argument != "*" {
			expression, err := parser.ParseExpression(argument)
			if err != nil {
				return nil, err
			}
			expressions[i] = expression
			// Resolve direct column references once; use the same value wrapper
			// as the generic evaluator for JSON, collation and NULL semantics.
			if identifier, ok := expression.(parser.Identifier); ok && identifier.Name != sessionLookupIdentifier && !strings.EqualFold(identifier.Name, "LAST_INSERT_ID()") {
				if position, found := queryColumnIndex(schema, identifier.Name); found {
					positions[i] = position
				}
			}
			typ, err = expressionTypeWithSession(expression, schema, schema.ColumnsView(), session)
			if err != nil {
				return nil, err
			}
		} else if kind != aggregateCount {
			return nil, errors.New("only COUNT accepts *")
		}
		if kind == aggregateSum && typ == storage.TypeInt {
			typ = storage.TypeBigInt
		}
		if kind == aggregateCount {
			typ = storage.TypeBigInt
		}
		if kind == aggregateAvg && typ != storage.TypeDecimal {
			typ = storage.TypeDouble
		}
		name := item.Alias
		if name == "" {
			name = item.Expression
		}
		result.Columns[i] = Column{Name: name, Type: typ}
	}
	err := source(func(row storage.Row) error {
		for i, kind := range kinds {
			var candidate any
			if expressions[i] != nil {
				var err error
				if position := positions[i]; position >= 0 {
					err = checkQuery(session)
					if err == nil {
						candidate = jsonColumnValue(columns[position], row[position])
					}
				} else {
					candidate, err = evaluateExprWithContext(expressions[i], schema, row, session, nil)
				}
				if err != nil {
					return err
				}
			}
			if err := updateAggregate(&states[i], kind, candidate, expressions[i] == nil, session); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if statement.Offset > 0 || statement.HasLimit && statement.Limit == 0 {
		return result, nil
	}
	values := make([]any, len(kinds))
	for i, kind := range kinds {
		values[i], err = finishAggregate(states[i], kind, result.Columns[i].Type)
		if err != nil {
			return nil, fmt.Errorf("aggregate: %w", err)
		}
	}
	result.Rows = [][]any{values}
	return result, nil
}

func scanMVCCRows(ctx context.Context, tx *mvcc.Tx, table versionedTable, schema *storage.Table, session *Session, where parser.Expr, yield func([]byte, []byte) error) error {
	return planMVCCAccess(parser.Select{Where: where}, table, schema, session).scan(ctx, tx, table, yield)
}
