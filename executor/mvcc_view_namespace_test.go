package executor

import (
	"fmt"
	"reflect"
	"testing"
)

// viewMetadataScript pins the metadata surface of views: SHOW TABLES lists views
// in one namespace with tables, SHOW FULL TABLES reports VIEW, and
// SHOW COLUMNS/DESCRIBE resolve a view definition.
var viewMetadataScript = []parityStep{
	{Query: "CREATE DATABASE vm", SkipAffect: true},
	{Query: "USE vm", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "INSERT INTO t VALUES(1,10)"},
	{Query: "CREATE VIEW v AS SELECT id,v FROM t", SkipAffect: true},
	{Query: "CREATE VIEW vv AS SELECT id FROM v", SkipAffect: true},
	{Query: "SHOW TABLES", Rows: true},
	{Query: "SHOW FULL TABLES", Rows: true},
	{Query: "SHOW COLUMNS FROM v", Rows: true},
	{Query: "DESCRIBE v", Rows: true},
	{Query: "SHOW COLUMNS FROM vv", Rows: true},
	{Query: "SHOW COLUMNS FROM v LIKE 'v'", Rows: true},
	{Query: "SHOW COLUMNS FROM v WHERE Field='v'", Rows: true},
	{Query: "SHOW CREATE VIEW v", Rows: true},
	{Query: "SHOW CREATE VIEW missing", Fail: true},
	{Query: "SHOW CREATE TABLE t", Rows: true},
	// SHOW CREATE TABLE on a view returns the view definition, like legacy.

	{Query: "SHOW CREATE TABLE v", Rows: true},
	{Query: "DROP VIEW v", SkipAffect: true},
	{Query: "SHOW TABLES", Rows: true},
	{Query: "SHOW COLUMNS FROM v", Fail: true},
	{Query: "CREATE VIEW v AS SELECT id FROM t", SkipAffect: true},
	{Query: "SHOW TABLES", Rows: true},
}

// viewNameCollisionScript pins the shared table/view namespace: a view name is
// not a valid table target, a view is not a valid rename destination, and the
// name survives every refused statement.
var viewNameCollisionScript = []parityStep{
	{Query: "CREATE DATABASE vc", SkipAffect: true},
	{Query: "USE vc", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "INSERT INTO t VALUES(1)"},
	{Query: "CREATE VIEW v AS SELECT id FROM t", SkipAffect: true},
	{Query: "CREATE TABLE v (id INT)", Fail: true},
	// IF NOT EXISTS swallows the conflict and leaves the view in place.
	{Query: "CREATE TABLE IF NOT EXISTS v (id INT)", SkipAffect: true},
	{Query: "CREATE TABLE v AS SELECT id FROM t", Fail: true},
	{Query: "CREATE TABLE IF NOT EXISTS v AS SELECT id FROM t", SkipAffect: true},
	// CREATE TABLE LIKE resolves its source as a table.
	{Query: "CREATE TABLE n LIKE v", Fail: true},
	{Query: "CREATE TABLE IF NOT EXISTS n LIKE v", Fail: true},
	// CTAS reads a view happily as long as the target name is free.
	{Query: "CREATE TABLE n2 AS SELECT id FROM v", SkipAffect: true},
	{Query: "SELECT COUNT(*) FROM n2", Rows: true},
	{Query: "RENAME TABLE v TO w", Fail: true},
	{Query: "RENAME TABLE v TO v", Fail: true},
	{Query: "CREATE TABLE t2(id INT)", SkipAffect: true},
	{Query: "RENAME TABLE t2 TO v", Fail: true},
	{Query: "DROP TABLE v", Fail: true},
	// DROP TABLE IF EXISTS ignores the view name and keeps the view.
	{Query: "DROP TABLE IF EXISTS v", SkipAffect: true},
	{Query: "SELECT COUNT(*) FROM v", Rows: true},
	{Query: "CREATE INDEX i ON v (id)", Fail: true},
	{Query: "TRUNCATE v", Fail: true},
	{Query: "ALTER TABLE v ADD COLUMN z INT", Fail: true},
	{Query: "INSERT INTO v VALUES(1)", Fail: true},
	{Query: "UPDATE v SET id=1", Fail: true},
	{Query: "DELETE FROM v", Fail: true},
	// DROP VIEW works on the view and not on a table.
	{Query: "DROP VIEW t", Fail: true},
	{Query: "DROP VIEW IF EXISTS t", SkipAffect: true},
	{Query: "DROP VIEW v", SkipAffect: true},
	{Query: "SHOW TABLES", Rows: true},
	{Query: "SELECT COUNT(*) FROM t", Rows: true},
	{Query: "SELECT COUNT(*) FROM t2", Rows: true},
}

