package storage

// cloneForTransaction shares only immutable row payloads. Every mutable map,
// index position array and schema collection is owned by the new table.
// The source is already valid; rebuilding and sorting its indexes is wasteful.
func (t *Table) cloneForTransaction() *Table {
	t.mu.RLock()
	defer t.mu.RUnlock()
	copied := &Table{
		name: t.name, comment: t.comment, createdAt: t.createdAt, updatedAt: t.updatedAt,
		columns:     append([]Column(nil), t.columns...),
		columnIndex: make(map[string]int, len(t.columnIndex)),
		indexes:     make(map[string]Index, len(t.indexes)),
		foreignKeys: cloneForeignKeys(t.foreignKeys),
		checks:      append([]CheckConstraint(nil), t.checks...),
		uniqueRows:  make(map[string]map[string]int, len(t.uniqueRows)),
		indexRows:   make(map[string][]int, len(t.indexRows)),
		autoNext:    make(map[string]int64, len(t.autoNext)),
		dataLength:  t.dataLength, rows: append([]Row(nil), t.rows...), cold: t.cold,
	}
	for name, position := range t.columnIndex {
		copied.columnIndex[name] = position
	}
	for name, definition := range t.indexes {
		definition.Columns = append([]string(nil), definition.Columns...)
		copied.indexes[name] = definition
	}
	for name, entries := range t.uniqueRows {
		values := make(map[string]int, len(entries))
		for key, position := range entries {
			values[key] = position
		}
		copied.uniqueRows[name] = values
	}
	for name, positions := range t.indexRows {
		copied.indexRows[name] = append([]int(nil), positions...)
	}
	for name, next := range t.autoNext {
		copied.autoNext[name] = next
	}
	copied.publishSchemaLocked()
	return copied
}
