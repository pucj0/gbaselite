package executor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_read_test.go is T034/T036: `SELECT ... FOR UPDATE`, `FOR SHARE` and `LOCK IN SHARE MODE`
// served by real SQL sessions.
//
// Every "it waited" claim here is an observed fact rather than a timing guess:
//
//   - a blocked statement is driven to its (short) session lock-wait budget, so the test asserts the
//     contract's ErrLockWaitTimeout instead of sleeping and hoping;
//   - the wait is synchronised through `lockedReadWaitHook`, which fires the moment the locking read
//     discovers a conflict, so the holder is released only after the reader is genuinely queued;
//   - "it did not wait" is asserted by a statement that succeeds while a conflicting transaction is
//     still open, plus the unchanged snapshot value it returns.

// t034Fixture opens one engine with the table the locking-read tests share.
func t034Fixture(t *testing.T, seed ...string) *t035Fixture {
	t.Helper()
	return newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", seed...)
}

// t034Session starts a session whose lock-wait budget is short, so a blocking statement fails quickly
// instead of holding the test for the contract's 50-second default.
func t034Session(t *testing.T, f *t035Fixture, seconds int) *Session {
	t.Helper()
	session := &Session{}
	f.must(session, "USE test")
	f.must(session, "SET SESSION innodb_lock_wait_timeout = "+itoa(seconds))
	return session
}

// itoa keeps the test free of a strconv import for one small formatting call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// t034ArmWaitHook makes the locking read report a conflict deterministically: the first conflict
// signals the channel, and the read then waits inside the manager exactly as it would in production.
func t034ArmWaitHook(t *testing.T) <-chan struct{} {
	t.Helper()
	queued := make(chan struct{})
	var fired bool
	lockedReadWaitHook = func(string) bool {
		if !fired {
			fired = true
			close(queued)
		}
		return true
	}
	t.Cleanup(func() { lockedReadWaitHook = nil })
	return queued
}

// TestT034ForUpdateTakesAnExclusiveRowLock pins the exclusive half: while one session holds
// `FOR UPDATE` on a row, another session's write to that row waits and fails on its budget, and it
// succeeds once the holder's transaction ends.
func TestT034ForUpdateTakesAnExclusiveRowLock(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
	holder := t034Session(t, f, 5)
	writer := t034Session(t, f, 1)

	f.must(holder, "BEGIN")
	if got := t035Rows(t, f.must(holder, "SELECT id,v FROM items WHERE id=10 FOR UPDATE")); got != "10,1;" {
		t.Fatalf("FOR UPDATE read = %q, want the row it locked", got)
	}

	// The holder's transaction is still open, so the row lock is still held: the writer must not be
	// able to change the row, and it reports the timeout the contract names.
	if _, err := f.engine.Execute(writer, "UPDATE items SET v=2 WHERE id=10"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("a write under a FOR UPDATE holder returned %v, want ErrLockWaitTimeout", err)
	}
	// The failed statement did not change anything, and the holder still sees its own value.
	if got := t035Rows(t, f.must(holder, "SELECT v FROM items WHERE id=10")); got != "1;" {
		t.Fatalf("the row changed while it was exclusively locked: %q", got)
	}

	f.must(holder, "COMMIT")
	// The lock lived to COMMIT and no longer: the same write now succeeds.
	f.must(writer, "UPDATE items SET v=2 WHERE id=10")
	if got := t035Rows(t, f.must(writer, "SELECT v FROM items WHERE id=10")); got != "2;" {
		t.Fatalf("after the holder committed the write read %q, want 2", got)
	}
}

// TestT034ForShareIsCompatibleWithAnotherShareAndExcludesWriters pins the shared half, including the
// LOCK IN SHARE MODE alias: two shared readers coexist on one row, while an exclusive writer does not.
func TestT034ForShareIsCompatibleWithAnotherShareAndExcludesWriters(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
	first := t034Session(t, f, 5)
	second := t034Session(t, f, 5)
	writer := t034Session(t, f, 1)

	f.must(first, "BEGIN")
	f.must(first, "SELECT v FROM items WHERE id=10 FOR SHARE")
	// A second shared reader is admitted: S and S are compatible.
	f.must(second, "BEGIN")
	if got := t035Rows(t, f.must(second, "SELECT v FROM items WHERE id=10 LOCK IN SHARE MODE")); got != "1;" {
		t.Fatalf("the second shared reader saw %q, want the row", got)
	}
	// An exclusive writer is not: it waits and reports its budget.
	if _, err := f.engine.Execute(writer, "UPDATE items SET v=3 WHERE id=10"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("a write under two FOR SHARE readers returned %v, want ErrLockWaitTimeout", err)
	}
	f.must(first, "COMMIT")
	f.must(second, "COMMIT")
	f.must(writer, "UPDATE items SET v=3 WHERE id=10")
	if got := t035Rows(t, f.must(writer, "SELECT v FROM items WHERE id=10")); got != "3;" {
		t.Fatalf("after both readers committed the write read %q, want 3", got)
	}
}

