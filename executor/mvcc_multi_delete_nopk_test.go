package executor

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gbaselite/storage"
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

// noPKJoinedTargetDeleteScript covers joined DELETE targets that have no primary
// key. Their physical row identity comes from the storage key the join scan
// observed, so identical duplicate rows stay distinct and a row matched by
// several join combinations is deleted once.
var noPKJoinedTargetDeleteScript = []parityStep{
	{Query: "CREATE DATABASE nj", SkipAffect: true},
	{Query: "USE nj", SkipAffect: true},
	{Query: "CREATE TABLE a(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE b(v INT)", SkipAffect: true},
	{Query: "INSERT INTO a VALUES(1,10),(2,20),(3,30)"},
	{Query: "INSERT INTO b VALUES(10),(10),(30)"},
	{Query: "DELETE b FROM a JOIN b ON a.v=b.v"},
	{Query: "SELECT v FROM b ORDER BY v", Rows: true},
	// Zero match keeps every row.
	{Query: "INSERT INTO b VALUES(99)"},
	{Query: "DELETE b FROM a JOIN b ON a.v=b.v WHERE a.id=999"},
	{Query: "SELECT v FROM b ORDER BY v", Rows: true},
	// Neither side has a primary key.
	{Query: "CREATE TABLE c(v INT)", SkipAffect: true},
	{Query: "CREATE TABLE d(v INT)", SkipAffect: true},
	{Query: "INSERT INTO c VALUES(1),(2),(2)"},
	{Query: "INSERT INTO d VALUES(1),(2),(3)"},
	{Query: "DELETE c,d FROM c JOIN d ON c.v=d.v"},
	{Query: "SELECT v FROM c ORDER BY v", Rows: true},
	{Query: "SELECT v FROM d ORDER BY v", Rows: true},
	// USING form naming the joined heap table as the only target.
	{Query: "CREATE TABLE k(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE l(v INT)", SkipAffect: true},
	{Query: "INSERT INTO k VALUES(1,7),(2,8)"},
	{Query: "INSERT INTO l VALUES(7),(8)"},
	{Query: "DELETE FROM l USING k JOIN l ON k.v=l.v WHERE k.id=2"},
	{Query: "SELECT v FROM l ORDER BY v", Rows: true},
}

// noPKJoinedTargetRightJoinScript pins RIGHT JOIN semantics for a heap target:
// the joined relation keeps every right row, so a right-hand target that matches
// nothing is still a deletion while a null-extended left target is not.
var noPKJoinedTargetRightJoinScript = []parityStep{
	{Query: "CREATE DATABASE rj", SkipAffect: true},
	{Query: "USE rj", SkipAffect: true},
	{Query: "CREATE TABLE g(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE h(v INT)", SkipAffect: true},
	{Query: "INSERT INTO g VALUES(1,1),(2,2)"},
	{Query: "INSERT INTO h VALUES(2),(3),(3)"},
	{Query: "DELETE h FROM g RIGHT JOIN h ON g.v=h.v"},
	{Query: "SELECT v FROM h ORDER BY v", Rows: true},
	{Query: "INSERT INTO h VALUES(1)"},
	// Only the matched left row is deleted; the unmatched one is null-extended.
	{Query: "DELETE g FROM g RIGHT JOIN h ON g.v=h.v"},
	{Query: "SELECT id FROM g ORDER BY id", Rows: true},
}

// noPKJoinedTargetLeftJoinScript pins the mirrored case for LEFT JOIN.
var noPKJoinedTargetLeftJoinScript = []parityStep{
	{Query: "CREATE DATABASE lj", SkipAffect: true},
	{Query: "USE lj", SkipAffect: true},
	{Query: "CREATE TABLE e(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE f(v INT)", SkipAffect: true},
	{Query: "INSERT INTO e VALUES(1,1),(2,2)"},
	{Query: "INSERT INTO f VALUES(2)"},
	// The null-extended heap target contributes no deletion.
	{Query: "DELETE f FROM e LEFT JOIN f ON e.v=f.v"},
	{Query: "SELECT v FROM f ORDER BY v", Rows: true},
	// The driving target keeps its WHERE-driven subset.
	{Query: "DELETE e FROM e LEFT JOIN f ON e.v=f.v WHERE f.v IS NULL"},
	{Query: "SELECT id FROM e ORDER BY id", Rows: true},
}

