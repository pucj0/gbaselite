package executor

import (
	"testing"
	"time"
)

// TestB02ConcurrentInsertSameUniqueKeyIsStillAccepted is a FIXED-NEGATIVE test.
//
// It pins the unresolved defect this round measured and could not fix: two transactions inserting
// the same unique key. The second INSERT takes the INSERT lock and still returns success in about a
// millisecond, so the duplicate is caught only later by the MVCC commit validator, where the loser
// receives a generic row-version conflict rather than a duplicate-key error.
//
// The test asserts today's behaviour so the defect cannot be mistaken for a fix. It must be
// rewritten into a waiting/re-checking test as soon as the cause is found.
func TestB02ConcurrentInsertSameUniqueKeyIsStillAccepted(t *testing.T) {
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
	must(setup, "INSERT INTO u VALUES(1,'A')")

	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(a)
	defer e.CloseSession(b)
	must(a, "BEGIN")
	must(b, "BEGIN")
	must(a, "INSERT INTO u VALUES(2,'X')")

	// Today the second writer does NOT wait. When that changes, this assertion fails and points at
	// the contract this test is standing in for.
	if _, err := e.Execute(b, "INSERT INTO u VALUES(3,'X')"); err != nil {
		t.Fatalf("the second insert was rejected early (%v); if it now waits, replace this test", err)
	}
	// The duplicate is caught at commit, and the constraint still holds in the data: the version
	// check is what prevents two committed rows with one unique key. That part is real protection,
	// which is why this is a contract gap rather than a corruption bug.
	if _, err := e.Execute(a, "COMMIT"); err != nil {
		t.Fatalf("A commit: %v", err)
	}
	if _, err := e.Execute(b, "COMMIT"); err == nil {
		t.Fatal("both transactions committed the same unique key")
	}
	r, err := e.Execute(setup, "SELECT COUNT(*) FROM u WHERE sku='X'")
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("committed rows with sku='X' = %v, want exactly 1", r.Rows)
	}
}

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

// TestB02UpdateTargetLockingIsNotEnabled pins the deliberate gap.
//
// UPDATE target locking is implemented but not enabled, because a reproducible shape left the
// second waiter blocked past the holder's commit. This test fails loudly if the gap is closed
// without the test being updated, so the gap cannot disappear silently.
func TestB02UpdateTargetLockingIsNotEnabled(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &Session{}
	must := func(sess *Session, sql string) {
		t.Helper()
		if _, err := e.Execute(sess, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	must(s, "CREATE DATABASE test")
	must(s, "USE test")
	must(s, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	must(s, "INSERT INTO items VALUES(1,1)")

	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(a)
	defer e.CloseSession(b)
	must(a, "BEGIN")
	must(b, "BEGIN")
	must(a, "UPDATE items SET v=2 WHERE id=1")
	// With locking enabled this would block until A commits. Today it proceeds, and the conflict
	// is caught by the commit validator instead.
	start := time.Now()
	if _, err := e.Execute(b, "UPDATE items SET v=3 WHERE id=1"); err != nil {
		t.Fatalf("the unlocked update failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("UPDATE blocked for %v, which means target locking became active; update this test", elapsed)
	}
	_, _ = e.Execute(a, "ROLLBACK")
	_, _ = e.Execute(b, "ROLLBACK")
}
