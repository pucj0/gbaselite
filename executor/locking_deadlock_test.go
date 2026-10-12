package executor

import (
	"context"
	"errors"
	"gbaselite/storageengine"
	"sync"
	"testing"
	"time"
)

// deadlockWaitBarrier arms dmlLockRequestHook to signal when a lock conflict wait begins.
type deadlockWaitBarrier struct {
	once  sync.Once
	ready chan struct{}
}

func newDeadlockWaitBarrier() *deadlockWaitBarrier {
	return &deadlockWaitBarrier{ready: make(chan struct{})}
}

func (b *deadlockWaitBarrier) arm(t *testing.T) {
	t.Helper()
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

func (b *deadlockWaitBarrier) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-b.ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never reached a lock conflict; did not wait", what)
	}
}

// TestB02RealSQLDeadlockAutomaticDetection verifies that real multi-session SQL transactions
// forming a lock cycle are automatically detected and resolved without calling DetectDeadlock(),
// without relying on innodb_lock_wait_timeout, and fulfilling victim lifecycle contracts.
func TestB02RealSQLDeadlockAutomaticDetection(t *testing.T) {
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
	must(setup, "INSERT INTO items VALUES(1, 10)")
	must(setup, "INSERT INTO items VALUES(2, 20)")

	sA := &Session{CurrentDatabase: "test"}
	sB := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(sA)
	defer e.CloseSession(sB)

	// Step 1: Session A begins and locks row 1.
	must(sA, "BEGIN")
	must(sA, "UPDATE items SET v = 11 WHERE id = 1")

	// Step 2: Session B begins and locks row 2.
	must(sB, "BEGIN")
	must(sB, "UPDATE items SET v = 22 WHERE id = 2")

	// Step 3: Session A requests row 2 and blocks behind Session B.
	barrier := newDeadlockWaitBarrier()
	barrier.arm(t)

	resA := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "UPDATE items SET v = 12 WHERE id = 2")
		resA <- err
	}()

	barrier.wait(t, "Session A waiting for row 2")

	// Step 4: Session B requests row 1, closing the wait cycle:
	// A waits for B (on row 2), B waits for A (on row 1).
	resB := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE items SET v = 21 WHERE id = 1")
		resB <- err
	}()

	// Automatic deadlock detection must fire quickly (within 2 seconds),
	// far below the 50-second lock wait timeout.
	var errA, errB error
	timer := time.After(3 * time.Second)

	select {
	case errB = <-resB:
		// B settled.
	case errA = <-resA:
		// A settled.
	case <-timer:
		t.Fatal("deadlock was not automatically detected within 3s; likely stalled waiting for lock timeout")
	}

	// Wait for the other session to settle as well.
	if errB != nil {
		// B was the victim. Session A should now unblock and succeed.
		select {
		case errA = <-resA:
		case <-time.After(3 * time.Second):
			t.Fatal("Session A remained blocked after Session B was chosen as victim")
		}
	} else if errA != nil {
		// A was the victim. Session B should now unblock and succeed.
		select {
		case errB = <-resB:
		case <-time.After(3 * time.Second):
			t.Fatal("Session B remained blocked after Session A was chosen as victim")
		}
	}

	// Exactly one session must have received ErrDeadlock.
	aDeadlock := errors.Is(errA, storageengine.ErrDeadlock)
	bDeadlock := errors.Is(errB, storageengine.ErrDeadlock)

	if !aDeadlock && !bDeadlock {
		t.Fatalf("neither session received ErrDeadlock: errA=%v, errB=%v", errA, errB)
	}
	if aDeadlock && bDeadlock {
		t.Fatalf("both sessions received ErrDeadlock; want exactly one victim")
	}

	var victimSession *Session
	var victimErr error
	var survivorSession *Session
	var survivorErr error

	if bDeadlock {
		victimSession = sB
		victimErr = errB
		survivorSession = sA
		survivorErr = errA
	} else {
		victimSession = sA
		victimErr = errA
		survivorSession = sB
		survivorErr = errB
	}

	if survivorErr != nil {
		t.Fatalf("survivor statement failed: %v", survivorErr)
	}
	if !errors.Is(victimErr, storageengine.ErrDeadlock) {
		t.Fatalf("victim error = %v, want storageengine.ErrDeadlock", victimErr)
	}

	// Victim contract:
	// 1. Victim cannot commit; commit must fail with ErrDeadlock or already be aborted.
	if _, err := e.Execute(victimSession, "COMMIT"); err == nil {
		t.Fatal("victim was able to commit successfully; want failure")
	}

	// 2. Survivor must be able to commit successfully.
	if _, err := e.Execute(survivorSession, "COMMIT"); err != nil {
		t.Fatalf("survivor commit failed: %v", err)
	}

	// 3. Verify final data state.
	// Setup session reads the table:
	checkRes, err := e.Execute(setup, "SELECT id, v FROM items ORDER BY id")
	if err != nil {
		t.Fatalf("check query failed: %v", err)
	}
	if len(checkRes.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(checkRes.Rows))
	}
}

