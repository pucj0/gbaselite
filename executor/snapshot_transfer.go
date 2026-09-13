package executor

import (
	"bytes"
	"context"
	"fmt"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// ImportSnapshot imports a logical SQL snapshot into an isolated empty destination.
// The caller must discard that destination on failure, including counter changes.
func (e *Engine) ImportSnapshot(ctx context.Context, snapshot storage.StoreSnapshot) error {
	tx, err := e.Backend.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Create all schemas before resolving foreign keys, including forward references.
	for _, db := range snapshot.Databases {
		if err = tx.Put(sqllayout.Catalog, sqllayout.DatabaseKey(strings.ToLower(db.Name)), []byte{1}); err != nil {
			return err
		}
		for i, table := range db.Tables {
			table.Rows = nil
			definition := versionedTable{CatalogName: strings.ToLower(db.Name) + "." + strings.ToLower(table.Name), ID: fmt.Sprintf("%s/%s/%d", tx.ID(), strings.ToLower(db.Name), i), Definition: table, RowEncoding: sqlCompactRowEncoding, SecondaryEncoding: 1}
			if _, ok := sqlIntegerPrimary(definition); ok {
				definition.KeyEncoding = sqlIntegerKeyEncoding
			}
			encoded, err := encodeVersioned(definition)
			if err != nil {
				return err
			}
			if err = tx.Put(sqllayout.Catalog, sqllayout.TableKey(strings.ToLower(db.Name), strings.ToLower(table.Name)), encoded); err != nil {
				return err
			}
		}
	}
	// Views are migrated as catalog entries: their definitions are already
	// validated SQL text, and the query binder re-parses them on every reference.
	for _, db := range snapshot.Databases {
		for _, view := range db.Views {
			definition := versionedView{
				CatalogName: strings.ToLower(db.Name) + "." + strings.ToLower(view.Name),
				Definition:  view.Definition,
				Columns:     append([]string(nil), view.Columns...),
			}
			encoded, err := encodeVersioned(definition)
			if err != nil {
				return err
			}
			if err = tx.Put(sqllayout.Catalog, sqllayout.ViewKey(strings.ToLower(db.Name), strings.ToLower(view.Name)), encoded); err != nil {
				return err
			}
		}
	}
	for _, db := range snapshot.Databases {
		session := &Session{CurrentDatabase: strings.ToLower(db.Name)}
		for _, table := range db.Tables {
			definition, schema, key, err := loadVersionedTable(tx, session, table.Name)
			if err != nil {
				return err
			}
			for _, check := range table.CheckConstraints {
				if err = validateSQLCheckDefinition(schema, check.Expression); err != nil {
					return err
				}
			}
			if err = prepareSQLForeignKeys(tx, &definition, session); err != nil {
				return fmt.Errorf("migrate %s: %w", definition.CatalogName, err)
			}
			encoded, err := encodeVersioned(definition)
			if err != nil {
				return err
			}
			if err = tx.Put(sqllayout.Catalog, key, encoded); err != nil {
				return err
			}
			disabled := context.WithValue(ctx, foreignChecksContextKey{}, true)
			for i, row := range table.Rows {
				if err = ctx.Err(); err != nil {
					return err
				}
				if err = writeVersionedRow(disabled, tx, definition, nil, nil, row, fmt.Sprintf("legacy/%020d", i)); err != nil {
					return fmt.Errorf("migrate %s row %d: %w", definition.CatalogName, i, err)
				}
			}
			for col, next := range table.AutoIncrementNext {
				if next < 1 {
					return fmt.Errorf("invalid auto increment counter in %s", definition.CatalogName)
				}
				if err := storageengine.AdvanceCounter(ctx, e.Backend, definition.counterKey(col), uint64(next-1)); err != nil {
					return err
				}
			}
		}
	}
	// Every migrated view must bind against the complete imported transaction.
	for _, db := range snapshot.Databases {
		for _, view := range db.Views {
			definition := versionedView{CatalogName: strings.ToLower(db.Name) + "." + strings.ToLower(view.Name), Definition: view.Definition, Columns: view.Columns}
			session := &Session{CurrentDatabase: strings.ToLower(db.Name)}
			if _, _, err := viewRelationFromDefinition(ctx, tx, session, definition, strings.ToLower(view.Name)); err != nil {
				return fmt.Errorf("migrate view %s: %w", definition.CatalogName, err)
			}
		}
	}
	// Validate every reference against the complete imported transaction.
	for _, db := range snapshot.Databases {
		for _, table := range db.Tables {
			definition, _, _, err := loadVersionedTable(tx, &Session{CurrentDatabase: db.Name}, table.Name)
			if err != nil {
				return err
			}
			for _, row := range table.Rows {
				if err = validateSQLReferences(ctx, tx, definition, nil, row); err != nil {
					return fmt.Errorf("migrate %s: %w", definition.CatalogName, err)
				}
			}
		}
	}
	_, err = tx.Commit(ctx)
	return err
}

// VerifySnapshot compares row counts and persisted row bytes after reopening.
func (e *Engine) VerifySnapshot(ctx context.Context, snapshot storage.StoreSnapshot) error {
	tx, err := e.Backend.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, db := range snapshot.Databases {
		for _, view := range db.Views {
			key := sqllayout.ViewKey(strings.ToLower(db.Name), strings.ToLower(view.Name))
			value, ok, err := tx.Get(sqllayout.Catalog, key)
			if err != nil {
				return err
			}
			var stored versionedView
			if !ok {
				return fmt.Errorf("migration view verification failed for %s.%s", db.Name, view.Name)
			}
			if err = decodeVersioned(value, &stored); err != nil {
				return err
			}
			if stored.Definition != view.Definition || strings.Join(stored.Columns, "\x00") != strings.Join(view.Columns, "\x00") {
				return fmt.Errorf("migration view definition mismatch for %s.%s", db.Name, view.Name)
			}
		}
		for _, table := range db.Tables {
			definition, _, _, err := loadVersionedTable(tx, &Session{CurrentDatabase: db.Name}, table.Name)
			if err != nil {
				return err
			}
			count := 0
			if err = tx.Scan(ctx, sqllayout.Rows(definition.ID), func(_, _ []byte) error { count++; return nil }); err != nil {
				return err
			}
			if count != len(table.Rows) {
				return fmt.Errorf("migration row count mismatch in %s", definition.CatalogName)
			}
			for i, row := range table.Rows {
				key := []byte(fmt.Sprintf("legacy/%020d", i))
				for _, index := range table.Indexes {
					if index.Primary {
						var ok bool
						key, ok = sqlPrimaryKey(definition, index, row)
						if !ok {
							return fmt.Errorf("invalid primary key")
						}
					}
				}
				got, ok, err := tx.Table(definition.ID).Get(key)
				if err != nil {
					return err
				}
				want, err := encodeSQLRow(definition, row)
				if err != nil {
					return err
				}
				if !ok || !bytes.Equal(got, want) {
					return fmt.Errorf("migration row verification failed in %s at row %d", definition.CatalogName, i)
				}
			}
		}
	}
	return nil
}
