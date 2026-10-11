package executor

import (
	"errors"
	"gbaselite/storage"
	"testing"
	"time"
)

// locking_unique_test.go holds the unique-index lock acceptance tests (B02 US4 / T038).
//
// The defect they cover: a unique index was never locked at all, and the identity the index lock
// *would* have used appended the owning primary key to the unique value. Two transactions that
// insert the same unique value under different primary keys therefore addressed two different
// resources, never met, and both ran on until the commit validator rejected the loser with a
// generic row-version conflict — a retry instruction for what is a permanent constraint violation.
//
// The tests below state the contract that replaces it: the same non-NULL unique value under
// different primary keys enters one wait relation, the waiter is woken by the holder's commit,
// and the waiter is then told the truth (a duplicate key) from the latest published index state
// rather than from the snapshot it took before it waited.
//
// The ordering barrier is the neutral `ShouldWait` decision the lock manager consults while it
// holds its own critical section (see locking_dml_wake_test.go): a test that used a sleep to guess
// that the waiter queued before the holder committed would be measuring the scheduler.

// uniqueFixture opens one engine with a unique-index table and returns the two sessions and the
// statement helper the tests below share.
type uniqueFixture struct {
	engine *Engine
	setup  *Session
	must   func(*Session, string)
}

func newUniqueFixture(t *testing.T, schema string, seed ...string) *uniqueFixture {
	t.Helper()
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	setup := &Session{}
	must := func(session *Session, sql string) {
		t.Helper()
		if _, err := e.Execute(session, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	must(setup, "CREATE DATABASE test")
	must(setup, "USE test")
	must(setup, schema)
	for _, statement := range seed {
		must(setup, statement)
	}
	return &uniqueFixture{engine: e, setup: setup, must: must}
}

// insertInFlight runs one statement on its own session and reports the error it produced.
func insertInFlight(engine *Engine, session *Session, sql string) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := engine.Execute(session, sql)
		done <- err
	}()
	return done
}

// awaitStatement waits for an in-flight statement to finish and fails the test if it does not.
func awaitStatement(t *testing.T, done chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never finished after the holder released", what)
		return nil
	}
}

// TestB02ConcurrentInsertSameUniqueValueWaitsThenReportsDuplicate is the replacement for the
// fixed-negative test that pinned the missing lock.
func TestB02ConcurrentInsertSameUniqueValueWaitsThenReportsDuplicate(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)")
	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(a)
	defer f.engine.CloseSession(b)

	f.must(a, "BEGIN")
	f.must(b, "BEGIN")
	f.must(a, "INSERT INTO u VALUES(1,'A')")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := insertInFlight(f.engine, b, "INSERT INTO u VALUES(2,'A')")
	// The second insert must meet the first one on the same resource: that is the barrier firing.
	barrier.wait(t, "the second INSERT of the same unique value")
	// It must still be waiting: the holder has not committed yet.
	select {
	case err := <-done:
		t.Fatalf("the conflicting INSERT returned before the holder committed: %v", err)
	default:
	}
	f.must(a, "COMMIT")
	// Once the holder is gone the waiter is woken and re-checks the *latest published* index, so it
	// reports the permanent constraint violation rather than a retryable version conflict.
	if err := awaitStatement(t, done, "the conflicting INSERT"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("the woken INSERT = %v, want storage.ErrDuplicateKey", err)
	}
	_, _ = f.engine.Execute(b, "ROLLBACK")

	rows, err := f.engine.Execute(f.setup, "SELECT COUNT(*) FROM u")
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil || len(rows.Rows) != 1 || rows.Rows[0][0] != int64(1) {
		t.Fatalf("committed rows = %v, want exactly the holder's row", rows.Rows)
	}
}

// TestB02ConcurrentInsertDifferentUniqueValuesStillProceed proves the reservation is per value and
// not per index: unrelated values never meet.
func TestB02ConcurrentInsertDifferentUniqueValuesStillProceed(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)")
	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(a)
	defer f.engine.CloseSession(b)

	f.must(a, "BEGIN")
	f.must(b, "BEGIN")
	f.must(a, "INSERT INTO u VALUES(1,'A')")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := insertInFlight(f.engine, b, "INSERT INTO u VALUES(2,'B')")
	if err := awaitStatement(t, done, "an unrelated INSERT"); err != nil {
		t.Fatalf("an unrelated INSERT failed: %v", err)
	}
	select {
	case <-barrier.ready:
		t.Fatal("an unrelated unique value conflicted with the holder")
	default:
	}
	f.must(a, "COMMIT")
	f.must(b, "COMMIT")
}

