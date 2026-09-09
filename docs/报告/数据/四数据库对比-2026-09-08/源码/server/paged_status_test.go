package server

import (
	"gbaselite/executor"
	"testing"
)

func TestPagedStatusUsesActualPersistenceAndFilters(t *testing.T) {
	engine, err := executor.OpenWithOptions(t.TempDir(), "root", "secret", executor.OpenOptions{StorageMode: "paged", PageCacheBytes: 4096, ColdRead: true})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Execute(&executor.Session{}, "CREATE DATABASE metrics"); err != nil {
		t.Fatal(err)
	}
	server := &MySQLServer{Engine: engine}
	result, err := server.executeCompatible(&executor.Session{}, "SHOW GLOBAL STATUS LIKE 'Gbaselite_page_%'")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, row := range result.Rows {
		found[row[0].(string)] = row[1].(string)
	}
	if found["Gbaselite_page_cache_budget_bytes"] != "4096" || found["Gbaselite_page_generation"] == "0" {
		t.Fatalf("paged status %#v", found)
	}
	if _, ok := found["Gbaselite_storage_mode"]; ok {
		t.Fatal("LIKE filter ignored")
	}
	result, err = server.executeCompatible(&executor.Session{}, "SHOW STATUS LIKE 'Gbaselite_storage_mode'")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][1] != "paged" {
		t.Fatalf("mode %+v %v", result, err)
	}
}
