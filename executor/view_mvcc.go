package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// versionedView is the persisted catalog form of a view: the SQL text that is
// re-parsed and bound on every reference, plus an optional column list.
type versionedView struct {
	CatalogName string
	Definition  string
	Columns     []string
}

// viewCatalogKey resolves a view name to its catalog key and reports whether the
// view namespace is taken, so table DDL can refuse to collide with a view exactly
// like the legacy engine does.
func viewCatalogKey(tx storageengine.Txn, session *Session, name string) ([]byte, bool, error) {
	database, view, err := versionedName(session, name)
	if err != nil {
		return nil, false, err
	}
	key := sqllayout.ViewKey(database, view)
	_, exists, err := tx.Get(sqllayout.Catalog, key)
	if err != nil {
		return nil, false, err
	}
	return key, exists, nil
}

func loadView(tx storageengine.Txn, session *Session, name string) (versionedView, []byte, error) {
	database, view, err := versionedName(session, name)
	if err != nil {
		return versionedView{}, nil, err
	}
	key := sqllayout.ViewKey(database, view)
	value, ok, err := tx.Get(sqllayout.Catalog, key)
	if err != nil {
		return versionedView{}, nil, err
	}
	if !ok {
		return versionedView{}, nil, storage.ErrViewNotFound
	}
	var definition versionedView
	if err = decodeVersioned(value, &definition); err != nil {
		return versionedView{}, nil, err
	}
	if definition.CatalogName == "" {
		definition.CatalogName = database + "." + view
	}
	return definition, key, nil
}

// viewRelation materializes a view by re-parsing its definition and binding it
// through the unified MVCC query pipeline. It reports ok=false when the name is
// not a view, so callers can fall through to base tables.
func viewRelation(ctx context.Context, read storageengine.Txn, session *Session, name, alias string) (*storage.Table, []storage.Row, bool, error) {
	view, _, err := loadView(read, session, name)
	if errors.Is(err, storage.ErrViewNotFound) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if session.viewStack == nil {
		session.viewStack = make(map[string]bool)
	}
	stackKey := strings.ToLower(view.CatalogName)
	if session.viewStack[stackKey] {
		return nil, nil, false, fmt.Errorf("circular view reference detected at %s", name)
	}
	session.viewStack[stackKey] = true
	defer delete(session.viewStack, stackKey)

	statement, err := parser.Parse(view.Definition)
	if err != nil {
		return nil, nil, false, fmt.Errorf("parse view %s: %w", name, err)
	}
	query, ok := statement.(parser.Query)
	if !ok {
		return nil, nil, false, fmt.Errorf("view %s definition is not a query", name)
	}
	if alias == "" {
		_, alias = splitTableName(name)
	}
	previousDatabase := session.CurrentDatabase
	if database, _ := splitTableName(view.CatalogName); database != "" {
		session.CurrentDatabase = database
	}
	defer func() { session.CurrentDatabase = previousDatabase }()
	schema, rows, err := derivedRelation(ctx, read, session, query, alias, view.Columns)
	if err != nil {
		return nil, nil, false, err
	}
	return schema, rows, true, nil
}

// createViewSQL persists a view definition in the MVCC catalog. The definition is
// validated by binding it before anything is written, and the catalog entry is
// stored through the statement child transaction.
func (e *Engine) createViewSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.CreateView) (*Result, error) {
	database, view, err := versionedName(session, statement.Name)
	if err != nil {
		return nil, err
	}
	if exists, existsErr := catalogEntryExists(read, sqllayout.Catalog, sqllayout.TableKey(database, view)); existsErr != nil {
		return nil, existsErr
	} else if exists {
		return nil, fmt.Errorf("%w: %q is a table", storage.ErrViewExists, view)
	}
	catalogKey := sqllayout.ViewKey(database, view)
	_, _, loadErr := loadView(read, session, statement.Name)
	existing := loadErr == nil
	if loadErr != nil && !errors.Is(loadErr, storage.ErrViewNotFound) {
		return nil, loadErr
	}
	switch {
	case statement.AlterOnly && !existing:
		return nil, storage.ErrViewNotFound
	case existing && !statement.OrReplace && !statement.AlterOnly:
		return nil, fmt.Errorf("%w: %q", storage.ErrViewExists, view)
	}
	definition := versionedView{CatalogName: database + "." + view, Definition: statement.Definition, Columns: append([]string(nil), statement.Columns...)}
	// Validate by parsing and binding the definition on the statement snapshot.
	if _, _, err = viewRelationFromDefinition(ctx, read, session, definition, view); err != nil {
		return nil, fmt.Errorf("invalid view %s: %w", statement.Name, err)
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
	return &Result{Message: "view created", MetadataChanged: true}, nil
}

// viewRelationFromDefinition binds one view definition (used for validation and
// for querying) without going through the catalog lookup.
func viewRelationFromDefinition(ctx context.Context, read storageengine.Txn, session *Session, view versionedView, alias string) (*storage.Table, []storage.Row, error) {
	statement, err := parser.Parse(view.Definition)
	if err != nil {
		return nil, nil, fmt.Errorf("parse view %s: %w", view.CatalogName, err)
	}
	query, ok := statement.(parser.Query)
	if !ok {
		return nil, nil, fmt.Errorf("view %s definition is not a query", view.CatalogName)
	}
	previousDatabase := session.CurrentDatabase
	if database, _ := splitTableName(view.CatalogName); database != "" {
		session.CurrentDatabase = database
	}
	defer func() { session.CurrentDatabase = previousDatabase }()
	return derivedRelation(ctx, read, session, query, alias, view.Columns)
}

// dropViewSQL removes one or more views from the MVCC catalog.
func (e *Engine) dropViewSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.DropView) (*Result, error) {
	dropped := uint64(0)
	for _, name := range statement.Names {
		_, key, err := loadView(read, session, name)
		if err != nil {
			if statement.IfExists && errors.Is(err, storage.ErrViewNotFound) {
				continue
			}
			return nil, err
		}
		if err = write.Guard(sqllayout.Catalog, sqllayout.DatabaseKey(mustDatabaseName(session, name))); err != nil {
			return nil, err
		}
		if err = write.Delete(sqllayout.Catalog, key); err != nil {
			return nil, err
		}
		dropped++
	}
	return &Result{AffectedRows: dropped, Message: "views dropped", MetadataChanged: dropped > 0}, nil
}

func mustDatabaseName(session *Session, name string) string {
	if database, _ := splitTableName(name); database != "" {
		return strings.ToLower(database)
	}
	return strings.ToLower(session.CurrentDatabase)
}

// showViewSQL serves SHOW CREATE VIEW from the MVCC catalog instead of the
// metadata mirror, reusing the legacy result shape.
func (e *Engine) showViewSQL(tx storageengine.Txn, session *Session, statement parser.Show) (*Result, bool, error) {
	if !strings.EqualFold(strings.TrimSpace(statement.What), "CREATE VIEW") {
		return nil, false, nil
	}
	view, _, err := loadView(tx, session, statement.Name)
	if err != nil {
		return nil, true, err
	}
	_, name := splitTableName(view.CatalogName)
	return createViewResult(storage.View{Name: name, Definition: view.Definition, Columns: view.Columns}), true, nil
}
