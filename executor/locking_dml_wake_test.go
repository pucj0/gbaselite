package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/storageengine"
	"sync"
	"testing"
	"time"
)

// locking_dml_wake_test.go holds the SQL-level acceptance tests for the DML lock
// wait/wake contract.
//
// Both tests need to know that a waiter is *queued* before the holder releases, because
// the defect they cover is "the waiter is never woken". Sleeping to guess that ordering
// is forbidden by FR-024, so the ordering is established with a neutral `ShouldWait`
// decision instead: the lock manager consults it, under its own critical section,
// exactly when a conflict with another owner was found and before the request joins the
// queue. Signalling from it therefore proves that the releasing transaction's
// `ReleaseOwner` — which needs the same critical section — cannot run before the
// waiter's queue entry exists.
//
// Everything else is a plain SQL session: the tests drive BEGIN/COMMIT/INSERT/UPDATE and
// never touch a lock-manager type.

// dmlWaitBarrier arms dmlLockRequestHook so that the next conflicting acquisition
// reports itself and still waits, as a transaction without a barrier does.
type dmlWaitBarrier struct {
	once  sync.Once
	ready chan struct{}
	armed bool
}

func newDMLWaitBarrier() *dmlWaitBarrier {
	return &dmlWaitBarrier{ready: make(chan struct{})}
}

// arm installs the hook and returns a release function that restores it.
func (b *dmlWaitBarrier) arm(t *testing.T) {
	t.Helper()
	b.armed = true
	previous := dmlLockRequestHook
	dmlLockRequestHook = func(request storageengine.LockRequest) storageengine.LockRequest {
		return storageengine.LockRequest{
			Components: request.Components,
			ShouldWait: func(context.Context) bool {
				b.once.Do(func() { close(b.ready) })
				return true
			},
		}
	}
	t.Cleanup(func() { dmlLockRequestHook = previous })
}

// wait blocks until a conflicting acquisition reported itself.
func (b *dmlWaitBarrier) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-b.ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached a lock conflict; the statement did not wait", what)
	}
}