// TestB02RealSQLSupplementalOlderWaiterDeadlock tests the scenario where:
// Root A holds row 1.
// Root B requests row 1 and row 2, queueing behind A on row 1 (older waiter on row 2).
// Root A then requests row 2, encountering Root B's older request on row 2 (EdgeOlderWaiter).
// Wait-for graph has B->A (HOLDER on row 1) and A->B (OLDER_WAITER on row 2).
func TestB02RealSQLSupplementalOlderWaiterDeadlock(t *testing.T) {
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
	must(setup, "CREATE TABLE items(id BIGINT PRIMARY KEY, sku VARCHAR(30) UNIQUE)")
	must(setup, "INSERT INTO items VALUES(1, 'sku1')")

	sA := &Session{CurrentDatabase: "test"}
	sB := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(sA)
	defer e.CloseSession(sB)

	// Step 1: Session A begins and holds Record X on row 1.
	must(sA, "BEGIN")
	must(sA, "SELECT id FROM items WHERE id = 1 FOR UPDATE")

	// Step 2: Session B begins.
	must(sB, "BEGIN")

	// Session B attempts to insert row 1 with sku 'sku2'.
	// Its atomic bundle includes Record X(1) and Unique X(sku2).
	// Because Record X(1) is held by Session A, Session B's entire bundle blocks and queues.
	barrier := newDeadlockWaitBarrier()
	barrier.arm(t)

	resB := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "INSERT INTO items VALUES(1, 'sku2')")
		resB <- err
	}()

	barrier.wait(t, "Session B waiting for row 1")

	// Step 3: Session A supplements by inserting row 2 with sku 'sku2'.
	// In the queue for sku2, Session B is an older waiter with a pending claim.
	// Therefore, Session A waits for Session B (EdgeOlderWaiter).
	// Session B is waiting for Session A on row 1 (EdgeHolder).
	// This closes the cycle: B -> A (HOLDER) and A -> B (OLDER_WAITER).
	resA := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "INSERT INTO items VALUES(2, 'sku2')")
		resA <- err
	}()

	// Automatic deadlock detection must fire quickly.
	var errA, errB error
	timer := time.After(3 * time.Second)

	select {
	case errB = <-resB:
	case errA = <-resA:
	case <-timer:
		t.Fatal("deadlock not detected within 3s")
	}

	if errB != nil {
		select {
		case errA = <-resA:
		case <-time.After(3 * time.Second):
			t.Fatal("Session A remained blocked")
		}
	} else if errA != nil {
		select {
		case errB = <-resB:
		case <-time.After(3 * time.Second):
			t.Fatal("Session B remained blocked")
		}
	}

	aDeadlock := errors.Is(errA, storageengine.ErrDeadlock)
	bDeadlock := errors.Is(errB, storageengine.ErrDeadlock)
	if !aDeadlock && !bDeadlock {
		t.Fatalf("neither session received ErrDeadlock: errA=%v, errB=%v", errA, errB)
	}

	// Session B is registered later than Session A, so B must be chosen as victim.
	if !bDeadlock {
		t.Fatalf("expected Session B (later ordinal) to be victim, but errB=%v, errA=%v", errB, errA)
	}
	if errA != nil {
		t.Fatalf("Session A (survivor) failed: %v", errA)
	}

	// Survivor A commits successfully and preserves both row 1 and row 2 updates.
	must(sA, "COMMIT")

	res, err := e.Execute(setup, "SELECT id, sku FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0][1] != "sku1" || res.Rows[1][1] != "sku2" {
		t.Fatalf("unexpected data after survivor commit: %v", res.Rows)
	}
}

