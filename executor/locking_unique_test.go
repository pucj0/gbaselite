package executor

import (
	"errors"
	"fmt"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"sync"
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

// TestT038ConcurrentInsertSamePrimaryKeyWaitsThenReportsDuplicate covers the primary-key half of the
// reservation rule, which is not a UNIQUE index at all and so was never reached by the value check.
//
// Without a primary-key re-check under the claim, the woken statement finds nothing wrong in its own
// snapshot, stages the row, and is rejected at commit with a generic version conflict: a retry
// instruction for a key that can never become free.
func TestT038ConcurrentInsertSamePrimaryKeyWaitsThenReportsDuplicate(t *testing.T) {
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
	// Different unique value, same primary key: the only thing the two statements meet on is the key.
	done := insertInFlight(f.engine, b, "INSERT INTO u VALUES(1,'B')")
	barrier.wait(t, "the second INSERT of the same primary key")
	f.must(a, "COMMIT")

	if err := awaitStatement(t, done, "the same-key INSERT"); !errors.Is(err, storage.ErrDuplicateKey) {
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

// TestT038InsertIgnoreSkipsTheRowItWaitedFor proves IGNORE still skips the row after the wait, rather
// than writing it or reporting the conflict the plain form reports.
func TestT038InsertIgnoreSkipsTheRowItWaitedFor(t *testing.T) {
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
	done := insertInFlight(f.engine, b, "INSERT IGNORE INTO u VALUES(2,'A')")
	barrier.wait(t, "the IGNORE insert of the reserved value")
	f.must(a, "COMMIT")

	if err := awaitStatement(t, done, "the IGNORE insert"); err != nil {
		t.Fatalf("INSERT IGNORE reported %v, want the row simply skipped", err)
	}
	f.must(b, "COMMIT")
	rows, err := f.engine.Execute(f.setup, "SELECT id FROM u")
	if err != nil {
		t.Fatal(err)
	}
	// The skipped row was never written, and the value still belongs to its original owner.
	if got := t035Rows(t, rows); got != "1;" {
		t.Fatalf("rows = %q, want only the holder's row", got)
	}
}

// TestT038ReplaceRewritesTheOccupantItCouldRead is the compatibility half of the REPLACE rule: an
// occupant the statement's own snapshot can read is the statement's to replace.
func TestT038ReplaceRewritesTheOccupantItCouldRead(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)",
		"INSERT INTO u VALUES(1,'A')")
	session := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(session)

	f.must(session, "REPLACE INTO u VALUES(2,'A')")
	rows, err := f.engine.Execute(f.setup, "SELECT id,sku FROM u")
	if err != nil {
		t.Fatal(err)
	}
	if got := t035Rows(t, rows); got != "2,A;" {
		t.Fatalf("rows = %q, want the value moved off row 1 onto row 2", got)
	}
}

// TestT038UpsertRewritesTheOccupantItCouldRead is the same compatibility half for ON DUPLICATE KEY
// UPDATE: the conflicting row is the one the statement updates, and it is one this statement can see.
func TestT038UpsertRewritesTheOccupantItCouldRead(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)",
		"INSERT INTO u VALUES(1,'A')")
	session := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(session)

	f.must(session, "INSERT INTO u VALUES(2,'A') ON DUPLICATE KEY UPDATE sku='B'")
	rows, err := f.engine.Execute(f.setup, "SELECT id,sku FROM u")
	if err != nil {
		t.Fatal(err)
	}
	if got := t035Rows(t, rows); got != "1,B;" {
		t.Fatalf("rows = %q, want the conflicting row itself updated", got)
	}
}

// TestT038UpsertRefusesToRewriteAnOccupantItNeverRead is the other half, and the reason the re-check
// exists: the occupant was published while this statement waited, so it is not a row the statement
// read. Rewriting it would rebase the statement onto post-readTS state, which is the retryable
// conflict — not a silent rewrite and not a permanent duplicate either, because the value is very
// much reachable once the statement runs again on a fresh snapshot.
func TestT038UpsertRefusesToRewriteAnOccupantItNeverRead(t *testing.T) {
	for _, variant := range []struct {
		name string
		sql  string
	}{
		{"REPLACE", "REPLACE INTO u VALUES(2,'A')"},
		{"ON DUPLICATE KEY UPDATE", "INSERT INTO u VALUES(2,'A') ON DUPLICATE KEY UPDATE sku='B'"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)")
			a := &Session{CurrentDatabase: "test"}
			b := &Session{CurrentDatabase: "test"}
			defer f.engine.CloseSession(a)
			defer f.engine.CloseSession(b)

			f.must(a, "BEGIN")
			f.must(b, "BEGIN")
			// The occupant of 'A' does not exist when b's snapshot is taken.
			f.must(a, "INSERT INTO u VALUES(1,'A')")

			barrier := newDMLWaitBarrier()
			barrier.arm(t)
			done := insertInFlight(f.engine, b, variant.sql)
			barrier.wait(t, "the "+variant.name+" of the reserved value")
			f.must(a, "COMMIT")

			if err := awaitStatement(t, done, "the "+variant.name); !errors.Is(err, storageengine.ErrConflict) {
				t.Fatalf("the %s = %v, want the retryable ErrConflict", variant.name, err)
			}
			_, _ = f.engine.Execute(b, "ROLLBACK")
			// Nothing was rewritten on top of a row the statement never read.
			rows, err := f.engine.Execute(f.setup, "SELECT id,sku FROM u")
			if err != nil {
				t.Fatal(err)
			}
			if got := t035Rows(t, rows); got != "1,A;" {
				t.Fatalf("rows = %q, want the holder's row untouched", got)
			}
		})
	}
}

// TestT038AtMostOneInsertWins is the acceptance criterion in its bluntest form: several sessions race
// to insert one unique value, and exactly one of them may end up committed.
func TestT038AtMostOneInsertWins(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)")
	const writers = 4

	results := make([]error, writers)
	sessions := make([]*Session, writers)
	for i := range sessions {
		sessions[i] = &Session{CurrentDatabase: "test"}
		defer f.engine.CloseSession(sessions[i])
	}
	var started sync.WaitGroup
	var finished sync.WaitGroup
	started.Add(writers)
	for i := 0; i < writers; i++ {
		finished.Add(1)
		go func(index int) {
			defer finished.Done()
			started.Done()
			started.Wait()
			_, err := f.engine.Execute(sessions[index], fmt.Sprintf("INSERT INTO u VALUES(%d,'A')", index+1))
			results[index] = err
		}(i)
	}
	finished.Wait()

	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d of %d concurrent inserts of one unique value succeeded, want exactly 1 (errors: %v)", winners, writers, results)
	}
	rows, err := f.engine.Execute(f.setup, "SELECT COUNT(*) FROM u")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 || rows.Rows[0][0] != int64(1) {
		t.Fatalf("committed rows = %v, want exactly one winner", rows.Rows)
	}
}
