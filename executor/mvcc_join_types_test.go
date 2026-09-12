package executor

import (
	"fmt"
	"reflect"
	"testing"
)

var rightCrossScript = []parityStep{
	{Query: "CREATE DATABASE rj", SkipAffect: true},
	{Query: "USE rj", SkipAffect: true},
	{Query: "CREATE TABLE a(id INT PRIMARY KEY,label VARCHAR(10))", SkipAffect: true},
	{Query: "CREATE TABLE b(id INT PRIMARY KEY,aid INT,note VARCHAR(10))", SkipAffect: true},
	{Query: "CREATE TABLE empty_t(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "INSERT INTO a VALUES(1,'one'),(3,'three')"},
	{Query: "INSERT INTO b VALUES(10,1,'x'),(11,2,'y'),(12,3,'z')"},

	// RIGHT JOIN null-extends the left side and keeps unmatched right rows.
	{Query: "SELECT a.id,a.label,b.id,b.note FROM a RIGHT JOIN b ON a.id=b.aid ORDER BY b.id", Rows: true},
	{Query: "SELECT a.id,b.id FROM a RIGHT JOIN b ON a.id=b.aid WHERE a.id IS NULL ORDER BY b.id", Rows: true},
	{Query: "SELECT COUNT(*),SUM(b.id) FROM a RIGHT JOIN b ON a.id=b.aid", Rows: true},
	{Query: "SELECT a.id,b.id FROM a RIGHT JOIN b ON a.id=b.aid AND b.note='x' ORDER BY b.id", Rows: true},
	{Query: "SELECT a.id,b.id FROM a LEFT JOIN b ON a.id=b.aid ORDER BY a.id,b.id", Rows: true},
	// CROSS JOIN and chained joins.
	{Query: "SELECT a.id,b.id FROM a CROSS JOIN b ORDER BY a.id,b.id", Rows: true},
	{Query: "SELECT COUNT(*) FROM a CROSS JOIN b", Rows: true},
	{Query: "SELECT a.id,b.id FROM a CROSS JOIN b WHERE b.note='y' ORDER BY a.id", Rows: true},
	{Query: "SELECT a.id,b.id FROM a CROSS JOIN b ORDER BY a.id,b.id LIMIT 2", Rows: true},
	{Query: "SELECT a.id,b.id FROM a RIGHT JOIN b ON a.id=b.aid JOIN empty_t e ON e.id=a.id ORDER BY b.id", Rows: true},
	// Derived relations compose with RIGHT JOIN.
	{Query: "SELECT x.id,b.id FROM (SELECT id FROM a) x RIGHT JOIN b ON x.id=b.aid ORDER BY b.id", Rows: true},
	// DML composition keeps the target driving the join.
	{Query: "UPDATE a RIGHT JOIN b ON a.id=b.aid SET a.label='hit'"},
	{Query: "SELECT id,label FROM a ORDER BY id", Rows: true},
	{Query: "UPDATE a CROSS JOIN b SET a.label='all'"},
	{Query: "SELECT id,label FROM a ORDER BY id", Rows: true},
}

func TestMVCCRightAndCrossJoinsMatchLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, rightCrossScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, rightCrossScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range rightCrossScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCRightJoinNullMetadataAndEmptySides(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE a(id INT PRIMARY KEY,label VARCHAR(10))")
	run("CREATE TABLE b(id INT PRIMARY KEY,aid INT)")
	run("INSERT INTO a VALUES(1,'one')")
	run("INSERT INTO b VALUES(10,1),(11,2)")
	// The left columns are null-extended by RIGHT JOIN.
	rows := run("SELECT a.id FROM a RIGHT JOIN b ON a.id=b.aid ORDER BY b.id")
	if got := fmt.Sprint(rows.Rows); got != "[[1] [<nil>]]" {
		t.Fatalf("right join rows = %s", got)
	}
	run("CREATE TABLE c(id INT PRIMARY KEY)")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM c CROSS JOIN a").Rows); got != "[[0]]" {
		t.Fatalf("empty cross join = %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM a RIGHT JOIN c ON a.id=c.id").Rows); got != "[[0]]" {
		t.Fatalf("empty right join = %s", got)
	}
}
