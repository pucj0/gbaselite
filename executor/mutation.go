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
		var columns []storage.Column
		primary := append([]string(nil), value.PrimaryKey...)
		var indexes []storage.Index
		for _, column := range value.Columns {
			if column.OnUpdate != "" {
				return nil, errors.New("ON UPDATE column expressions are not supported")
			}
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
			indexes = append(indexes, storage.Index{Name: index.Name, Columns: index.Columns, Unique: index.Unique})
		}
		mirror := storage.NewStore()
		database, _ := mirror.CreateDatabase(db)
		table, err := database.CreateTableWithIndexes(name, columns, primary, indexes)
		if err != nil {
			return nil, err
		}
		checks := make([]storage.CheckConstraint, 0, len(value.Checks))
		for _, check := range value.Checks {
			checks = append(checks, storage.CheckConstraint{Name: check.Name, Expression: check.Expression})
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
		if !exists {
			if value.IfExists {
				return &Result{}, nil
			}
			return nil, storage.ErrDatabaseNotFound
		}
		err = read.Scan(ctx, sqllayout.Catalog, func(k, v []byte) error {
			if strings.HasPrefix(string(k), sqllayout.TablesPrefix(name)) {
				var dropping versionedTable
				if err := decodeVersioned(v, &dropping); err != nil {
					return err
				}
				if err := rejectSQLReferencedDrop(write, dropping, session.ForeignKeyChecksDisabled); err != nil {
					return err
				}
				return write.Delete(sqllayout.Catalog, k)
			}
			return nil
		})
		if err != nil {
			return nil, err
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
		// Always qualify the evaluation schema, like the legacy executor, so a
		// correlated subquery can bind an explicitly qualified outer column.
		schema, err = qualifySchema(schema, mutationQualifier(value.Table, value.TableAlias))
		if err != nil {
			return nil, err
		}
		var updated storage.Row
		count := 0
		limit := -1
		if value.HasLimit {
			limit = value.Limit
		}
		err = runRowModification(ctx, read, definition, schema, session, value.Where, limit, func(key []byte, row storage.Row) error {
			if cap(updated) < len(row) {
				updated = make(storage.Row, len(row))
			} else {
				updated = updated[:len(row)]
			}
			copy(updated, row)
			for _, assignment := range value.Assignments {
				position, ok := queryColumnIndex(schema, assignment.Column)
				if !ok {
					return storage.ErrColumnNotFound
				}
				raw, err := evaluateExprWithContext(assignment.Value, schema, updated, session, nil)
				if err != nil {
					return err
				}
				updated[position], err = interfaceToColumnValue(raw, definition.Definition.Columns[position])
				if err != nil {
					return err
				}
			}
			if err := writeVersionedRow(ctx, write, definition, key, row, updated, ""); err != nil {
				return err
			}
			count++
			return nil
		})
		return &Result{AffectedRows: uint64(count)}, err
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

		count := 0
		limit := -1
		if value.HasLimit {
			limit = value.Limit
		}
		err = runRowModification(ctx, read, definition, schema, session, value.Where, limit, func(key []byte, row storage.Row) error {
			if err := writeVersionedRow(ctx, write, definition, key, row, nil, ""); err != nil {
				return err
			}
			count++
			return nil
		})
		return &Result{AffectedRows: uint64(count)}, err
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
	result := &Result{}
	lastGenerated := uint64(0)
	floors, sent, next, last := make([]uint64, len(columns)), make([]uint64, len(columns)), make([]uint64, len(columns)), make([]uint64, len(columns))
	advance := func(i int) error {
		if floors[i] <= sent[i] {
			return nil
		}
		if err := storageengine.AdvanceCounter(ctx, e.Backend, definition.counterKey(columns[i].Name), floors[i]); err != nil {
			return err
		}
		sent[i] = floors[i]
		return nil
	}
	// The destination view and duplicate-key policy are shared with the
	// INSERT SELECT writer so both sources handle conflicts identically.
	target := &insertTarget{definition: definition, columns: columns, positions: positions, floors: floors, sent: sent, next: next, last: last}
	mode := insertStatementMode(statement)
	input := physical.Source[int](func(_ context.Context, y physical.Yield[int]) error {
		for i := range statement.Values {
			if err := y(i); err != nil {
				return err
			}
		}
		return nil
	})
	modify := physical.Modify[int, struct{}]{Input: input, Apply: func(ctx context.Context, rowIndex int) (struct{}, error) {
		literals := statement.Values[rowIndex]
		if err := ctx.Err(); err != nil {
			return struct{}{}, err
		}
		generated := uint64(0)
		if len(literals) != len(positions) {
			return struct{}{}, errors.New("INSERT value count does not match columns")
		}
		row := make(storage.Row, len(columns))
		for i, column := range columns {
			row[i] = storage.NullValue(column.Type)
			if column.HasDefault {
				row[i], err = columnDefaultValue(column, session)
				if err != nil {
					return struct{}{}, err
				}
			}
		}
		for valueIndex, literal := range literals {
			position := positions[valueIndex]
			if expression, ok := statement.ValueExpressions[[2]int{rowIndex, valueIndex}]; ok {
				raw, err := evaluateExprWithContext(expression, schema, row, session, nil)
				if err != nil {
					return struct{}{}, err
				}
				row[position], err = interfaceToColumnValue(raw, columns[position])
				if err != nil {
					return struct{}{}, err
				}
			} else {
				row[position], err = literalToValue(literal, columns[position])
				if err != nil {
					return struct{}{}, err
				}
			}
		}
		for i, column := range columns {
			if !column.AutoIncrement {
				continue
			}
			if !row[i].Null && row[i].Int64 > 0 {
				value := uint64(row[i].Int64)
				if value > floors[i] {
					floors[i] = value
				}
				if value >= next[i] {
					next[i] = value + 1
				}
			}
			if row[i].Null {
				if next[i] == 0 || next[i] > last[i] {
					if err := advance(i); err != nil {
						return struct{}{}, err
					}
					count := uint64(len(statement.Values) - rowIndex)
					reserved, err := storageengine.ReserveCounter(ctx, e.Backend, definition.counterKey(column.Name), count)
					if err != nil {
						return struct{}{}, err
					}
					next[i] = reserved
					last[i] = reserved + count - 1
				}
				id := next[i]
				next[i]++
				row[i], err = storage.NewValue(column.Type, int64(id))
				if err != nil {
					return struct{}{}, err
				}
				if generated == 0 {
					generated = id
				}
			}
		}
		outcome, writeErr := writeInsertedRow(ctx, write, session, target, mode, row, uint64(rowIndex))
		if writeErr != nil {
			return struct{}{}, writeErr
		}
		result.AffectedRows += uint64(outcome.affected)
		if outcome.inserted && generated != 0 && lastGenerated == 0 {
			lastGenerated = generated
		}
		return struct{}{}, nil
	}}
	if err := modify.Run(ctx, func(struct{}) error { return nil }); err != nil {
		return nil, err
	}

	for i := range columns {
		if err := advance(i); err != nil {
			return nil, err
		}
	}
	if lastGenerated != 0 {
		result.LastInsertID = lastGenerated
	}
	return result, nil
}
func writeVersionedRow(ctx context.Context, tx storageengine.Txn, table versionedTable, oldKey []byte, oldRow, newRow storage.Row, fallback string) error {
	columns := table.Definition.Columns
	if err := validateSQLReferences(ctx, tx, table, oldRow, newRow); err != nil {
		return err
	}
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
