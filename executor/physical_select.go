package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

func executePhysicalSelect(ctx context.Context, tx storageengine.Txn, session *Session, statement parser.Select) (*Result, error) {
	if err := validateMVCCSelectShape(statement); err != nil {
		return nil, err
	}
	if statement.Table == "" && statement.Subquery == nil {
		return executeScalarSelect(session, statement)
	}
	if len(statement.Joins) > 0 {
		schema, source, err := joinedInput(tx, session, statement)
		if err != nil {
			return nil, err
		}
		return finishPhysicalSelect(session, statement, schema, source, false)
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
	ordered := plan.ordered
	op := bindScan(tx, definition, plan, func(encoded []byte) (storage.Row, error) {
		if err := checkQuery(session); err != nil {
			return nil, err
		}
		return decodeMVCCRowInto(definition, encoded, needed, nil)
	})
	if statement.Where != nil {
		evaluate := bindMVCCFilter(statement.Where, schema, session)
		op = physical.Filter[storage.Row]{Input: op, Predicate: func(row storage.Row) (bool, error) {
			var value any
			var err error
			if evaluate != nil {
				value, err = evaluate(row)
			} else {
				value, err = evaluateExprWithContext(statement.Where, schema, row, session, nil)
			}
			return truthy(value), err
		}}
	}
	return finishPhysicalSelect(session, statement, schema, op, ordered)
}
func finishPhysicalSelect(session *Session, statement parser.Select, schema *storage.Table, source physical.Operator[storage.Row], ordered bool) (*Result, error) {
	if selectHasWindow(statement.Items) {
		base := statement
		base.Distinct = false
		if statement.Distinct {
			base.HasLimit = false
			base.Offset = 0
		}
		result, err := executeWindowWithInput(schema, base, schema.ColumnsView(), session, source)
		if err != nil || !statement.Distinct {
			return result, err
		}
		limit := -1
		if statement.HasLimit {
			limit = statement.Limit
		}
		local := *session
		local.StreamResults = false
		return executeBudgetedDistinct(&local, result, statement.Offset, limit)
	}

	if len(statement.GroupBy) > 0 || statement.Having != nil {
		if statement.Distinct {
			base := statement
			base.Distinct = false
			base.HasLimit = false
			base.Offset = 0
			result, err := executeGroupedSelectWithInput(schema, base, schema.ColumnsView(), session, source)
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
		return executeGroupedSelectWithInput(schema, statement, schema.ColumnsView(), session, source)
	}
	aggregate := selectHasAggregate(statement.Items)
	if aggregate {
		return executeGlobalAggregate(session, statement, schema, source)
	}
	if statement.Distinct {
		base := statement
		base.Distinct = false
		base.HasLimit = false
		base.Offset = 0
		base.Limit = 0
		result, err := finishPhysicalSelect(session, base, schema, source, ordered)
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
		if !scalarExpressionSupported(expression) {
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
		return executeBudgetedExpressionOrderWithInput(nil, &local, statement, schema, result.Columns, project, source)
	}
	used := int64(0)
	count := -1
	if statement.HasLimit {
		count = statement.Limit
	}
	input := physical.Limit[storage.Row]{Input: source, Offset: statement.Offset, Count: count}
	op := physical.Projection[storage.Row, []any]{Input: input, Project: project}
	err := op.Run(operatorContext(session), func(values []any) error {
		var err error
		used, err = checkResultMemory(session.query.options.ResultMemoryBytes, used, values)
		if err != nil {
			return err
		}
		result.Rows = append(result.Rows, values)
		return nil
	})
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
func executeGlobalAggregate(session *Session, statement parser.Select, schema *storage.Table, source physical.Operator[storage.Row]) (*Result, error) {
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
	add := func(row storage.Row) error {
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
	}
	op := physical.Aggregate[storage.Row, []any]{Input: source, New: func() (physical.Accumulator[storage.Row, []any], error) {
		return &aggregateBinding[storage.Row, []any]{add: add, finish: func(y physical.Yield[[]any]) error {
			values := make([]any, len(kinds))
			for i, kind := range kinds {
				value, err := finishAggregate(states[i], kind, result.Columns[i].Type)
				if err != nil {
					return fmt.Errorf("aggregate: %w", err)
				}
				values[i] = value
			}
			return y(values)
		}}, nil
	}}
	err := op.Run(operatorContext(session), func(values []any) error {
		if statement.Offset > 0 || statement.HasLimit && statement.Limit == 0 {
			return nil
		}
		result.Rows = append(result.Rows, values)
		return nil
	})
	return result, err
}
