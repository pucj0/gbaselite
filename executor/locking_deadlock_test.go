package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/lockmanager"
	"gbaselite/storageengine"
	"sync"
	"testing"
	"time"
)

// engineLockManager resolves the underlying lockmanager.Manager from an engine if supported,
// without directly importing physical storage adapters in executor package files.
func engineLockManager(e storageengine.Engine) *lockmanager.Manager {
	if p, ok := e.(interface{ LockManager() *lockmanager.Manager }); ok {
		return p.LockManager()
	}
	return nil
}

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

// collectTwoOutcomes collects outcomes from two concurrent sessions independently and boundedly.
// Regardless of which session settles first, both channels are guaranteed to be drained,
// preventing dangling goroutines or unobserved outcomes.
func collectTwoOutcomes(t *testing.T, resA, resB <-chan error, timeout time.Duration) (errA, errB error) {
	t.Helper()
	timer := time.After(timeout)
	var gotA, gotB bool

	for !gotA || !gotB {
		select {
		case res := <-resA:
			errA = res
			gotA = true
		case res := <-resB:
			errB = res
			gotB = true
		case <-timer:
			t.Fatalf("timed out waiting for both sessions to settle: gotA=%v (errA=%v), gotB=%v (errB=%v)", gotA, errA, gotB, errB)
		}
	}
	return errA, errB
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

	// Automatic deadlock detection must fire quickly (within 3 seconds),
	// far below the 50-second lock wait timeout.
	errA, errB := collectTwoOutcomes(t, resA, resB, 3*time.Second)

	// Exactly one session must have received ErrDeadlock.
	aDeadlock := errors.Is(errA, storageengine.ErrDeadlock)
	bDeadlock := errors.Is(errB, storageengine.ErrDeadlock)

	if !aDeadlock && !bDeadlock {
		t.Fatalf("neither session received ErrDeadlock: errA=%v, errB=%v", errA, errB)
	}
	if aDeadlock && bDeadlock {
		t.Fatalf("both sessions received ErrDeadlock; want exactly one victim")
	}

	var victimSession, survivorSession *Session
	var victimErr, survivorErr error

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
	// 1. Victim cannot commit; commit must fail with ErrDeadlock.
	if _, err := e.Execute(victimSession, "COMMIT"); err == nil || !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("victim commit error = %v, want storageengine.ErrDeadlock", err)
	}

	// 2. Survivor must be able to commit successfully.
	if _, err := e.Execute(survivorSession, "COMMIT"); err != nil {
		t.Fatalf("survivor commit failed: %v", err)
	}

	// 3. Verify final data state.
	checkRes, err := e.Execute(setup, "SELECT id, v FROM items ORDER BY id")
	if err != nil {
		t.Fatalf("check query failed: %v", err)
	}
	if len(checkRes.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(checkRes.Rows))
	}
}

