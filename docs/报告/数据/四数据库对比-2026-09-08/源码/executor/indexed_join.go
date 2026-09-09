package executor

import (
	"strings"

	"gbaselite/parser"
	"gbaselite/storage"
)

// tryIndexedJoinRelations avoids copying/indexing the entire right relation
// when an INNER/LEFT equality join targets a single-column unique index.
// Pass the original right table before qualifyRelation discards its indexes.
func tryIndexedJoinRelations(store *storage.Store, session *Session, left, right *storage.Table, rightQualifier string, join parser.Join) (*storage.Table, bool, error) {
	if join.Type != "INNER" && join.Type != "LEFT" && join.Type != "" {
		return nil, false, nil
	}
	rightSchema, err := qualifySchema(right, rightQualifier)
	if err != nil {
		return nil, false, err
	}
	leftPosition, rightPosition, eligible := equalityJoinColumns(join.On, left, rightSchema)
	if !eligible {
		return nil, false, nil
	}
	leftColumns, rightColumns := left.ColumnsView(), right.ColumnsView()
	target := rightColumns[rightPosition]
	if !right.HasUniqueIndex([]string{target.Name}) || !indexedJoinTypesCompatible(leftColumns[leftPosition], target, session) {
		return nil, false, nil
	}
	joinedColumns := append([]storage.Column(nil), leftColumns...)
	for _, column := range rightSchema.ColumnsView() {
		if join.Type == "LEFT" {
			column.MetadataVersion = 1
			column.Nullable = true
		}
		joinedColumns = append(joinedColumns, column)
	}
	joined, err := storage.NewTransientTable("indexed_join_result", joinedColumns)
	if err != nil {
		return nil, true, err
	}
	account := newQueryMemoryAccount(session, "indexed JOIN result")
	q := session.query
	err = visitQueryTable(q, left, nil, func(leftRow storage.Row) error {
		matched := false
		emit := func(rightRow storage.Row) error {
			candidate := make(storage.Row, 0, len(joinedColumns))
			candidate = append(candidate, leftRow...)
			candidate = append(candidate, rightRow...)
			accepted, evaluationErr := evaluateExprWithContext(join.On, joined, candidate, session, store)
			if evaluationErr != nil {
				return evaluationErr
			}
			if !truthy(accepted) {
				return nil
			}
			if err := account.Reserve(queryStorageRowBytes(candidate)); err != nil {
				return err
			}
			matched = true
			return joined.Insert(candidate)
		}
		value := leftRow[leftPosition]
		if !value.Null {
			if safeIndexedJoinValue(value) {
				value.Type = target.Type
				rightRow, found, indexed := right.LookupUnique(target.Name, value)
				if !indexed {
					return storage.ErrIndexNotFound
				}
				if found {
					if err := emit(rightRow); err != nil {
						return err
					}
				}
			} else {
				// Legacy mixed numeric comparisons around 2^53 can compare
				// neighboring integers equal. Preserve that path by scanning
				// only for exceptional source values instead of missing rows.
				if err := visitQueryTable(q, right, nil, emit); err != nil {
					return err
				}
			}
		}
		if !matched && join.Type == "LEFT" {
			candidate := append(storage.Row(nil), leftRow...)
			for _, column := range rightColumns {
				candidate = append(candidate, storage.NullValue(column.Type))
			}
			if err := account.Reserve(queryStorageRowBytes(candidate)); err != nil {
				return err
			}
			return joined.Insert(candidate)
		}
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	return joined, true, nil
}

func indexedJoinTypesCompatible(left, right storage.Column, session *Session) bool {
	integer := func(typ storage.DataType) bool { return typ == storage.TypeInt || typ == storage.TypeBigInt }
	if integer(left.Type) && integer(right.Type) {
		return true
	}
	if left.Type == storage.TypeBoolean && right.Type == storage.TypeBoolean {
		return true
	}
	if (left.Type == storage.TypeVarchar || left.Type == storage.TypeText) && (right.Type == storage.TypeVarchar || right.Type == storage.TypeText) {
		if strings.EqualFold(left.SQLType, "JSON") || strings.EqualFold(right.SQLType, "JSON") {
			return false
		}
		binary := func(column storage.Column) bool {
			if column.Collation != "" {
				return strings.EqualFold(column.Collation, "binary") || strings.HasSuffix(strings.ToLower(column.Collation), "_bin")
			}
			return session.IsBinaryCollation()
		}
		return binary(left) && binary(right)
	}
	// DECIMAL scales, case-insensitive keys, float coercions, composite and
	// non-unique indexes retain the existing general join until planned safely.
	return false
}
func safeIndexedJoinValue(value storage.Value) bool {
	if value.Type == storage.TypeInt || value.Type == storage.TypeBigInt {
		return value.Int64 > -(1<<53) && value.Int64 < (1<<53)
	}
	return true
}
func queryStorageRowBytes(row storage.Row) int64 {
	size := int64(48 + 96*len(row))
	for _, value := range row {
		size += int64(len(value.Text))
	}
	return size
}
