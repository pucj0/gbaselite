package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_read_lifecycle_test.go is P2b: the statement scope's lifecycle around a locking read.
//
// Every case asserts the same consequences, because they are what a leaked or over-eager scope would
// break:
//
//   - nothing is left waiting or granted: a fresh writer can write the rows the statement touched, and
//     it succeeds *without* waiting (a phantom pending request would be granted the moment the holder
//     released, so it would block that writer and time out instead);
//   - a grant the statement took stays with its transaction: another session is still blocked until
//     COMMIT/ROLLBACK, and a later statement of the same transaction still runs and still holds it;
//   - after the transaction ends, the rows are free again.
//
// The engine's counter-level lock diagnostics (`storageengine.LockDiagnostics`) are declared but have
// no implementation in this release, so they cannot be read here; the counter assertions of the same
// invariants live in the adapter tests (`storageengine/mvccadapter`, which reaches the lock manager
// directly). What is asserted here is the observable effect of those counters being right.

// t034RequireWritable proves no lock of a previous statement survived: a fresh session writes the given
// row with a short budget and must succeed rather than wait.
func t034RequireWritable(t *testing.T, f *t035Fixture, what, statement string) {
	t.Helper()
	writer := t034Session(t, f, 1)
	if _, err := f.engine.Execute(writer, statement); err != nil {
		t.Fatalf("%s: the rows are not free: %s: %v", what, statement, err)
	}
}

