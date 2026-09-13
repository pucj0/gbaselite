package executor

import (
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

// ctasRow keeps the raw query values so column nullability and decimal
// declarations can be derived before conversion.
type ctasRow struct{ values []any }

// createTableAsSQL implements CREATE TABLE ... AS SELECT: the query runs on the
// statement snapshot through the unified pipeline, the result columns become the
// table definition, and the catalog entry plus every row are written through the
// statement child transaction, so a failure leaves neither a table nor rows.
func (e *Engine) createTableAsSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.CreateTableAs) (*Result, error) {
	database, table, err := versionedName(session, statement.Name)
	if err != nil {
		return nil, err
	}
	catalogKey := sqllayout.TableKey(database, table)
	if exists, err := catalogEntryExists(read, sqllayout.Catalog, catalogKey); err != nil {
		return nil, err
	} else if exists {
		if statement.IfNotExists {
			return &Result{Message: "table already exists"}, nil
		}
		return nil, fmt.Errorf("%w: %q", storage.ErrTableExists, table)
	}
	bound, err := bindSubqueryQuery(ctx, read, session, statement.Query)
	if err != nil {
		return nil, err
	}
	uniquifyResultColumnNames(bound.Columns)
	limit := int64(16 << 20)
	if session != nil && session.query != nil && session.query.options.ResultMemoryBytes > 0 {
		limit = session.query.options.ResultMemoryBytes
	}
	used := int64(0)
	type materializedRow struct {
		values []any
		row    storage.Row
	}
	materialized := make([]ctasRow, 0, 8)
	buffer := physical.Materialize[[]any]{Input: bound.Input, Clone: func(row []any) []any {
		return append([]any(nil), row...)
	}, Charge: func(row []any) error {
		var chargeErr error
		used, chargeErr = checkResultMemory(limit, used, row)
		return chargeErr
	}}
	err = buffer.Run(ctx, func(values []any) error {
		materialized = append(materialized, ctasRow{values: values})
		return nil
	})
	if err != nil {
		return nil, err
	}
	columns := make([]storage.Column, len(bound.Columns))
	for index, column := range bound.Columns {
		length := column.Length
		if column.Type == storage.TypeVarchar && length <= 0 {
			length = 65535
		}
		nullable := column.Nullable
		for _, row := range materialized {
			if index < len(row.values) && row.values[index] == nil {
				nullable = true
				break
			}
		}
		columns[index] = storage.Column{Name: column.Name, Type: column.Type, SQLType: column.SQLType, Collation: column.Collation, Length: length, MetadataVersion: 1, Nullable: nullable}
		if column.Type == storage.TypeDecimal {
			declaration, declarationErr := decimalResultDeclaration(column, valuesOnly(materialized), index)
			if declarationErr != nil {
				return nil, declarationErr
			}
			columns[index].SQLType = declaration
		}
		if column.jsonValue {
			columns[index].SQLType = "JSON"
		}
	}
	definition := versionedTable{
		CatalogName:       database + "." + table,
		ID:                write.ID() + "/" + table,
		RowEncoding:       sqlCompactRowEncoding,
		SecondaryEncoding: 1,
		Definition:        storage.TableSnapshot{Name: table, Columns: columns},
	}
	if _, ok := sqlIntegerPrimary(definition); ok {
		definition.KeyEncoding = sqlIntegerKeyEncoding
	}
	encoded, err := encodeVersioned(definition)
	if err != nil {
		return nil, err
	}
	if err = write.Guard(sqllayout.Catalog, sqllayout.DatabaseKey(database)); err != nil {
		return nil, err
	}
	if err = write.Put(sqllayout.Catalog, catalogKey, encoded); err != nil {
		return nil, err
	}
	for ordinal, row := range materialized {
		if len(row.values) != len(columns) {
			return nil, fmt.Errorf("%w: expected %d values, got %d", storage.ErrColumnCount, len(columns), len(row.values))
		}
		converted := make(storage.Row, len(columns))
		for index := range columns {
			value, conversionErr := interfaceToColumnValue(row.values[index], columns[index])
			if conversionErr != nil {
				return nil, conversionErr
			}
			converted[index] = value
		}
		if err := writeVersionedRow(ctx, write, definition, nil, nil, converted, fmt.Sprintf("%s/%020d", write.ID(), ordinal)); err != nil {
			return nil, err
		}
	}
	return &Result{AffectedRows: uint64(len(materialized)), Message: "table created"}, nil
}

