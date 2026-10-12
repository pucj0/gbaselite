package executor

import (
	"errors"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_stale_version_test.go is the version-validation suite for locked DML targets.
//
// The previous round compared the row the statement's snapshot read with the row its claim covers as
// *bytes*. That catches a change and a change back to the same bytes not at all: (1,7) 鈫?(1,8) 鈫?(1,7)
// compares equal while the row was in fact rewritten twice after the statement's readTS, and both the
// row's version and every index entry that pointed at it were replaced. The same hole existed for a
// referential action whose child row was deleted (the comparison was skipped when the row was absent)
// and for a primary key that was deleted and re-inserted.
//
// Every test below drives real SQL and checks the contract's answer: a retryable conflict for a target
// whose newest committed change is newer than the statement's snapshot, no write over the newer version,
// and unchanged current read-your-writes semantics for the statement's own staged rows.

// t02VersionFixture is the shared table for the ABA tests.
func t02VersionFixture(t *testing.T) *t035Fixture {
	t.Helper()
	return newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1,7)")
}

// TestP2UpsertRejectsAnABAChangeAfterTheSnapshot is task A: the row is changed and changed back while the
// statement waits, so its bytes match the snapshot again while its version does not.
func TestP2UpsertRejectsAnABAChangeAfterTheSnapshot(t *testing.T) {
	f := t02VersionFixture(t)
	first := t034Session(t, f, 5)
	second := t034Session(t, f, 5)
	upsert := t034Session(t, f, 5)

	// T2 takes its snapshot here, before either rewrite.
	f.must(upsert, "BEGIN")

	// (1,7) -> (1,8) -> (1,7): the bytes are back to what T2 read, the version is not.
	f.must(first, "UPDATE items SET v=8 WHERE id=1")
	f.must(second, "UPDATE items SET v=7 WHERE id=1")

	if _, err := f.engine.Execute(upsert, "INSERT INTO items VALUES(1,7) ON DUPLICATE KEY UPDATE v=9"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the ABA upsert returned %v, want the retryable ErrConflict: equal bytes are not an equal version", err)
	}
	f.must(upsert, "ROLLBACK")

	// Neither rewrite was overwritten by the statement that could not validate its target.
	if got := t035Rows(t, f.must(second, "SELECT id,v FROM items")); got != "1,7;" {
		t.Fatalf("the final row is %q, want the second rewrite's value", got)
	}
}

// TestP2UpsertRejectsAReinsertedPrimaryKey is the same rule for a key that disappears and comes back:
// the row that owns it now is a different version of the key than the one the snapshot saw.
func TestP2UpsertRejectsAReinsertedPrimaryKey(t *testing.T) {
	f := t02VersionFixture(t)
	holder := t034Session(t, f, 5)
	upsert := t034Session(t, f, 5)

	barrier := newDMLWaitBarrier()
	barrier.arm(t)

	f.must(upsert, "BEGIN")

	// T1 owns the row, and rewrites the key entirely: the row is removed and a new one installed under
	// the same primary key and the same value.
	f.must(holder, "BEGIN")
	f.must(holder, "SELECT v FROM items WHERE id=1 FOR UPDATE")
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(upsert, "INSERT INTO items VALUES(1,7) ON DUPLICATE KEY UPDATE v=9")
		done <- err
	}()
	barrier.wait(t, "the upsert's row lock")

	f.must(holder, "DELETE FROM items WHERE id=1")
	f.must(holder, "INSERT INTO items VALUES(1,7)")
	f.must(holder, "COMMIT")

	select {
	case err := <-done:
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("the re-inserted key was accepted (%v), want the retryable ErrConflict", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the upsert never resumed after the holder committed")
	}
	f.must(upsert, "ROLLBACK")
	if got := t035Rows(t, f.must(holder, "SELECT id,v FROM items")); got != "1,7;" {
		t.Fatalf("the re-inserted row was overwritten: %q", got)
	}
}

