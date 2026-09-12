package executor

import (
	"fmt"
	"reflect"
	"testing"
)

// subqueryScript is the shared legacy/MVCC fixture for scalar, IN and EXISTS
// subqueries, including correlated subqueries and subqueries inside writes.
var subqueryScript = []parityStep{
	{Query: "CREATE DATABASE sq", SkipAffect: true},
	{Query: "USE sq", SkipAffect: true},
	{Query: "CREATE TABLE users(id INT PRIMARY KEY,balance INT,snapshot INT)", SkipAffect: true},
	{Query: "CREATE TABLE sessions(id INT PRIMARY KEY,user_id INT,revoked_at DATETIME)", SkipAffect: true},
	{Query: "CREATE TABLE inner_t(id INT PRIMARY KEY,marker INT)", SkipAffect: true},
	{Query: "CREATE TABLE outer_t(id INT PRIMARY KEY,marker INT)", SkipAffect: true},
	{Query: "CREATE TABLE nullable_src(id INT PRIMARY KEY,y INT)", SkipAffect: true},
	{Query: "INSERT INTO users VALUES (1,10,0),(2,20,0),(3,30,0)"},
	{Query: "INSERT INTO sessions VALUES (100,2,NULL)"},
	{Query: "INSERT INTO inner_t VALUES (1,900)"},
	{Query: "INSERT INTO outer_t VALUES (1,100),(2,200)"},
	{Query: "INSERT INTO nullable_src VALUES (1,2),(2,NULL)"},

	// Uncorrelated scalar, EXISTS and IN subqueries.
	{Query: "SELECT id,(SELECT MAX(balance) FROM users) FROM users ORDER BY id", Rows: true},
	{Query: "SELECT id,(SELECT balance FROM users WHERE id=99) FROM users WHERE id=1", Rows: true},
	{Query: "SELECT id FROM users WHERE EXISTS (SELECT id FROM sessions WHERE revoked_at IS NULL) AND id=(SELECT user_id FROM sessions WHERE id=100)", Rows: true},
	{Query: "SELECT id FROM users WHERE NOT EXISTS (SELECT id FROM sessions WHERE id=999) ORDER BY id", Rows: true},
	{Query: "SELECT id FROM outer_t WHERE id IN (SELECT y FROM nullable_src) ORDER BY id", Rows: true},
	{Query: "SELECT id FROM outer_t WHERE id NOT IN (SELECT y FROM nullable_src) ORDER BY id", Rows: true},
	{Query: "SELECT id FROM outer_t WHERE id IN (SELECT y FROM nullable_src WHERE y IS NOT NULL) ORDER BY id", Rows: true},
	// Correlated scalar and EXISTS.
	{Query: "SELECT o.id,(SELECT i.marker FROM inner_t i WHERE i.id=o.id) FROM outer_t o ORDER BY o.id", Rows: true},
	{Query: "SELECT o.id FROM outer_t o WHERE EXISTS (SELECT 1 FROM inner_t i WHERE i.id=o.id) ORDER BY o.id", Rows: true},
	{Query: "SELECT o.id FROM outer_t o WHERE EXISTS (SELECT 1 FROM inner_t i WHERE o.marker=200) ORDER BY o.id", Rows: true},
	{Query: "SELECT u.id FROM users u WHERE EXISTS (SELECT s.id FROM sessions s WHERE s.user_id=u.id AND s.revoked_at IS NULL) ORDER BY u.id", Rows: true},
	{Query: "SELECT u.id FROM users u WHERE u.id IN (SELECT s.user_id FROM sessions s WHERE s.user_id=u.id) ORDER BY u.id", Rows: true},
	// Subqueries in writes.
	{Query: "UPDATE users SET snapshot=(SELECT MAX(balance) FROM users)"},
	{Query: "SELECT id,snapshot FROM users ORDER BY id", Rows: true},
	{Query: "UPDATE users SET snapshot=(SELECT balance FROM users WHERE id=1) WHERE id=2"},
	{Query: "SELECT id,snapshot FROM users ORDER BY id", Rows: true},
	{Query: "UPDATE users AS u SET u.snapshot=(SELECT s.id FROM sessions s WHERE s.user_id=u.id) WHERE u.id<=2"},
	{Query: "SELECT id,snapshot FROM users WHERE id<=2 ORDER BY id", Rows: true},
	{Query: "DELETE FROM users WHERE id IN (SELECT s.user_id FROM sessions s WHERE s.revoked_at IS NULL)"},
	{Query: "SELECT id FROM users ORDER BY id", Rows: true},
	{Query: "INSERT INTO outer_t VALUES (3,(SELECT MAX(marker) FROM outer_t))"},
	{Query: "INSERT INTO outer_t SET id=4,marker=(SELECT MAX(marker) FROM outer_t)"},
	{Query: "SELECT id,marker FROM outer_t ORDER BY id", Rows: true},
	{Query: "UPDATE outer_t SET marker=marker+1 WHERE id IN (SELECT id FROM inner_t)"},
	{Query: "SELECT id,marker FROM outer_t ORDER BY id", Rows: true},
	// A scalar subquery with more than one row fails on both engines.
	{Query: "INSERT INTO sessions VALUES (101,1,NULL)"},
	{Query: "SELECT id,(SELECT user_id FROM sessions) FROM users", Fail: true},
}

func TestMVCCSubqueriesMatchLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, subqueryScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, subqueryScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range subqueryScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCScalarSubqueryAtomicity(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE target(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE lookup(lid INT PRIMARY KEY,tid INT,amount INT)")
	run("INSERT INTO target VALUES(1,0),(2,0),(3,0)")
	run("INSERT INTO lookup VALUES(1,1,7),(2,2,8),(3,3,9),(4,3,10)")
	// Target row 3 matches two lookup rows: the statement must fail without
	// leaving the already-computed rows 1 and 2 updated.
	if _, err := e.Execute(s, "UPDATE target SET v=(SELECT l.amount FROM lookup l WHERE l.tid=target.id) WHERE id<=3"); err == nil {
		t.Fatal("multi-row scalar subquery accepted")
	}
	if got := fmt.Sprint(run("SELECT id,v FROM target ORDER BY id").Rows); got != "[[1 0] [2 0] [3 0]]" {
		t.Fatalf("scalar subquery partial mutation: %s", got)
	}
	if _, err := e.Execute(s, "UPDATE target SET v=(SELECT l.amount FROM lookup l WHERE l.tid=target.id) WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM target WHERE id=1").Rows); got != "[[1 7]]" {
		t.Fatalf("single-row scalar subquery = %s", got)
	}
}

func TestMVCCScalarSubqueryRowCounts(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,10)")
	if got := fmt.Sprint(run("SELECT id,(SELECT x.v FROM t x WHERE x.id=99) FROM t WHERE id=1").Rows); got != "[[1 <nil>]]" {
		t.Fatalf("empty scalar subquery = %s", got)
	}
	if got := fmt.Sprint(run("SELECT id,(SELECT x.v FROM t x WHERE x.id=1) FROM t WHERE id=1").Rows); got != "[[1 10]]" {
		t.Fatalf("single-row scalar subquery = %s", got)
	}
	run("INSERT INTO t VALUES(2,20)")
	if _, err := e.Execute(s, "SELECT id,(SELECT x.v FROM t x) FROM t WHERE id=1"); err == nil {
		t.Fatal("multi-row scalar subquery accepted")
	}
}

