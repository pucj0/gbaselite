package storage

import (
	"fmt"
	"gbaselite/internal/mysqlcompat"
	"strings"
)

func validateColumnCollation(column Column) error {
	if column.Collation == "" {
		return nil
	}
	if column.Type != TypeVarchar && column.Type != TypeText {
		return fmt.Errorf("COLLATE requires a character column")
	}
	_, err := mysqlcompat.ResolveCollation(column.Collation)
	return err
}
func indexCollation(definition Index, position int) string {
	if position < len(definition.Collations) {
		return definition.Collations[position]
	}
	return ""
}
func compareIndexValue(left, right Value, collation string) int {
	if !left.Null && !right.Null && collation != "" && (left.Type == TypeVarchar || left.Type == TypeText) && left.Type == right.Type {
		return mysqlcompat.CompareStrings(left.Text, right.Text, collation)
	}
	return compareValue(left, right)
}
func foldIndexValue(value Value, collation string) Value {
	if collation != "" && strings.HasSuffix(strings.ToLower(collation), "_ci") && (value.Type == TypeVarchar || value.Type == TypeText) {
		value.Text = strings.ToLower(value.Text)
	}
	return value
}
func collationsForIndex(definition Index, columns []Column, columnIndex map[string]int) []string {
	var result []string
	for i, name := range definition.Columns {
		column := columns[columnIndex[normalizeName(name)]]
		if column.Collation != "" {
			if result == nil {
				result = make([]string, len(definition.Columns))
			}
			result[i] = column.Collation
		}
	}
	return result
}
