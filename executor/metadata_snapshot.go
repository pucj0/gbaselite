package executor

import (
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
)

// ExecuteMetadataStatement evaluates read-only statements against a caller-supplied
// metadata snapshot. The isolated store cannot access or modify business data.
func ExecuteMetadataStatement(session *Session, schema, name string, source *Result, statement parser.Statement) (*Result, error) {
	switch value := statement.(type) {
	case parser.Select:
		if len(value.Joins) != 0 || value.Subquery != nil {
			return nil, fmt.Errorf("metadata JOIN and derived tables are not supported")
		}
	case parser.Show:
		if value.What != "COLUMNS" && value.What != "INDEX" {
			return nil, fmt.Errorf("unsupported metadata SHOW %s", value.What)
		}
	default:
		return nil, fmt.Errorf("metadata statements must be read-only SELECT or SHOW")
	}
	store := storage.NewStore()
	database, err := store.CreateDatabase(schema)
	if err != nil {
		return nil, err
	}
	columns := make([]storage.Column, len(source.Columns))
	for i, column := range source.Columns {
		columns[i] = storage.Column{Name: column.Name, Type: column.Type, SQLType: column.SQLType, Length: column.Length, MetadataVersion: 1, Nullable: true}
		if column.Type == storage.TypeVarchar && columns[i].Length == 0 {
			columns[i].Length = 65535
		}
	}
	table, err := database.CreateTable(name, columns)
	if err != nil {
		return nil, err
	}
	for _, values := range source.Rows {
		if len(values) != len(columns) {
			return nil, fmt.Errorf("invalid metadata row width")
		}
		row := make(storage.Row, len(columns))
		for i, value := range values {
			row[i], err = interfaceToColumnValue(value, columns[i])
			if err != nil {
				return nil, err
			}
		}
		if err = table.Insert(row); err != nil {
			return nil, err
		}
	}
	local := *session
	local.CurrentDatabase = schema
	local.StreamResults = false
	switch value := statement.(type) {
	case parser.Select:
		return executeSelect(store, &local, value)
	case parser.Show:
		return executeShow(store, &local, value)
	}
	panic("unreachable metadata statement")
}