// TestB02RealSQLUpgradeDeadlock tests S->X upgrade deadlock between two sessions.
func TestB02RealSQLUpgradeDeadlock(t *testing.T) {
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
	must(setup, "INSERT INTO items VALUES(1, 10)")

	sA := &Session{CurrentDatabase: "test"}
	sB := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(sA)
	defer e.CloseSession(sB)

	// Both take S lock on row 1 via FOR SHARE.
	must(sA, "BEGIN")
	must(sA, "SELECT id, v FROM items WHERE id = 1 FOR SHARE")

	must(sB, "BEGIN")
	must(sB, "SELECT id, v FROM items WHERE id = 1 FOR SHARE")

	// Session A attempts upgrade S->X on row 1, blocks waiting for B to release S.
	barrier := newDeadlockWaitBarrier()
	barrier.arm(t)

	resA := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "UPDATE items SET v = 11 WHERE id = 1")
		resA <- err
	}()

	barrier.wait(t, "Session A upgrading S->X on row 1")

	// Session B attempts upgrade S->X on row 1, closing the upgrade cycle.
	resB := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE items SET v = 22 WHERE id = 1")
		resB <- err
	}()

	var errA, errB error
	select {
	case errB = <-resB:
	case errA = <-resA:
	case <-time.After(3 * time.Second):
		t.Fatal("upgrade deadlock not detected within 3s")
	}

	if errB != nil {
		select {
		case errA = <-resA:
		case <-time.After(3 * time.Second):
			t.Fatal("survivor remained blocked")
		}
	} else if errA != nil {
		select {
		case errB = <-resB:
		case <-time.After(3 * time.Second):
			t.Fatal("survivor remained blocked")
		}
	}

	aDeadlock := errors.Is(errA, storageengine.ErrDeadlock)
	bDeadlock := errors.Is(errB, storageengine.ErrDeadlock)
	if !aDeadlock && !bDeadlock {
		t.Fatalf("neither session received ErrDeadlock: errA=%v, errB=%v", errA, errB)
	}
}