func TestMVCCMultiTableDeleteJoinedTargetWithoutPrimaryKeyMatchesLegacy(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	e, session, _ := rangeTestEngine(t)
	for _, script := range [][]parityStep{noPKJoinedTargetDeleteScript, noPKJoinedTargetRightJoinScript, noPKJoinedTargetLeftJoinScript} {
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

func TestMVCCMultiTableDeleteJoinedHeapTargetAffectedRows(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE a(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE b(v INT)")
	run("INSERT INTO a VALUES(1,10),(2,20)")
	run("INSERT INTO b VALUES(10),(10),(11)")
	deleted, err := e.Execute(s, "DELETE b FROM a JOIN b ON a.v=b.v")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.AffectedRows != 2 {
		t.Fatalf("affected rows=%d, want both duplicate rows", deleted.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT v FROM b ORDER BY v").Rows); got != "[[11]]" {
		t.Fatalf("rows=%s", got)
	}
	if deleted, err = e.Execute(s, "DELETE b FROM a JOIN b ON a.v=b.v"); err != nil || deleted.AffectedRows != 0 {
		t.Fatalf("empty match = %#v, %v", deleted, err)
	}
}

func TestMVCCMultiTableDeleteJoinedHeapTargetForeignKeyRollback(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE p(id INT UNIQUE)")
	run("CREATE TABLE q(pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES p(id))")
	run("CREATE TABLE r(id INT PRIMARY KEY)")
	run("INSERT INTO p VALUES(1),(2)")
	run("INSERT INTO q VALUES(1),(2)")
	run("INSERT INTO r VALUES(1),(2)")
	// Every referenced heap row is protected, so the statement must roll back whole.
	if _, err := e.Execute(s, "DELETE p FROM r JOIN p ON r.id=p.id"); !errors.Is(err, storage.ErrForeignKey) {
		t.Fatalf("RESTRICT error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM p").Rows); got != "[[2]]" {
		t.Fatalf("failed DELETE removed rows: %s", got)
	}
	// Removing the referencing heap rows through the same statement shape works.
	deleted, err := e.Execute(s, "DELETE q FROM r JOIN q ON r.id=q.pid")
	if err != nil || deleted.AffectedRows != 2 {
		t.Fatalf("referencing heap DELETE = %#v, %v", deleted, err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM q").Rows); got != "[[0]]" {
		t.Fatalf("q=%s", got)
	}
}

func TestMVCCMultiTableDeleteJoinedHeapTargetReadYourOwnWrite(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE a(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE b(v INT)")
	run("INSERT INTO a VALUES(1,10)")
	run("INSERT INTO b VALUES(10),(11)")
	run("BEGIN")
	run("DELETE b FROM a JOIN b ON a.v=b.v")
	if got := fmt.Sprint(run("SELECT v FROM b ORDER BY v").Rows); got != "[[11]]" {
		t.Fatalf("in-transaction rows=%s", got)
	}
	run("INSERT INTO b VALUES(10)")
	if got := fmt.Sprint(run("SELECT v FROM b ORDER BY v").Rows); got != "[[10] [11]]" {
		t.Fatalf("re-inserted rows=%s", got)
	}
	run("ROLLBACK")
	if got := fmt.Sprint(run("SELECT v FROM b ORDER BY v").Rows); got != "[[10] [11]]" {
		t.Fatalf("rolled-back rows=%s", got)
	}
}

func TestMVCCMultiTableDeleteRejectsDerivedTarget(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE b(v INT)")
	run("INSERT INTO b VALUES(1)")
	run("CREATE TABLE a(id INT PRIMARY KEY)")
	run("INSERT INTO a VALUES(1)")
	// Derived relations expose no deletable physical identity.
	for _, query := range []string{
		"DELETE d FROM (SELECT 1 AS v) d JOIN a ON a.id=d.v",
		"DELETE FROM (SELECT 1 AS v) d USING d JOIN a ON a.id=d.v",
	} {
		if _, err := e.Execute(s, query); err == nil {
			t.Errorf("accepted derived DELETE target: %s", query)
		}
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM b").Rows); got != "[[1]]" {
		t.Fatalf("rejected DELETE changed rows: %s", got)
	}
}
