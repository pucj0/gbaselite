package executor

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

var recursiveCTEScript = []parityStep{
	{Query: "CREATE DATABASE rc", SkipAffect: true},
	{Query: "USE rc", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT PRIMARY KEY,parent_id INT,label VARCHAR(10))", SkipAffect: true},
	{Query: "INSERT INTO t VALUES(1,NULL,'root'),(2,1,'a'),(3,1,'b'),(4,2,'c')"},
	// Seed only, iteration, accumulation, aggregate and derived composition.
	{Query: "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n WHERE x<10) SELECT x FROM n ORDER BY x", Rows: true},
	{Query: "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n WHERE x<3) SELECT COUNT(*),SUM(x) FROM n", Rows: true},
	{Query: "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n WHERE 1=0) SELECT x FROM n", Rows: true},
	{Query: "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n WHERE x<3) SELECT y.x FROM n y WHERE y.x>1 ORDER BY y.x", Rows: true},
	{Query: "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n WHERE x<3) SELECT COUNT(*) FROM (SELECT x FROM n) d", Rows: true},
	{Query: "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n WHERE x<5) SELECT m.x FROM n m JOIN t ON t.id=1 ORDER BY m.x", Rows: true},
	// Recursive JOIN walking a parent/child graph.
	{Query: "WITH RECURSIVE n AS (SELECT id,label FROM t WHERE parent_id IS NULL UNION ALL SELECT c.id,c.label FROM t c JOIN n p ON c.parent_id=p.id) SELECT id,label FROM n ORDER BY id", Rows: true},
	// Failure paths: the depth limit and a column-count mismatch must match legacy.
	{Query: "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n) SELECT COUNT(*) FROM n", Fail: true},
	{Query: "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x,x+1 FROM n WHERE x<3) SELECT x FROM n", Fail: true},
}

func TestMVCCRecursiveCTEMatchesLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, recursiveCTEScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, recursiveCTEScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range recursiveCTEScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCRecursiveCTEStateDoesNotLeak(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY)")
	run("INSERT INTO t VALUES(1)")
	if got := fmt.Sprint(run("WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n WHERE x<3) SELECT x FROM n ORDER BY x").Rows); got != "[[1] [2] [3]]" {
		t.Fatalf("recursive rows = %s", got)
	}
	// The recursive relation is statement-local: the next statement cannot see it.
	if _, err := e.Execute(&Session{CurrentDatabase: "test"}, "SELECT x FROM n"); err == nil {
		t.Fatal("recursive CTE leaked past its statement")
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[1]]" {
		t.Fatalf("table state changed: %s", got)
	}
}

func TestMVCCRecursiveCTEDepthLimitIsLegacyValue(t *testing.T) {
	e, s, _ := rangeTestEngine(t)
	if _, err := e.Execute(s, "WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n) SELECT COUNT(*) FROM n"); err == nil || !strings.Contains(err.Error(), "exceeded 1000 iterations") {
		t.Fatalf("depth limit error = %v", err)
	}
}
