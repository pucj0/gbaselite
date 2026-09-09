package storage

import "strings"

// hasReferencedValues uses the parent's unique key, falling back to an early
// exit read-only scan when a matching index is unavailable. No rows are copied.
func (t *Table) hasReferencedValues(columns []string, values []Value) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	usable := true
	for _, value := range values {
		if value.Null {
			return false
		}
		// Existing float keys retain signed zero; comparison treats both as equal.
		if (value.Type == TypeFloat || value.Type == TypeDouble) && value.Float == 0 {
			usable = false
		}
	}
	if usable {
		for key, definition := range t.indexes {
			if !definition.Unique || len(definition.Columns) != len(columns) {
				continue
			}
			match := true
			for i, column := range columns {
				if !strings.EqualFold(column, definition.Columns[i]) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			encoded := make([]byte, 0, len(values)*16)
			for _, value := range values {
				encoded = appendIndexValueKey(encoded, value)
			}
			_, exists := t.uniqueRows[key][string(encoded)]
			return exists
		}
	}
	for _, row := range t.rows {
		match := true
		for i, column := range columns {
			position, ok := t.columnIndex[normalizeName(column)]
			if !ok || compareValue(row[position], values[i]) != 0 {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