// TestP2UpsertRejectsAUniqueValueThatCameBack is task C's ABA on the unique side: the value the snapshot
// saw is owned by the same row again, byte for byte, but the reservation itself was replaced.
func TestP2UpsertRejectsAUniqueValueThatCameBack(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, UNIQUE KEY uq_v(v))",
		"INSERT INTO items VALUES(1,7)")
	holder := t034Session(t, f, 5)
	upsert := t034Session(t, f, 5)

	barrier := newDMLWaitBarrier()
	barrier.arm(t)

	f.must(upsert, "BEGIN")
	f.must(upsert, "SELECT v FROM items WHERE id=1") // the snapshot that sees (1,7)

	// The holder frees the reservation for 7 and gives it straight back to the same row inside one
	// transaction, so by the time the waiter is granted the entry exists again with the same owner value
	// and a newer version. Doing both steps in one transaction is what makes the ABA deterministic: the
	// waiter is granted the moment the holder releases, so no third transaction can interleave.
	f.must(holder, "BEGIN")
	f.must(holder, "UPDATE items SET v=8 WHERE id=1")
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(upsert, "INSERT INTO items VALUES(2,7) ON DUPLICATE KEY UPDATE v=9")
		done <- err
	}()
	barrier.wait(t, "the upsert's reservation")
	f.must(holder, "UPDATE items SET v=7 WHERE id=1")
	f.must(holder, "COMMIT")

	select {
	case err := <-done:
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("the restored unique value was accepted (%v), want the retryable ErrConflict", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the upsert never resumed")
	}
	f.must(upsert, "ROLLBACK")
	// Row 1 still owns v=7 and no candidate row appeared.
	if got := t035Rows(t, f.must(holder, "SELECT id,v FROM items ORDER BY id")); got != "1,7;" {
		t.Fatalf("the final rows are %q, want only the restored row", got)
	}
}

// TestP2UpsertAcceptsItsOwnStagedOverlay proves the version rule does not reject read-your-writes: a row
// this transaction staged itself has no committed version newer than the snapshot, so the upsert must
// proceed and update it.
func TestP2UpsertAcceptsItsOwnStagedOverlay(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	session := t034Session(t, f, 5)

	f.must(session, "BEGIN")
	f.must(session, "INSERT INTO items VALUES(1,7)")
	// The duplicate the second statement finds is this transaction's own staged row.
	f.must(session, "INSERT INTO items VALUES(1,7) ON DUPLICATE KEY UPDATE v=9")
	f.must(session, "COMMIT")
	if got := t035Rows(t, f.must(session, "SELECT id,v FROM items")); got != "1,9;" {
		t.Fatalf("the overlay upsert left %q, want the updated staged row", got)
	}
}

// t02CascadeFixture builds a parent and one cascading child whose rewritten column carries no index.
//
// The indexes matter here: a holder that deletes the child needs the child's index entries too, and
// another transaction's pending cascading request claims the entries of the row it would write. With a
// unique index on the column the cascade rewrites, the two requests meet on the same entry and the holder
// queues behind the waiter — a queue-order deadlock that has nothing to do with the version check under
// test. Leaving the rewritten column unindexed keeps the wait on the row itself.
func t02CascadeFixture(t *testing.T) *t035Fixture {
	t.Helper()
	return newT035Fixture(t,
		"CREATE TABLE parent(id BIGINT PRIMARY KEY, v INT)",
		"CREATE TABLE child(id BIGINT PRIMARY KEY, pid BIGINT, note VARCHAR(32), "+
			"KEY idx_pid(pid), FOREIGN KEY (pid) REFERENCES parent(id) ON UPDATE CASCADE ON DELETE CASCADE)",
		"INSERT INTO parent VALUES(1,10)",
		"INSERT INTO child VALUES(100,1,'keep')",
	)
}