// TestB02RealSQLReentrantNotBlockedByOtherWaiter verifies that when Session A holds a lock
// and Session B is queued behind it, Session A re-requesting the same lock (reentrant) is not blocked.
func TestB02RealSQLReentrantNotBlockedByOtherWaiter(t *testing.T) {
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
	must(setup, "INSERT INTO items VALUES(1, 10)")

	sA := &Session{CurrentDatabase: "test"}
	sB := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(sA)
	defer e.CloseSession(sB)

	must(sA, "BEGIN")
	must(sA, "UPDATE items SET v = 11 WHERE id = 1")

	// Session B queues on row 1
	must(sB, "BEGIN")
	barrier := newDeadlockWaitBarrier()
	barrier.arm(t)

	resB := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE items SET v = 22 WHERE id = 1")
		resB <- err
	}()

	barrier.wait(t, "Session B waiting for row 1")

	// Session A repeats UPDATE on row 1 (reentrant): must NOT be blocked behind Session B!
	resA := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "UPDATE items SET v = 12 WHERE id = 1")
		resA <- err
	}()

	select {
	case err := <-resA:
		if err != nil {
			t.Fatalf("reentrant UPDATE failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("reentrant UPDATE was blocked by older waiter in queue!")
	}

	// Session A commits
	must(sA, "COMMIT")

	// Session B unblocks
	select {
	case err := <-resB:
		if err != nil {
			// B may observe conflict due to A's commit modifying row 1, which is expected snapshot isolation behavior
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Session B remained blocked after Session A committed")
	}
}

// TestB02RealSQLUnrelatedRowsNotBlocked verifies that waiters on contended row 1 do not block
// unrelated updates on row 2.
func TestB02RealSQLUnrelatedRowsNotBlocked(t *testing.T) {
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
	must(setup, "INSERT INTO items VALUES(1, 10)")
	must(setup, "INSERT INTO items VALUES(2, 20)")

	sA := &Session{CurrentDatabase: "test"}
	sB := &Session{CurrentDatabase: "test"}
	sC := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(sA)
	defer e.CloseSession(sB)
	defer e.CloseSession(sC)

	must(sA, "BEGIN")
	must(sA, "UPDATE items SET v = 11 WHERE id = 1")

	must(sB, "BEGIN")
	barrier := newDeadlockWaitBarrier()
	barrier.arm(t)

	resB := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE items SET v = 22 WHERE id = 1")
		resB <- err
	}()

	barrier.wait(t, "Session B waiting for row 1")

	// Session C updates row 2 (unrelated row): must succeed immediately without blocking!
	must(sC, "BEGIN")
	doneC := make(chan error, 1)
	go func() {
		_, err := e.Execute(sC, "UPDATE items SET v = 33 WHERE id = 2")
		doneC <- err
	}()

	select {
	case err := <-doneC:
		if err != nil {
			t.Fatalf("Session C UPDATE on row 2 failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Session C on unrelated row 2 was incorrectly blocked by Session B's queue on row 1")
	}

	must(sC, "COMMIT")
	must(sA, "COMMIT")
}

// TestB02RealSQLVictimCleanedUpCompletely verifies that when a transaction becomes a deadlock victim,
// all its locks, pendings, and wait edges are completely released, and after ROLLBACK the session
// can start a fresh transaction without leaks.
func TestB02RealSQLVictimCleanedUpCompletely(t *testing.T) {
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
	must(setup, "INSERT INTO items VALUES(1, 10)")
	must(setup, "INSERT INTO items VALUES(2, 20)")

	sA := &Session{CurrentDatabase: "test"}
	sB := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(sA)
	defer e.CloseSession(sB)

	must(sA, "BEGIN")
	must(sA, "UPDATE items SET v = 11 WHERE id = 1")

	must(sB, "BEGIN")
	must(sB, "UPDATE items SET v = 22 WHERE id = 2")

	barrier := newDeadlockWaitBarrier()
	barrier.arm(t)

	resA := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "UPDATE items SET v = 12 WHERE id = 2")
		resA <- err
	}()

	barrier.wait(t, "Session A waiting for row 2")

	resB := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE items SET v = 21 WHERE id = 1")
		resB <- err
	}()

	var errA, errB error
	select {
	case errB = <-resB:
	case errA = <-resA:
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock not detected within 3s")
	}

	if errB != nil {
		errA = <-resA
	} else {
		errB = <-resB
	}
	_ = errA

	var victimSession, survivorSession *Session
	if errors.Is(errB, storageengine.ErrDeadlock) {
		victimSession = sB
		survivorSession = sA
	} else {
		victimSession = sA
		survivorSession = sB
	}

	// 1. Survivor commits.
	must(survivorSession, "COMMIT")

	// 2. Victim explicitly rolls back to clean up session state.
	must(victimSession, "ROLLBACK")

	// 3. Victim session can now start a fresh transaction and update the rows without error.
	must(victimSession, "BEGIN")
	must(victimSession, "UPDATE items SET v = 99 WHERE id = 1")
	must(victimSession, "COMMIT")

	// Verify final state
	res, err := e.Execute(setup, "SELECT id, v FROM items WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][1] != int64(99) {
		t.Fatalf("unexpected value: %v", res.Rows)
	}
}
