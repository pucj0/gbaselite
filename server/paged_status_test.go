package server

import (
	"gbaselite/executor"
	"testing"
)

func TestMVCCStatusOmitsRetiredPageMetrics(t *testing.T) {
	engine, err := openTestEngineWithOptions(t, t.TempDir(), "root", "secret", executor.OpenOptions{})
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
	if len(result.Rows) != 0 {
		t.Fatalf("retired paged metrics: %+v", result.Rows)
	}
	result, err = server.executeCompatible(&executor.Session{}, "SHOW STATUS LIKE 'Gbaselite_storage_mode'")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][1] != "mvcc" {
		t.Fatalf("mode %+v %v", result, err)
	}
}
