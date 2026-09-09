package storage

import "fmt"

// IndexValueKey shares the exact in-memory constraint semantics with the
// versioned disk backend. NULL-containing unique keys are not constrained.
func IndexValueKey(definition Index, columns []Column, row Row) (string, bool) {
	positions := make(map[string]int, len(columns))
	for i, column := range columns {
		positions[normalizeName(column.Name)] = i
	}
	definition.Collations = collationsForIndex(definition, columns, positions)
	return indexKey(definition, positions, row)
}
func ValidateRowValues(columns []Column, row Row) error {
	if len(columns) != len(row) {
		return fmt.Errorf("row has %d values, expected %d", len(row), len(columns))
	}
	for i, value := range row {
		if err := validateValue(columns[i], value); err != nil {
			return err
		}
	}
	return nil
}