// TestT034PlainSelectKeepsTheSnapshot proves the locking path did not change ordinary reads: a plain
// SELECT returns the snapshot value, without waiting and without seeing an uncommitted change.
func TestT034PlainSelectKeepsTheSnapshot(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
	holder := t034Session(t, f, 5)
	reader := t034Session(t, f, 1)

	f.must(holder, "BEGIN")
	f.must(holder, "UPDATE items SET v=7 WHERE id=10")

	// No wait and no new value: the plain read is a snapshot read of the row's committed version.
	if got := t035Rows(t, f.must(reader, "SELECT v FROM items WHERE id=10")); got != "1;" {
		t.Fatalf("a plain SELECT saw %q, want the committed snapshot value 1", got)
	}
	f.must(holder, "COMMIT")
	if got := t035Rows(t, f.must(reader, "SELECT v FROM items WHERE id=10")); got != "7;" {
		t.Fatalf("after the commit the plain SELECT saw %q, want 7", got)
	}
}

// TestT034LockingReadWaitsAndThenSeesTheLatestCommittedValue is the core Current Read behaviour: the
// read is queued (observed through the wait hook), the holder commits, and only then does the
// statement return — with the version the holder committed, not the one it saw before waiting.
func TestT034LockingReadWaitsAndThenSeesTheLatestCommittedValue(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
	holder := t034Session(t, f, 5)
	reader := t034Session(t, f, 10)

	f.must(holder, "BEGIN")
	f.must(holder, "UPDATE items SET v=9 WHERE id=10")

	queued := t034ArmWaitHook(t)
	type outcome struct {
		rows string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := f.engine.Execute(reader, "SELECT v FROM items WHERE id=10 FOR UPDATE")
		if err != nil {
			done <- outcome{err: err}
			return
		}
		done <- outcome{rows: t035Rows(t, result)}
	}()

	select {
	case <-queued:
		// The read is provably queued on the holder's row lock.
	case <-time.After(10 * time.Second):
		t.Fatal("the locking read never reported a conflict with the open holder")
	}
	f.must(holder, "COMMIT")
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the locking read failed after the holder committed: %v", got.err)
		}
		if got.rows != "9;" {
			t.Fatalf("the locking read returned %q, want the value committed while it waited (9)", got.rows)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the locking read never returned after the holder committed")
	}
}

// TestT034LockingReadDoesNotReturnADeletedRow proves the re-read: a row deleted while the statement
// waited is not returned from the version the statement saw before the wait.
func TestT034LockingReadDoesNotReturnADeletedRow(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
	holder := t034Session(t, f, 5)
	reader := t034Session(t, f, 10)

	f.must(holder, "BEGIN")
	f.must(holder, "DELETE FROM items WHERE id=10")

	queued := t034ArmWaitHook(t)
	type outcome struct {
		rows string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := f.engine.Execute(reader, "SELECT id FROM items WHERE id=10 FOR UPDATE")
		if err != nil {
			done <- outcome{err: err}
			return
		}
		done <- outcome{rows: t035Rows(t, result)}
	}()
	select {
	case <-queued:
	case <-time.After(10 * time.Second):
		t.Fatal("the locking read never reported a conflict with the open deleter")
	}
	f.must(holder, "COMMIT")
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the locking read failed after the delete committed: %v", got.err)
		}
		if got.rows != "" {
			t.Fatalf("the locking read returned %q, want no row: it was deleted while the statement waited", got.rows)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the locking read never returned after the delete committed")
	}
}

// TestT034LockingScanDiscoversRowsCommittedAfterTheSnapshot proves the SQL-visible half of the fixed
// enumeration: a row committed after the statement's snapshot is found by a locking scan and not by a
// plain one of the same transaction.
func TestT034LockingScanDiscoversRowsCommittedAfterTheSnapshot(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
	reader := t034Session(t, f, 5)
	writer := t034Session(t, f, 5)

	f.must(reader, "BEGIN")
	// The reader's snapshot is taken here, by reading one row through it.
	f.must(reader, "SELECT v FROM items WHERE id=10")
	// Another session inserts a row inside the scan's range and commits it.
	f.must(writer, "INSERT INTO items VALUES(11,2)")

	// The plain scan of the same transaction still does not see it.
	if got := t035Rows(t, f.must(reader, "SELECT id FROM items WHERE id >= 10")); got != "10;" {
		t.Fatalf("a plain scan saw %q, want only the row its snapshot contains", got)
	}
	// The locking scan does, because it reads the current state under the locks it takes.
	if got := t035Rows(t, f.must(reader, "SELECT id FROM items WHERE id >= 10 FOR UPDATE")); got != "10;11;" {
		t.Fatalf("the locking scan saw %q, want both rows", got)
	}
	f.must(reader, "COMMIT")
}

