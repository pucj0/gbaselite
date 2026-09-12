package executor

import (
	"fmt"
	"reflect"
	"testing"
)

var derivedCTEScript = []parityStep{
	{Query: "CREATE DATABASE derive", SkipAffect: true},
	{Query: "USE derive", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE a(id INT PRIMARY KEY,label VARCHAR(10))", SkipAffect: true},
	{Query: "CREATE TABLE b(id INT PRIMARY KEY,aid INT,note VARCHAR(10))", SkipAffect: true},
	{Query: "INSERT INTO t VALUES(1,10),(2,20),(3,30)"},
	{Query: "INSERT INTO a VALUES(1,'one'),(2,'two')"},
	{Query: "INSERT INTO b VALUES(10,1,'x'),(11,1,'y'),(12,2,'z')"},

	// Derived tables in FROM and in JOIN, with alias resolution.
	{Query: "SELECT * FROM (SELECT id,v FROM t) x ORDER BY x.id", Rows: true},
	{Query: "SELECT x.id FROM (SELECT id FROM t WHERE id>=2) x ORDER BY x.id", Rows: true},
	{Query: "SELECT * FROM a JOIN (SELECT aid,note FROM b) d ON a.id=d.aid ORDER BY a.id,d.note", Rows: true},
	{Query: "SELECT id FROM (SELECT id FROM t) x WHERE id=2", Rows: true},
	{Query: "SELECT COUNT(*) FROM (SELECT id FROM t WHERE id>99) x", Rows: true},
	// Aggregate, UNION, ORDER/LIMIT sources.
	{Query: "SELECT x.n,COUNT(*) FROM (SELECT v AS n FROM t GROUP BY v) x GROUP BY x.n ORDER BY x.n", Rows: true},
	{Query: "SELECT x.id FROM (SELECT id FROM t WHERE id=1 UNION ALL SELECT id FROM a) x ORDER BY x.id", Rows: true},
	{Query: "SELECT * FROM (SELECT id FROM t ORDER BY id DESC LIMIT 2) x ORDER BY x.id", Rows: true},
	// Derived relations nest inside subqueries and feed INSERT SELECT.
	{Query: "SELECT COUNT(*) FROM (SELECT id FROM t) x WHERE x.id IN (SELECT y.id FROM (SELECT id FROM a) y)", Rows: true},
	{Query: "CREATE TABLE dst(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "INSERT INTO dst SELECT x.id,x.v FROM (SELECT id,v FROM t WHERE id<=2) x"},
	{Query: "SELECT id,v FROM dst ORDER BY id", Rows: true},

	// Non-recursive CTEs, including column lists, shadowing and multiple CTEs.
	{Query: "WITH x AS (SELECT id,v FROM t) SELECT * FROM x ORDER BY id", Rows: true},
	{Query: "WITH x AS (SELECT id FROM t), y AS (SELECT id FROM a) SELECT x.id,y.id FROM x JOIN y ON x.id=y.id ORDER BY x.id", Rows: true},
	{Query: "WITH x AS (SELECT id FROM t WHERE id>=2) SELECT a.label,x.id FROM a JOIN x ON a.id=x.id ORDER BY x.id", Rows: true},
	{Query: "WITH x(p,q) AS (SELECT id,v FROM t) SELECT p,q FROM x ORDER BY p", Rows: true},
	{Query: "WITH x AS (SELECT id FROM t WHERE id>99) SELECT COUNT(*) FROM x", Rows: true},
	{Query: "WITH x AS (SELECT v AS n,COUNT(*) AS c FROM t GROUP BY v) SELECT n,c FROM x ORDER BY n", Rows: true},
	{Query: "WITH x AS (SELECT id FROM t WHERE id=1 UNION ALL SELECT id FROM a) SELECT id FROM x ORDER BY id", Rows: true},
	{Query: "WITH x AS (SELECT id FROM t) SELECT l.id,r.id FROM x l JOIN x r ON l.id=r.id WHERE l.id=2", Rows: true},
	{Query: "WITH t AS (SELECT 99 AS id) SELECT id FROM t", Rows: true},
	{Query: "WITH x AS (SELECT id FROM t) SELECT COUNT(*) FROM (SELECT id FROM x WHERE id>1) y", Rows: true},
	{Query: "WITH x AS (SELECT id FROM (SELECT id FROM t WHERE id<=2) d) SELECT id FROM x ORDER BY id", Rows: true},
}

func TestMVCCDerivedTablesAndCTEsMatchLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, derivedCTEScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, derivedCTEScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range derivedCTEScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCDerivedTableRequiresAliasAndResolvesAmbiguity(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE a(id INT PRIMARY KEY)")
	run("INSERT INTO t VALUES(1,10)")
	run("INSERT INTO a VALUES(1)")
	if _, err := e.Execute(s, "SELECT * FROM (SELECT id FROM t)"); err == nil {
		t.Fatal("derived table without an alias accepted")
	}
	// Two derived inputs publishing the same unqualified column are ambiguous.
	if _, err := e.Execute(s, "SELECT id FROM (SELECT id FROM t) x JOIN (SELECT id FROM a) y ON x.id=y.id"); err == nil {
		t.Fatal("ambiguous derived column accepted")
	}
	if got := fmt.Sprint(run("SELECT x.id,y.id FROM (SELECT id FROM t) x JOIN (SELECT id FROM a) y ON x.id=y.id").Rows); got != "[[1 1]]" {
		t.Fatalf("qualified derived resolution = %s", got)
	}
}

func TestMVCCDerivedTableBudgetFailure(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,payload VARCHAR(400))")
	for i := 0; i < 40; i++ {
		run(fmt.Sprintf("INSERT INTO t VALUES(%d,REPEAT('x',300))", i))
	}
	e.QueryOptions = QueryOptions{ResultMemoryBytes: 512, MaxTempBytes: 1 << 20}
	if _, err := e.Execute(&Session{CurrentDatabase: "test"}, "SELECT COUNT(*) FROM (SELECT id,payload FROM t) x"); err == nil {
		t.Fatal("derived table ignored the result memory budget")
	}
	// The budget failure must not corrupt later statements.
	run("SELECT COUNT(*) FROM t")
}

func TestMVCCCTEScopeIsStatementLocal(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY)")
	run("INSERT INTO t VALUES(1)")
	if got := fmt.Sprint(run("WITH x AS (SELECT id FROM t) SELECT id FROM x").Rows); got != "[[1]]" {
		t.Fatalf("CTE body = %s", got)
	}
	// The CTE must not leak into the next statement.
	if _, err := e.Execute(&Session{CurrentDatabase: "test"}, "SELECT id FROM x"); err == nil {
		t.Fatal("CTE leaked past its statement")
	}
}
