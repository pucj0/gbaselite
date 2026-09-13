package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"sort"
	"strings"
)

// exportDatabaseSQL implements EXPORT DATABASE ... TO 'path' on the MVCC runtime.
// The dump is rendered from the statement snapshot, so it contains exactly the
// rows the statement can read (including the session's own uncommitted writes),
// and it reuses the legacy logical-backup renderer so both runtimes emit the same
// file format.
func (e *Engine) exportDatabaseSQL(ctx context.Context, tx storageengine.Txn, session *Session, statement parser.ExportDatabase) (*Result, error) {
	database, err := catalogDatabaseSnapshot(ctx, tx, session, statement.Name)
	if err != nil {
		return nil, err
	}
	if err = writeBackupFile(statement.Path, []storage.DatabaseSnapshot{database}, BackupOptions{}); err != nil {
		return nil, err
	}
	return &Result{Message: "database exported to " + statement.Path}, nil
}

// catalogDatabaseSnapshot materializes one MVCC database as a legacy database
// snapshot: every table definition with its decoded rows, plus every view. Table
// order is sorted because the legacy store iterates a map, so its own dump order
// was never stable.
func catalogDatabaseSnapshot(ctx context.Context, tx storageengine.Txn, session *Session, name string) (storage.DatabaseSnapshot, error) {
	database := strings.ToLower(strings.TrimSpace(name))
	if database == "" {
		database = strings.ToLower(session.CurrentDatabase)
	}
	if database == "" {
		return storage.DatabaseSnapshot{}, errors.New("no database selected")
	}
	if _, exists, err := tx.Get(sqllayout.Catalog, sqllayout.DatabaseKey(database)); err != nil {
		return storage.DatabaseSnapshot{}, err
	} else if !exists {
		return storage.DatabaseSnapshot{}, fmt.Errorf("%w: %q", storage.ErrDatabaseNotFound, database)
	}
	tables, _, err := catalogTables(ctx, tx, session, database)
	if err != nil {
		return storage.DatabaseSnapshot{}, err
	}
	snapshot := storage.DatabaseSnapshot{Name: database}
	names := make([]string, 0, len(tables))
	for catalogName := range tables {
		names = append(names, catalogName)
	}
	sort.Strings(names)
	for _, catalogName := range names {
		table := tables[catalogName]
		definition := table.Definition
		definition.Rows = nil
		err = tx.ScanRange(ctx, sqllayout.Rows(table.ID), storageengine.KeyRange{}, func(_, value []byte) error {
			if err := checkQuery(session); err != nil {
				return err
			}
			row, err := decodeSQLRow(table, value)
			if err != nil {
				return err
			}
			definition.Rows = append(definition.Rows, row)
			return nil
		})
		if err != nil {
			return storage.DatabaseSnapshot{}, err
		}
		snapshot.Tables = append(snapshot.Tables, definition)
	}
	err = tx.Scan(ctx, sqllayout.Catalog, func(key, value []byte) error {
		if !strings.HasPrefix(string(key), sqllayout.ViewsPrefix(database)) {
			return nil
		}
		var view versionedView
		if err := decodeVersioned(value, &view); err != nil {
			return err
		}
		_, viewName := splitTableName(view.CatalogName)
		if viewName == "" {
			viewName = strings.TrimPrefix(string(key), sqllayout.ViewsPrefix(database))
		}
		snapshot.Views = append(snapshot.Views, storage.ViewSnapshot{Name: viewName, Definition: view.Definition, Columns: append([]string(nil), view.Columns...)})
		return nil
	})
	if err != nil {
		return storage.DatabaseSnapshot{}, err
	}
	return snapshot, nil
}
