package executor

import (
	"fmt"
	"reflect"
	"testing"
)

var fkActionsScript = []parityStep{
	{Query: "CREATE DATABASE fk", SkipAffect: true},
	{Query: "USE fk", SkipAffect: true},
	{Query: "CREATE TABLE p(id INT PRIMARY KEY,label VARCHAR(10))", SkipAffect: true},
	{Query: "CREATE TABLE c(id INT PRIMARY KEY,pid INT,note VARCHAR(10),CONSTRAINT fk_c FOREIGN KEY(pid) REFERENCES p(id) ON DELETE CASCADE ON UPDATE CASCADE)", SkipAffect: true},
	{Query: "CREATE TABLE g(id INT PRIMARY KEY,cid INT,CONSTRAINT fk_g FOREIGN KEY(cid) REFERENCES c(id) ON DELETE CASCADE)", SkipAffect: true},
	{Query: "CREATE TABLE n(id INT PRIMARY KEY,pid INT,CONSTRAINT fk_n FOREIGN KEY(pid) REFERENCES p(id) ON DELETE SET NULL ON UPDATE SET NULL)", SkipAffect: true},
	{Query: "INSERT INTO p VALUES(1,'one'),(2,'two')"},
	{Query: "INSERT INTO c VALUES(10,1,'a'),(11,1,'b'),(20,2,'c')"},
	{Query: "INSERT INTO g VALUES(100,10),(200,20)"},
	{Query: "INSERT INTO n VALUES(1000,1)"},
	// UPDATE CASCADE and UPDATE SET NULL follow the referenced key change.
	{Query: "UPDATE p SET id=11 WHERE id=1"},
	{Query: "SELECT id,pid FROM c ORDER BY id", Rows: true},
	{Query: "SELECT id,pid FROM n ORDER BY id", Rows: true},
	{Query: "SELECT COUNT(*) FROM g", Rows: true},
	// DELETE CASCADE walks parent -> child -> grandchild.
	{Query: "DELETE FROM p WHERE id=11"},
	{Query: "SELECT COUNT(*) FROM p", Rows: true},
	{Query: "SELECT COUNT(*) FROM c", Rows: true},
	{Query: "SELECT COUNT(*) FROM g", Rows: true},
	{Query: "SELECT id,pid FROM n ORDER BY id", Rows: true},
	// Self-referencing cascade.
	{Query: "CREATE TABLE tree(id INT PRIMARY KEY,parent_id INT,CONSTRAINT fk_tree FOREIGN KEY(parent_id) REFERENCES tree(id) ON DELETE CASCADE)", SkipAffect: true},
	{Query: "INSERT INTO tree VALUES(1,NULL),(2,1),(3,2)"},
	{Query: "DELETE FROM tree WHERE id=1"},
	{Query: "SELECT COUNT(*) FROM tree", Rows: true},
	// A downstream CHECK failure rolls the whole statement back.
	{Query: "CREATE TABLE keep(id INT PRIMARY KEY,pid INT,CHECK(pid IS NOT NULL),CONSTRAINT fk_keep FOREIGN KEY(pid) REFERENCES p(id) ON DELETE SET NULL)", SkipAffect: true},
	{Query: "INSERT INTO p VALUES(3,'three')"},
	{Query: "INSERT INTO keep VALUES(1,3)"},
	{Query: "DELETE FROM p WHERE id=3", Fail: true},
	{Query: "SELECT COUNT(*) FROM p", Rows: true},
	{Query: "SELECT COUNT(*) FROM keep", Rows: true},
}

func TestMVCCForeignKeyActionsMatchLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, fkActionsScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, fkActionsScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range fkActionsScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCForeignKeyActionsComposeWithJoinsAndReplace(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE p(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE c(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES p(id) ON DELETE CASCADE)")
	run("CREATE TABLE d(id INT PRIMARY KEY,note VARCHAR(10))")
	run("INSERT INTO p VALUES(1,10),(2,20)")
	run("INSERT INTO c VALUES(10,1),(20,2)")
	run("INSERT INTO d VALUES(1,'x'),(2,'y')")
	// UPDATE JOIN over the parent cascades into the child.
	updated, err := e.Execute(s, "UPDATE p JOIN d ON d.id=p.id SET p.v=p.v+1")
	if err != nil || updated.AffectedRows != 2 {
		t.Fatalf("update join = %#v, %v", updated, err)
	}
	// Multi-table DELETE removes parent and child together.
	deleted, err := e.Execute(s, "DELETE p,c FROM p JOIN c ON c.pid=p.id WHERE p.id=1")
	if err != nil || deleted.AffectedRows != 2 {
		t.Fatalf("multi delete = %#v, %v", deleted, err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM c").Rows); got != "[[1]]" {
		t.Fatalf("children after multi delete = %s", got)
	}
	// REPLACE of a referenced parent cascades its children away like legacy.
	if _, err := e.Execute(s, "REPLACE INTO p VALUES(2,99)"); err != nil {
		t.Fatalf("replace parent: %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM c").Rows); got != "[[0]]" {
		t.Fatalf("children after replace = %s", got)
	}
}

func TestMVCCForeignKeySelfReferenceAndSetNullValidation(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE tree(id INT PRIMARY KEY,parent_id INT,CONSTRAINT fk FOREIGN KEY(parent_id) REFERENCES tree(id) ON DELETE CASCADE)")
	run("INSERT INTO tree VALUES(1,NULL),(2,1)")
	run("DELETE FROM tree WHERE id=1")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM tree").Rows); got != "[[0]]" {
		t.Fatalf("self cascade = %s", got)
	}
	// SET NULL requires a nullable child column.
	if _, err := e.Execute(s, "CREATE TABLE bad(id INT PRIMARY KEY,pid INT NOT NULL,CONSTRAINT fk2 FOREIGN KEY(pid) REFERENCES tree(id) ON DELETE SET NULL)"); err == nil {
		t.Fatal("SET NULL on NOT NULL column accepted")
	}
}