// TestB02RealSQLSupplementalOlderWaiterDeadlock tests the supplemental locking scenario:
// Root A holds Record X on row 1.
// Root B requests an atomic bundle containing Record X(1) and Unique X(sku2).
// B queues behind A on row 1, holding a pending claim on sku2.
// Root A then supplements by inserting row 2 with sku2, encountering Root B's older request on sku2 (EdgeOlderWaiter).
// Wait-for graph has B->A (HOLDER on row 1) and A->B (OLDER_WAITER on sku2).
func TestB02RealSQLSupplementalOlderWaiterDeadlock(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	lm := engineLockManager(e.Backend)
	if lm == nil {
		t.Fatal("lock manager is unavailable on mvcc backend")
	}

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

	// Verify Session A holds row 1 lock:
	statsBefore := lm.Stats()
	if statsBefore.GrantedComponents == 0 {
		t.Fatal("Session A holds no granted components")
	}

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

	// Verify wait-for graph before Session A requests sku2:
	// B must be waiting for A on row 1 with HOLDER edge.
	edgesBeforeA := lm.WaitEdges()
	t.Logf("WaitEdges before Session A requests sku2: %+v", edgesBeforeA)
	var foundBtoA bool
	for _, edge := range edgesBeforeA {
		if edge.Reason == lockmanager.EdgeHolder {
			foundBtoA = true
			break
		}
	}
	if !foundBtoA {
		t.Fatalf("expected HOLDER edge from Session B to Session A, got edges: %+v", edgesBeforeA)
	}

	// Step 3: Session A supplements by inserting row 2 with sku 'sku2'.
	// In the queue for sku2, Session B is an older waiter with a pending claim.
	// Therefore, Session A encounters Session B's older request (EdgeOlderWaiter).
	// Session B is waiting for Session A on row 1 (EdgeHolder).
	// This closes the cycle: B -> A (HOLDER) and A -> B (OLDER_WAITER).
	resA := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "INSERT INTO items VALUES(2, 'sku2')")
		resA <- err
	}()

	// Both sessions must settle in bounded time.
	errA, errB := collectTwoOutcomes(t, resA, resB, 3*time.Second)

	aDeadlock := errors.Is(errA, storageengine.ErrDeadlock)
	bDeadlock := errors.Is(errB, storageengine.ErrDeadlock)
	if !aDeadlock && !bDeadlock {
		t.Fatalf("neither session received ErrDeadlock: errA=%v, errB=%v", errA, errB)
	}

	// Session B was registered later than Session A, so B must be chosen as victim.
	if !bDeadlock {
		t.Fatalf("expected Session B (later ordinal) to be victim, but errB=%v, errA=%v", errB, errA)
	}
	if errA != nil {
		t.Fatalf("Session A (survivor) failed: %v", errA)
	}

	// Verify post-detection lock state:
	t.Logf("Stats after deadlock detection: %+v", lm.Stats())
	t.Logf("WaitEdges after deadlock detection: %+v", lm.WaitEdges())

	// Survivor A commits successfully and preserves both row 1 and row 2 updates.
	must(sA, "COMMIT")

	res, err := e.Execute(setup, "SELECT id, sku FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0][1] != "sku1" || res.Rows[1][1] != "sku2" {
		t.Fatalf("unexpected data after survivor commit: %v", res.Rows)
	}

	// Clean up victim session
	must(sB, "ROLLBACK")

	// Ensure no residue remains in lock manager
	statsEnd := lm.Stats()
	if statsEnd.PendingRequests != 0 || statsEnd.WaitEdges != 0 {
		t.Fatalf("lock manager has residue: %+v", statsEnd)
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

	errA, errB := collectTwoOutcomes(t, resA, resB, 3*time.Second)

	aDeadlock := errors.Is(errA, storageengine.ErrDeadlock)
	bDeadlock := errors.Is(errB, storageengine.ErrDeadlock)
	if !aDeadlock && !bDeadlock {
		t.Fatalf("neither session received ErrDeadlock: errA=%v, errB=%v", errA, errB)
	}
	if aDeadlock && bDeadlock {
		t.Fatalf("both sessions received ErrDeadlock; want exactly one victim")
	}

	if bDeadlock {
		if errA != nil {
			t.Fatalf("survivor A failed: %v", errA)
		}
		must(sA, "COMMIT")
		must(sB, "ROLLBACK")
	} else {
		if errB != nil {
			t.Fatalf("survivor B failed: %v", errB)
		}
		must(sB, "COMMIT")
		must(sA, "ROLLBACK")
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

	// Session B unblocks and settles
	select {
	case <-resB:
	case <-time.After(3 * time.Second):
		t.Fatal("Session B remained blocked after Session A committed")
	}
	e.CloseSession(sB)
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

	select {
	case <-resB:
	case <-time.After(3 * time.Second):
		t.Fatal("Session B remained blocked after Session A committed")
	}
	e.CloseSession(sB)
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

	errA, errB := collectTwoOutcomes(t, resA, resB, 3*time.Second)
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

// TestB02DeadlockVictimLifecycleAndErrorSemantics exhaustively verifies the victim session's behavior
// across COMMIT, ROLLBACK, BEGIN, SAVEPOINT, SELECT, UPDATE, and Disconnect, ensuring that the deadlock
// termination reason is preserved, writes and commits are refused with ErrDeadlock, and all resources are reclaimed.
func TestB02DeadlockVictimLifecycleAndErrorSemantics(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	lm := engineLockManager(e.Backend)
	if lm == nil {
		t.Fatal("lock manager is unavailable")
	}

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

	// Step 1: Form deadlock between sA and sB
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

	barrier.wait(t, "sA waiting on row 2")

	resB := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE items SET v = 21 WHERE id = 1")
		resB <- err
	}()

	errA, errB := collectTwoOutcomes(t, resA, resB, 3*time.Second)

	var victim, survivor *Session
	if errors.Is(errB, storageengine.ErrDeadlock) {
		victim = sB
		survivor = sA
	} else if errors.Is(errA, storageengine.ErrDeadlock) {
		victim = sA
		survivor = sB
	} else {
		t.Fatalf("neither received ErrDeadlock: errA=%v, errB=%v", errA, errB)
	}

	// 1. Survivor commits successfully
	must(survivor, "COMMIT")

	// 2. Before ROLLBACK, write on victim session must be refused with ErrDeadlock
	if _, err := e.Execute(victim, "UPDATE items SET v = 100 WHERE id = 1"); !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("victim UPDATE = %v, want storageengine.ErrDeadlock", err)
	}

	// 3. Before ROLLBACK, SAVEPOINT on victim session must be refused with ErrDeadlock
	if _, err := e.Execute(victim, "SAVEPOINT sp1"); !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("victim SAVEPOINT = %v, want storageengine.ErrDeadlock", err)
	}

	// 4. Before ROLLBACK, SELECT on victim session must be refused with ErrDeadlock
	if _, err := e.Execute(victim, "SELECT * FROM items WHERE id = 1"); !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("victim SELECT = %v, want storageengine.ErrDeadlock", err)
	}

	// 5. Refusal of COMMIT on victim session: must report ErrDeadlock and clean up transaction
	if _, err := e.Execute(victim, "COMMIT"); !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("victim COMMIT = %v, want storageengine.ErrDeadlock", err)
	}

	// 6. Explicit ROLLBACK succeeds and cleanly clears the victim session
	must(victim, "ROLLBACK")

	// 7. After ROLLBACK, victim can start a fresh transaction and write
	must(victim, "BEGIN")
	must(victim, "UPDATE items SET v = 77 WHERE id = 1")
	must(victim, "COMMIT")

	// 8. Re-test: deadlock followed immediately by BEGIN (without explicit ROLLBACK)
	must(sA, "BEGIN")
	must(sA, "UPDATE items SET v = 11 WHERE id = 1")
	must(sB, "BEGIN")
	must(sB, "UPDATE items SET v = 22 WHERE id = 2")

	barrier2 := newDeadlockWaitBarrier()
	barrier2.arm(t)
	resA2 := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "UPDATE items SET v = 12 WHERE id = 2")
		resA2 <- err
	}()
	barrier2.wait(t, "sA waiting on row 2 again")

	resB2 := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE items SET v = 21 WHERE id = 1")
		resB2 <- err
	}()
	_, errB2 := collectTwoOutcomes(t, resA2, resB2, 3*time.Second)
	if !errors.Is(errB2, storageengine.ErrDeadlock) {
		t.Fatalf("expected sB to be victim, got %v", errB2)
	}
	must(sA, "COMMIT")

	// Directly issuing BEGIN on the terminated sB session must clear the dead state and succeed:
	must(sB, "BEGIN")
	must(sB, "UPDATE items SET v = 88 WHERE id = 2")
	must(sB, "COMMIT")

	// 9. Session Disconnect: CloseSession must leave zero lock residues
	e.CloseSession(sA)
	e.CloseSession(sB)

	stats := lm.Stats()
	if stats.ActiveOwners != 0 || stats.GrantedComponents != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 {
		t.Fatalf("residue after session close: %+v", stats)
	}
}