// TestP2bLockingReadScopeLifecycle walks the seven terminal shapes of a locked statement.
func TestP2bLockingReadScopeLifecycle(t *testing.T) {
	t.Run("binding failure before the source is opened", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
		session := t034Session(t, f, 5)

		// The projection cannot be bound (no such column), so the statement fails while binding: the
		// scope must never have been opened, and the row stays free.
		if _, err := f.engine.Execute(session, "SELECT nope FROM items WHERE id=10 FOR UPDATE"); err == nil {
			t.Fatal("a projection that cannot bind was accepted")
		}
		t034RequireWritable(t, f, "after a binding failure", "UPDATE items SET v=2 WHERE id=10")
	})

	t.Run("open failure with a cancelled context", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
		session := t034Session(t, f, 5)

		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		session.Context = cancelled
		if _, err := f.engine.Execute(session, "SELECT id FROM items WHERE id=10 FOR UPDATE"); err == nil {
			t.Fatal("a locked read with a cancelled context was accepted")
		}
		session.Context = nil
		// The failure path closed the scope it had opened: nothing holds the row.
		t034RequireWritable(t, f, "after a cancelled locked read", "UPDATE items SET v=2 WHERE id=10")
	})

	t.Run("range lock claim times out", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)", "INSERT INTO items VALUES(30,1)")
		holder := t034Session(t, f, 5)
		waiter := t034Session(t, f, 1)

		f.must(holder, "BEGIN")
		f.must(holder, "SELECT id FROM items WHERE id BETWEEN 10 AND 20 FOR UPDATE")
		// The waiter's range claim conflicts with the holder's rows and ends on its budget. The error must
		// be the neutral one: a range scan waits inside the adapter, and a lock-manager sentinel leaking
		// through there would be unrecognisable to a caller that only knows the storage contract.
		if _, err := f.engine.Execute(waiter, "SELECT id FROM items WHERE id BETWEEN 10 AND 20 FOR UPDATE"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("the conflicting range claim returned %v, want the neutral ErrLockWaitTimeout", err)
		}
		// The failed statement left no claim of its own: the holder is still the only owner, and once
		// it rolls back the rows are free for a fresh session.
		f.must(holder, "ROLLBACK")
		t034RequireWritable(t, f, "after a timed-out range claim", "UPDATE items SET v=2 WHERE id=20")
	})

	t.Run("cancel during iteration", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)")
		holder := t034Session(t, f, 5)
		reader := t034Session(t, f, 60)

		f.must(holder, "BEGIN")
		f.must(holder, "UPDATE items SET v=2 WHERE id=10")
		queued := t034ArmWaitHook(t)
		cancelled, cancel := context.WithCancel(context.Background())
		reader.Context = cancelled
		done := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(reader, "SELECT v FROM items WHERE id=10 FOR UPDATE")
			done <- err
		}()
		select {
		case <-queued:
		case <-time.After(10 * time.Second):
			t.Fatal("the locking read never queued")
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a cancelled locking read reported no error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a cancelled locking read never ended")
		}
		reader.Context = nil
		f.must(holder, "COMMIT")
		// A cancelled waiter that had been queued must not be granted when the holder releases: if it
		// were, this writer would block and time out.
		t034RequireWritable(t, f, "after cancelling a queued read", "UPDATE items SET v=3 WHERE id=10")
	})

	t.Run("limit stops the scan early", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)", "INSERT INTO items VALUES(30,1)")
		session := t034Session(t, f, 5)

		// A LIMIT that stops after one row must still close the source's scope. The interval it took is
		// kept for the transaction, so row 20 is *not* necessarily protected; what must hold is that
		// nothing is left waiting and the transaction can end cleanly.
		f.must(session, "BEGIN")
		if got := t035Rows(t, f.must(session, "SELECT id FROM items WHERE id >= 10 LIMIT 1 FOR UPDATE")); got != "10;" {
			t.Fatalf("the limited locked scan returned %q, want one row", got)
		}
		// The row the limited scan did visit stays locked for the transaction.
		writer := t034Session(t, f, 1)
		if _, err := f.engine.Execute(writer, "UPDATE items SET v=2 WHERE id=10"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("a write to the visited row returned %v, want it still locked", err)
		}
		f.must(session, "ROLLBACK")
		t034RequireWritable(t, f, "after the limited statement's transaction ended", "UPDATE items SET v=2 WHERE id=10")
	})

	t.Run("normal end of scan", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)")
		session := t034Session(t, f, 5)

		f.must(session, "BEGIN")
		if got := t035Rows(t, f.must(session, "SELECT id FROM items WHERE id >= 10 FOR UPDATE")); got != "10;20;" {
			t.Fatalf("the locked scan returned %q, want both rows", got)
		}
		// The grants the statement took are still held after the scan reached EOF.
		writer := t034Session(t, f, 1)
		if _, err := f.engine.Execute(writer, "UPDATE items SET v=2 WHERE id=10"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("a write after the scan ended returned %v, want the grant to be kept", err)
		}
		f.must(session, "ROLLBACK")
		t034RequireWritable(t, f, "after the transaction ended", "UPDATE items SET v=2 WHERE id=10")
	})

	t.Run("explicit transaction continues with later statements", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)")
		session := t034Session(t, f, 5)
		writer := t034Session(t, f, 1)

		f.must(session, "BEGIN")
		f.must(session, "SELECT id FROM items WHERE id=10 FOR UPDATE")
		// A later statement in the same transaction still runs, and the first statement's lock is still
		// held: closing that statement's scope must not have released it.
		f.must(session, "SELECT v FROM items WHERE id=20")
		if _, err := f.engine.Execute(writer, "UPDATE items SET v=2 WHERE id=10"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("a write during the transaction returned %v, want the row still locked", err)
		}
		// The lock is re-entrant for its own transaction: a second locking read of the same row works.
		if got := t035Rows(t, f.must(session, "SELECT v FROM items WHERE id=10 FOR UPDATE")); got != "1;" {
			t.Fatalf("the re-entrant locked read returned %q, want the row", got)
		}
		f.must(session, "COMMIT")
		// After COMMIT the writer proceeds, which is what proves the lock lived exactly that long.
		f.must(writer, "UPDATE items SET v=2 WHERE id=10")
	})
}

// TestP2bRefusedPlanStillLeavesNothingBehind proves the refusal path of the plan check, which happens
// before any operator or scope exists: the statement fails and the rows are untouched.
func TestP2bRefusedPlanStillLeavesNothingBehind(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "CREATE TABLE other(id BIGINT PRIMARY KEY)")
	f.must(&Session{}, "USE test")
	session := t034Session(t, f, 5)

	if _, err := f.engine.Execute(session, "SELECT i.id FROM items i JOIN other o ON i.id=o.id FOR UPDATE"); err == nil {
		t.Fatal("a locking read over a join was accepted")
	} else if !strings.Contains(err.Error(), "locking") {
		t.Fatalf("the refusal does not name row locking: %v", err)
	}
	t034RequireWritable(t, f, "after a refused shape", "UPDATE items SET v=2 WHERE id=10")
}