// TestB02UpdateWaiterIsWokenByHolderCommit is the acceptance test for the reported
// UPDATE wake failure (tasks.md, .tmp/b02-phase13-dml-validation.md §5.1).
//
// The shape is the recorded reproduction: two transactions update different rows and
// commit, then two transactions update the *same* row. The second target update must
// wait and must be woken by the holder's commit, not by a timeout.
func TestB02UpdateWaiterIsWokenByHolderCommit(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	setup := &Session{}
	must := func(session *Session, sql string) {
		t.Helper()
		if _, err := e.Execute(session, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	must(setup, "CREATE DATABASE test")
	must(setup, "USE test")
	must(setup, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, name VARCHAR(30) UNIQUE)")
	must(setup, "INSERT INTO items VALUES(10,1,'a')")
	must(setup, "INSERT INTO items VALUES(11,1,'b')")

	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(a)
	defer e.CloseSession(b)

	// Round one: different rows. Both must commit without waiting.
	must(a, "BEGIN")
	must(b, "BEGIN")
	must(a, "UPDATE items SET v=2 WHERE id=10")
	must(b, "UPDATE items SET v=2 WHERE id=11")
	must(a, "COMMIT")
	must(b, "COMMIT")

	// Round two: the same row.
	must(a, "BEGIN")
	must(b, "BEGIN")
	must(a, "UPDATE items SET v=3 WHERE id=10")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	waited := make(chan error, 1)
	go func() {
		_, err := e.Execute(b, "UPDATE items SET v=4 WHERE id=10")
		waited <- err
	}()
	barrier.wait(t, "the second UPDATE")

	// The holder ends. From here on the only thing that can release the waiter is the
	// holder's owner release.
	must(a, "COMMIT")

	select {
	case err := <-waited:
		// The candidate this statement selected describes the version it read from its parent
		// snapshot, and the holder has since replaced that version. The contract's answer is the
		// retryable conflict, reported here rather than discovered at commit: the statement must not
		// write over the newer row, and it must not silently rebase its SET onto it either.
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("the woken UPDATE = %v, want the retryable ErrConflict", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second UPDATE was still blocked after the holder committed")
	}
	// The conflict ends the transaction, not just the statement: the rest of the unit of work was
	// computed from the snapshot that has just been proven too old, so it must be retried rather than
	// partially published. COMMIT is therefore refused with the same retryable conflict, and the row
	// the holder committed stays exactly as it is.
	if _, err := e.Execute(b, "COMMIT"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("COMMIT after the stale target = %v, want the retryable ErrConflict", err)
	}
	// ROLLBACK is the statement that performs the retry the caller was asked for.
	if _, err := e.Execute(b, "ROLLBACK"); err != nil {
		t.Fatalf("ROLLBACK after the stale target: %v", err)
	}
	// The holder's value survives, which is the point of the check: the woken statement never wrote.
	row, err := e.Execute(a, "SELECT v FROM items WHERE id=10")
	if err != nil {
		t.Fatal(err)
	}
	if got := t035Rows(t, row); got != "3;" {
		t.Fatalf("row 10 holds %q, want the holder's committed value: the woken statement wrote anyway", got)
	}
}

// TestB02DeleteWaiterIsWokenByHolderCommit is the DELETE half of the same contract.
func TestB02DeleteWaiterIsWokenByHolderCommit(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	setup := &Session{}
	must := func(session *Session, sql string) {
		t.Helper()
		if _, err := e.Execute(session, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	must(setup, "CREATE DATABASE test")
	must(setup, "USE test")
	must(setup, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	for _, id := range []int{10, 11, 12, 13} {
		must(setup, fmt.Sprintf("INSERT INTO items VALUES(%d,1)", id))
	}

	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(a)
	defer e.CloseSession(b)

	must(a, "BEGIN")
	must(b, "BEGIN")
	must(a, "DELETE FROM items WHERE id=12")
	must(b, "DELETE FROM items WHERE id=13")
	must(a, "COMMIT")
	must(b, "COMMIT")

	must(a, "BEGIN")
	must(b, "BEGIN")
	must(a, "DELETE FROM items WHERE id=10")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	waited := make(chan error, 1)
	go func() {
		_, err := e.Execute(b, "DELETE FROM items WHERE id=10")
		waited <- err
	}()
	barrier.wait(t, "the second DELETE")
	must(a, "COMMIT")

	select {
	case err := <-waited:
		// The row this statement's snapshot read is gone, so its candidate names a version that no
		// longer exists. Deleting on that candidate would remove whatever occupies the key now.
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("the woken DELETE = %v, want the retryable ErrConflict", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second DELETE was still blocked after the holder committed")
	}
	// The conflict ends the transaction, so the delete that was abandoned cannot be published either.
	if _, err := e.Execute(b, "COMMIT"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("COMMIT after the stale target = %v, want the retryable ErrConflict", err)
	}
	if _, err := e.Execute(b, "ROLLBACK"); err != nil {
		t.Fatalf("ROLLBACK after the stale target: %v", err)
	}
	// The holder removed the row and nothing put it back.
	row, err := e.Execute(setup, "SELECT COUNT(*) FROM items WHERE id=10")
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Rows) != 1 || row.Rows[0][0] != int64(0) {
		t.Fatalf("row 10 is still present after the holder's delete: %v", row.Rows)
	}
}

// TestB02UpdateWaiterDoesNotBlockUnrelatedRows proves the fix does not serialise
// unrelated rows: the lock is per row, not per transaction.
func TestB02UpdateWaiterDoesNotBlockUnrelatedRows(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	setup := &Session{}
	must := func(session *Session, sql string) {
		t.Helper()
		if _, err := e.Execute(session, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	must(setup, "CREATE DATABASE test")
	must(setup, "USE test")
	must(setup, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	must(setup, "INSERT INTO items VALUES(10,1)")
	must(setup, "INSERT INTO items VALUES(11,1)")

	a := &Session{CurrentDatabase: "test"}
	b := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(a)
	defer e.CloseSession(b)
	must(a, "BEGIN")
	must(b, "BEGIN")
	must(a, "UPDATE items SET v=2 WHERE id=10")
	// A conflict on row 10 would make the barrier fire; this statement must not conflict.
	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	must(b, "UPDATE items SET v=2 WHERE id=11")
	select {
	case <-barrier.ready:
		t.Fatal("an unrelated row conflicted with the holder")
	default:
	}
	must(a, "COMMIT")
	must(b, "COMMIT")
}
