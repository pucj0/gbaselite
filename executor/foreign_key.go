package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

func sqlFKIndex(parent versionedTable, fk storage.ForeignKey) (storage.Index, error) {
	for _, idx := range parent.Definition.Indexes {
		if !idx.Primary && !idx.Unique || len(idx.Columns) != len(fk.RefColumns) {
			continue
		}
		same := true
		for i, c := range idx.Columns {
			same = same && strings.EqualFold(c, fk.RefColumns[i])
		}
		if same {
			return idx, nil
		}
	}
	return storage.Index{}, fmt.Errorf("%w: referenced columns need a complete unique index", storage.ErrForeignKey)
}
func sqlColumnPosition(table versionedTable, name string) int {
	for i, c := range table.Definition.Columns {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}
func prepareSQLForeignKeys(tx storageengine.Txn, table *versionedTable, session *Session) error {
	seen := map[string]bool{}
	for i := range table.Definition.ForeignKeys {
		fk := &table.Definition.ForeignKeys[i]
		if fk.Name == "" {
			fk.Name = fmt.Sprintf("fk_%d", i+1)
		}
		if seen[strings.ToLower(fk.Name)] {
			return fmt.Errorf("duplicate foreign key name")
		}
		seen[strings.ToLower(fk.Name)] = true
		for _, action := range []string{fk.OnDelete, fk.OnUpdate} {
			if action != "" && !strings.EqualFold(action, "RESTRICT") && !strings.EqualFold(action, "NO ACTION") {
				return fmt.Errorf("foreign keys currently support RESTRICT/NO ACTION")
			}
		}
		childDB, _ := splitTableName(table.CatalogName)
		db, name, err := versionedName(&Session{CurrentDatabase: childDB}, fk.RefTable)
		if err != nil {
			return err
		}
		if !strings.EqualFold(db, childDB) {
			return fmt.Errorf("cross-database foreign keys are not supported")
		}
		fk.RefTable = db + "." + name
		if strings.EqualFold(fk.RefTable, table.CatalogName) {
			return fmt.Errorf("self-referencing foreign keys are not supported")
		}
		if len(fk.Columns) == 0 || len(fk.Columns) != len(fk.RefColumns) {
			return storage.ErrForeignKey
		}
		for _, name := range fk.Columns {
			if sqlColumnPosition(*table, name) < 0 {
				return storage.ErrColumnNotFound
			}
		}
		if err := tx.Put("fkref/"+fk.RefTable, []byte(table.CatalogName), []byte{1}); err != nil {
			return err
		}
		parent, _, key, err := loadVersionedTable(tx, session, fk.RefTable)
		if session.ForeignKeyChecksDisabled && (errors.Is(err, storage.ErrTableNotFound) || errors.Is(err, storage.ErrDatabaseNotFound)) {
			continue
		}
		if err != nil {
			return err
		}
		if len(fk.Columns) == 0 || len(fk.Columns) != len(fk.RefColumns) {
			return storage.ErrForeignKey
		}
		if _, err = sqlFKIndex(parent, *fk); err != nil {
			return err
		}
		for j, name := range fk.Columns {
			a, b := sqlColumnPosition(*table, name), sqlColumnPosition(parent, fk.RefColumns[j])
			if a < 0 || b < 0 {
				return storage.ErrColumnNotFound
			}
			ca, cb := table.Definition.Columns[a], parent.Definition.Columns[b]
			if ca.Type != cb.Type || !strings.EqualFold(ca.Collation, cb.Collation) {
				return fmt.Errorf("%w: foreign key type/collation mismatch", storage.ErrForeignKey)
			}
		}
		if session.ForeignKeyChecksDisabled {
			continue
		}
		parent.CatalogName = fk.RefTable
		found := false
		for _, ref := range parent.Referrers {
			found = found || strings.EqualFold(ref, table.CatalogName)
		}
		if !found {
			parent.Referrers = append(parent.Referrers, table.CatalogName)
		}
		encoded, err := encodeVersioned(parent)
		if err != nil {
			return err
		}
		if err = tx.Put(sqllayout.Catalog, key, encoded); err != nil {
			return err
		}
	}
	return nil
}
func sqlFKParentRow(child, parent versionedTable, fk storage.ForeignKey, row storage.Row) (storage.Row, bool, error) {
	candidate := make(storage.Row, len(parent.Definition.Columns))
	for i, c := range parent.Definition.Columns {
		candidate[i] = storage.NullValue(c.Type)
	}
	for i, c := range fk.Columns {
		a, b := sqlColumnPosition(child, c), sqlColumnPosition(parent, fk.RefColumns[i])
		if a < 0 || b < 0 {
			return nil, false, storage.ErrColumnNotFound
		}
		if row[a].Null {
			return nil, false, nil
		}
		candidate[b] = row[a]
	}
	return candidate, true, nil
}

type foreignChecksContextKey struct{}

func validateSQLReferences(ctx context.Context, tx storageengine.Txn, table versionedTable, oldRow, newRow storage.Row) error {
	if disabled, _ := ctx.Value(foreignChecksContextKey{}).(bool); disabled {
		return nil
	}
	if newRow != nil {
		for _, fk := range table.Definition.ForeignKeys {
			parent, _, catalog, err := loadVersionedTable(tx, &Session{}, fk.RefTable)
			if err != nil {
				return err
			}
			idx, err := sqlFKIndex(parent, fk)
			if err != nil {
				return err
			}
			candidate, check, err := sqlFKParentRow(table, parent, fk, newRow)
			if err != nil {
				return err
			}
			if !check {
				continue
			}
			var owner []byte
			var exists bool
			if idx.Primary {
				owner, exists = sqlPrimaryKey(parent, idx, candidate)
			} else {
				key, ok := storage.IndexValueKey(idx, parent.Definition.Columns, candidate)
				if !ok {
					return storage.ErrForeignKey
				}
				owner, exists, err = tx.Get(sqllayout.UniqueIndex(parent.ID, idx.Name), []byte(key))
				if err != nil {
					return err
				}
			}
			if !exists {
				return storage.ErrForeignKey
			}
			if _, exists, err = tx.Table(parent.ID).Get(owner); err != nil {
				return err
			}
			if !exists {
				return storage.ErrForeignKey
			}
			if err = tx.Guard(sqllayout.Catalog, catalog); err != nil {
				return err
			}
			if err = tx.Table(parent.ID).Guard(owner); err != nil {
				return err
			}
		}
	}
	if oldRow == nil {
		return nil
	}
	refs, refErr := sqlForeignReferrers(tx, table)
	if refErr != nil {
		return refErr
	}
	for _, ref := range refs {
		child, _, catalog, err := loadVersionedTable(tx, &Session{}, ref)
		if errors.Is(err, storage.ErrTableNotFound) || errors.Is(err, storage.ErrDatabaseNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, fk := range child.Definition.ForeignKeys {
			if !strings.EqualFold(fk.RefTable, table.CatalogName) {
				continue
			}
			idx, err := sqlFKIndex(table, fk)
			if err != nil {
				return err
			}
			oldKey, ok := storage.IndexValueKey(idx, table.Definition.Columns, oldRow)
			if !ok {
				continue
			}
			if newRow != nil {
				nextKey, _ := storage.IndexValueKey(idx, table.Definition.Columns, newRow)
				if oldKey == nextKey {
					continue
				}
			}
			if err = tx.Guard(sqllayout.Catalog, catalog); err != nil {
				return err
			}
			if err = tx.GuardRange(sqllayout.Rows(child.ID), storageengine.KeyRange{}); err != nil {
				return err
			}
			err = tx.ScanRange(ctx, sqllayout.Rows(child.ID), storageengine.KeyRange{}, func(_, v []byte) error {
				row, err := decodeSQLRow(child, v)
				if err != nil {
					return err
				}
				candidate, check, err := sqlFKParentRow(child, table, fk, row)
				if err != nil || !check {
					return err
				}
				key, _ := storage.IndexValueKey(idx, table.Definition.Columns, candidate)
				if bytes.Equal([]byte(key), []byte(oldKey)) {
					return storage.ErrForeignKey
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
func rejectSQLReferencedDrop(tx storageengine.Txn, table versionedTable, disabled ...bool) error {
	if len(disabled) > 0 && disabled[0] {
		return nil
	}
	refs, refErr := sqlForeignReferrers(tx, table)
	if refErr != nil {
		return refErr
	}
	for _, ref := range refs {
		child, _, _, err := loadVersionedTable(tx, &Session{}, ref)
		if errors.Is(err, storage.ErrTableNotFound) || errors.Is(err, storage.ErrDatabaseNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, fk := range child.Definition.ForeignKeys {
			if strings.EqualFold(fk.RefTable, table.CatalogName) {
				return fmt.Errorf("%w: table is referenced by %s", storage.ErrForeignKey, ref)
			}
		}
	}
	return nil
}

func sqlForeignReferrers(tx storageengine.Txn, table versionedTable) ([]string, error) {
	refs := append([]string(nil), table.Referrers...)
	seen := map[string]bool{}
	for _, ref := range refs {
		seen[ref] = true
	}
	err := tx.Scan(context.Background(), "fkref/"+table.CatalogName, func(k, v []byte) error {
		if !seen[string(k)] {
			refs = append(refs, string(k))
			seen[string(k)] = true
		}
		return nil
	})
	return refs, err
}