// tableNameCollisionScript pins the mirror case: a table name blocks CREATE VIEW
// and a view name blocks CREATE TABLE in the same namespace.
var tableNameCollisionScript = []parityStep{
	{Query: "CREATE DATABASE vt", SkipAffect: true},
	{Query: "USE vt", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "INSERT INTO t VALUES(1)"},
	{Query: "CREATE VIEW t AS SELECT id FROM t", Fail: true},
	{Query: "CREATE OR REPLACE VIEW t AS SELECT id FROM t", Fail: true},
	{Query: "CREATE VIEW n AS SELECT id FROM t", SkipAffect: true},
	{Query: "DROP VIEW t", Fail: true},
	{Query: "DROP TABLE t", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT)", SkipAffect: true},
	{Query: "CREATE VIEW z AS SELECT id FROM t", SkipAffect: true},
	// The parser has no CREATE VIEW IF NOT EXISTS form; both runtimes reject it.

	{Query: "CREATE VIEW IF NOT EXISTS z AS SELECT id FROM t", Fail: true},
	{Query: "CREATE OR REPLACE VIEW z AS SELECT id FROM t", SkipAffect: true},
	{Query: "CREATE TABLE z (id INT)", Fail: true},
	{Query: "CREATE TABLE IF NOT EXISTS z (id INT)", SkipAffect: true},
	{Query: "SHOW TABLES", Rows: true},
	{Query: "SELECT COUNT(*) FROM n", Rows: true},
	{Query: "SELECT COUNT(*) FROM z", Rows: true},
	{Query: "INSERT INTO t SELECT id FROM z"},
	{Query: "SELECT COUNT(*) FROM t", Rows: true},
}

func TestMVCCViewMetadataMatchesLegacyEngine(t *testing.T) {
	assertViewScriptParity(t, viewMetadataScript)
}

func TestMVCCViewNamespaceMatchesLegacyEngine(t *testing.T) {
	assertViewScriptParity(t, viewNameCollisionScript, tableNameCollisionScript)
}

func assertViewScriptParity(t *testing.T, scripts ...[]parityStep) {
	t.Helper()
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	e, session, _ := rangeTestEngine(t)
	for _, script := range scripts {
		legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, script)
		mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, script)
		if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
			for i, step := range script {
				if legacyOutcomes[i] != mvccOutcomes[i] {
					t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
				}
			}
		}
	}
}

func TestMVCCViewNameCollisionKeepsCatalogAtomic(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY)")
	run("INSERT INTO t VALUES(1)")
	run("CREATE VIEW v AS SELECT id FROM t")
	// Every refused statement must leave the view queryable and the table intact.
	for _, query := range []string{
		"CREATE TABLE v (id INT)",
		"CREATE TABLE v AS SELECT id FROM t",
		"RENAME TABLE t TO v",
	} {
		if _, err := e.Execute(s, query); err == nil {
			t.Errorf("accepted conflicting statement: %s", query)
		}
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM v").Rows); got != "[[1]]" {
		t.Fatalf("view rows=%s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[1]]" {
		t.Fatalf("table rows=%s", got)
	}
	if got := fmt.Sprint(run("SHOW TABLES").Rows); got != "[[t] [v]]" {
		t.Fatalf("relations=%s", got)
	}
}