// TestT034LockingReadProtectsAMissingRowsInsertionPosition proves the absent-target rule: a locking
// read of a row that does not exist holds the position, so another session's insert of that key waits
// and reports its budget instead of succeeding.
func TestT034LockingReadProtectsAMissingRowsInsertionPosition(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
	holder := t034Session(t, f, 5)
	inserter := t034Session(t, f, 1)

	f.must(holder, "BEGIN")
	if got := t035Rows(t, f.must(holder, "SELECT id FROM items WHERE id=42 FOR UPDATE")); got != "" {
		t.Fatalf("a locking read of an absent row returned %q, want no row", got)
	}
	if _, err := f.engine.Execute(inserter, "INSERT INTO items VALUES(42,1)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("an insert into a protected absent key returned %v, want ErrLockWaitTimeout", err)
	}
	f.must(holder, "COMMIT")
	f.must(inserter, "INSERT INTO items VALUES(42,1)")
	if got := t035Rows(t, f.must(inserter, "SELECT id FROM items WHERE id=42")); got != "42;" {
		t.Fatalf("after the holder committed the insert read %q, want the row", got)
	}
}

// TestT034UnsupportedShapesAndCapabilitiesAreRefused proves the fail-closed boundary: a locking read
// on a shape this release cannot protect (a join) is refused with an error that names it, instead of
// being served from the snapshot as if it were locked.
func TestT034UnsupportedShapesAndCapabilitiesAreRefused(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "CREATE TABLE other(id BIGINT PRIMARY KEY, v INT)")
	f.must(&Session{}, "USE test")
	session := t034Session(t, f, 5)
	f.must(session, "INSERT INTO other VALUES(10,2)")

	if _, err := f.engine.Execute(session, "SELECT i.id FROM items i JOIN other o ON i.id=o.id FOR UPDATE"); err == nil {
		t.Fatal("a locking read over a join was served instead of refused")
	} else if !strings.Contains(err.Error(), "locking") {
		t.Fatalf("the refusal does not name row locking: %v", err)
	}
	// The same statement without the locking clause still works, so the refusal is about locking.
	if got := t035Rows(t, f.must(session, "SELECT i.id FROM items i JOIN other o ON i.id=o.id")); got != "10;" {
		t.Fatalf("the plain join read %q, want the joined row", got)
	}

	// A unique-index predicate stays safe even when the planner cannot use the index: it falls back to
	// a whole-namespace plan, which protects everything and returns the row. The *index* plan itself is
	// refused by the guard below, because its key is an index entry rather than a row.
	f.must(session, "CREATE TABLE people(id BIGINT PRIMARY KEY, email VARCHAR(64), UNIQUE KEY uq_email(email))")
	f.must(session, "INSERT INTO people VALUES(1,'a@b')")
	if got := t035Rows(t, f.must(session, "SELECT id FROM people WHERE email='a@b' FOR UPDATE")); got != "1;" {
		t.Fatalf("the locking read through a unique predicate returned %q, want the row", got)
	}
}

// TestT034IndexAccessGuardRefusesAHalfProtectedPlan pins the guard itself: a secondary-index access
// plan is refused, because protecting it means claiming an entry set that can hold several entries per
// value; a unique-index plan is served instead, because its value has exactly one entry and the path
// claims the entry's reservation and the common primary row.
func TestT034IndexAccessGuardRefusesAHalfProtectedPlan(t *testing.T) {
	if err := lockedReadIndexGuard(sqlAccessPlan{kind: "ref", index: "idx_v", space: "secondary/people/idx_v", key: []byte("v")}); err == nil {
		t.Fatal("a secondary-index access plan was accepted for a locking read")
	} else if !strings.Contains(err.Error(), "secondary index") {
		t.Fatalf("the refusal does not name the access path: %v", err)
	}
	if err := lockedReadIndexGuard(sqlAccessPlan{kind: sqlAccessUnique, index: "uq_email", space: "index/people/uq_email", key: []byte("a@b")}); err != nil {
		t.Fatalf("a unique-index access plan was refused: %v", err)
	}
	for _, kind := range []string{sqlAccessPoint, sqlAccessRange, sqlAccessOrdered, sqlAccessAll} {
		if err := lockedReadIndexGuard(sqlAccessPlan{kind: kind}); err != nil {
			t.Fatalf("access kind %q was refused: %v", kind, err)
		}
	}
}
