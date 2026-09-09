package executor

import (
	"errors"
	"fmt"
	"strings"

	"gbaselite/parser"
	"gbaselite/storage"
)

var errColdSelectDone = errors.New("cold select limit reached")

// prepareColdStatement is called after authorization and under the ordinary
// transaction gate. Only independently safe streaming shapes bypass legacy
// materialization; complex queries and writes receive explicit I/O/budget
// errors before any legacy API can observe an unloaded table as empty.
func (e *legacyEngine) prepareColdStatement(store *storage.Store, session *Session, statement parser.Statement) (*Result, bool, error) {
	if !e.ColdRead || !store.HasColdTables() {
		return nil, false, nil
	}
	switch value := statement.(type) {
	case parser.Empty, parser.Use, parser.ShowGrants, parser.ShowCreateUser, parser.CreateUser, parser.AlterUser, parser.DropUser, parser.RenameUser, parser.SetPassword, parser.Grant, parser.Revoke, parser.Commit, parser.Rollback, parser.Savepoint, parser.RollbackTo, parser.ReleaseSavepoint:
		return nil, false, nil
	case parser.Show:
		if coldShowMetadataOnly(store, session, value) {
			return nil, false, nil
		}
	case parser.Select:
		if coldStandaloneSelect(value) {
			return nil, false, nil
		}
		databaseName, _ := splitTableName(value.Table)
		if databaseName == "" {
			databaseName = session.CurrentDatabase
		}
		switch strings.ToLower(databaseName) {
		case "information_schema", "mysql", "performance_schema", "sys":
			if coldSelectShape(value) {
				return nil, false, nil
			}
		}
		if coldSelectShape(value) && (!value.Distinct || querySortEnabled(session)) {
			if value.Table == "" {
				return nil, false, nil
			}
			databaseName, tableName := splitTableName(value.Table)
			database, err := selectedDatabase(store, session, databaseName)
			if err != nil {
				return nil, true, err
			}
			if session.temporaryTables != nil && !strings.Contains(value.Table, ".") {
				if _, exists := session.temporaryTables[strings.ToLower(tableName)]; exists {
					return nil, false, nil
				}
			}
			table, err := database.Table(tableName)
			if err == nil {
				if !table.IsCold() {
					return nil, false, nil
				}
				if len(value.OrderBy) == 0 || coldSelectOrderSupported(value, table, session) || querySortEnabled(session) {
					result, err := executeColdSelect(store, session, table, value)
					return result, true, err
				}
			}
			// Views may contain arbitrary queries, so they use the guarded fallback.
		}
	}
	if err := checkQuery(session); err != nil {
		return nil, true, err
	}
	if err := store.Materialize(e.ColdMaterializeBytes); err != nil {
		return nil, true, err
	}
	return nil, false, checkQuery(session)
}

func coldSelectShape(statement parser.Select) bool {
	for _, order := range statement.OrderBy {
		expression, err := parser.ParseExpression(order.Column)
		if err != nil || !scalarExpressionSupported(expression) {
			return false
		}
	}
	if statement.Subquery != nil || len(statement.Joins) > 0 || len(statement.GroupBy) > 0 || statement.Having != nil || statement.Locking || selectHasWindow(statement.Items) {
		return false
	}
	count := len(statement.Items) == 1 && isCountExpression(statement.Items[0].Expression)
	if selectHasAggregate(statement.Items) && !count {
		return false
	}
	if !scalarExpressionSupported(statement.Where) {
		return false
	}
	for _, item := range statement.Items {
		if strings.TrimSpace(item.Expression) == "*" || count {
			continue
		}
		expression, err := parser.ParseExpression(item.Expression)
		if err != nil || !scalarExpressionSupported(expression) {
			return false
		}
	}
	return true
}