func TestMVCCInSubqueryNullSemantics(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE outer_t(id INT PRIMARY KEY,x INT)")
	run("CREATE TABLE nullable_src(id INT PRIMARY KEY,y INT)")
	run("INSERT INTO outer_t VALUES(1,1),(2,2),(3,3),(4,4),(5,NULL)")
	run("INSERT INTO nullable_src VALUES(1,2),(2,NULL)")
	// Expectations follow the legacy executor, including its handling of a NULL
	// outer value against an empty subquery result.
	cases := []struct{ query, want string }{
		{"SELECT id FROM outer_t WHERE x IN (SELECT y FROM nullable_src) ORDER BY id", "[[2]]"},
		{"SELECT id FROM outer_t WHERE x NOT IN (SELECT y FROM nullable_src) ORDER BY id", "[]"},
		{"SELECT id FROM outer_t WHERE x IN (SELECT y FROM nullable_src WHERE id=2) ORDER BY id", "[]"},
		{"SELECT id FROM outer_t WHERE x NOT IN (SELECT y FROM nullable_src WHERE id=2) ORDER BY id", "[]"},
		{"SELECT id FROM outer_t WHERE x IN (SELECT y FROM nullable_src WHERE id=999) ORDER BY id", "[]"},
		{"SELECT id FROM outer_t WHERE x NOT IN (SELECT y FROM nullable_src WHERE id=999) ORDER BY id", "[[1] [2] [3] [4]]"},
		{"SELECT id FROM outer_t WHERE x IN (SELECT y FROM nullable_src WHERE y IS NOT NULL) ORDER BY id", "[[2]]"},
		{"SELECT id FROM outer_t WHERE x NOT IN (SELECT y FROM nullable_src WHERE y IS NOT NULL) ORDER BY id", "[[1] [3] [4]]"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(run(c.query).Rows); got != c.want {
			t.Errorf("%s => %s, want %s", c.query, got, c.want)
		}
	}
}

func TestMVCCCorrelatedSubqueryScopeResolution(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE outer_t(id INT PRIMARY KEY,marker INT)")
	run("CREATE TABLE inner_t(id INT PRIMARY KEY,marker INT)")
	run("CREATE TABLE leaf_t(id INT PRIMARY KEY,parent_id INT)")
	run("INSERT INTO outer_t VALUES(1,100),(2,200)")
	run("INSERT INTO inner_t VALUES(1,900),(3,300)")
	run("INSERT INTO leaf_t VALUES(10,1),(11,2)")
	// An unqualified inner column resolves to the inner scope.
	rows := run("SELECT o.id FROM outer_t o WHERE EXISTS (SELECT 1 FROM inner_t i WHERE id=o.id) ORDER BY o.id")
	if got := fmt.Sprint(rows.Rows); got != "[[1]]" {
		t.Fatalf("inner scope priority = %s", got)
	}
	// A qualifier that the inner scope does not publish binds to the outer row.
	rows = run("SELECT o.id FROM outer_t o WHERE EXISTS (SELECT 1 FROM inner_t i WHERE o.marker=200) ORDER BY o.id")
	if got := fmt.Sprint(rows.Rows); got != "[[2]]" {
		t.Fatalf("outer qualifier binding = %s", got)
	}
	// Two-level correlation: the innermost query references both outer scopes.
	rows = run("SELECT o.id FROM outer_t o WHERE EXISTS (SELECT 1 FROM inner_t i WHERE i.id=o.id AND EXISTS (SELECT 1 FROM leaf_t l WHERE l.parent_id=o.id AND l.id=i.id+9)) ORDER BY o.id")
	if got := fmt.Sprint(rows.Rows); got != "[[1]]" {
		t.Fatalf("nested correlation = %s", got)
	}
	// An unknown column inside a correlated subquery still fails.
	if _, err := e.Execute(s, "SELECT o.id FROM outer_t o WHERE EXISTS (SELECT 1 FROM inner_t i WHERE i.missing=o.id)"); err == nil {
		t.Fatal("unknown correlated column accepted")
	}
}

func TestMVCCSubqueryUsesStatementSnapshot(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,10),(2,20)")
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
	runA("SELECT COUNT(*) FROM src")
	// Another session commits after this transaction fixed its snapshot.
	run("INSERT INTO src VALUES(3,30)")
	runA("INSERT INTO dst SELECT id,v FROM src WHERE id IN (SELECT s.id FROM src s)")
	runA("INSERT INTO dst VALUES(9,(SELECT MAX(s.v) FROM src s))")
	if got := fmt.Sprint(runA("SELECT id,v FROM dst ORDER BY id").Rows); got != "[[1 10] [2 20] [9 20]]" {
		t.Fatalf("subquery saw rows outside the snapshot: %s", got)
	}
	runA("COMMIT")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[3]]" {
		t.Fatalf("committed rows = %s", got)
	}
}
