package executor

import (
	"context"
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

func (t versionedTable) counterKey(name string) string {
	if key := t.CounterKeys[strings.ToLower(name)]; key != "" {
		return key
	}
	return t.ID + "/" + name
}
func mvccAlterTarget(statement parser.Statement) (string, bool) {
	switch v := statement.(type) {
	case parser.CreateIndex:
		return v.Table, true
	case parser.DropIndex:
		return v.Table, true
	case parser.RenameIndex:
		return v.Table, true
	case parser.AlterColumn:
		return v.Table, true
	case parser.AlterColumnDefault:
		return v.Table, true
	case parser.AddColumn:
		return v.Table, true
	case parser.DropColumn:
		return v.Table, true
	case parser.RenameColumn:
		return v.Table, true
	case parser.AlterCheck:
		return v.Table, true
	case parser.AlterForeignKey:
		return v.Table, true
	case parser.AlterTableComment:
		return v.Table, true
	case parser.AlterTableBatch:
		return v.Table, true
	}
	return "", false
}
func (e *Engine) alterMVCC(ctx context.Context, read, write storageengine.Txn, session *Session, name string, statement parser.Statement) (*Result, error) {
	old, _, catalog, err := loadVersionedTable(read, session, name)
	if err != nil {
		return nil, err
	}
	if err = rejectMVCCReferencedDrop(write, old, session.ForeignKeyChecksDisabled); err != nil {
		return nil, fmt.Errorf("alter referenced table: %w", err)
	}
	dbName, tableName, err := versionedName(session, name)
	if err != nil {
		return nil, err
	}
	mirror := storage.NewStore()
	db, err := mirror.CreateDatabase(dbName)
	if err != nil {
		return nil, err
	}
	var primary []string
	var indexes []storage.Index
	for _, idx := range old.Definition.Indexes {
		if idx.Primary {
			primary = idx.Columns
		} else {
			indexes = append(indexes, idx)
		}
	}
	table, err := db.CreateTableWithIndexes(tableName, old.Definition.Columns, primary, indexes)
	if err != nil {
		return nil, err
	}
	table.SetNamedConstraints(old.Definition.ForeignKeys, old.Definition.CheckConstraints)
	origins := make(map[string]int)
	for i, c := range old.Definition.Columns {
		origins[strings.ToLower(c.Name)] = i
	}
	actions := []parser.Statement{statement}
	if batch, ok := statement.(parser.AlterTableBatch); ok {
		actions = batch.Actions
	}
	for _, action := range actions {
		if _, ok := mvccAlterTarget(action); !ok {
			return nil, fmt.Errorf("unsupported MVCC ALTER action")
		}
		switch v := action.(type) {
		case parser.AlterCheck:
			if !v.Drop {
				if err := validateMVCCCheckDefinition(table, v.Check.Expression); err != nil {
					return nil, err
				}
			}
		}
		if fk, ok := action.(parser.AlterForeignKey); ok {
			refs := table.ForeignKeys()
			if fk.Drop {
				found := false
				kept := refs[:0]
				for _, r := range refs {
					if strings.EqualFold(r.Name, fk.Name) {
						found = true
					} else {
						kept = append(kept, r)
					}
				}
				if !found {
					return nil, fmt.Errorf("foreign key not found")
				}
				refs = kept
			} else {
				r := fk.ForeignKey
				refs = append(refs, storage.ForeignKey{Name: r.Name, Columns: r.Columns, RefTable: r.RefTable, RefColumns: r.RefColumns, OnDelete: r.OnDelete, OnUpdate: r.OnUpdate})
			}
			table.SetNamedConstraints(refs, table.CheckConstraints())
			continue
		}
		if _, err = executeAlterTableAction(mirror, session, action); err != nil {
			return nil, err
		}
		switch v := action.(type) {
		case parser.RenameColumn:
			if strings.EqualFold(v.OldName, v.NewName) {
				break
			}
			origins[strings.ToLower(v.NewName)] = origins[strings.ToLower(v.OldName)]
			delete(origins, strings.ToLower(v.OldName))
		case parser.AlterColumn:
			if !strings.EqualFold(v.OldName, v.Column.Name) {
				origins[strings.ToLower(v.Column.Name)] = origins[strings.ToLower(v.OldName)]
				delete(origins, strings.ToLower(v.OldName))
			}
		case parser.DropColumn:
			delete(origins, strings.ToLower(v.Name))
		}
	}
	definition := versionedTable{CatalogName: dbName + "." + tableName, ID: write.ID() + "/" + tableName, RowEncoding: mvccCompactRowEncoding, SecondaryEncoding: 1, Definition: table.Snapshot(), CounterKeys: make(map[string]string)}
	if _, ok := mvccIntegerPrimary(definition); ok {
		definition.KeyEncoding = mvccIntegerKeyEncoding
	}
	for _, c := range definition.Definition.Columns {
		if c.AutoIncrement {
			if pos, ok := origins[strings.ToLower(c.Name)]; ok && old.Definition.Columns[pos].AutoIncrement {
				definition.CounterKeys[strings.ToLower(c.Name)] = old.counterKey(old.Definition.Columns[pos].Name)
			} else {
				return nil, fmt.Errorf("adding AUTO_INCREMENT requires an explicit data migration")
			}
		}
		if c.OnUpdate != "" {
			return nil, fmt.Errorf("MVCC ON UPDATE column expressions are not supported")
		}
	}
	if err = prepareMVCCForeignKeys(write, &definition, session); err != nil {
		return nil, err
	}
	if err = write.GuardRange("row/" + old.ID); err != nil {
		return nil, err
	}
	count := uint64(0)
	err = read.ScanRange(ctx, "row/"+old.ID, storageengine.KeyRange{}, func(k, v []byte) error {
		row, err := decodeMVCCRow(old, v)
		if err != nil {
			return err
		}
		converted := make(storage.Row, len(definition.Definition.Columns))
		for i, c := range definition.Definition.Columns {
			if pos, ok := origins[strings.ToLower(c.Name)]; ok {
				converted[i], err = interfaceToColumnValue(row[pos].Interface(), c)
			} else if c.HasDefault {
				converted[i], err = columnDefaultValue(c, session)
			} else {
				converted[i] = storage.NullValue(c.Type)
			}
			if err != nil {
				return err
			}
		}
		if err = writeVersionedRow(ctx, write, definition, nil, nil, converted, string(k)); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return nil, err
	}
	encoded, err := encodeVersioned(definition)
	if err != nil {
		return nil, err
	}
	if err = write.Put("catalog", catalog, encoded); err != nil {
		return nil, err
	}
	return &Result{AffectedRows: count, Message: "MVCC schema and rebuilt indexes staged atomically", MetadataChanged: true}, nil
}
func validateMVCCCheckDefinition(table *storage.Table, definition string) error {
	expr, err := parser.ParseExpression(definition)
	if err != nil {
		return err
	}
	var pure func(parser.Expr) bool
	pure = func(e parser.Expr) bool {
		switch v := e.(type) {
		case parser.Identifier, parser.LiteralExpr:
			return true
		case parser.BinaryExpr:
			return pure(v.Left) && pure(v.Right)
		case parser.UnaryExpr:
			return pure(v.Value)
		case parser.IsExpr:
			return pure(v.Value) && pure(v.Target)
		case parser.BetweenExpr:
			return pure(v.Value) && pure(v.Lower) && pure(v.Upper)
		default:
			return false
		}
	}
	if !pure(expr) {
		return fmt.Errorf("MVCC CHECK requires deterministic scalar comparisons/arithmetic")
	}
	return validateCheckDefinition(table, definition)
}
func validateVersionedChecks(table versionedTable, row storage.Row) error {
	if len(table.Definition.CheckConstraints) == 0 && len(table.Definition.Checks) == 0 {
		return nil
	}
	schema, err := storage.NewTransientTable(table.Definition.Name, table.Definition.Columns)
	if err != nil {
		return err
	}
	if len(table.Definition.CheckConstraints) > 0 {
		schema.SetNamedConstraints(nil, table.Definition.CheckConstraints)
	} else {
		schema.SetConstraints(nil, table.Definition.Checks)
	}
	return validateCheckConstraints(schema, row)
}