func executeColdSelect(store *storage.Store, session *Session, table *storage.Table, statement parser.Select) (*Result, error) {
	if statement.Distinct {
		base := statement
		base.Distinct = false
		base.HasLimit = false
		base.Offset = 0
		streaming := *session
		streaming.StreamResults = true
		source, err := executeColdSelect(store, &streaming, table, base)
		if err != nil {
			return nil, err
		}
		limit := -1
		if statement.HasLimit {
			limit = statement.Limit
		}
		return executeBudgetedDistinct(session, source, statement.Offset, limit)
	}
	schema := table
	if statement.TableAlias != "" {
		var err error
		schema, err = qualifySchema(table, statement.TableAlias)
		if err != nil {
			return nil, err
		}
	}
	columns := schema.ColumnsView()
	q := session.query
	accessPlan := coldSelectIndexPlan(statement, table, schema, session)
	visit := func(yield func(storage.Row) error) error {
		if accessPlan != nil {
			return table.StreamIndex(accessPlan.Scan, nil, 0, -1, yield)
		}
		return table.Visit(nil, yield)
	}
	matches := func(row storage.Row) (bool, error) {
		if err := q.check(); err != nil {
			return false, err
		}
		if statement.Where == nil {
			return true, nil
		}
		value, err := evaluateExprWithContext(statement.Where, schema, row, session, store)
		return err == nil && truthy(value), err
	}
	if len(statement.Items) == 1 && isCountExpression(statement.Items[0].Expression) {
		label := statement.Items[0].Alias
		if label == "" {
			label = "COUNT(*)"
		}
		result := &Result{Columns: []Column{{Name: label, Type: storage.TypeBigInt}}}
		if statement.Offset > 0 || statement.HasLimit && statement.Limit == 0 {
			return result, nil
		}
		count := table.RowCount()
		if statement.Where != nil {
			count = 0
			err := visit(func(row storage.Row) error {
				matched, err := matches(row)
				if err != nil {
					return err
				}
				if matched {
					count++
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		result.Rows = [][]any{{int64(count)}}
		return result, nil
	}
	sourceSchema, sourceTable := splitTableName(statement.Table)
	if sourceSchema == "" {
		sourceSchema = session.CurrentDatabase
	}
	sourceColumn := func(column storage.Column, label string) Column {
		result := Column{Name: label, Type: column.Type, SQLType: column.SQLType, Collation: column.Collation, jsonValue: strings.EqualFold(strings.TrimSpace(column.SQLType), "JSON"), Length: column.Length, Schema: sourceSchema, Table: sourceTable, OriginalName: stripQualifier(column.Name), Nullable: storage.ColumnNullable(column), AutoIncrement: column.AutoIncrement}
		switch table.ColumnKey(stripQualifier(column.Name)) {
		case "PRI":
			result.PrimaryKey = true
		case "UNI":
			result.UniqueKey = true
		case "MUL":
			result.MultipleKey = true
		}
		return result
	}
	plans := make([]projectedExpression, 0, len(statement.Items))
	for _, item := range statement.Items {
		if strings.TrimSpace(item.Expression) == "*" {
			for _, column := range columns {
				plans = append(plans, projectedExpression{expression: parser.Identifier{Name: column.Name}, column: sourceColumn(column, stripQualifier(column.Name))})
			}
			continue
		}
		expression, err := parser.ParseExpression(item.Expression)
		if err != nil {
			return nil, err
		}
		label := item.Alias
		if label == "" {
			label = item.Expression
			if identifier, ok := expression.(parser.Identifier); ok {
				label = stripQualifier(identifier.Name)
			}
		}
		kind, err := expressionTypeWithSession(expression, schema, columns, session)
		if err != nil {
			return nil, err
		}
		output := Column{Name: label, Type: kind, jsonValue: expressionReturnsJSON(expression, schema)}
		if identifier, ok := expression.(parser.Identifier); ok {
			index, ok := queryColumnIndex(schema, identifier.Name)
			if !ok {
				return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, identifier.Name)
			}
			column := columns[index]
			output = sourceColumn(column, label)
		}
		plans = append(plans, projectedExpression{expression: expression, column: output})
	}
	result := &Result{Columns: make([]Column, len(plans))}
	for index, plan := range plans {
		result.Columns[index] = plan.column
	}
	project := func(row storage.Row) ([]any, error) {
		values := make([]any, len(plans))
		for index, plan := range plans {
			value, err := evaluateExprWithContext(plan.expression, schema, row, session, store)
			if err != nil {
				return nil, err
			}
			values[index] = value
		}
		return values, nil
	}
	if len(statement.OrderBy) > 0 && (accessPlan == nil || !accessPlan.OrderSatisfied) {
		return executeBudgetedExpressionOrderWithSource(store, session, statement, schema, result.Columns, project, func(yield func(storage.Row) error) error {
			return visit(func(row storage.Row) error {
				matched, err := matches(row)
				if err != nil {
					return err
				}
				if !matched {
					return nil
				}
				return yield(row)
			})
		})
	}
	run := func(yield func([]any) error) error {
		if statement.HasLimit && statement.Limit == 0 {
			return q.check()
		}
		offset, emitted := statement.Offset, 0
		err := visit(func(row storage.Row) error {
			matched, err := matches(row)
			if err != nil {
				return err
			}
			if !matched {
				return nil
			}
			if offset > 0 {
				offset--
				return nil
			}
			values := make([]any, len(plans))
			for index, plan := range plans {
				value, err := evaluateExprWithContext(plan.expression, schema, row, session, store)
				if err != nil {
					return err
				}
				values[index] = value
			}
			if err := yield(values); err != nil {
				return err
			}
			emitted++
			if statement.HasLimit && emitted >= statement.Limit {
				return errColdSelectDone
			}
			return nil
		})
		if errors.Is(err, errColdSelectDone) {
			return nil
		}
		return err
	}
	if session.StreamResults {
		result.StreamRows = run
		return result, nil
	}
	used := int64(0)
	memoryLimit := int64(64 << 20)
	if q != nil && q.options.ResultMemoryBytes > 0 {
		memoryLimit = q.options.ResultMemoryBytes
	}
	err := run(func(row []any) error {
		var err error
		used, err = checkResultMemory(memoryLimit, used, row)
		if err != nil {
			return err
		}
		result.Rows = append(result.Rows, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func coldShowMetadataOnly(store *storage.Store, session *Session, statement parser.Show) bool {
	if !scalarExpressionSupported(statement.Where) {
		return false
	}
	if statement.What != "COLUMNS" {
		return true
	}
	resolved := resolveCopyTargetReference(store, session, statement.Name)
	databaseName, tableName := splitTableName(resolved)
	if !strings.Contains(resolved, ".") && session.temporaryTables != nil {
		if _, exists := session.temporaryTables[strings.ToLower(tableName)]; exists {
			return true
		}
	}
	database, err := selectedDatabase(store, session, databaseName)
	if err != nil {
		return true
	} // Preserve the usual unknown-database error.
	if _, err := database.Table(tableName); err == nil {
		return true
	}
	return false // Views execute their definition before inferring columns.
}

func coldSelectIndexPlan(statement parser.Select, table, schema *storage.Table, session *Session) *indexAccessPlan {
	if statement.Where != nil && !safeMutationIndexExpression(statement.Where, schema) {
		// Preserve predicate precision/coercion: still allow an ordered full index
		// walk, but do not narrow its interval using an unsafe conversion.
		statement.Where = nil
	}
	return planIndexAccess(statement, table, session)
}

func coldSelectOrderSupported(statement parser.Select, table *storage.Table, session *Session) bool {
	schema := table
	if statement.TableAlias != "" {
		var err error
		schema, err = qualifySchema(table, statement.TableAlias)
		if err != nil {
			return false
		}
	}
	for _, order := range statement.OrderBy {
		if _, ok := queryColumnIndex(schema, order.Column); !ok {
			return false
		}
	}
	plan := coldSelectIndexPlan(statement, table, schema, session)
	return plan != nil && plan.OrderSatisfied
}

func coldStandaloneSelect(statement parser.Select) bool {
	for _, order := range statement.OrderBy {
		expression, err := parser.ParseExpression(order.Column)
		if err != nil || !scalarExpressionSupported(expression) {
			return false
		}
	}
	for _, group := range statement.GroupBy {
		expression, err := parser.ParseExpression(group)
		if err != nil || !scalarExpressionSupported(expression) {
			return false
		}
	}
	if statement.Table != "" || statement.Subquery != nil || len(statement.Joins) > 0 || !scalarExpressionSupported(statement.Where) || !scalarExpressionSupported(statement.Having) {
		return false
	}
	for _, item := range statement.Items {
		expression, err := parser.ParseExpression(item.Expression)
		if err == nil && scalarExpressionSupported(expression) {
			continue
		}
		variable := strings.Join(strings.Fields(item.Expression), "")
		if !strings.HasPrefix(variable, "@") {
			return false
		}
		for _, character := range variable {
			if character != '@' && character != '_' && character != '.' && !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') && !(character >= '0' && character <= '9') {
				return false
			}
		}
	}
	return true
}
