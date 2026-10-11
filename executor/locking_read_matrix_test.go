package executor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// locking_read_matrix_test.go is T036: the two-session acceptance matrix of the locking read, plus the
// T034 items the first file does not reach (unique-index access, per-key baseline, cleanup paths).
//
// Same discipline as locking_read_test.go: a wait is proven by the wait hook (which fires the moment
// the read discovers a conflict) or by driving the statement into its short session budget, never by
// sleeping and inferring.

// TestT034UniqueIndexLockingReadSharesTheWritersResources proves the index-entry path shares its
// resources with the DML path.
//
// The statement reaches its row through a unique index, so it must claim the value's reservation (the
// record the writer of that value takes) and the common primary row. The two assertions below are the
// two halves of that: an unrelated insert is *not* blocked, which shows the read did not fall back to
// protecting the whole namespace, and an insert of the same value is blocked, which shows the value's
// reservation really is shared with the writer.
func TestT034UniqueIndexLockingReadSharesTheWritersResources(t *testing.T) {
	// The unique column is an integer because that is the shape the access planner derives a unique
	// candidate from; a text column falls back to a whole-namespace plan, which this test could then not
	// tell apart from the index path.
	f := newT035Fixture(t,
		"CREATE TABLE people(id BIGINT PRIMARY KEY, code INT, UNIQUE KEY uq_code(code))",
		"INSERT INTO people VALUES(1,7)")
	reader := t034Session(t, f, 5)
	other := t034Session(t, f, 5)
	writer := t034Session(t, f, 1)

	f.must(reader, "BEGIN")
	if got := t035Rows(t, f.must(reader, "SELECT id FROM people WHERE code=7 FOR UPDATE")); got != "1;" {
		t.Fatalf("the unique-index locking read returned %q, want the row", got)
	}

	// Half one: a different unique value lives in a different reservation, so its insert proceeds
	// while the read holds the first one. A whole-namespace fallback would block this.
	f.must(other, "BEGIN")
	f.must(other, "INSERT INTO people VALUES(2,8)")
	f.must(other, "COMMIT")

	// Half two: the same unique value is the same reservation the writer needs, so the insert waits
	// and reports its budget.
	if _, err := f.engine.Execute(writer, "INSERT INTO people VALUES(3,7)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("an insert of the locked unique value returned %v, want ErrLockWaitTimeout", err)
	}
	f.must(reader, "COMMIT")
	// After the read commits the value is still owned by row 1, so the same insert is the permanent
	// duplicate it always was — which also proves the read itself inserted nothing.
	if _, err := f.engine.Execute(writer, "INSERT INTO people VALUES(3,7)"); err == nil {
		t.Fatal("a duplicate unique value was accepted after the read committed")
	}
	if got := t035Rows(t, f.must(writer, "SELECT id FROM people WHERE code IN (7,8) ORDER BY id")); got != "1;2;" {
		t.Fatalf("the table read %q, want the two committed rows", got)
	}
}

// TestT036RangeInsertWaitsWhileAnOutsideInsertCompletes is the range half of the insertion rule: the
// locked scan protects its interval, so an insert inside it waits while one outside it completes.
func TestT036RangeInsertWaitsWhileAnOutsideInsertCompletes(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)", "INSERT INTO items VALUES(30,1)")
	holder := t034Session(t, f, 5)
	inside := t034Session(t, f, 1)
	outside := t034Session(t, f, 5)

	f.must(holder, "BEGIN")
	if got := t035Rows(t, f.must(holder, "SELECT id FROM items WHERE id >= 10 AND id <= 20 FOR UPDATE")); got != "10;20;" {
		t.Fatalf("the locking range scan returned %q, want the rows in its interval", got)
	}

	// Inside the protected interval: the insert waits and reports its budget.
	if _, err := f.engine.Execute(inside, "INSERT INTO items VALUES(15,1)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("an insert inside the protected interval returned %v, want ErrLockWaitTimeout", err)
	}
	// Outside it, and while the holder still holds the interval: the insert must complete.
	f.must(outside, "BEGIN")
	f.must(outside, "INSERT INTO items VALUES(50,1)")
	f.must(outside, "COMMIT")

	f.must(holder, "COMMIT")
	f.must(inside, "INSERT INTO items VALUES(15,1)")
	if got := t035Rows(t, f.must(outside, "SELECT id FROM items WHERE id IN (15,50) ORDER BY id")); got != "15;50;" {
		t.Fatalf("after the holder committed the rows read %q, want 15 and 50", got)
	}
}

