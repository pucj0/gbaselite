package executor

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

var viewScript = []parityStep{
	{Query: "CREATE DATABASE vw", SkipAffect: true},
	{Query: "USE vw", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT PRIMARY KEY,v INT,label VARCHAR(10))", SkipAffect: true},
	{Query: "CREATE TABLE u(id INT PRIMARY KEY,tid INT,note VARCHAR(10))", SkipAffect: true},
	{Query: "INSERT INTO t VALUES(1,10,'a'),(2,20,'b')"},
	{Query: "INSERT INTO u VALUES(100,1,'x'),(200,2,'y')"},
	// View over a base table with alias and qualified column resolution.
	{Query: "CREATE VIEW v1 AS SELECT id,v FROM t"},
	{Query: "SELECT * FROM v1 ORDER BY id", Rows: true},
	{Query: "SELECT x.id FROM v1 x WHERE x.v>10", Rows: true},
	{Query: "SHOW CREATE VIEW v1", Rows: true},
	// Views over JOIN, aggregate, UNION, derived table and a subquery.
	{Query: "CREATE VIEW v2 AS SELECT t.id,u.note FROM t JOIN u ON u.tid=t.id"},
	{Query: "SELECT id,note FROM v2 ORDER BY id", Rows: true},
	{Query: "CREATE VIEW v3 AS SELECT COUNT(*) AS n,SUM(v) AS s FROM t"},
	{Query: "SELECT n,s FROM v3", Rows: true},
	{Query: "CREATE VIEW v4 AS SELECT id FROM t WHERE id=1 UNION ALL SELECT id FROM t WHERE id=2"},
	{Query: "SELECT id FROM v4 ORDER BY id", Rows: true},
	{Query: "CREATE VIEW v5 AS SELECT d.id FROM (SELECT id FROM t) d"},
	{Query: "SELECT id FROM v5 ORDER BY id", Rows: true},
	{Query: "CREATE VIEW v6 AS SELECT id FROM t WHERE id IN (SELECT tid FROM u)"},
	{Query: "SELECT id FROM v6 ORDER BY id", Rows: true},
	// Nested view and view referenced from a subquery / INSERT SELECT source.
	{Query: "CREATE VIEW v7 AS SELECT id FROM v2"},
	{Query: "SELECT id FROM v7 ORDER BY id", Rows: true},
	{Query: "SELECT COUNT(*) FROM (SELECT id FROM v7) d", Rows: true},
	// Views are reachable from subqueries, including correlated ones.
	{Query: "SELECT id FROM t WHERE id IN (SELECT id FROM v7)", Rows: true},
	// A correlated outer reference is rejected by both engines when the subquery
	// FROM is a view; the boundary is shared with legacy, not an MVCC gap.
	{Query: "SELECT t.id FROM t WHERE EXISTS (SELECT 1 FROM v7 x WHERE x.id=t.id)", Fail: true},
	{Query: "CREATE TABLE copy(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "INSERT INTO copy SELECT id,v FROM v1"},
	{Query: "SELECT id,v FROM copy ORDER BY id", Rows: true},
	// DROP VIEW, IF EXISTS and multiple names.
	{Query: "DROP VIEW v7"},
	{Query: "SELECT id FROM v7", Fail: true},
	{Query: "DROP VIEW IF EXISTS v7"},
	{Query: "DROP VIEW IF EXISTS missing_view"},
	{Query: "DROP VIEW v1,v4"},
	{Query: "SELECT COUNT(*) FROM v1", Fail: true},
	// Recreating a dropped name works.
	{Query: "CREATE VIEW v1 AS SELECT id FROM t"},
	{Query: "SELECT COUNT(*) FROM v1", Rows: true},
}

func TestMVCCViewsMatchLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, viewScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, viewScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range viewScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCViewPersistenceAndTransactionAtomicity(t *testing.T) {
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
	run("CREATE DATABASE vp")
	run("USE vp")
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,10)")
	run("CREATE VIEW keep AS SELECT id,v FROM t")
	// A transaction that creates a view and rolls back must leave no view behind.
	run("BEGIN")
	if _, err := e.Execute(s, "CREATE VIEW rolled AS SELECT id FROM t"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(s, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(s, "SELECT id FROM rolled"); err == nil {
		t.Fatal("rolled-back view survived")
	}
	if got := fmt.Sprint(run("SELECT id,v FROM keep").Rows); got != "[[1 10]]" {
		t.Fatalf("view rows = %s", got)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s = &Session{CurrentDatabase: "vp"}
	result, err := e.Execute(s, "SELECT id,v FROM keep")
	if err != nil {
		t.Fatalf("view after reopen: %v", err)
	}
	if got := fmt.Sprint(result.Rows); got != "[[1 10]]" {
		t.Fatalf("reopened view rows = %s", got)
	}
	show, err := e.Execute(s, "SHOW CREATE VIEW keep")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(show.Rows[0][1]); !strings.Contains(got, "CREATE VIEW `keep` AS SELECT id,v FROM t") {
		t.Fatalf("show create view = %s", got)
	}
}

func TestMVCCViewCycleDetection(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY)")
	run("INSERT INTO t VALUES(1)")
	run("CREATE VIEW v1 AS SELECT id FROM t")
	run("CREATE VIEW v2 AS SELECT id FROM v1")
	// Replacing v1 so it reads v2 creates a cycle that must be detected, not loop.
	if _, err := e.Execute(s, "CREATE OR REPLACE VIEW v1 AS SELECT id FROM v2"); err != nil {
		t.Fatalf("replace view: %v", err)
	}
	_, err := e.Execute(s, "SELECT id FROM v1")
	if err == nil {
		t.Fatal("circular view query accepted")
	}
	if !strings.Contains(err.Error(), "circular view reference") {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestMVCCViewUsesStatementSnapshot(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,10)")
	run("CREATE VIEW v AS SELECT id,v FROM t")
	a := &Session{CurrentDatabase: "test"}
	runA := func(query string) *Result {
		t.Helper()
		result, err := e.Execute(a, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return result
	}
	runA("BEGIN")
	runA("SELECT COUNT(*) FROM v")
	run("INSERT INTO t VALUES(2,20)")
	if got := fmt.Sprint(runA("SELECT COUNT(*) FROM v").Rows); got != "[[1]]" {
		t.Fatalf("view saw rows outside the snapshot: %s", got)
	}
	runA("INSERT INTO t VALUES(3,30)")
	if got := fmt.Sprint(runA("SELECT COUNT(*) FROM v").Rows); got != "[[2]]" {
		t.Fatalf("view did not read own writes: %s", got)
	}
	runA("COMMIT")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM v").Rows); got != "[[3]]" {
		t.Fatalf("committed view rows = %s", got)
	}
}
