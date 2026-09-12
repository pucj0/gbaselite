package executor

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gbaselite/storage"
)

// deleteParityStep is one scripted statement in a legacy/MVCC multi-table DELETE
// parity run. Fail marks statements that must error on both engines; error text
// differs between the runtimes, so only the failure itself is compared.
type deleteParityStep struct {
	Query      string
	Rows       bool
	Fail       bool
	SkipAffect bool
}

type deleteParityOutcome struct {
	Failed   bool
	Affected uint64
	Rows     string
}

func runDeleteParity(t *testing.T, execute func(string) (*Result, error), steps []deleteParityStep) []deleteParityOutcome {
	t.Helper()
	outcomes := make([]deleteParityOutcome, 0, len(steps))
	for _, step := range steps {
		result, err := execute(step.Query)
		if err != nil {
			if !step.Fail {
				t.Fatalf("%s: %v", step.Query, err)
			}
			outcomes = append(outcomes, deleteParityOutcome{Failed: true})
			continue
		}
		if step.Fail {
			t.Fatalf("%s: expected an error", step.Query)
		}
		outcome := deleteParityOutcome{}
		if result != nil {
			if !step.SkipAffect {
				outcome.Affected = result.AffectedRows
			}
			if step.Rows {
				outcome.Rows = fmt.Sprint(result.Rows)
			}
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

var multiDeleteScript = []deleteParityStep{
	{Query: "CREATE DATABASE md", SkipAffect: true},
	{Query: "USE md", SkipAffect: true},
	{Query: "CREATE TABLE a(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE b(id INT PRIMARY KEY,aid INT)", SkipAffect: true},
	{Query: "INSERT INTO a VALUES(1,10),(2,20),(3,30)"},
	{Query: "INSERT INTO b VALUES(1,1),(2,1),(3,2)"},
	{Query: "CREATE TABLE parents(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "CREATE TABLE children(id INT PRIMARY KEY,parent_id INT,CONSTRAINT fk FOREIGN KEY(parent_id) REFERENCES parents(id))", SkipAffect: true},
	{Query: "INSERT INTO parents VALUES(1),(2)"},
	{Query: "INSERT INTO children VALUES(100,1),(101,1),(200,2)"},

	// One target matched by two join rows is deleted once.
	{Query: "DELETE a FROM a JOIN b ON a.id=b.aid WHERE a.id=1"},
	{Query: "SELECT id FROM a ORDER BY id", Rows: true},
	// RESTRICT still applies when only the parent is targeted; nothing is deleted.
	{Query: "DELETE p FROM parents p JOIN children c ON c.parent_id=p.id WHERE p.id=1", Fail: true},
	{Query: "SELECT COUNT(*) FROM children", Rows: true},
	// Both targets, one of them matched repeatedly.
	{Query: "DELETE a,b FROM a JOIN b ON a.id=b.aid WHERE a.id=2"},
	{Query: "SELECT COUNT(*) FROM a", Rows: true},
	{Query: "SELECT COUNT(*) FROM b", Rows: true},
	// LEFT JOIN: a target on the null-extended side is skipped, not deleted.
	{Query: "DELETE c FROM parents p LEFT JOIN children c ON c.parent_id=p.id"},
	{Query: "SELECT COUNT(*) FROM children", Rows: true},
	// Parent and child deleted in the same statement (child first for RESTRICT).
	{Query: "DELETE p,c FROM parents p JOIN children c ON c.parent_id=p.id WHERE p.id=1"},
	{Query: "SELECT COUNT(*) FROM parents", Rows: true},
	{Query: "SELECT COUNT(*) FROM children", Rows: true},
	// USING form with the targets written in reverse order.
	{Query: "DELETE FROM children,parents USING parents p JOIN children c ON c.parent_id=p.id WHERE p.id=2"},
	{Query: "SELECT COUNT(*) FROM parents", Rows: true},
	{Query: "SELECT COUNT(*) FROM children", Rows: true},

	// Failures must leave the statement atomic.
	{Query: "DELETE a FROM a JOIN b ON a.id=b.aid LIMIT 1", Fail: true},
	{Query: "DELETE missing FROM a JOIN b ON a.id=b.aid", Fail: true},
	{Query: "SELECT COUNT(*) FROM a", Rows: true},
}

func TestMVCCMultiTableDeleteMatchesLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runDeleteParity(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, multiDeleteScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runDeleteParity(t, func(q string) (*Result, error) { return e.Execute(session, q) }, multiDeleteScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range multiDeleteScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCMultiTableDeleteDeduplicatesTargetRows(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE a(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE b(id INT PRIMARY KEY,aid INT)")
	run("INSERT INTO a VALUES(1,10),(2,20)")
	run("INSERT INTO b VALUES(1,1),(2,1),(3,1),(4,2)")
	deleted := run("DELETE a FROM a JOIN b ON a.id=b.aid WHERE a.id<=2")
	if deleted.AffectedRows != 2 {
		t.Fatalf("affected rows=%d, want one mutation per target row", deleted.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT id FROM a ORDER BY id").Rows); got != "[]" {
		t.Fatalf("rows=%s", got)
	}
}

func TestMVCCMultiTableDeleteForeignKeyOrder(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE parents(id INT PRIMARY KEY)")
	run("CREATE TABLE children(id INT PRIMARY KEY,parent_id INT,CONSTRAINT fk FOREIGN KEY(parent_id) REFERENCES parents(id))")
	run("INSERT INTO parents VALUES(1),(2)")
	run("INSERT INTO children VALUES(10,1),(11,1),(20,2)")
	// Both targets in one statement: the referencing table is deleted first.
	deleted, err := e.Execute(s, "DELETE p,c FROM parents p JOIN children c ON c.parent_id=p.id WHERE p.id=1")
	if err != nil || deleted.AffectedRows != 3 {
		t.Fatalf("multi-table DELETE = %#v, %v", deleted, err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM parents").Rows); got != "[[1]]" {
		t.Fatalf("parents=%s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM children").Rows); got != "[[1]]" {
		t.Fatalf("children=%s", got)
	}
	// Deleting only the parent keeps RESTRICT: children still reference it.
	if _, err := e.Execute(s, "DELETE p FROM parents p JOIN children c ON c.parent_id=p.id WHERE p.id=2"); !errors.Is(err, storage.ErrForeignKey) {
		t.Fatalf("parent-only DELETE error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM parents").Rows); got != "[[1]]" {
		t.Fatalf("failed DELETE changed parents: %s", got)
	}
}

func TestMVCCMultiTableDeleteRollsBackPartialMutations(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE parents(id INT PRIMARY KEY)")
	run("CREATE TABLE children(id INT PRIMARY KEY,parent_id INT,CONSTRAINT fk FOREIGN KEY(parent_id) REFERENCES parents(id))")
	run("CREATE TABLE grandchildren(id INT PRIMARY KEY,child_id INT,CONSTRAINT fk FOREIGN KEY(child_id) REFERENCES children(id))")
	run("INSERT INTO parents VALUES(1)")
	run("INSERT INTO children VALUES(100,1),(101,1)")
	run("INSERT INTO grandchildren VALUES(1,101)")
	// child 100 deletes cleanly, child 101 is still referenced: the whole
	// statement must roll back instead of keeping the first deletion.
	if _, err := e.Execute(s, "DELETE p,c FROM parents p JOIN children c ON c.parent_id=p.id"); !errors.Is(err, storage.ErrForeignKey) {
		t.Fatalf("grandchild RESTRICT error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM parents").Rows); got != "[[1]]" {
		t.Fatalf("failed DELETE changed parents: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM children").Rows); got != "[[2]]" {
		t.Fatalf("failed DELETE kept partial child deletions: %s", got)
	}
}

func TestMVCCMultiTableDeleteRejectsUnsupportedTargets(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE heap(id INT,v INT)")
	run("CREATE TABLE b(id INT PRIMARY KEY,aid INT)")
	run("INSERT INTO heap VALUES(1,10)")
	run("INSERT INTO b VALUES(1,1)")
	for _, query := range []string{
		"DELETE heap FROM heap JOIN b ON heap.id=b.aid",
		"DELETE missing FROM heap JOIN b ON heap.id=b.aid",
		"DELETE heap FROM heap JOIN b ON heap.id=b.aid LIMIT 1",
	} {
		if _, err := e.Execute(s, query); err == nil {
			t.Errorf("accepted unsupported multi-table DELETE: %s", query)
		}
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM heap").Rows); got != "[[1]]" {
		t.Fatalf("rejected DELETE changed rows: %s", got)
	}
}

func TestMVCCMultiTableDeleteLeftJoinSkipsNullExtendedTargets(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE parents(id INT PRIMARY KEY)")
	run("CREATE TABLE children(id INT PRIMARY KEY,parent_id INT)")
	run("INSERT INTO parents VALUES(1),(2)")
	run("INSERT INTO children VALUES(10,1)")
	deleted := run("DELETE c FROM parents p LEFT JOIN children c ON c.parent_id=p.id")
	if deleted.AffectedRows != 1 {
		t.Fatalf("affected rows=%d, want only the matched child", deleted.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM children").Rows); got != "[[0]]" {
		t.Fatalf("children=%s", got)
	}
}
