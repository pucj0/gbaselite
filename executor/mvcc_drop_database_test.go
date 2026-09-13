package executor

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gbaselite/sqllayout"
)

// dropDatabaseViewScript pins the DROP DATABASE namespace contract: one statement
// removes db/<name>, every table/<name>/... entry and every view/<name>/... entry,
// and a recreated database never resurrects them. It also pins that a parent/child
// foreign-key pair inside the dropped database does not depend on catalog key order.
var dropDatabaseViewScript = []parityStep{
	{Query: "CREATE DATABASE dd", SkipAffect: true},
	{Query: "USE dd", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "INSERT INTO t VALUES(1)"},
	{Query: "CREATE VIEW v AS SELECT id FROM t", SkipAffect: true},
	{Query: "DROP DATABASE dd", SkipAffect: true},
	{Query: "SHOW DATABASES", Rows: true},
	{Query: "CREATE DATABASE dd", SkipAffect: true},
	{Query: "USE dd", SkipAffect: true},
	{Query: "SHOW TABLES", Rows: true},
	{Query: "SELECT id FROM v", Fail: true},
	// The view name is free for a brand new view and, once dropped, for a table.
	{Query: "CREATE VIEW v AS SELECT 1 AS id", SkipAffect: true},
	{Query: "SELECT * FROM v", Rows: true},
	{Query: "DROP VIEW v", SkipAffect: true},
	{Query: "CREATE TABLE v(id INT)", SkipAffect: true},
	{Query: "INSERT INTO v VALUES(7)"},
	{Query: "SELECT id FROM v", Rows: true},
	{Query: "DROP TABLE v", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT)", SkipAffect: true},
	{Query: "CREATE VIEW v AS SELECT id FROM t", SkipAffect: true},
	{Query: "SELECT id FROM v", Rows: true},
	{Query: "DROP DATABASE IF EXISTS absent_database", SkipAffect: true},
	// A parent/child pair inside the database drops regardless of key order.
	{Query: "CREATE DATABASE fkd", SkipAffect: true},
	{Query: "USE fkd", SkipAffect: true},
	{Query: "CREATE TABLE aaa(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "CREATE TABLE zzz(id INT PRIMARY KEY,pid INT,CONSTRAINT fkz FOREIGN KEY(pid) REFERENCES aaa(id))", SkipAffect: true},
	{Query: "INSERT INTO aaa VALUES(1)"},
	{Query: "INSERT INTO zzz VALUES(2,1)"},
	{Query: "DROP DATABASE fkd", SkipAffect: true},
	{Query: "CREATE DATABASE fkd", SkipAffect: true},
	{Query: "USE fkd", SkipAffect: true},
	{Query: "SHOW TABLES", Rows: true},
}

// dropDatabaseRollbackScript pins transaction semantics: DROP DATABASE is a normal
// MVCC statement, not an implicit commit, so ROLLBACK restores the database with
// its tables and views.
var dropDatabaseRollbackScript = []parityStep{
	{Query: "CREATE DATABASE rb", SkipAffect: true},
	{Query: "USE rb", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "INSERT INTO t VALUES(1)"},
	{Query: "CREATE VIEW v AS SELECT id FROM t", SkipAffect: true},
	{Query: "BEGIN"},
	{Query: "DROP DATABASE rb", SkipAffect: true},
	{Query: "SELECT id FROM v", Fail: true},
	{Query: "CREATE TABLE t2(id INT)", Fail: true},
	{Query: "ROLLBACK"},
	{Query: "USE rb", SkipAffect: true},
	{Query: "SELECT id FROM v", Rows: true},
	{Query: "SHOW TABLES", Rows: true},
	{Query: "DROP DATABASE rb", SkipAffect: true},
}

func TestMVCCDropDatabaseMatchesLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	legacySession, session := &Session{}, &Session{}
	for _, script := range [][]parityStep{dropDatabaseViewScript, dropDatabaseRollbackScript} {
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

// catalogKeys lists the catalog keys of one space so tests can assert that a
// dropped database leaves no orphan entry behind.
func catalogKeys(t *testing.T, e *Engine, space string) []string {
	t.Helper()
	tx, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var keys []string
	if err = tx.Scan(context.Background(), space, func(key, _ []byte) error {
		keys = append(keys, string(key))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestMVCCDropDatabaseRemovesViewCatalogEntries(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY)")
	run("INSERT INTO t VALUES(1)")
	run("CREATE VIEW v AS SELECT id FROM t")
	run("CREATE VIEW vv AS SELECT id FROM v")
	if keys := catalogKeys(t, e, sqllayout.Catalog); !containsPrefix(keys, sqllayout.ViewsPrefix("test")) {
		t.Fatalf("view entries missing before drop: %v", keys)
	}
	run("DROP DATABASE test")
	for _, key := range catalogKeys(t, e, sqllayout.Catalog) {
		if strings.HasPrefix(key, sqllayout.ViewsPrefix("test")) || strings.HasPrefix(key, sqllayout.TablesPrefix("test")) || key == sqllayout.DatabasePrefix+"test" {
			t.Fatalf("DROP DATABASE left an orphan catalog entry: %s", key)
		}
	}
	// Recreating the database must not resurrect the old relations.
	run("CREATE DATABASE test")
	run("USE test")
	if got := fmt.Sprint(run("SHOW TABLES").Rows); got != "[]" {
		t.Fatalf("recreated database lists old relations: %s", got)
	}
	if _, err := e.Execute(s, "SELECT id FROM vv"); err == nil {
		t.Fatal("old view survived DROP DATABASE")
	}
	run("CREATE TABLE v(id INT)")
	run("DROP TABLE v")
	run("CREATE VIEW v AS SELECT 1 AS id")
	if got := fmt.Sprint(run("SELECT * FROM v").Rows); got != "[[1]]" {
		t.Fatalf("recreated view rows=%s", got)
	}
}

func containsPrefix(keys []string, prefix string) bool {
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func TestMVCCDropDatabaseViewStaysDroppedAfterReopen(t *testing.T) {
	dir := t.TempDir()
	e, err := OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{}
	run := func(query string) *Result {
		t.Helper()
		result, err := e.Execute(s, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return result
	}
	run("CREATE DATABASE rp")
	run("USE rp")
	run("CREATE TABLE t(id INT PRIMARY KEY)")
	run("INSERT INTO t VALUES(1)")
	run("CREATE VIEW v AS SELECT id FROM t")
	run("DROP DATABASE rp")
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s = &Session{}
	run("CREATE DATABASE rp")
	run("USE rp")
	if got := fmt.Sprint(run("SHOW TABLES").Rows); got != "[]" {
		t.Fatalf("reopened database lists dropped relations: %s", got)
	}
	if _, err := e.Execute(s, "SELECT id FROM v"); err == nil {
		t.Fatal("dropped view survived reopen")
	}
	run("CREATE TABLE t(id INT)")
	run("CREATE TABLE v(id INT)")
	run("INSERT INTO v VALUES(3)")
	run("DROP TABLE v")
	run("CREATE VIEW v AS SELECT id FROM t")
	if got := fmt.Sprint(run("SELECT id FROM v").Rows); got != "[]" {
		t.Fatalf("recreated view rows=%s", got)
	}
	if keys := catalogKeys(t, e, sqllayout.Catalog); !containsPrefix(keys, sqllayout.ViewsPrefix("rp")) {
		t.Fatalf("recreated view is missing from the catalog: %v", keys)
	}
}

func TestMVCCDropDatabaseClearsSelectedDatabase(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY)")
	run("CREATE VIEW v AS SELECT id FROM t")
	run("DROP DATABASE test")
	// Legacy clears the session's selected database, so later unqualified
	// statements fail instead of writing into the dropped database.
	if s.CurrentDatabase != "" {
		t.Fatalf("selected database = %q, want cleared", s.CurrentDatabase)
	}
	if _, err := e.Execute(s, "SHOW TABLES"); err == nil {
		t.Fatal("SHOW TABLES succeeded without a selected database")
	}
	run("CREATE DATABASE test")
	if _, err := e.Execute(s, "CREATE TABLE late(id INT)"); err == nil {
		t.Fatal("unqualified CREATE TABLE wrote into a database that was dropped")
	}
	run("USE test")
	if got := fmt.Sprint(run("SHOW TABLES").Rows); got != "[]" {
		t.Fatalf("recreated database lists old relations: %s", got)
	}
}