// createTableLikeSQL copies the source definition - columns, key, indexes,
// checks and comment - without rows or foreign keys, matching the legacy engine.
func (e *Engine) createTableLikeSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.CreateTableLike) (*Result, error) {
	database, table, err := versionedName(session, statement.Name)
	if err != nil {
		return nil, err
	}
	catalogKey := sqllayout.TableKey(database, table)
	if exists, err := catalogEntryExists(read, sqllayout.Catalog, catalogKey); err != nil {
		return nil, err
	} else if exists {
		if statement.IfNotExists {
			return &Result{Message: "table already exists"}, nil
		}
		return nil, fmt.Errorf("%w: %q", storage.ErrTableExists, table)
	}
	source, _, _, err := loadVersionedTable(read, session, statement.Source)
	if err != nil {
		return nil, err
	}
	snapshot := source.Definition
	snapshot.Name = table
	snapshot.Rows = nil
	// Legacy CREATE TABLE LIKE does not copy foreign keys.
	snapshot.ForeignKeys = nil
	definition := versionedTable{
		CatalogName:       database + "." + table,
		ID:                write.ID() + "/" + table,
		RowEncoding:       source.RowEncoding,
		SecondaryEncoding: source.SecondaryEncoding,
		Definition:        snapshot,
	}
	if _, ok := sqlIntegerPrimary(definition); ok {
		definition.KeyEncoding = sqlIntegerKeyEncoding
	}
	encoded, err := encodeVersioned(definition)
	if err != nil {
		return nil, err
	}
	if err = write.Guard(sqllayout.Catalog, sqllayout.DatabaseKey(database)); err != nil {
		return nil, err
	}
	if err = write.Put(sqllayout.Catalog, catalogKey, encoded); err != nil {
		return nil, err
	}
	return &Result{Message: "table created"}, nil
}

// renameTablesSQL renames one or more tables of the same database inside the
// statement child transaction. Catalog keys, catalog names, index/data namespaces
// (the table ID is preserved, so stored rows follow the rename) and every table's
// foreign-key references and referrer lists are updated together.
func (e *Engine) renameTablesSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.RenameTable) (*Result, error) {
	type renamePair struct {
		from, to string
	}
	pairs := make([]renamePair, 0, len(statement.Pairs))
	databaseName := ""
	for _, pair := range statement.Pairs {
		fromDatabase, fromTable := splitTableName(pair.From)
		toDatabase, toTable := splitTableName(pair.To)
		if fromDatabase == "" {
			fromDatabase = session.CurrentDatabase
		}
		if toDatabase == "" {
			toDatabase = fromDatabase
		}
		if fromDatabase == "" {
			return nil, errors.New("no database selected")
		}
		if !strings.EqualFold(fromDatabase, toDatabase) {
			return nil, errors.New("cross-database RENAME TABLE is not supported")
		}
		if databaseName == "" {
			databaseName = strings.ToLower(fromDatabase)
		} else if !strings.EqualFold(databaseName, fromDatabase) {
			return nil, errors.New("all RENAME TABLE pairs must use the same database")
		}
		pairs = append(pairs, renamePair{from: strings.ToLower(fromTable), to: strings.ToLower(toTable)})
	}
	if len(pairs) == 0 {
		return &Result{Message: "no tables renamed"}, nil
	}
	tables, keys, err := catalogTables(ctx, read, session, databaseName)
	if err != nil {
		return nil, err
	}
	sources := make(map[string]bool, len(pairs))
	targets := make(map[string]bool, len(pairs))
	for _, pair := range pairs {
		if pair.from == pair.to {
			continue
		}
		if !hasCatalogTable(tables, databaseName, pair.from) {
			return nil, fmt.Errorf("%w: %q", storage.ErrTableNotFound, pair.from)
		}
		if targets[pair.to] {
			return nil, fmt.Errorf("duplicate rename target %q", pair.to)
		}
		targets[pair.to] = true
		sources[pair.from] = true
		if hasCatalogTable(tables, databaseName, pair.to) && !sources[pair.to] {
			return nil, fmt.Errorf("%w: %q", storage.ErrTableExists, pair.to)
		}
	}
	// Resolve the final name of every table: a renamed table keeps its ID (and thus
	// its rows) and only changes its catalog identity.
	finalName := func(name string) string {
		seen := map[string]bool{}
		for sources[name] {
			if seen[name] {
				break
			}
			seen[name] = true
			for _, pair := range pairs {
				if pair.from == name {
					name = pair.to
					break
				}
			}
		}
		return name
	}
	reference := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		if pair.from == pair.to {
			continue
		}
		reference[databaseName+"."+pair.from] = databaseName + "." + pair.to
	}
	renamed := 0
	for catalogName, definition := range tables {
		_, tableName := splitTableName(catalogName)
		name := finalName(tableName)
		updated := definition
		updated.Definition.ForeignKeys = append([]storage.ForeignKey(nil), definition.Definition.ForeignKeys...)
		for index := range updated.Definition.ForeignKeys {
			if next, ok := reference[strings.ToLower(updated.Definition.ForeignKeys[index].RefTable)]; ok {
				updated.Definition.ForeignKeys[index].RefTable = next
			}
		}
		updated.Referrers = append([]string(nil), definition.Referrers...)
		for index, referrer := range updated.Referrers {
			if next, ok := reference[strings.ToLower(referrer)]; ok {
				updated.Referrers[index] = next
			}
		}
		updated.CatalogName = databaseName + "." + name
		updated.Definition.Name = name
		changedName := name != tableName
		if !changedName && sameForeignKeyReferences(definition, updated) && sameReferrers(definition, updated) {
			continue
		}
		encoded, encodeErr := encodeVersioned(updated)
		if encodeErr != nil {
			return nil, encodeErr
		}
		if err = write.Guard(sqllayout.Catalog, keys[catalogName]); err != nil {
			return nil, err
		}
		if changedName {
			if err = write.Delete(sqllayout.Catalog, keys[catalogName]); err != nil {
				return nil, err
			}
			if err = write.Put(sqllayout.Catalog, sqllayout.TableKey(databaseName, name), encoded); err != nil {
				return nil, err
			}
			renamed++
		} else if err = write.Put(sqllayout.Catalog, keys[catalogName], encoded); err != nil {
			return nil, err
		}
	}
	// Reverse foreign-key index entries (fkref/<referenced table>) follow the rename.
	for _, pair := range pairs {
		if pair.from == pair.to {
			continue
		}
		if err = write.Delete("fkref/"+databaseName+"."+pair.from, []byte(databaseName+"."+pair.from)); err != nil {
			return nil, err
		}
	}
	for catalogName, definition := range tables {
		if len(definition.Definition.ForeignKeys) == 0 {
			continue
		}
		child := finalName(mustTableName(catalogName))
		for _, foreignKey := range definition.Definition.ForeignKeys {
			parent := strings.ToLower(foreignKey.RefTable)
			if next, ok := reference[parent]; ok {
				parent = next
			}
			if err := write.Put("fkref/"+parent, []byte(databaseName+"."+child), []byte{1}); err != nil {
				return nil, err
			}
		}
	}
	return &Result{AffectedRows: uint64(renamed), Message: "tables renamed", MetadataChanged: true}, nil
}

