package server

import (
	"gbaselite/executor"
	"gbaselite/internal/mysqlcompat"
	"gbaselite/parser"
	"gbaselite/storage"
	"strings"
)

func staticMetadata(name string) *executor.Result {
	text := func(name string) executor.Column { return executor.Column{Name: name, Type: storage.TypeVarchar} }
	number := func(name string) executor.Column { return executor.Column{Name: name, Type: storage.TypeBigInt} }
	switch strings.ToUpper(name) {
	case "ENGINES":
		return &executor.Result{
			Columns: []executor.Column{text("ENGINE"), text("SUPPORT"), text("COMMENT"), text("TRANSACTIONS"), text("XA"), text("SAVEPOINTS")},
			Rows:    [][]any{{"GBaseLite", "DEFAULT", "GBaseLite storage engine", "YES", "NO", "YES"}},
		}
	case "CHARACTER_SETS":
		result := &executor.Result{Columns: []executor.Column{text("CHARACTER_SET_NAME"), text("DEFAULT_COLLATE_NAME"), text("DESCRIPTION"), number("MAXLEN")}}
		for _, value := range mysqlcompat.CharacterSets() {
			result.Rows = append(result.Rows, []any{value.Name, value.DefaultCollation, value.Description, value.MaxLength})
		}
		return result
	case "COLLATIONS":
		result := &executor.Result{Columns: []executor.Column{text("COLLATION_NAME"), text("CHARACTER_SET_NAME"), number("ID"), text("IS_DEFAULT"), text("IS_COMPILED"), number("SORTLEN")}}
		for _, value := range mysqlcompat.Collations() {
			isDefault := ""
			if value.IsDefault {
				isDefault = "Yes"
			}
			result.Rows = append(result.Rows, []any{value.Name, value.Charset, value.ID, isDefault, "Yes", int64(1)})
		}
		return result
	}
	return nil
}

func virtualMetadataStatement(engine *executor.Engine, session *executor.Session, query string) (*executor.Result, bool, error) {
	statement, err := parser.Parse(query)
	if err != nil {
		return nil, false, nil
	}
	name := ""
	switch value := statement.(type) {
	case parser.Select:
		name = value.Table
	case parser.Show:
		if value.What != "COLUMNS" && value.What != "INDEX" {
			return nil, false, nil
		}
		name = value.Name
	default:
		return nil, false, nil
	}
	schema := session.CurrentDatabase
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		schema, name = name[:dot], name[dot+1:]
	}
	if !strings.EqualFold(schema, "information_schema") {
		return nil, false, nil
	}
	source := staticMetadata(name)
	if _, ok := statement.(parser.Show); ok && source == nil {
		directory, directoryErr := metadataBrowseDatabase(engine, schema)
		if directoryErr != nil {
			return nil, true, directoryErr
		}
		if _, viewErr := directory.View(name); viewErr != nil {
			return nil, true, viewErr
		}
		// Only the column definitions are needed. Restrict schema readers so no
		// business-table rows are materialized merely to describe a virtual view.
		source, err = ExecuteCompatible(engine, session, "SELECT * FROM information_schema.`"+strings.ReplaceAll(name, "`", "``")+"` WHERE TABLE_SCHEMA='information_schema'")
		if err != nil {
			return nil, true, err
		}
	}
	if source == nil {
		return nil, false, nil
	}
	if _, ok := statement.(parser.Show); ok {
		source = &executor.Result{Columns: source.Columns}
	}
	result, err := executor.ExecuteMetadataStatement(session, "information_schema", name, source, statement)
	return result, true, err
}
