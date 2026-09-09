package server

import (
	"fmt"
	"gbaselite/executor"
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
	run(reader, "UPDATE ac SET v=70 WHERE id=1")
	if _, err = ExecuteCompatible(e, writer, "SET autocommit=1"); err == nil {
		t.Fatal("expected conflict")
	}
	if !writer.AutocommitDisabled {
		t.Fatal("failed commit changed mode")
	}
	if got := fmt.Sprint(run(writer, "SELECT @@autocommit").Rows); got != "[[0]]" {
		t.Fatal(got)
	}
}