// TestP2CascadeConflictsWhenTheChildWasDeletedWhileWaiting is task B: the derived write expects a child
// row, the wait ends with that row gone, and the cascade must report the conflict instead of writing the
// row back into existence.
func TestP2CascadeConflictsWhenTheChildWasDeletedWhileWaiting(t *testing.T) {
	f := t02CascadeFixture(t)
	holder := t034Session(t, f, 10)
	cascade := t034Session(t, f, 10)

	barrier := newDMLWaitBarrier()
	barrier.arm(t)

	f.must(holder, "BEGIN")
	f.must(holder, "SELECT id FROM child WHERE id=100 FOR UPDATE")
	// The holder finishes its own write before the waiter queues: the waiter is still queued while the
	// holder owns the row (which is what this test needs), and the holder never asks for a second claim on
	// that row while a conflicting request is already waiting for it — that shape is a queue-order
	// deadlock of its own, reported separately in this round's report.
	f.must(holder, "DELETE FROM child WHERE id=100")

	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(cascade, "UPDATE parent SET id=2 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "the cascaded child write")

	// The child the cascade is about to write is gone, and the holder commits while the cascade waits.
	f.must(holder, "COMMIT")

	select {
	case err := <-done:
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("the cascade over a deleted child returned %v, want the retryable ErrConflict", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cascade never resumed")
	}

	// The deleted child was not written back, and the parent key did not move.
	if got := t035Rows(t, f.must(cascade, "SELECT id FROM child WHERE id=100")); got != "" {
		t.Fatalf("the cascade resurrected the deleted child row: %q", got)
	}
	if got := t035Rows(t, f.must(cascade, "SELECT id FROM parent")); got != "1;" {
		t.Fatalf("the parent key moved despite the conflict: %q", got)
	}
}

// TestP2CascadeRejectsAnABAChangeWhileWaiting covers the same hole for a referential action: the child's
// note is changed and restored inside the holder's transaction, so when the cascade is granted the row is
// byte-identical to the one it read and a newer version at the same time.
func TestP2CascadeRejectsAnABAChangeWhileWaiting(t *testing.T) {
	f := t02CascadeFixture(t)
	holder := t034Session(t, f, 10)
	cascade := t034Session(t, f, 10)

	barrier := newDMLWaitBarrier()
	barrier.arm(t)

	f.must(holder, "BEGIN")
	f.must(holder, "SELECT id FROM child WHERE id=100 FOR UPDATE")
	// The change and its restoration happen before the waiter queues, for the same queue-order reason as
	// the deleted-child test above.
	f.must(holder, "UPDATE child SET note='changed' WHERE id=100")
	f.must(holder, "UPDATE child SET note='keep' WHERE id=100")

	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(cascade, "UPDATE parent SET id=2 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "the cascaded child write")

	f.must(holder, "COMMIT")

	select {
	case err := <-done:
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("the ABA cascade returned %v, want the retryable ErrConflict", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cascade never resumed")
	}
	rows := f.must(cascade, "SELECT pid,note FROM child WHERE id=100")
	if len(rows.Rows) != 1 || rows.Rows[0][0] != int64(1) || rows.Rows[0][1] != "keep" {
		t.Fatalf("the child row is %#v, want its original parent and note", rows.Rows)
	}
	if got := t035Rows(t, f.must(cascade, "SELECT id FROM parent")); got != "1;" {
		t.Fatalf("the parent key moved despite the conflict: %q", got)
	}
}

// TestP2UpsertResolvesPKAndUniqueHitsOnTheSameRow covers task C's remaining shapes: one row hit by both
// the candidate's primary key and one of its unique values resolves to a single target, while a value
// whose owner moved to another row cannot be resolved from this statement's snapshot at all.
func TestP2UpsertResolvesPKAndUniqueHitsOnTheSameRow(t *testing.T) {
	t.Run("primary key and unique value name one row", func(t *testing.T) {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, UNIQUE KEY uq_v(v))",
			"INSERT INTO items VALUES(1,7)")
		session := t034Session(t, f, 5)

		// The candidate collides on both the key and the value, and both name row 1: the statement updates
		// it once, not twice.
		f.must(session, "INSERT INTO items VALUES(1,7) ON DUPLICATE KEY UPDATE v=9")
		if got := t035Rows(t, f.must(session, "SELECT id,v FROM items")); got != "1,9;" {
			t.Fatalf("the row is %q, want a single updated row", got)
		}
	})

	t.Run("unique owner moved to another row while waiting", func(t *testing.T) {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, UNIQUE KEY uq_v(v))",
			"INSERT INTO items VALUES(1,7)")
		holder := t034Session(t, f, 10)
		upsert := t034Session(t, f, 10)

		barrier := newDMLWaitBarrier()
		barrier.arm(t)

		f.must(upsert, "BEGIN")
		f.must(upsert, "SELECT v FROM items WHERE id=1")

		// The holder takes the value away from row 1 and gives it to row 2, all before releasing it.
		f.must(holder, "BEGIN")
		f.must(holder, "UPDATE items SET v=8 WHERE id=1")
		f.must(holder, "INSERT INTO items VALUES(2,7)")
		done := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(upsert, "INSERT INTO items VALUES(3,7) ON DUPLICATE KEY UPDATE v=9")
			done <- err
		}()
		barrier.wait(t, "the upsert's reservation")
		f.must(holder, "COMMIT")

		select {
		case err := <-done:
			if !errors.Is(err, storageengine.ErrConflict) {
				t.Fatalf("the transferred unique value returned %v, want the retryable ErrConflict", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the upsert never resumed")
		}
		f.must(upsert, "ROLLBACK")
		// Neither row 1 nor row 2 was rewritten by the statement that could not validate its target.
		if got := t035Rows(t, f.must(holder, "SELECT id,v FROM items ORDER BY id")); got != "1,8;2,7;" {
			t.Fatalf("the final rows are %q, want the holder's two rows", got)
		}
	})
}
