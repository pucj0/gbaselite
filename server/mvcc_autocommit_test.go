package server

import (
	"errors"
	"fmt"
	"gbaselite/executor"
	"gbaselite/storageengine"
	"testing"
)

func TestMVCCAutocommitState(t *testing.T) {
	e, err := openTestEngineWithOptions(t, t.TempDir(), "root", "pw", executor.OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	writer, reader := &executor.Session{}, &executor.Session{}
	run := func(s *executor.Session, q string) *executor.Result {
		t.Helper()
		r, err := ExecuteCompatible(e, s, q)
		if err != nil {
			t.Fatal(q, err)
		}
		return r
	}
	for _, variable := range []string{"@@session.tx_isolation", "@@transaction_isolation"} {
		if r := run(writer, "SELECT "+variable); r.Rows[0][0] != "REPEATABLE-READ" {
			t.Fatal(r)
		}
	}
	run(writer, "CREATE DATABASE test")
	run(writer, "USE test")
	run(reader, "USE test")
	run(writer, "CREATE TABLE ac(id INT PRIMARY KEY,v INT)")
	run(writer, "INSERT INTO ac VALUES(1,10)")
	r := run(writer, "SET autocommit=0")
	if !r.AutocommitDisabled || r.InTransaction {
		t.Fatal("mode transition", r)
	}
	run(writer, "UPDATE ac SET v=20 WHERE id=1")
	if !writer.InTransaction() {
		t.Fatal("missing lazy transaction")
	}
	if got := fmt.Sprint(run(reader, "SELECT v FROM ac").Rows); got != "[[10]]" {
		t.Fatal("dirty read", got)
	}
	if _, err = ExecuteCompatible(e, writer, "INSERT INTO ac VALUES(1,30)"); err == nil {
		t.Fatal("expected duplicate")
	}
	run(writer, "COMMIT")
	if !writer.AutocommitDisabled || writer.InTransaction() {
		t.Fatal("COMMIT changed mode")
	}
	if got := fmt.Sprint(run(reader, "SELECT v FROM ac").Rows); got != "[[20]]" {
		t.Fatal(got)
	}
	run(writer, "UPDATE ac SET v=30 WHERE id=1")
	run(writer, "ROLLBACK")
	run(writer, "UPDATE ac SET v=40 WHERE id=1")
	run(writer, "SET autocommit=1")
	if writer.AutocommitDisabled || writer.InTransaction() {
		t.Fatal("SET ON failed")
	}
	if got := fmt.Sprint(run(reader, "SELECT v FROM ac").Rows); got != "[[40]]" {
		t.Fatal(got)
	}
	run(writer, "SET autocommit=0")
	run(writer, "UPDATE ac SET v=50 WHERE id=1")
	e.ResetConnection(writer)
	if writer.AutocommitDisabled || writer.InTransaction() {
		t.Fatal("reset state")
	}
	if got := fmt.Sprint(run(reader, "SELECT v FROM ac").Rows); got != "[[40]]" {
		t.Fatal("reset committed", got)
	}
	run(writer, "SET autocommit=0")
	run(writer, "UPDATE ac SET v=60 WHERE id=1")
	// The commit of the open transaction must be refused, and a refused commit must not change
	// the autocommit mode. The conflicting change cannot come from another SQL statement touching
	// row id=1: since B02 row locking, such a statement waits for this transaction's record lock
	// instead of racing it (see executor/locking_dml_wake_test.go), so the transaction's own
	// catalog guard is invalidated instead — a schema change to the very table it updated.
	run(reader, "ALTER TABLE ac ADD COLUMN w INT")
	if _, err = ExecuteCompatible(e, writer, "SET autocommit=1"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("expected a catalog conflict, got %v", err)
	}
	if !writer.AutocommitDisabled {
		t.Fatal("failed commit changed mode")
	}
	if got := fmt.Sprint(run(writer, "SELECT @@autocommit").Rows); got != "[[0]]" {
		t.Fatal(got)
	}
}
