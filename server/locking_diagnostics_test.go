package server

import (
	"context"
	"strings"
	"testing"

	"gbaselite/executor"
)

// locking_diagnostics_test.go tests SHOW ENGINE diagnostics over the MySQL protocol layer.

func TestT062ServerShowEngineDiagnosticsProtocol(t *testing.T) {
	dir := t.TempDir()
	e, err := executor.OpenWithOptions(dir, "root", "password", executor.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()
	_, _ = e.Execute(&executor.Session{Username: "root", Host: "%"}, "CREATE DATABASE test")

	// 1. Root admin session
	adminSession := &executor.Session{
		Username:        "root",
		Host:            "%",
		CurrentDatabase: "test",
	}

	for _, cmd := range []string{
		"SHOW ENGINE LOCKS",
		"SHOW ENGINE LOCK WAITS",
		"SHOW ENGINE DEADLOCKS",
		"SHOW ENGINE INNODB STATUS",
	} {
		res, err := e.Execute(adminSession, cmd)
		if err != nil {
			t.Fatalf("admin %s failed: %v", cmd, err)
		}
		if res == nil || len(res.Columns) == 0 {
			t.Fatalf("expected ResultSet columns for %s, got nil/empty", cmd)
		}
	}

	// 2. Non-admin user session
	_, _ = e.Execute(adminSession, "CREATE USER 'bob'@'%' IDENTIFIED BY 'pass'")
	bobSession := &executor.Session{
		Username:        "bob",
		Host:            "%",
		CurrentDatabase: "test",
	}

	for _, cmd := range []string{
		"SHOW ENGINE LOCKS",
		"SHOW ENGINE LOCK WAITS",
		"SHOW ENGINE DEADLOCKS",
		"SHOW ENGINE INNODB STATUS",
	} {
		_, err := e.Execute(bobSession, cmd)
		if err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Fatalf("expected access denied for %s, got: %v", cmd, err)
		}
	}

	_ = ctx
}