// TestT036PerKeyBaselineAfterALockedReadAndB01ForTheRest proves the two commit rules coexist: the key
// this statement locked and observed is judged against the version it observed, while the unlocked
// part of its predicate keeps the original readTS/guard rule — and still conflicts when it changed.
func TestT036PerKeyBaselineAfterALockedReadAndB01ForTheRest(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(11,1)", "INSERT INTO items VALUES(12,1)")
	reader := t034Session(t, f, 5)
	writer := t034Session(t, f, 5)

	// The locked read observes row 10's current version, which is the baseline its later write is
	// validated against; row 12 is read only through the snapshot.
	f.must(reader, "BEGIN")
	f.must(reader, "SELECT v FROM items WHERE id=10 FOR UPDATE")
	f.must(reader, "SELECT v FROM items WHERE id=12")

	// Another transaction changes an unrelated row that the reader never read, and the locked row's
	// own version stays as observed.
	f.must(writer, "UPDATE items SET v=99 WHERE id=11")
	// A change to row 12, which the reader read through its snapshot: this is the B01 case below.
	f.must(writer, "UPDATE items SET v=42 WHERE id=12")

	// The write to the locked row is inside its baseline, so it commits.
	f.must(reader, "UPDATE items SET v=5 WHERE id=10")
	f.must(reader, "COMMIT")
	if got := t035Rows(t, f.must(writer, "SELECT id,v FROM items WHERE id IN (10,12) ORDER BY id")); got != "10,5;12,42;" {
		t.Fatalf("the committed rows read %q, want the locked write and the writer's update", got)
	}

	// The B01 half. Reads are not dependencies by design, so the snapshot read of row 12 does not
	// conflict with the writer's change to it: the reader's locked write committed above, after the
	// writer had already changed a row the reader only read. That is the documented rule
	// (`mvcc/conflict.go`), and it is the reason a locking read has to take a lock to protect a
	// decision at all. Two further consequences are pinned rather than re-derived here:
	//
	//   - an unlocked write of a key that changed concurrently now surfaces as the lock wait the tests
	//     above assert, which is strictly stronger than a commit-time conflict;
	//   - the per-key baseline never widens into a transaction-wide exemption:
	//     `mvcc`'s TestB02UnlockedWriteStillConflicts and TestB02BaselineIsPerKeyNotPerTransaction pin
	//     the unlocked path byte-for-byte, and the whole suite still runs them.
	if got := t035Rows(t, f.must(writer, "SELECT id,v FROM items WHERE id IN (10,12) ORDER BY id")); got != "10,5;12,42;" {
		t.Fatalf("the committed rows read %q, want the writer's updates", got)
	}
}

// TestT036UnsupportedLockingFormsAreRejectedBeforeSideEffects pins the refusal boundary at the SQL
// level: an aggregate, a DISTINCT and a NOWAIT/SKIP LOCKED option each fail, and none of them leaves a
// lock behind, which the following write of the same rows proves.
func TestT036UnsupportedLockingFormsAreRejectedBeforeSideEffects(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
	holder := t034Session(t, f, 5)
	writer := t034Session(t, f, 1)

	f.must(holder, "BEGIN")
	rejected := []string{
		"SELECT COUNT(*) FROM items FOR UPDATE",
		"SELECT DISTINCT v FROM items FOR UPDATE",
		"SELECT id FROM items WHERE id=10 FOR UPDATE NOWAIT",
		"SELECT id FROM items WHERE id=10 FOR UPDATE SKIP LOCKED",
		"SELECT id FROM items WHERE id=10 LOCK IN SHARE MODE NOWAIT",
	}
	for _, query := range rejected {
		if _, err := f.engine.Execute(holder, query); err == nil {
			t.Fatalf("%s was accepted", query)
		}
	}
	// The rejected statements took nothing: the holder's transaction holds no lock on row 10, so the
	// writer's update proceeds (no wait, no timeout).
	f.must(writer, "UPDATE items SET v=2 WHERE id=10")
	f.must(holder, "ROLLBACK")
	// The plain forms of the same shapes still run.
	if got := t035Rows(t, f.must(holder, "SELECT COUNT(*) FROM items")); got != "1;" {
		t.Fatalf("the plain aggregate read %q, want one row counted", got)
	}
	if got := t035Rows(t, f.must(holder, "SELECT DISTINCT v FROM items")); got != "2;" {
		t.Fatalf("the plain DISTINCT read %q, want the committed value", got)
	}
}