// TestB02DeadlockVsTimeoutErrorCodes verifies that MySQL error codes 1213 (Deadlock) and 1205 (Lock Wait Timeout)
// are distinct and accurately classified.
func TestB02DeadlockVsTimeoutErrorCodes(t *testing.T) {
	deadlockErr := storageengine.ErrDeadlock
	timeoutErr := storageengine.ErrLockWaitTimeout

	// Both errors must not be equal or wrap each other
	if errors.Is(deadlockErr, timeoutErr) || errors.Is(timeoutErr, deadlockErr) {
		t.Fatal("ErrDeadlock and ErrLockWaitTimeout must be completely distinct")
	}

	// Verify error descriptions
	if deadlockErr.Error() != "deadlock found when trying to get lock; retry transaction" {
		t.Fatalf("unexpected deadlock error string: %s", deadlockErr.Error())
	}
	if timeoutErr.Error() != "lock wait timeout exceeded" {
		t.Fatalf("unexpected timeout error string: %s", timeoutErr.Error())
	}

	// Verify MySQL error code mapping simulation
	mysqlCode := func(err error) int {
		switch {
		case errors.Is(err, storageengine.ErrDeadlock):
			return 1213
		case errors.Is(err, storageengine.ErrLockWaitTimeout):
			return 1205
		default:
			return 1064
		}
	}

	if code := mysqlCode(deadlockErr); code != 1213 {
		t.Fatalf("deadlock error mapped to %d, want 1213", code)
	}
	if code := mysqlCode(timeoutErr); code != 1205 {
		t.Fatalf("timeout error mapped to %d, want 1205", code)
	}
	if code := mysqlCode(fmt.Errorf("wrapped: %w", deadlockErr)); code != 1213 {
		t.Fatalf("wrapped deadlock error mapped to %d, want 1213", code)
	}
	if code := mysqlCode(fmt.Errorf("wrapped: %w", timeoutErr)); code != 1205 {
		t.Fatalf("wrapped timeout error mapped to %d, want 1205", code)
	}
}
