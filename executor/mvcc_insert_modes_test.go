package executor

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gbaselite/storage"
)

// insertModesScript is the shared legacy/MVCC fixture for INSERT SET, INSERT
// IGNORE, REPLACE and ON DUPLICATE KEY UPDATE across VALUES, SET and SELECT
// sources. Every step runs on both engines and compares affected rows,
// LastInsertID, result rows and which statements fail.
var insertModesScript = []parityStep{
	{Query: "CREATE DATABASE im", SkipAffect: true},
	{Query: "USE im", SkipAffect: true},
	{Query: "CREATE TABLE profiles(id BIGINT PRIMARY KEY AUTO_INCREMENT,phone VARCHAR(32) NOT NULL,name VARCHAR(64) NOT NULL,score INT NOT NULL DEFAULT 0,marker INT,UNIQUE KEY uq_phone(phone),CHECK(score BETWEEN 0 AND 10))", SkipAffect: true},
	{Query: "INSERT INTO profiles(phone,name,score) VALUES('100','Alice',5)"},
	{Query: "CREATE TABLE accounts(id INT PRIMARY KEY,email VARCHAR(40) UNIQUE,label VARCHAR(20))", SkipAffect: true},
	{Query: "INSERT INTO accounts VALUES(1,'a@x','one'),(2,'b@x','two')"},
	{Query: "CREATE TABLE src(id INT PRIMARY KEY,email VARCHAR(40),label VARCHAR(20))", SkipAffect: true},
	{Query: "INSERT INTO src VALUES(4,'d@x','four')"},
	{Query: "CREATE TABLE parents(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "CREATE TABLE kids(id INT PRIMARY KEY,parent_id INT,CONSTRAINT fk FOREIGN KEY(parent_id) REFERENCES parents(id))", SkipAffect: true},
	{Query: "INSERT INTO parents VALUES(1)"},
	{Query: "INSERT INTO kids VALUES(10,1)"},

	// INSERT SET: defaults for omitted columns, expressions, auto-increment.
	{Query: "INSERT INTO profiles SET phone='200',name='Bob'", InsertID: true},
	{Query: "SELECT phone,score,marker FROM profiles WHERE phone='200'", Rows: true},
	{Query: "INSERT INTO profiles SET phone='201',name='Carl',score=2+3,marker=score+1", InsertID: true},
	{Query: "SELECT phone,score,marker FROM profiles WHERE phone='201'", Rows: true},
	// ON DUPLICATE KEY UPDATE: assignment expressions and VALUES(column).
	{Query: "INSERT INTO profiles SET phone='202',name='Dan',score=1 ON DUPLICATE KEY UPDATE score=score+1", InsertID: true},
	{Query: "SELECT phone,score FROM profiles WHERE phone='202'", Rows: true},
	{Query: "INSERT INTO profiles SET phone='202',name='Dan',score=9 ON DUPLICATE KEY UPDATE score=VALUES(score)"},
	{Query: "SELECT phone,score FROM profiles WHERE phone='202'", Rows: true},
	{Query: "INSERT INTO profiles SET phone='202',name='Dup',score=99 ON DUPLICATE KEY UPDATE score=score+10", Fail: true},
	{Query: "SELECT phone,score,name FROM profiles WHERE phone='202'", Rows: true},
	// INSERT IGNORE skips duplicates but still reports other constraint errors.
	{Query: "INSERT IGNORE INTO profiles(phone,name,score) VALUES('100','dup',3),('300','New',3)", InsertID: true},
	{Query: "SELECT phone,score FROM profiles WHERE phone IN ('100','300') ORDER BY phone", Rows: true},
	{Query: "INSERT IGNORE INTO kids VALUES(30,999)", Fail: true},
	{Query: "SELECT COUNT(*) FROM kids", Rows: true},
	// REPLACE removes every conflicting row, across PK and unique keys.
	{Query: "REPLACE INTO accounts VALUES (1,'b@x','merged')"},
	{Query: "SELECT id,email,label FROM accounts ORDER BY id", Rows: true},
	{Query: "REPLACE accounts SET id=3,email='c@x',label='three'"},
	{Query: "SELECT id,email,label FROM accounts ORDER BY id", Rows: true},
	{Query: "REPLACE INTO accounts(id,email,label) SELECT id,email,label FROM src"},
	{Query: "SELECT id,email,label FROM accounts ORDER BY id", Rows: true},
	{Query: "INSERT INTO accounts(id,email,label) SELECT id,email,label FROM src ON DUPLICATE KEY UPDATE label='dup'"},
	{Query: "SELECT id,label FROM accounts ORDER BY id", Rows: true},
	{Query: "REPLACE INTO accounts SET id=5,email='c@x',label='moved'"},
	{Query: "SELECT id,email,label FROM accounts ORDER BY id", Rows: true},
	// Referenced target rows stay protected and statements stay atomic.
	{Query: "REPLACE INTO parents VALUES (1)", Fail: true},
	{Query: "SELECT COUNT(*) FROM kids", Rows: true},
	{Query: "REPLACE INTO kids VALUES (11,999)", Fail: true},
	{Query: "REPLACE INTO kids VALUES (10,1),(12,999)", Fail: true},
	{Query: "SELECT COUNT(*) FROM kids", Rows: true},
	{Query: "REPLACE INTO profiles SET phone='500',name='Frank',score=3", InsertID: true},
	{Query: "SELECT phone,score FROM profiles WHERE phone='500'", Rows: true},
}

func TestMVCCInsertModesMatchLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, insertModesScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, insertModesScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range insertModesScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCInsertSetDefaultsAndExpressions(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT AUTO_INCREMENT PRIMARY KEY,label VARCHAR(20) DEFAULT 'd',score INT,marker INT)")
	first := run("INSERT INTO t SET score=2+3,marker=score+1,label=NULL")
	if first.AffectedRows != 1 || first.LastInsertID != 1 {
		t.Fatalf("INSERT SET = affected %d last id %d", first.AffectedRows, first.LastInsertID)
	}
	if got := fmt.Sprint(run("SELECT id,label,score,marker FROM t").Rows); got != "[[1 <nil> 5 6]]" {
		t.Fatalf("INSERT SET row = %s", got)
	}
	run("INSERT INTO t SET label='x'")
	if got := fmt.Sprint(run("SELECT id,label,score,marker FROM t ORDER BY id").Rows); got != "[[1 <nil> 5 6] [2 x <nil> <nil>]]" {
		t.Fatalf("INSERT SET defaults = %s", got)
	}
}