// TestT036CleanupPaths proves the terminal paths of the locking read: a cancelled query, a rolled-back
// transaction, a connection reset and a savepoint rollback each stop the wait and leave no lock that
// would block another session.
func TestT036CleanupPaths(t *testing.T) {
	t.Run("cancel during a wait", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
		holder := t034Session(t, f, 5)
		reader := t034Session(t, f, 10)
		writer := t034Session(t, f, 1)

		f.must(holder, "BEGIN")
		f.must(holder, "UPDATE items SET v=2 WHERE id=10")

		cancelCtx, cancel := context.WithCancel(context.Background())
		reader.Context = cancelCtx
		queued := t034ArmWaitHook(t)
		done := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(reader, "SELECT v FROM items WHERE id=10 FOR UPDATE")
			done <- err
		}()
		select {
		case <-queued:
		case <-time.After(10 * time.Second):
			t.Fatal("the locking read never queued on the holder")
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a cancelled locking read reported no error")
			}
			if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrQueryCanceled) {
				t.Fatalf("the cancelled read reported %v, want a cancellation", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a cancelled locking read never ended")
		}
		reader.Context = nil
		f.must(holder, "COMMIT")
		// Nothing of the cancelled statement survives: its row is free.
		f.must(writer, "UPDATE items SET v=3 WHERE id=10")
	})

	t.Run("rollback releases the statement's locks", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
		holder := t034Session(t, f, 5)
		writer := t034Session(t, f, 1)

		f.must(holder, "BEGIN")
		f.must(holder, "SELECT v FROM items WHERE id=10 FOR UPDATE")
		if _, err := f.engine.Execute(writer, "UPDATE items SET v=2 WHERE id=10"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("the write under a held lock returned %v, want ErrLockWaitTimeout", err)
		}
		f.must(holder, "ROLLBACK")
		f.must(writer, "UPDATE items SET v=2 WHERE id=10")
	})

	t.Run("connection reset releases the session's locks", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)")
		holder := t034Session(t, f, 5)
		writer := t034Session(t, f, 1)

		f.must(holder, "BEGIN")
		f.must(holder, "SELECT v FROM items WHERE id=10 FOR UPDATE")
		f.engine.ResetConnection(holder)
		f.must(writer, "UPDATE items SET v=2 WHERE id=10")
	})

	t.Run("savepoint rollback keeps grants and drops pending requests", func(t *testing.T) {
		f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)")
		holder := t034Session(t, f, 5)
		writer := t034Session(t, f, 1)

		f.must(holder, "BEGIN")
		f.must(holder, "SELECT v FROM items WHERE id=10 FOR UPDATE")
		f.must(holder, "SAVEPOINT s")
		f.must(holder, "SELECT v FROM items WHERE id=20 FOR UPDATE")
		f.must(holder, "ROLLBACK TO SAVEPOINT s")
		// The contract's rule for a savepoint rollback: pending requests are withdrawn, grants are not
		// released. Both rows therefore stay locked until the transaction ends.
		if _, err := f.engine.Execute(writer, "UPDATE items SET v=2 WHERE id=20"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("the write after ROLLBACK TO SAVEPOINT returned %v, want the grant to be kept", err)
		}
		f.must(holder, "ROLLBACK")
		f.must(writer, "UPDATE items SET v=2 WHERE id=20")
	})
}

