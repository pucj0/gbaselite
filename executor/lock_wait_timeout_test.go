package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// lock_wait_timeout_test.go holds the T035 acceptance tests for
// `SET SESSION innodb_lock_wait_timeout = N` (B02 FR-010/012/028).
//
// The setting is a *lock wait* subset: it may only bound the wait of a lock
// request, never the execution of an ordinary statement. Every test below
// therefore states both halves of that rule — the value that is accepted or
// rejected, and the proof that a plain snapshot read is untouched.
//
// The timeout lifecycle test reuses the deterministic lock-conflict fixture from
// locking_dml_wake_test.go: the neutral `ShouldWait` decision reports that a
// conflicting request is genuinely queued, so "the statement was waiting when the
// deadline fired" is an observed fact rather than a sleep-based guess.

// t035Fixture opens one MVCC engine with the small table every T035 test shares.
type t035Fixture struct {
	engine *Engine
	must   func(*Session, string) *Result
}

func newT035Fixture(t *testing.T, schema string, seed ...string) *t035Fixture {
	t.Helper()
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	setup := &Session{}
	must := func(session *Session, sql string) *Result {
		t.Helper()
		result, err := e.Execute(session, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return result
	}
	must(setup, "CREATE DATABASE test")
	must(setup, "USE test")
	must(setup, schema)
	for _, statement := range seed {
		must(setup, statement)
	}
	return &t035Fixture{engine: e, must: must}
}

// t035Rows renders a result as a comparable string so a row assertion reads as the
// data it checked instead of as a slice dump.
func t035Rows(t *testing.T, result *Result) string {
	t.Helper()
	if result == nil {
		return "<nil>"
	}
	var builder strings.Builder
	for _, row := range result.Rows {
		for index, value := range row {
			if index > 0 {
				builder.WriteByte(',')
			}
			fmt.Fprintf(&builder, "%v", value)
		}
		builder.WriteByte(';')
	}
	return builder.String()
}

// TestT035DefaultLockWaitTimeoutIs50Seconds covers group 1: a fresh and an
// initialised session both report the contract default, and a session that never
// touched the setting still runs its plain SELECT unchanged.
func TestT035DefaultLockWaitTimeoutIs50Seconds(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(11,1)")

	// A zero-value session must report the default, not zero: a caller that reads the
	// budget before any statement must see the same 50 seconds the engine uses.
	fresh := &Session{}
	if got, want := fresh.LockWaitTimeout(), 50*time.Second; got != want {
		t.Fatalf("zero-value session lock-wait timeout = %s, want %s", got, want)
	}
	fresh.InitializeSettings()
	if got, want := fresh.LockWaitTimeout(), 50*time.Second; got != want {
		t.Fatalf("initialised session lock-wait timeout = %s, want %s", got, want)
	}

	// The default session has never been told about the variable; its snapshot read
	// must keep returning exactly the stored rows.
	session := &Session{CurrentDatabase: "test"}
	result := f.must(session, "SELECT id, v FROM items ORDER BY id")
	if got, want := t035Rows(t, result), "10,1;11,1;"; got != want {
		t.Fatalf("default-session plain SELECT rows = %q, want %q", got, want)
	}
	if got, want := session.LockWaitTimeout(), 50*time.Second; got != want {
		t.Fatalf("session lock-wait timeout after a plain SELECT = %s, want %s", got, want)
	}
}

// TestT035SessionLockWaitTimeoutAcceptsBoundaries covers group 2: the two ends of
// the legal range are accepted and readable through the accessor.
func TestT035SessionLockWaitTimeoutAcceptsBoundaries(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	for _, seconds := range []int64{1, 86400} {
		session := &Session{}
		statement := fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", seconds)
		if _, err := e.Execute(session, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		if got, want := session.LockWaitTimeout(), time.Duration(seconds)*time.Second; got != want {
			t.Fatalf("after %q the lock-wait timeout = %s, want %s", statement, got, want)
		}
	}

	// The unscoped form is the session form in MySQL, and the explicit @@session form
	// must reach the same stored value.
	unscoped := &Session{}
	if _, err := e.Execute(unscoped, "SET innodb_lock_wait_timeout = 7"); err != nil {
		t.Fatalf("unscoped SET: %v", err)
	}
	if got, want := unscoped.LockWaitTimeout(), 7*time.Second; got != want {
		t.Fatalf("unscoped SET stored %s, want %s", got, want)
	}
	explicit := &Session{}
	if _, err := e.Execute(explicit, "SET @@session.innodb_lock_wait_timeout = 9"); err != nil {
		t.Fatalf("@@session SET: %v", err)
	}
	if got, want := explicit.LockWaitTimeout(), 9*time.Second; got != want {
		t.Fatalf("@@session SET stored %s, want %s", got, want)
	}
}

// TestT035SessionLockWaitTimeoutRejectsIllegalValues covers group 3: every illegal
// value is refused with an actionable message and leaves the stored value alone.
func TestT035SessionLockWaitTimeoutRejectsIllegalValues(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	for _, statement := range []string{
		"SET SESSION innodb_lock_wait_timeout = 0",
		"SET SESSION innodb_lock_wait_timeout = -1",
		"SET SESSION innodb_lock_wait_timeout = 1.5",
		"SET SESSION innodb_lock_wait_timeout = 86401",
		"SET SESSION innodb_lock_wait_timeout = abc",
		"SET GLOBAL innodb_lock_wait_timeout = 60",
	} {
		session := &Session{}
		// Start from a known accepted value so "unchanged" is a real observation
		// rather than a comparison of two defaults.
		if _, err := e.Execute(session, "SET SESSION innodb_lock_wait_timeout = 9"); err != nil {
			t.Fatalf("priming SET: %v", err)
		}
		if _, err := e.Execute(session, statement); err == nil {
			t.Fatalf("%s was accepted, want an error", statement)
		} else {
			message := strings.ToLower(err.Error())
			if !strings.Contains(message, "innodb_lock_wait_timeout") && !strings.Contains(message, "86400") {
				t.Fatalf("%s error %q does not name the variable or the legal range", statement, err)
			}
		}
		if got, want := session.LockWaitTimeout(), 9*time.Second; got != want {
			t.Fatalf("%s changed the stored value to %s, want %s", statement, got, want)
		}
	}
}

// TestT035LockWaitEndsWithTimeoutAndKeepsRootUsable covers group 4: with a
// one-second budget a statement that genuinely waits ends on the lock-wait
// timeout, the statement child is rolled back, the explicit root survives and the
// blocking transaction is unaffected.
func TestT035LockWaitEndsWithTimeoutAndKeepsRootUsable(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(11,1)")

	holder := &Session{CurrentDatabase: "test"}
	waiter := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(holder)
	defer f.engine.CloseSession(waiter)

	f.must(holder, "BEGIN")
	f.must(waiter, "BEGIN")
	f.must(holder, "UPDATE items SET v=2 WHERE id=10")
	f.must(waiter, "SET SESSION innodb_lock_wait_timeout = 1")
	if got, want := waiter.LockWaitTimeout(), time.Second; got != want {
		t.Fatalf("waiter budget = %s, want %s", got, want)
	}

	// The barrier proves the request was queued on a real conflict before the
	// deadline is allowed to decide anything.
	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(waiter, "UPDATE items SET v=3 WHERE id=10")
		done <- err
	}()
	barrier.wait(t, "the blocked UPDATE")

	started := time.Now()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the waiting UPDATE never ended; the session budget did not bound the wait")
	}
	if !errors.Is(waitErr, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("the waiting UPDATE = %v, want storageengine.ErrLockWaitTimeout", waitErr)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the waiting UPDATE took %s after a 1s session budget", elapsed)
	}

	// The statement failed, so its child transaction must be gone while the explicit
	// root stays usable: ROLLBACK has to observe an active transaction.
	if _, err := f.engine.Execute(waiter, "ROLLBACK"); err != nil {
		t.Fatalf("the explicit root was not usable after the timeout: %v", err)
	}

	// The holder was never the victim: it commits normally and its value is published.
	if _, err := f.engine.Execute(holder, "COMMIT"); err != nil {
		t.Fatalf("the blocking transaction was harmed by the waiter's timeout: %v", err)
	}
	published := f.must(holder, "SELECT v FROM items WHERE id=10")
	if got, want := t035Rows(t, published), "2;"; got != want {
		t.Fatalf("committed row after the timeout = %q, want %q", got, want)
	}

	// The timed-out request must not linger as a phantom waiter: a later writer that
	// sees no holder has to proceed immediately.
	third := &Session{CurrentDatabase: "test"}
	f.must(third, "UPDATE items SET v=4 WHERE id=10")
	if got, want := t035Rows(t, f.must(third, "SELECT v FROM items WHERE id=10")), "4;"; got != want {
		t.Fatalf("a later writer read %q, want %q; the timed-out request was still queued", got, want)
	}
}

