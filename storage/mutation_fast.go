package storage

import (
	"fmt"
	"sort"
	"strings"
)

// tryIndependentMutation avoids staging unrelated tables when no foreign key
// value or referenced key can change. constraintMu is held by the caller.
func (d *Database) tryIndependentMutation(mutations []RowMutation) (int, bool, error) {
	if len(mutations) != 1 || len(mutations[0].Inserts) != 0 {
		return 0, false, nil
	}
	mutation := mutations[0]
	_, name := splitQualifiedName(mutation.Table)
	table, err := d.Table(name)
	if err != nil {
		return 0, true, err
	}
	var protected []string
	for _, fk := range table.ForeignKeys() {
		protected = append(protected, fk.Columns...)
	}
	for _, childName := range d.ListTables() {
		child, _ := d.Table(childName)
		for _, fk := range child.ForeignKeys() {
			_, parent := splitQualifiedName(fk.RefTable)
			if strings.EqualFold(parent, name) {
				if len(mutation.Delete) > 0 {
					return 0, false, nil
				}
				protected = append(protected, fk.RefColumns...)
			}
		}
	}
	return table.applyIndependentMutation(mutation, protected)
}

func (t *Table) applyIndependentMutation(mutation RowMutation, protected []string) (int, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	replacements := make(map[int]Row, len(mutation.Replacements))
	deleted := make(map[int]bool, len(mutation.Delete))
	for position, row := range mutation.Replacements {
		if position < 0 || position >= len(t.rows) {
			return 0, true, fmt.Errorf("row index %d is out of range", position)
		}
		copied, err := t.validateAndCloneRow(row)
		if err != nil {
			return 0, true, err
		}
		for _, name := range protected {
			column, ok := t.columnIndex[normalizeName(name)]
			if !ok || compareValue(t.rows[position][column], copied[column]) != 0 {
				return 0, false, nil
			}
		}
		replacements[position] = copied
	}
	for _, position := range mutation.Delete {
		if position < 0 || position >= len(t.rows) {
			return 0, true, fmt.Errorf("row index %d is out of range", position)
		}
		if deleted[position] || replacements[position] != nil {
			return 0, true, fmt.Errorf("row %d has duplicate mutations", position)
		}
		deleted[position] = true
	}
	affected := len(replacements) + len(deleted)
	if affected == 0 {
		return 0, true, nil
	}
	// Validate the final unique-key delta before publishing any row or index.
	// Removing all old keys first also permits a valid multi-row key swap.
	changedIndexes := make(map[string]bool)
	for name, definition := range t.indexes {
		changed := len(deleted) > 0
		added := make(map[string]int)
		for position, row := range replacements {
			oldKey, oldComparable := t.indexKey(definition, t.rows[position])
			key, comparable := t.indexKey(definition, row)
			if compareRowsByIndex(t.rows[position], row, definition, t.columnIndex) != 0 || oldKey != key || oldComparable != comparable {
				changed = true
			}
			if !definition.Unique || !comparable {
				continue
			}
			if _, exists := added[key]; exists {
				return 0, true, fmt.Errorf("%w for index %q", ErrDuplicateKey, definition.Name)
			}
			added[key] = position
			if owner, exists := t.uniqueRows[name][key]; exists && owner != position && !deleted[owner] {
				other, changing := replacements[owner]
				if !changing {
					return 0, true, fmt.Errorf("%w for index %q", ErrDuplicateKey, definition.Name)
				}
				otherKey, otherComparable := t.indexKey(definition, other)
				if otherComparable && otherKey == key {
					return 0, true, fmt.Errorf("%w for index %q", ErrDuplicateKey, definition.Name)
				}
			}
		}
		changedIndexes[name] = changed
	}
	// Row headers are immutable to existing stream/snapshot readers too.
	nextRows := make([]Row, 0, len(t.rows)-len(deleted))
	var remap []int
	if len(deleted) > 0 {
		remap = make([]int, len(t.rows))
	}
	nextLength := t.dataLength
	for position, row := range t.rows {
		if deleted[position] {
			remap[position] = -1
			nextLength -= rowDataLength(row)
			continue
		}
		if remap != nil {
			remap[position] = len(nextRows)
		}
		if replacement, ok := replacements[position]; ok {
			nextLength += rowDataLength(replacement) - rowDataLength(row)
			row = replacement
		}
		nextRows = append(nextRows, row)
	}
	newPosition := func(old int) int {
		if remap != nil {
			return remap[old]
		}
		return old
	}
	for name, definition := range t.indexes {
		if !changedIndexes[name] {
			continue
		}
		if definition.Unique {
			entries := t.uniqueRows[name]
			// Do not rehash keys from unchanged row payloads.
			for position := range replacements {
				if key, ok := t.indexKey(definition, t.rows[position]); ok {
					delete(entries, key)
				}
			}
			if remap != nil {
				for key, position := range entries {
					if remap[position] < 0 {
						delete(entries, key)
					} else {
						entries[key] = remap[position]
					}
				}
			}
			for position, row := range replacements {
				if key, ok := t.indexKey(definition, row); ok {
					entries[key] = newPosition(position)
				}
			}
		}
		ordered := make([]int, 0, len(nextRows))
		for _, position := range t.indexRows[name] {
			if deleted[position] || replacements[position] != nil {
				continue
			}
			ordered = append(ordered, newPosition(position))
		}
		added := make([]int, 0, len(replacements))
		for position := range replacements {
			added = append(added, newPosition(position))
		}
		less := func(a, b int) bool {
			comparison := compareRowsByIndex(nextRows[a], nextRows[b], definition, t.columnIndex)
			return comparison < 0 || comparison == 0 && a < b
		}
		sort.Slice(added, func(i, j int) bool { return less(added[i], added[j]) })
		// Merge backward into the filtered array, avoiding a second N-row array.
		left, right := len(ordered)-1, len(added)-1
		ordered = ordered[:len(nextRows)]
		for out := len(ordered) - 1; out >= 0; out-- {
			if right < 0 || left >= 0 && less(added[right], ordered[left]) {
				ordered[out] = ordered[left]
				left--
			} else {
				ordered[out] = added[right]
				right--
			}
		}
		t.indexRows[name] = ordered
	}
	t.rows = nextRows
	t.dataLength = nextLength
	for _, row := range replacements {
		t.updateAutoNextLocked(row)
	}
	t.touchLocked()
	return affected, true, nil
}

// VisitMutationRows provides immutable pre-statement rows with physical row
// positions. Index candidates are restored to physical order to preserve LIMIT
// selection and assignment/error order. Engine writes hold the transaction gate.
// The visitor must not modify rows; returning false stops the scan.
func (t *Table) VisitMutationRows(scan *IndexScan, visitor func(int, Row) (bool, error)) error {
	t.mu.RLock()
	rows := t.rows
	var positions []int
	if scan != nil {
		indexed, start, end, err := t.indexIntervalLocked(*scan)
		if err != nil {
			t.mu.RUnlock()
			return err
		}
		positions = append([]int{}, indexed[start:end]...)
	}
	t.mu.RUnlock()
	if positions != nil {
		sort.Ints(positions)
		for _, position := range positions {
			more, err := visitor(position, rows[position])
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
	} else {
		for position, row := range rows {
			more, err := visitor(position, row)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
	}
	return nil
}
