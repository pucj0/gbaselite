package executor

import (
	"fmt"
	"reflect"
	"testing"
)

var noPKMultiDeleteScript = []parityStep{
	{Query: "CREATE DATABASE np", SkipAffect: true},
	{Query: "USE np", SkipAffect: true},
	{Query: "CREATE TABLE a(v INT)", SkipAffect: true},
	{Query: "CREATE TABLE b(v INT)", SkipAffect: true},
	{Query: "INSERT INTO a VALUES(1),(2),(2),(3)"},
	{Query: "INSERT INTO b VALUES(2),(3)"},
	// Identical rows are distinct storage rows: each matched row is deleted once.
	{Query: "DELETE a FROM a JOIN b ON a.v=b.v"},
	{Query: "SELECT v FROM a ORDER BY v", Rows: true},
	{Query: "DELETE a FROM a LEFT JOIN b ON a.v=b.v WHERE b.v IS NULL"},
	{Query: "SELECT v FROM a ORDER BY v", Rows: true},
	{Query: "CREATE TABLE c(v INT)", SkipAffect: true},
	{Query: "CREATE TABLE d(v INT)", SkipAffect: true},
	{Query: "INSERT INTO c VALUES(1),(2)"},
	{Query: "INSERT INTO d VALUES(1),(2)"},
	{Query: "DELETE c FROM c CROSS JOIN d WHERE c.v=d.v AND c.v=1"},
	{Query: "SELECT v FROM c ORDER BY v", Rows: true},
	{Query: "CREATE TABLE e(v INT)", SkipAffect: true},
	{Query: "CREATE TABLE f(v INT)", SkipAffect: true},
	{Query: "INSERT INTO e VALUES(1),(2),(3)"},
	{Query: "INSERT INTO f VALUES(2)"},
	{Query: "DELETE e FROM e RIGHT JOIN f ON e.v=f.v"},
	{Query: "SELECT v FROM e ORDER BY v", Rows: true},
}

func TestMVCCMultiTableDeleteWithoutPrimaryKeyMatchesLegacy(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, noPKMultiDeleteScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, noPKMultiDeleteScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range noPKMultiDeleteScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCMultiTableDeleteWithoutPrimaryKeyAtomicRollback(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE a(v INT)")
	run("CREATE TABLE b(v INT,PRIMARY KEY(v))")
	run("INSERT INTO a VALUES(1),(2),(2)")
	run("INSERT INTO b VALUES(1),(2)")
	// A failing statement must not remove any of the matched duplicate rows.
	if _, err := e.Execute(s, "DELETE a FROM a JOIN b ON a.v=b.v WHERE b.missing=1"); err == nil {
		t.Fatal("unknown column accepted")
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM a").Rows); got != "[[3]]" {
		t.Fatalf("failed delete changed rows: %s", got)
	}
}