// TestT035PlainSelectIsUnaffectedByTheLockWaitTimeout covers group 5: after the
// setting is applied, an ordinary SELECT returns the same rows as before and takes
// no lock — a concurrent holder of the row is not consulted at all.
func TestT035PlainSelectIsUnaffectedByTheLockWaitTimeout(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(11,1)")

	holder := &Session{CurrentDatabase: "test"}
	reader := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(holder)
	defer f.engine.CloseSession(reader)

	before := t035Rows(t, f.must(reader, "SELECT id, v FROM items WHERE id=10"))
	f.must(reader, "SET SESSION innodb_lock_wait_timeout = 1")

	// The holder takes the row exclusively and does not release it.
	f.must(holder, "BEGIN")
	f.must(holder, "UPDATE items SET v=2 WHERE id=10")

	// If the plain SELECT locked or waited, the conflict hook would fire.
	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	after := t035Rows(t, f.must(reader, "SELECT id, v FROM items WHERE id=10"))
	select {
	case <-barrier.ready:
		t.Fatal("a plain SELECT took a lock and met the holder's row lock")
	default:
	}
	if after != before {
		t.Fatalf("plain SELECT rows changed after the setting: %q -> %q", before, after)
	}
	if got, want := after, "10,1;"; got != want {
		t.Fatalf("plain SELECT rows = %q, want the snapshot row %q", got, want)
	}

	f.must(holder, "COMMIT")
}
