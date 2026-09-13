package executor

import (
	"fmt"
	"reflect"
	"testing"
)

var savepointScript = []parityStep{
	{Query: "CREATE DATABASE sp", SkipAffect: true},
	{Query: "USE sp", SkipAffect: true},
	{Query: "CREATE TABLE t(id INT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE)", SkipAffect: true},
	// SAVEPOINT outside a transaction keeps the legacy no-op message.
	{Query: "SAVEPOINT outside", Rows: true},
	{Query: "BEGIN"},
	{Query: "INSERT INTO t(v) VALUES(1)"},
	{Query: "SAVEPOINT s1"},
	{Query: "INSERT INTO t(v) VALUES(2)"},
	{Query: "UPDATE t SET v=20 WHERE v=1"},
	{Query: "SAVEPOINT s2"},
	{Query: "DELETE FROM t WHERE v=2"},
	{Query: "ROLLBACK TO SAVEPOINT s2"},
	{Query: "SELECT id,v FROM t ORDER BY id", Rows: true},
	{Query: "ROLLBACK TO SAVEPOINT s1"},
	{Query: "SELECT id,v FROM t ORDER BY id", Rows: true},
	// Same-name replacement moves the restore point.
	{Query: "SAVEPOINT s1"},
	{Query: "INSERT INTO t(v) VALUES(3)"},
	{Query: "ROLLBACK TO SAVEPOINT s1"},
	{Query: "SELECT id,v FROM t ORDER BY id", Rows: true},
	// RELEASE drops the name; later savepoints stay usable, the name is unknown.
	{Query: "RELEASE SAVEPOINT s1"},
	{Query: "ROLLBACK TO SAVEPOINT s1", Fail: true},
	{Query: "SAVEPOINT s3"},
	{Query: "INSERT INTO t(v) VALUES(4)"},
	{Query: "RELEASE SAVEPOINT s3"},
	{Query: "ROLLBACK TO SAVEPOINT s3", Fail: true},
	{Query: "SELECT id,v FROM t ORDER BY id", Rows: true},
	{Query: "ROLLBACK TO SAVEPOINT missing", Fail: true},
	// A failed statement inside a savepoint rolls back on ROLLBACK TO.
	{Query: "INSERT INTO t(v) VALUES(20),(20)", Fail: true},
	{Query: "SAVEPOINT s4"},
	{Query: "UPDATE t SET v=99 WHERE v=20"},
	{Query: "ROLLBACK TO SAVEPOINT s4"},
	{Query: "SELECT COUNT(*) FROM t", Rows: true},
	{Query: "COMMIT"},
	{Query: "SELECT id,v FROM t ORDER BY id", Rows: true},
	// A full ROLLBACK discards every savepoint layer.
	{Query: "BEGIN"},
	{Query: "SAVEPOINT r1"},
	{Query: "INSERT INTO t(v) VALUES(5)"},
	{Query: "ROLLBACK"},
	{Query: "SELECT COUNT(*) FROM t", Rows: true},
}

func TestMVCCSavepointsMatchLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, savepointScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, savepointScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range savepointScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCSavepointCommitAndReopen(t *testing.T) {
	dir := t.TempDir()
	e, err := OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(s *Session, q string) *Result {
		t.Helper()
		result, err := e.Execute(s, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return result
	}
	s := &Session{}
	run(s, "CREATE DATABASE sp2")
	run(s, "USE sp2")
	run(s, "CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run(s, "BEGIN")
	run(s, "INSERT INTO t VALUES(1,10)")
	run(s, "SAVEPOINT a")
	run(s, "INSERT INTO t VALUES(2,20)")
	run(s, "ROLLBACK TO SAVEPOINT a")
	run(s, "INSERT INTO t VALUES(3,30)")
	run(s, "COMMIT")
	if got := fmt.Sprint(run(s, "SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 10] [3 30]]" {
		t.Fatalf("committed savepoint rows = %s", got)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if got := fmt.Sprint(run(s, "SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 10] [3 30]]" {
		t.Fatalf("reopened rows = %s", got)
	}
	// Disconnect rolls back the pending chain.
	other := &Session{CurrentDatabase: "sp2"}
	run(other, "BEGIN")
	run(other, "SAVEPOINT d")
	run(other, "INSERT INTO t VALUES(9,90)")
	e.CloseSession(other)
	if got := fmt.Sprint(run(s, "SELECT COUNT(*) FROM t").Rows); got != "[[2]]" {
		t.Fatalf("disconnect rollback rows = %s", got)
	}
}
