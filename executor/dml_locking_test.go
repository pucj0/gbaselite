package executor

import (
	"testing"
	"time"
)

// TestB02ConcurrentInsertSameUniqueKeyIsStillAccepted was a FIXED-NEGATIVE test that pinned the
// missing unique-index lock. It was replaced by the blocking/wake acceptance tests in
// locking_unique_test.go: the same non-NULL unique value under different primary keys now enters one
// wait relation, and the waiter is told a duplicate key from the latest published index instead of
// being handed a retryable version conflict.
//
// TestB02ConcurrentInsertDifferentUniqueKeysProceed below is the positive half of the same contract
// and must keep passing.

// TestB02ConcurrentInsertDifferentUniqueKeysProceed proves the lock does not serialise unrelated
// inserts.
func TestB02ConcurrentInsertDifferentUniqueKeysProceed(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	setup := &Session{}
	must := func(s *Session, sql string) {
		t.Helper()
		if _, err := e.Execute(s, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	must(setup, "CREATE DATABASE test")
	must(setup, "USE test")
	must(setup, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)")

	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(a)
	defer e.CloseSession(b)
	must(a, "BEGIN")
	must(b, "BEGIN")
	must(a, "INSERT INTO u VALUES(1,'A')")
	// A different key must not wait for A's transaction.
	start := time.Now()
	must(b, "INSERT INTO u VALUES(2,'B')")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("an unrelated insert waited %v", elapsed)
	}
	must(a, "COMMIT")
	must(b, "COMMIT")
	r, err := e.Execute(setup, "SELECT COUNT(*) FROM u")
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || len(r.Rows) == 0 || r.Rows[0][0] != int64(2) {
		t.Fatalf("rows = %v, want both distinct keys committed", r.Rows)
	}
}

// Update target locking is enabled: the fixed-negative test that used to pin its absence
// was replaced by the blocking/wake acceptance tests in locking_dml_wake_test.go, because
// the wake failure it recorded was a property of the sequential test harness rather than
// of the lock manager (see .tmp/b02r1-fix-report.md §问题 4).