// catalogTables loads every table definition of one database plus its catalog key,
// keyed by catalog name.
func catalogTables(ctx context.Context, read storageengine.Txn, session *Session, database string) (map[string]versionedTable, map[string][]byte, error) {
	tables := make(map[string]versionedTable)
	keys := make(map[string][]byte)
	err := read.Scan(ctx, sqllayout.Catalog, func(key, value []byte) error {
		name := string(key)
		if !strings.HasPrefix(name, sqllayout.TablesPrefix(database)) {
			return nil
		}
		var definition versionedTable
		if err := decodeVersioned(value, &definition); err != nil {
			return err
		}
		tables[strings.ToLower(definition.CatalogName)] = definition
		keys[strings.ToLower(definition.CatalogName)] = append([]byte(nil), key...)
		return nil
	})
	return tables, keys, err
}

func hasCatalogTable(tables map[string]versionedTable, database, table string) bool {
	_, ok := tables[database+"."+table]
	return ok
}

func mustTableName(catalogName string) string {
	_, name := splitTableName(catalogName)
	return name
}

func sameForeignKeyReferences(left, right versionedTable) bool {
	if len(left.Definition.ForeignKeys) != len(right.Definition.ForeignKeys) {
		return false
	}
	for index := range left.Definition.ForeignKeys {
		if !strings.EqualFold(left.Definition.ForeignKeys[index].RefTable, right.Definition.ForeignKeys[index].RefTable) {
			return false
		}
	}
	return true
}

func sameReferrers(left, right versionedTable) bool {
	if len(left.Referrers) != len(right.Referrers) {
		return false
	}
	for index := range left.Referrers {
		if !strings.EqualFold(left.Referrers[index], right.Referrers[index]) {
			return false
		}
	}
	return true
}

func catalogEntryExists(tx storageengine.Txn, space string, key []byte) (bool, error) {
	_, exists, err := tx.Get(space, key)
	return exists, err
}

func valuesOnly(rows []ctasRow) [][]any {
	values := make([][]any, len(rows))
	for index := range rows {
		values[index] = rows[index].values
	}
	return values
}
