package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// normalizeDump removes the generation timestamp and orders the lines so the
// comparison does not depend on the (unsorted, map-derived) legacy table order.
func normalizeDump(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "" || strings.HasPrefix(trimmed, "-- Generated:") {
			continue
		}
		lines = append(lines, trimmed)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestMVCCExportDatabaseMatchesLegacyEngine(t *testing.T) {
	legacyDir, mvccDir := t.TempDir(), t.TempDir()
	legacy, err := openLegacy(legacyDir, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	e, session, _ := rangeTestEngine(t)
	t.Cleanup(func() { e.Close() })
	legacySession := &Session{}

	script := []string{
		"CREATE DATABASE exp",
		"USE exp",
		"CREATE TABLE t(id INT PRIMARY KEY,v VARCHAR(10),n INT)",
		"CREATE TABLE u(id INT PRIMARY KEY,tid INT)",
		"INSERT INTO t VALUES(1,'a',10),(2,'b',20)",
		"INSERT INTO u VALUES(9,1)",
		"CREATE VIEW v AS SELECT id,v FROM t",
	}
	for _, query := range script {
		if _, err = legacy.Execute(legacySession, query); err != nil {
			t.Fatalf("legacy %s: %v", query, err)
		}
		if _, err = e.Execute(session, query); err != nil {
			t.Fatalf("mvcc %s: %v", query, err)
		}
	}
	legacyPath := filepath.Join(legacyDir, "legacy.sql")
	mvccPath := filepath.Join(mvccDir, "mvcc.sql")
	if _, err = legacy.Execute(legacySession, fmt.Sprintf("EXPORT DATABASE exp TO '%s'", filepath.ToSlash(legacyPath))); err != nil {
		t.Fatalf("legacy export: %v", err)
	}
	result, err := e.Execute(session, fmt.Sprintf("EXPORT DATABASE exp TO '%s'", filepath.ToSlash(mvccPath)))
	if err != nil {
		t.Fatalf("mvcc export: %v", err)
	}
	if result.Message != "database exported to "+filepath.ToSlash(mvccPath) {
		t.Fatalf("export message = %q", result.Message)
	}
	legacyDump, mvccDump := normalizeDump(t, legacyPath), normalizeDump(t, mvccPath)
	if legacyDump != mvccDump {
		t.Fatalf("dumps differ\nlegacy:\n%s\nmvcc:\n%s", legacyDump, mvccDump)
	}
	for _, want := range []string{"CREATE TABLE `t`", "INSERT INTO `t` VALUES", "CREATE VIEW `v` AS SELECT id,v FROM t", "SET FOREIGN_KEY_CHECKS=0;"} {
		if !strings.Contains(mvccDump, want) {
			t.Errorf("dump is missing %q", want)
		}
	}
}

func TestMVCCExportDatabaseUsesStatementSnapshot(t *testing.T) {
	dir := t.TempDir()
	e, err := OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &Session{}
	run := func(query string) *Result {
		t.Helper()
		result, err := e.Execute(s, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return result
	}
	run("CREATE DATABASE snap")
	run("USE snap")
	run("CREATE TABLE t(id INT PRIMARY KEY)")
	run("INSERT INTO t VALUES(1)")
	run("BEGIN")
	run("INSERT INTO t VALUES(2)")
	path := filepath.Join(dir, "in-tx.sql")
	if _, err := e.Execute(s, fmt.Sprintf("EXPORT DATABASE snap TO '%s'", filepath.ToSlash(path))); err != nil {
		t.Fatal(err)
	}
	// The dump carries the transaction's own uncommitted row.
	if dump := normalizeDump(t, path); !strings.Contains(dump, "(1),") || !strings.Contains(dump, "(2);") {
		t.Fatalf("in-transaction dump = %s", dump)
	}
	run("ROLLBACK")
	if _, err := e.Execute(s, "EXPORT DATABASE missing TO '"+filepath.ToSlash(filepath.Join(dir, "missing.sql"))+"'"); err == nil {
		t.Fatal("export of a missing database was accepted")
	}
}