func TestMVCCInsertIgnoreOnlySkipsDuplicates(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,score INT,CHECK(score>=0))")
	run("INSERT INTO t VALUES(1,10)")
	ignored := run("INSERT IGNORE INTO t VALUES(1,99),(2,20)")
	if ignored.AffectedRows != 1 {
		t.Fatalf("INSERT IGNORE affected=%d", ignored.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT id,score FROM t ORDER BY id").Rows); got != "[[1 10] [2 20]]" {
		t.Fatalf("INSERT IGNORE rows = %s", got)
	}
	if _, err := e.Execute(s, "INSERT IGNORE INTO t VALUES(3,-1)"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("INSERT IGNORE CHECK error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[2]]" {
		t.Fatalf("failed INSERT IGNORE changed rows: %s", got)
	}
}

func TestMVCCReplaceDeletesEveryConflictingRow(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,email VARCHAR(40) UNIQUE,label VARCHAR(20))")
	run("INSERT INTO t VALUES(1,'a@x','one'),(2,'b@x','two'),(3,'c@x','three')")
	// The candidate conflicts with id=1 (primary key) and id=2 (unique email).
	replaced := run("REPLACE INTO t VALUES(1,'b@x','merged')")
	if replaced.AffectedRows != 3 {
		t.Fatalf("REPLACE affected=%d, want two removed rows plus one insert", replaced.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT id,email,label FROM t ORDER BY id").Rows); got != "[[1 b@x merged] [3 c@x three]]" {
		t.Fatalf("REPLACE rows = %s", got)
	}
	// A unique conflict on a different primary key removes that row instead.
	moved := run("REPLACE INTO t SET id=4,email='c@x',label='moved'")
	if moved.AffectedRows != 2 {
		t.Fatalf("REPLACE unique conflict affected=%d", moved.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT id,email,label FROM t ORDER BY id").Rows); got != "[[1 b@x merged] [4 c@x moved]]" {
		t.Fatalf("REPLACE unique rows = %s", got)
	}
}

func TestMVCCReplaceRejectsReferencedRow(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE parents(id INT PRIMARY KEY,label VARCHAR(20))")
	run("CREATE TABLE kids(id INT PRIMARY KEY,parent_id INT,CONSTRAINT fk FOREIGN KEY(parent_id) REFERENCES parents(id))")
	run("INSERT INTO parents VALUES(1,'one')")
	run("INSERT INTO kids VALUES(10,1)")
	if _, err := e.Execute(s, "REPLACE INTO parents VALUES(1,'replacement')"); !errors.Is(err, storage.ErrForeignKeyReferenced) {
		t.Fatalf("REPLACE referenced row error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,label FROM parents").Rows); got != "[[1 one]]" {
		t.Fatalf("failed REPLACE changed parents: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM kids").Rows); got != "[[1]]" {
		t.Fatalf("failed REPLACE changed kids: %s", got)
	}
}

func TestMVCCReplaceStatementAtomicity(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE parents(id INT PRIMARY KEY)")
	run("CREATE TABLE kids(id INT PRIMARY KEY,parent_id INT,label VARCHAR(20),CONSTRAINT fk FOREIGN KEY(parent_id) REFERENCES parents(id))")
	run("INSERT INTO parents VALUES(1)")
	run("INSERT INTO kids VALUES(10,1,'original')")
	// The first row replaces cleanly; the second row violates the foreign key, so
	// the whole statement must roll back.
	if _, err := e.Execute(s, "REPLACE INTO kids VALUES(10,1,'changed'),(11,999,'invalid')"); !errors.Is(err, storage.ErrForeignKey) {
		t.Fatalf("REPLACE atomic failure = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,parent_id,label FROM kids").Rows); got != "[[10 1 original]]" {
		t.Fatalf("failed REPLACE left partial rows: %s", got)
	}
}

func TestMVCCOnDuplicateKeyUpdateCandidateAndLastInsertID(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT AUTO_INCREMENT PRIMARY KEY,phone VARCHAR(20) UNIQUE,score INT,note VARCHAR(20),CHECK(score BETWEEN 0 AND 10))")
	run("INSERT INTO t(phone,score,note) VALUES('100',1,'first')")
	s.LastInsertID = 77
	// VALUES(column) reads the candidate row; plain names read the stored row and
	// see earlier assignments in the same list.
	updated := run("INSERT INTO t SET phone='100',score=4,note='second' ON DUPLICATE KEY UPDATE score=VALUES(score),note=CONCAT(note,'-',score)")
	if updated.AffectedRows != 1 || updated.LastInsertID != 0 {
		t.Fatalf("duplicate update = affected %d last id %d", updated.AffectedRows, updated.LastInsertID)
	}
	if got := fmt.Sprint(run("SELECT phone,score,note FROM t").Rows); got != "[[100 4 first-4]]" {
		t.Fatalf("duplicate update rows = %s", got)
	}
	if s.LastInsertID != 77 {
		t.Fatalf("duplicate update published LastInsertID=%d", s.LastInsertID)
	}
	if _, err := e.Execute(s, "INSERT INTO t SET phone='100',score=1 ON DUPLICATE KEY UPDATE score=score+10"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("duplicate update CHECK error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT score FROM t").Rows); got != "[[4]]" {
		t.Fatalf("failed duplicate update changed the row: %s", got)
	}
}
