package executor

import (
	"fmt"
	"testing"

	"gbaselite/storageengine"
	"gbaselite/storageengine/testkit"
)

// savepointFailureFixture builds an engine whose Nth commit can be forced to
// fail, so savepoint cleanup can be verified without relying on timing.
func savepointFailureFixture(t *testing.T) (*Engine, *Session, *commitFailingBackend) {
	t.Helper()
	backend := &commitFailingBackend{Engine: testkit.NewMemory()}
	e, err := OpenWithOptions(t.TempDir(), "root", "test", OpenOptions{BackendFactory: func(string, storageengine.Options) (storageengine.Engine, error) {
		return backend, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	s := &Session{}
	run := func(query string) *Result {
		t.Helper()
		result, err := e.Execute(s, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return result
	}
	run("CREATE DATABASE spf")
	run("USE spf")
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	return e, s, backend
}

func assertSessionClean(t *testing.T, e *Engine, s *Session) {
	t.Helper()
	if s.transaction != nil {
		t.Fatal("session transaction leaked after failure")
	}
	if len(s.savepoints) != 0 {
		t.Fatalf("savepoint layers leaked after failure: %d", len(s.savepoints))
	}
	e.CloseSession(s)
	if s.transaction != nil || len(s.savepoints) != 0 {
		t.Fatal("session leaked after CloseSession")
	}
}

func TestMVCCSavepointStatementCommitFailureAbortsTransaction(t *testing.T) {
	e, s, backend := savepointFailureFixture(t)
	if _, err := e.Execute(s, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(s, "SAVEPOINT s1"); err != nil {
		t.Fatal(err)
	}
	backend.arm(1)
	if _, err := e.Execute(s, "INSERT INTO t VALUES(1,10)"); err == nil {
		t.Fatal("forced statement commit failure was not reported")
	}
	backend.disarm()
	assertSessionClean(t, e, s)
	if got := fmt.Sprint(mustQuery(t, e, "SELECT COUNT(*) FROM t").Rows); got != "[[0]]" {
		t.Fatalf("failed statement committed rows: %s", got)
	}
	// The session must be reusable afterwards.
	if _, err := e.Execute(s, "BEGIN"); err != nil {
		t.Fatalf("begin after failure: %v", err)
	}
	if _, err := e.Execute(s, "INSERT INTO t VALUES(2,20)"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(s, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(mustQuery(t, e, "SELECT COUNT(*) FROM t").Rows); got != "[[1]]" {
		t.Fatalf("rows after recovery = %s", got)
	}
}

func TestMVCCSavepointNestedCommitFailureAbortsTransaction(t *testing.T) {
	e, s, backend := savepointFailureFixture(t)
	if _, err := e.Execute(s, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s1", "s2"} {
		if _, err := e.Execute(s, "SAVEPOINT "+name); err != nil {
			t.Fatal(err)
		}
	}
	backend.arm(1)
	if _, err := e.Execute(s, "INSERT INTO t VALUES(1,10)"); err == nil {
		t.Fatal("forced statement commit failure was not reported")
	}
	backend.disarm()
	assertSessionClean(t, e, s)
	if got := fmt.Sprint(mustQuery(t, e, "SELECT COUNT(*) FROM t").Rows); got != "[[0]]" {
		t.Fatalf("nested failure committed rows: %s", got)
	}
}

func TestMVCCSavepointCommitMergeFailureAbortsTransaction(t *testing.T) {
	e, s, backend := savepointFailureFixture(t)
	if _, err := e.Execute(s, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(s, "SAVEPOINT s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(s, "INSERT INTO t VALUES(1,10)"); err != nil {
		t.Fatal(err)
	}
	// The outer COMMIT first merges the savepoint layer; force that merge to fail.
	backend.arm(1)
	if _, err := e.Execute(s, "COMMIT"); err == nil {
		t.Fatal("forced commit merge failure was not reported")
	}
	backend.disarm()
	assertSessionClean(t, e, s)
	if got := fmt.Sprint(mustQuery(t, e, "SELECT COUNT(*) FROM t").Rows); got != "[[0]]" {
		t.Fatalf("failed commit persisted rows: %s", got)
	}
}

func TestMVCCSavepointWithoutTransactionKeepsLegacyMessage(t *testing.T) {
	e, s, _ := savepointFailureFixture(t)
	result, err := e.Execute(s, "SAVEPOINT alone")
	if err != nil {
		t.Fatal(err)
	}
	if result.Message != "no active transaction" {
		t.Fatalf("savepoint outside transaction message = %q", result.Message)
	}
}

func mustQuery(t *testing.T, e *Engine, query string) *Result {
	t.Helper()
	result, err := e.Execute(&Session{CurrentDatabase: "spf"}, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return result
}
