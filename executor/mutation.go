package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

func (e *Engine) mutateSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Statement) (*Result, error) {
	if tableName, ok := sqlAlterTarget(statement); ok {
		return e.alterSQL(ctx, read, write, session, tableName, statement)
	}
	switch value := statement.(type) {
	case parser.CreateDatabase:
		name := strings.ToLower(value.Name)
		if strings.ContainsAny(name, "/\x00") {
			return nil, errors.New("invalid database identifier")
		}
		k := sqllayout.DatabaseKey(name)
		_, exists, err := write.Get(sqllayout.Catalog, k)
		if err != nil {
			return nil, err
		}
		if exists {
			if value.IfNotExists {
				return &Result{}, nil
			}
			return nil, storage.ErrDatabaseExists
		}
		return &Result{AffectedRows: 1}, write.Put(sqllayout.Catalog, k, []byte{1})
	case parser.CreateTableAs:
		return e.createTableAsSQL(ctx, read, write, session, value)
	case parser.CreateTableLike:
		return e.createTableLikeSQL(ctx, read, write, session, value)
	case parser.CreateView:
		return e.createViewSQL(ctx, read, write, session, value)
	case parser.DropView:
		return e.dropViewSQL(ctx, read, write, session, value)
	case parser.RenameTable:
		return e.renameTablesSQL(ctx, read, write, session, value)
	case parser.CreateTable:
		db, name, err := versionedName(session, value.Name)
		if err != nil {
			return nil, err
		}
		if _, ok, err := write.Get(sqllayout.Catalog, sqllayout.DatabaseKey(db)); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			return nil, storage.ErrDatabaseNotFound
		}
		k := sqllayout.TableKey(db, name)
		if _, exists, err := write.Get(sqllayout.Catalog, k); err != nil {
			return nil, err
		} else if exists {
			if value.IfNotExists {
				return &Result{}, nil
			}
			return nil, storage.ErrTableExists
		}
		// A view owns the same namespace: the legacy engine refuses the table and
		// only swallows the conflict for IF NOT EXISTS.
		if _, exists, err := viewCatalogKey(write, session, value.Name); err != nil {
			return nil, err
		} else if exists {
			if value.IfNotExists {
				return &Result{}, nil
			}
			return nil, fmt.Errorf("%w: %q is a view", storage.ErrTableExists, name)
		}
		var columns []storage.Column
		primary := append([]string(nil), value.PrimaryKey...)
		var indexes []storage.Index
		for _, column := range value.Columns {
			// Legacy accepts ON UPDATE expressions and applies the CURRENT_TIMESTAMP
			// forms on UPDATE; other expressions are kept as metadata only.
			definition, err := storageColumnDefinition(column)
			if err != nil {
				return nil, err
			}
			columns = append(columns, definition)
			if column.PrimaryKey {
				found := false
				for _, name := range primary {
					if strings.EqualFold(name, column.Name) {
						found = true
					}
				}
				if !found {
					primary = append(primary, column.Name)
				}
			}
			if column.Unique {
				indexes = append(indexes, storage.Index{Name: column.Name, Columns: []string{column.Name}, Unique: true})
			}
		}
		for _, index := range value.Indexes {
			indexes = append(indexes, storage.Index{Name: index.Name, Columns: index.Columns, Unique: index.Unique, Comment: index.Comment})
		}
		mirror := storage.NewStore()
		database, _ := mirror.CreateDatabase(db)
		table, err := database.CreateTableWithIndexes(name, columns, primary, indexes)
		if err != nil {
			return nil, err
		}
		checks := make([]storage.CheckConstraint, 0, len(value.Checks))
		for _, check := range value.Checks {
			checks = append(checks, storage.CheckConstraint{Name: check.Name, Expression: check.Expression, NotEnforced: check.NotEnforced})
		}
		for _, column := range value.Columns {
			if column.Check != "" {
				checks = append(checks, storage.CheckConstraint{Expression: column.Check})
			}
		}
		for _, check := range checks {
			if err := validateSQLCheckDefinition(table, check.Expression); err != nil {
				return nil, err
			}
		}
		table.SetNamedConstraints(nil, checks)
		snapshot := table.Snapshot()
		snapshot.Comment = value.Comment
		definition := versionedTable{CatalogName: db + "." + name, ID: write.ID() + "/" + name, Definition: snapshot, RowEncoding: sqlCompactRowEncoding, SecondaryEncoding: 1}
		if _, ok := sqlIntegerPrimary(definition); ok {
			definition.KeyEncoding = sqlIntegerKeyEncoding
		}
		for _, fk := range value.ForeignKeys {
			definition.Definition.ForeignKeys = append(definition.Definition.ForeignKeys, storage.ForeignKey{Name: fk.Name, Columns: fk.Columns, RefTable: fk.RefTable, RefColumns: fk.RefColumns, OnDelete: fk.OnDelete, OnUpdate: fk.OnUpdate})
		}
		if err = prepareSQLForeignKeys(write, &definition, session); err != nil {
			return nil, err
		}
		encoded, err := encodeVersioned(definition)
		if err != nil {
			return nil, err
		}
		if err = write.Guard(sqllayout.Catalog, sqllayout.DatabaseKey(db)); err != nil {
			return nil, err
		}
		return &Result{AffectedRows: 1}, write.Put(sqllayout.Catalog, k, encoded)
	case parser.DropDatabase:
		name := strings.ToLower(value.Name)
		k := sqllayout.DatabaseKey(name)
		_, exists, err := read.Get(sqllayout.Catalog, k)
		if err != nil {
			return nil, err
		}
		// The legacy engine clears the session's selected database whenever the
		// statement names it, including the IF EXISTS no-op path, so later unqualified
		// statements fail with "no database selected" instead of writing into a stale
		// database.
		if strings.EqualFold(session.CurrentDatabase, name) {
			session.CurrentDatabase = ""
		}
		if !exists {
			if value.IfExists {
				return &Result{}, nil
			}
			return nil, storage.ErrDatabaseNotFound
		}
		// Collect every catalog entry of the database before mutating anything: the
		// namespace holds table/<db>/... and view/<db>/... keys, the scan must never
		// observe its own deletes, and no orphan view entry may survive the drop.
		type databaseEntry struct {
			key     []byte
			table   versionedTable
			isTable bool
		}
		var (
			entries  []databaseEntry
			dropping = make(map[string]bool)
		)
		tablePrefix := sqllayout.TablesPrefix(name)
		viewPrefix := sqllayout.ViewsPrefix(name)
		err = read.Scan(ctx, sqllayout.Catalog, func(entry, v []byte) error {
			catalogName := string(entry)
			switch {
			case strings.HasPrefix(catalogName, tablePrefix):
				var table versionedTable
				if err := decodeVersioned(v, &table); err != nil {
					return err
				}
				dropping[strings.ToLower(table.CatalogName)] = true
				entries = append(entries, databaseEntry{key: append([]byte(nil), entry...), table: table, isTable: true})
			case strings.HasPrefix(catalogName, viewPrefix):
				entries = append(entries, databaseEntry{key: append([]byte(nil), entry...)})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		// Tables and views of the dropped database go away in one statement, so only
		// references held by relations outside it can block the drop. Legacy drops the
		// database unconditionally, and a parent/child pair inside it must not depend
		// on catalog key order.
		for _, entry := range entries {
			if !entry.isTable {
				continue
			}
			if err = rejectSQLReferencedDropExcluding(write, entry.table, dropping, session.ForeignKeyChecksDisabled); err != nil {
				return nil, err
			}
		}
		for _, entry := range entries {
			if err = write.Delete(sqllayout.Catalog, entry.key); err != nil {
				return nil, err
			}
		}
		return &Result{AffectedRows: 1}, write.Delete(sqllayout.Catalog, k)
	case parser.Truncate:
		definition, _, k, err := loadVersionedTable(read, session, value.Table)
		if err != nil {
			return nil, err
		}
		if err = rejectSQLReferencedDrop(write, definition, session.ForeignKeyChecksDisabled); err != nil {
			return nil, err
		}
		definition.CounterKeys = nil
		definition.ID = write.ID() + "/truncate"
		encoded, err := encodeVersioned(definition)
		if err != nil {
			return nil, err
		}
		return &Result{}, write.Put(sqllayout.Catalog, k, encoded)
	case parser.DropTable:
		for _, name := range value.Names {
			dropping, _, k, err := loadVersionedTable(write, session, name)
			if err != nil {
				if value.IfExists && errors.Is(err, storage.ErrTableNotFound) {
					continue
				}
				return nil, err
			}
			if err = rejectSQLReferencedDrop(write, dropping, session.ForeignKeyChecksDisabled); err != nil {
				return nil, err
			}
			if err = write.Delete(sqllayout.Catalog, k); err != nil {
				return nil, err
			}
		}
		return &Result{AffectedRows: uint64(len(value.Names))}, nil
	case parser.Insert:
		return e.insertSQL(ctx, read, write, session, value)
	case parser.Update:
		if len(value.Joins) > 0 {
			return e.joinUpdateSQL(ctx, read, write, session, value)
		}
		definition, schema, k, err := loadVersionedTable(read, session, value.Table)
		if err != nil {
			return nil, err
		}
		if err = write.Guard(sqllayout.Catalog, k); err != nil {
			return nil, err
		}
		qualifier := mutationQualifier(value.Table, value.TableAlias)
		// Always qualify the evaluation schema, like the legacy executor, so a
		// correlated subquery can bind an explicitly qualified outer column.
		schema, err = qualifySchema(schema, qualifier)
		if err != nil {
			return nil, err
		}
		_, targetTable := splitTableName(value.Table)
		// Assignments resolve before the scan starts, so an unknown or duplicated
		// target column fails before the first mutation.
		assignments, err := resolveUpdateAssignments(value, definition, schema, qualifier, targetTable, qualifier, schema)
		if err != nil {
			return nil, err
		}
		limit := -1
		if value.HasLimit {
			limit = value.Limit
		}
		operator := &UpdateOperator{
			Input:       updateCandidates(mutationScan(ctx, read, definition, schema, session, value.Where, limit), definition),
			Target:      definition,
			Schema:      schema,
			Assignments: assignments,
			Write:       write,
			Session:     session,
		}
		if err = operator.Run(ctx); err != nil {
			return nil, err
		}
		return &Result{AffectedRows: operator.Result.AffectedRows}, nil
	case parser.Delete:
		if len(value.Joins) > 0 || len(value.Targets) > 0 {
			return e.multiTableDeleteSQL(ctx, read, write, session, value)
		}
		definition, schema, k, err := loadVersionedTable(read, session, value.Table)
		if err != nil {
			return nil, err
		}
		if err = write.Guard(sqllayout.Catalog, k); err != nil {
			return nil, err
		}
		// Always qualify the evaluation schema so a correlated subquery can bind
		// an explicitly qualified outer column.
		schema, err = qualifySchema(schema, mutationQualifier(value.Table, value.TableAlias))
		if err != nil {
			return nil, err
		}

		// A plain DELETE lowers into the unified delete pipeline: the access plan decides how
		// the target rows are found, the scan and WHERE select them from the parent statement
		// snapshot, and DeleteOperator performs the deletion through the statement child
		// transaction.
		limit := -1
		if value.HasLimit {
			limit = value.Limit
		}
		operator := &DeleteOperator{
			Input:   deleteCandidates(mutationScan(ctx, read, definition, schema, session, value.Where, limit), definition),
			Target:  definition,
			Write:   write,
			Session: session,
		}
		if err = operator.Run(ctx); err != nil {
			return nil, err
		}
		return &Result{AffectedRows: operator.Result.AffectedRows}, nil
	default:
		return nil, fmt.Errorf("storage engine does not support statement %T", statement)
	}
}
func (e *Engine) insertSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Insert) (*Result, error) {
	if len(statement.SetValues) > 0 {
		statement = insertSetStatement(statement)
	}
	if statement.Select != nil {
		return e.insertSelectSQL(ctx, read, write, session, statement)
	}
	definition, schema, catalogKey, err := loadVersionedTable(write, session, statement.Table)
	if err != nil {
		return nil, err
	}
	if err = write.Guard(sqllayout.Catalog, catalogKey); err != nil {
		return nil, err
	}
	columns := definition.Definition.Columns
	positions := make([]int, len(columns))
	for i := range positions {
		positions[i] = i
	}
	if len(statement.Columns) > 0 {
		positions = make([]int, len(statement.Columns))
		seen := map[int]bool{}
		for i, name := range statement.Columns {
			position, ok := schema.ColumnIndex(name)
			if !ok {
				return nil, storage.ErrColumnNotFound
			}
			if seen[position] {
				return nil, errors.New("duplicate insert column")
			}
			seen[position] = true
			positions[i] = position
		}
	}
	// INSERT VALUES lowers its literal rows into InsertCandidate and writes them
	// through the same InsertOperator as INSERT SELECT, so target column mapping,
	// DEFAULT handling, auto-increment, conflict policy and result accounting all have
	// exactly one implementation.
	target := &insertTarget{definition: definition, columns: columns, positions: positions}
	operator := &InsertOperator{
		Input:   valuesSource(statement, schema, columns, positions, session),
		Target:  target,
		Write:   write,
		Engine:  e,
		Session: session,
		Mode:    insertStatementMode(statement),
		Rows:    uint64(len(statement.Values)),
	}
	if err = operator.Run(ctx); err != nil {
		return nil, err
	}
	return operator.Publish(ctx)
}

// valuesSource lowers the statement's literal rows into InsertCandidate values.
// Each row is assembled the way the INSERT VALUES path has always assembled it:
// defaults first, then the mapped literals or value expressions, with every
// conversion error surfacing before any write for that row happens.
func valuesSource(statement parser.Insert, schema *storage.Table, columns []storage.Column, positions []int, session *Session) physical.Operator[InsertCandidate] {
	return physical.Source[InsertCandidate](func(_ context.Context, y physical.Yield[InsertCandidate]) error {
		for rowIndex := range statement.Values {
			candidate, err := valuesCandidate(statement, schema, columns, positions, session, rowIndex)
			if err != nil {
				return err
			}
			if err := y(candidate); err != nil {
				return err
			}
		}
		return nil
	})
}

// valuesCandidate assembles one INSERT VALUES row into a candidate: defaults first,
// then the mapped literals or value expressions. The row is freshly allocated per
// candidate because the operator mutates it in place (auto-increment and, for the
// conflict policies, expression evaluation).
func valuesCandidate(statement parser.Insert, schema *storage.Table, columns []storage.Column, positions []int, session *Session, rowIndex int) (InsertCandidate, error) {
	literals := statement.Values[rowIndex]
	if len(literals) != len(positions) {
		return InsertCandidate{}, errors.New("INSERT value count does not match columns")
	}
	row := make(storage.Row, len(columns))
	for i, column := range columns {
		row[i] = storage.NullValue(column.Type)
		if column.HasDefault {
			value, err := columnDefaultValue(column, session)
			if err != nil {
				return InsertCandidate{}, err
			}
			row[i] = value
		}
	}
	for valueIndex := range literals {
		position := positions[valueIndex]
		if expression, ok := statement.ValueExpressions[[2]int{rowIndex, valueIndex}]; ok {
			raw, err := evaluateExprWithContext(expression, schema, row, session, nil)
			if err != nil {
				return InsertCandidate{}, err
			}
			value, err := interfaceToColumnValue(raw, columns[position])
			if err != nil {
				return InsertCandidate{}, err
			}
			row[position] = value
			continue
		}
		value, err := literalToValue(literals[valueIndex], columns[position])
		if err != nil {
			return InsertCandidate{}, err
		}
		row[position] = value
	}
	return InsertCandidate{Values: row, Ordinal: uint64(rowIndex)}, nil
}

// writeVersionedRow validates foreign-key references before storing the row.
// Cascade helpers use writeVersionedRowUnchecked because they must update child
// rows before the parent row that satisfies their reference exists.
func writeVersionedRow(ctx context.Context, tx storageengine.Txn, table versionedTable, oldKey []byte, oldRow, newRow storage.Row, fallback string) error {
	if err := validateSQLReferences(ctx, tx, table, oldRow, newRow); err != nil {
		return err
	}
	return writeVersionedRowUnchecked(ctx, tx, table, oldKey, oldRow, newRow, fallback)
}

func writeVersionedRowUnchecked(ctx context.Context, tx storageengine.Txn, table versionedTable, oldKey []byte, oldRow, newRow storage.Row, fallback string) error {
	columns := table.Definition.Columns
	rows := tx.Table(table.ID)
	var newKey []byte
	if newRow != nil {
		if err := validateVersionedChecks(table, newRow); err != nil {
			return err
		}
		if err := storage.ValidateRowValues(columns, newRow); err != nil {
			return err
		}
		newKey = []byte(fallback)
		if oldKey != nil {
			newKey = oldKey
		}
		for _, index := range table.Definition.Indexes {
			if index.Primary {
				key, ok := sqlPrimaryKey(table, index, newRow)
				if !ok {
					return errors.New("NULL primary key")
				}
				newKey = key
			}
		}
		if !bytes.Equal(newKey, oldKey) {
			if _, exists, err := rows.Get(newKey); err != nil {
				return err
			} else if exists {
				return storage.ErrDuplicateKey
			}
		}
	}
	if err := writeSecondaryEntries(tx, table, oldKey, newKey, oldRow, newRow); err != nil {
		return err
	}
	if oldRow != nil {
		for _, index := range table.Definition.Indexes {
			if index.Primary && table.KeyEncoding == sqlIntegerKeyEncoding && table.RowEncoding == sqlCompactRowEncoding {
				continue
			}
			if !index.Unique && !index.Primary {
				continue
			}
			k, ok := storage.IndexValueKey(index, columns, oldRow)
			if ok {
				if err := rows.Index(index.Name, storageengine.UniqueIndex).Delete([]byte(k)); err != nil {
					return err
				}
			}
		}
		if newRow == nil || !bytes.Equal(oldKey, newKey) {
			if err := rows.Delete(oldKey); err != nil {
				return err
			}
		}
	}
	if newRow == nil {
		return nil
	}
	for _, index := range table.Definition.Indexes {
		if index.Primary && table.KeyEncoding == sqlIntegerKeyEncoding && table.RowEncoding == sqlCompactRowEncoding {
			continue
		}
		if !index.Unique && !index.Primary {
			continue
		}
		k, ok := storage.IndexValueKey(index, columns, newRow)
		if !ok {
			continue
		}
		indexHandle := rows.Index(index.Name, storageengine.UniqueIndex)
		if owner, exists, err := indexHandle.Get([]byte(k)); err != nil {
			return err
		} else if exists && !bytes.Equal(owner, oldKey) {
			return storage.ErrDuplicateKey
		}
		if err := indexHandle.Put([]byte(k), newKey); err != nil {
			return err
		}
	}
	encoded, err := encodeSQLRow(table, newRow)
	if err != nil {
		return err
	}
	return rows.Put(newKey, encoded)
}