// TestB02ConcurrentInsertNullUniqueValuesProceed proves a NULL unique value reserves nothing, which
// is the existing uniqueness rule: NULLs are not constrained.
func TestB02ConcurrentInsertNullUniqueValuesProceed(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)")
	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(a)
	defer f.engine.CloseSession(b)

	f.must(a, "BEGIN")
	f.must(b, "BEGIN")
	f.must(a, "INSERT INTO u VALUES(1,NULL)")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := insertInFlight(f.engine, b, "INSERT INTO u VALUES(2,NULL)")
	if err := awaitStatement(t, done, "a NULL-valued INSERT"); err != nil {
		t.Fatalf("a NULL-valued INSERT failed: %v", err)
	}
	select {
	case <-barrier.ready:
		t.Fatal("a NULL unique value was reserved; NULLs are not constrained")
	default:
	}
	f.must(a, "COMMIT")
	f.must(b, "COMMIT")
}

// TestB02ConcurrentInsertCompositeUniqueValueWaits proves a multi-column unique index reserves its
// whole value, so the second row conflicts on the composite and not on a column alone.
func TestB02ConcurrentInsertCompositeUniqueValueWaits(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, a INT, b INT, UNIQUE KEY uq_ab(a,b))")
	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(a)
	defer f.engine.CloseSession(b)

	f.must(a, "BEGIN")
	f.must(b, "BEGIN")
	f.must(a, "INSERT INTO u VALUES(1,7,9)")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := insertInFlight(f.engine, b, "INSERT INTO u VALUES(2,7,9)")
	barrier.wait(t, "the second INSERT of the same composite unique value")
	f.must(a, "COMMIT")
	if err := awaitStatement(t, done, "the composite-conflicting INSERT"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("the woken INSERT = %v, want storage.ErrDuplicateKey", err)
	}
	_, _ = f.engine.Execute(b, "ROLLBACK")
}

// TestB02ConcurrentInsertCollationEquivalentValuesWait proves collation-equal unique values use one
// lock identity, so 'A' and 'a' cannot both be inserted under a case-insensitive collation.
func TestB02ConcurrentInsertCollationEquivalentValuesWait(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) COLLATE utf8mb4_general_ci UNIQUE)")
	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(a)
	defer f.engine.CloseSession(b)

	f.must(a, "BEGIN")
	f.must(b, "BEGIN")
	f.must(a, "INSERT INTO u VALUES(1,'A')")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := insertInFlight(f.engine, b, "INSERT INTO u VALUES(2,'a')")
	barrier.wait(t, "the collation-equivalent INSERT")
	f.must(a, "COMMIT")
	if err := awaitStatement(t, done, "the collation-equivalent INSERT"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("the woken INSERT = %v, want storage.ErrDuplicateKey", err)
	}
	_, _ = f.engine.Execute(b, "ROLLBACK")
}

// TestB02UpdateReservesTheUniqueValueItInstalls proves the UPDATE side of the same rule: an
// uncommitted update that installs a unique value makes a concurrent insert of that value wait.
func TestB02UpdateReservesTheUniqueValueItInstalls(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)",
		"INSERT INTO u VALUES(1,'A')")
	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(a)
	defer f.engine.CloseSession(b)

	f.must(a, "BEGIN")
	f.must(b, "BEGIN")
	f.must(a, "UPDATE u SET sku='B' WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := insertInFlight(f.engine, b, "INSERT INTO u VALUES(2,'B')")
	barrier.wait(t, "the INSERT of a value an uncommitted UPDATE installs")
	f.must(a, "ROLLBACK")
	// The update was rolled back, so the value is free again and the insert succeeds.
	if err := awaitStatement(t, done, "the INSERT after the updater rolled back"); err != nil {
		t.Fatalf("the INSERT after the rollback failed: %v", err)
	}
	f.must(b, "COMMIT")
}
