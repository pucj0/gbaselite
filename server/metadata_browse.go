package server

import (
	"gbaselite/executor"
	"gbaselite/storage"
	"strings"
)

// This directory contains only implemented compatibility views. It is never
// installed into the persistent catalog and contains no business data.
func metadataBrowseDatabase(engine *executor.Engine, name string) (*storage.Database, error) {
	if !strings.EqualFold(name, "information_schema") {
		return engine.Store.Database(name)
	}
	store := storage.NewStore()
	database, err := store.CreateDatabase("information_schema")
	if err != nil {
		return nil, err
	}
	for _, table := range []string{"ENGINES", "CHARACTER_SETS", "COLLATIONS", "SCHEMATA", "TABLES", "COLUMNS", "STATISTICS", "VIEWS", "TABLE_CONSTRAINTS", "KEY_COLUMN_USAGE", "REFERENTIAL_CONSTRAINTS", "CHECK_CONSTRAINTS", "ROUTINES", "PARAMETERS", "TRIGGERS", "EVENTS", "PARTITIONS", "FILES", "TABLESPACES", "USER_PRIVILEGES", "SCHEMA_PRIVILEGES", "TABLE_PRIVILEGES"} {
		if err := database.CreateView(table, "SELECT 1", nil, false); err != nil {
			return nil, err
		}
	}
	return database, nil
}