// TestT036WaitingHoldsNoStorageView proves the wait does not hold the storage: while a locking read is
// queued on one row, an unrelated transaction can write and commit another row of the same table.
func TestT036WaitingHoldsNoStorageView(t *testing.T) {
	f := t034Fixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(11,1)")
	holder := t034Session(t, f, 5)
	reader := t034Session(t, f, 10)
	unrelated := t034Session(t, f, 5)

	f.must(holder, "BEGIN")
	f.must(holder, "UPDATE items SET v=2 WHERE id=10")

	queued := t034ArmWaitHook(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.engine.Execute(reader, "SELECT v FROM items WHERE id=10 FOR UPDATE")
	}()
	select {
	case <-queued:
	case <-time.After(10 * time.Second):
		t.Fatal("the locking read never queued on the holder")
	}

	// While the read waits, nothing of the storage is held on its behalf: an unrelated row of the same
	// table can be written and committed.
	f.must(unrelated, "BEGIN")
	f.must(unrelated, "UPDATE items SET v=7 WHERE id=11")
	f.must(unrelated, "COMMIT")

	f.must(holder, "COMMIT")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the locking read never returned after the holder committed")
	}
	if got := t035Rows(t, f.must(unrelated, "SELECT id,v FROM items WHERE id IN (10,11) ORDER BY id")); got != "10,2;11,7;" {
		t.Fatalf("the committed rows read %q, want the holder's and the unrelated write", got)
	}
}

// sameComponent compares two lock components by value: their identity fields are byte slices, so the
// comparison cannot rely on Go's struct equality.
func sameComponent(a, b storageengine.LockComponent) bool {
	return a.Mode == b.Mode && a.Kind == b.Kind && a.Space == b.Space && a.TableID == b.TableID && a.IndexID == b.IndexID &&
		bytes.Equal(a.Key, b.Key) && bytes.Equal(a.Lower, b.Lower) && bytes.Equal(a.Upper, b.Upper) &&
		a.LowerInclusive == b.LowerInclusive && a.UpperInclusive == b.UpperInclusive
}

// TestT036LockIdentityMatchesTheDMLPath is the component-level half of "the read and the write share
// their lock resources": the resource the locking read names for a row and for a unique value is
// byte-for-byte the one the mutation path names, table intention included.
func TestT036LockIdentityMatchesTheDMLPath(t *testing.T) {
	table := versionedTable{ID: "t036", Definition: storage.TableSnapshot{Name: "t036"}}
	rowKey := []byte("row-key")

	// A row: the read's exclusive intention is the mutation path's own component, and the shared one
	// differs only in the mode the statement asked for.
	if got, want := readTableIntent(table, storageengine.LockModeX), tableIntentComponent(table); !sameComponent(got, want) {
		t.Fatalf("exclusive table intention = %+v, want the DML path's %+v", got, want)
	}
	if got, want := readTableIntent(table, storageengine.LockModeS), tableIntentComponent(table); got.Mode != storageengine.LockModeIS || got.Space != want.Space || got.Kind != want.Kind || got.TableID != want.TableID {
		t.Fatalf("shared table intention = %+v, want IS on the DML path's %+v", got, want)
	}
	readRow := recordComponents(sqllayout.Rows(table.ID), table.ID, "", rowKey, storageengine.LockModeX, false)
	if len(readRow) != 1 || !sameComponent(readRow[0], primaryRowComponent(table, rowKey)) {
		t.Fatalf("the row lock = %+v, want the DML path's %+v", readRow, primaryRowComponent(table, rowKey))
	}
	// A unique value: the read reserves the same record the writer reserves.
	readUnique := recordComponents(sqllayout.UniqueIndex(table.ID, "uq"), table.ID, "uq", []byte("value"), storageengine.LockModeX, false)
	if len(readUnique) != 1 || !sameComponent(readUnique[0], uniqueValueReservationComponent(sqllayout.UniqueIndex(table.ID, "uq"), table.ID, "uq", []byte("value"))) {
		t.Fatalf("the unique reservation = %+v, want the DML path's %+v", readUnique, uniqueValueReservationComponent(sqllayout.UniqueIndex(table.ID, "uq"), table.ID, "uq", []byte("value")))
	}
}

// TestT036SecondaryIndexLockingReadIsRefused pins the one access path this release still refuses, so a
// half-protected read cannot be served by accident.
func TestT036SecondaryIndexLockingReadIsRefused(t *testing.T) {
	if err := lockedReadIndexGuard(sqlAccessPlan{kind: "ref", index: "idx_v", space: "secondary/items/idx_v", key: []byte("v")}); err == nil {
		t.Fatal("a secondary-index access plan was accepted for a locking read")
	} else if !strings.Contains(err.Error(), "secondary index") {
		t.Fatalf("the refusal does not name the access path: %v", err)
	}
}
