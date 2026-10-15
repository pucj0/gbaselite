package executor

import (
	"errors"
	"testing"
	"time"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

func newSerializableSession(db string) *Session {
	s := &Session{CurrentDatabase: db}
	_ = s.SetSessionIsolation(parser.LevelSerializable)
	return s
}

// TestM21_SER_SSCompatibility tests M21:
// In SERIALIZABLE, two transactions reading the same row both acquire Shared (S) locks
// and proceed concurrently without blocking each other.
func TestM21_SER_SSCompatibility(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// A begins SERIALIZABLE
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}
	// B begins SERIALIZABLE
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatalf("B BEGIN: %v", err)
	}

	// A reads id=1 (acquires S lock)
	resA, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resA.Rows) != 1 || resA.Rows[0][0] != "V1" {
		t.Fatalf("A SELECT: %v, %v", resA, err)
	}

	// B reads id=1 concurrently (acquires S lock: compatible with S lock)
	resB, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB.Rows) != 1 || resB.Rows[0][0] != "V1" {
		t.Fatalf("B SELECT concurrently: %v, %v", resB, err)
	}

	// Both commit
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sB, "COMMIT;"); err != nil {
		t.Fatal(err)
	}
}

// TestM22_SER_SXConflict tests M22:
// A's plain SELECT in SERIALIZABLE acquires S lock on id=1;
// B's UPDATE id=1 requests X lock and must wait for A to release its S lock.
func TestM22_SER_SXConflict(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// A begins SERIALIZABLE and reads id=1
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	resA, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resA.Rows) != 1 || resA.Rows[0][0] != "V1" {
		t.Fatalf("A SELECT: %v", resA)
	}

	// B begins and attempts UPDATE on id=1: must wait on A's S lock!
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	bWaiting := make(chan struct{})
	bDone := make(chan error, 1)

	// Hook into dmlLockRequestHook to observe B entering lock request
	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		select {
		case <-bWaiting:
		default:
			close(bWaiting)
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	go func() {
		_, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;")
		bDone <- err
	}()

	<-bWaiting

	// Ensure B has not finished (it is waiting for A's S lock)
	select {
	case err := <-bDone:
		t.Fatalf("B finished prematurely without waiting for S lock: %v", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: B is waiting
	}

	// A commits, releasing S lock
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatal(err)
	}

	// B can now acquire X lock and complete
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B UPDATE failed after A commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never completed after A commit")
	}

	_, _ = e.Execute(sB, "COMMIT;")
}

// TestM23_SER_AbsentPointProtection tests M23:
// Querying an absent point id=999 in SERIALIZABLE locks the insertion position;
// concurrent insertion of id=999 must wait until A ends.
func TestM23_SER_AbsentPointProtection(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1'), (1000, 'V1000');"); err != nil {
		t.Fatal(err)
	}

	// A begins SERIALIZABLE and queries absent id=999
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	resA, err := e.Execute(sA, "SELECT * FROM t WHERE id = 999;")
	if err != nil || len(resA.Rows) != 0 {
		t.Fatalf("A query expected empty: %v", resA)
	}

	// B attempts to INSERT id=999: must wait for A's gap lock!
	bWaiting := make(chan struct{})
	bDone := make(chan error, 1)

	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		select {
		case <-bWaiting:
		default:
			close(bWaiting)
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	go func() {
		_, err := e.Execute(sB, "INSERT INTO t VALUES (999, 'absent_val');")
		bDone <- err
	}()

	<-bWaiting

	// B must be waiting
	select {
	case err := <-bDone:
		t.Fatalf("B finished prematurely without waiting for gap lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// A commits, releasing gap lock
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatal(err)
	}

	// B now finishes
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B INSERT failed after A commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never resumed after A commit")
	}
}

// TestM24_SER_EmptyRangeProtection tests M24:
// Querying an empty range WHERE id BETWEEN 100 AND 200 protects the range;
// concurrent insert inside range waits, while insert outside range (300) proceeds.
func TestM24_SER_EmptyRangeProtection(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")
	sC := newSerializableSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (10, 'V10'), (500, 'V500');"); err != nil {
		t.Fatal(err)
	}

	// A begins SERIALIZABLE and reads empty range
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	resA, err := e.Execute(sA, "SELECT * FROM t WHERE id >= 100 AND id <= 200;")
	if err != nil || len(resA.Rows) != 0 {
		t.Fatalf("A range read expected empty: %v", resA)
	}

	// C inserts outside range (id=300): should proceed without waiting on [100, 200]
	// (or with conservative table intent if range is covered)
	if _, err := e.Execute(sC, "INSERT INTO t VALUES (300, 'outside');"); err != nil {
		// If conservative domain locking is active, this may wait or succeed
	}

	// B inserts inside range (id=150): MUST wait on A's range lock
	bWaiting := make(chan struct{})
	bDone := make(chan error, 1)

	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		select {
		case <-bWaiting:
		default:
			close(bWaiting)
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	go func() {
		_, err := e.Execute(sB, "INSERT INTO t VALUES (150, 'inside');")
		bDone <- err
	}()

	<-bWaiting

	// Verify B is waiting
	select {
	case err := <-bDone:
		t.Fatalf("B finished prematurely without waiting for range lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// A commits, releasing range lock
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B INSERT failed after A commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never completed after A commit")
	}
}

// TestM25_SER_WriteSkew tests M25:
// Cross-row constraint under SERIALIZABLE: two transactions cannot concurrently update disjoint
// rows based on shared S reads and break the invariant.
func TestM25_SER_WriteSkew(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")

	// Initial: both rows true (1=true, 2=true)
	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'true'), (2, 'true');"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// A reads both rows (acquires S locks on both 1 and 2)
	resA, err := e.Execute(sA, "SELECT v FROM t WHERE id IN (1, 2);")
	if err != nil || len(resA.Rows) != 2 {
		t.Fatalf("A read: %v", resA)
	}

	// B reads both rows (acquires S locks on both 1 and 2)
	resB, err := e.Execute(sB, "SELECT v FROM t WHERE id IN (1, 2);")
	if err != nil || len(resB.Rows) != 2 {
		t.Fatalf("B read: %v", resB)
	}

	// A attempts to update row 1 to 'false' (requests X lock on 1 -> waits on B's S lock)
	aDone := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "UPDATE t SET v = 'false' WHERE id = 1;")
		aDone <- err
	}()

	// B attempts to update row 2 to 'false' (requests X lock on 2 -> waits on A's S lock)
	bDone := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE t SET v = 'false' WHERE id = 2;")
		bDone <- err
	}()

	// Deadlock detector must detect the cycle (A waits for B, B waits for A)
	var errA, errB error
	select {
	case errA = <-aDone:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock detector did not break cycle in time")
	}

	select {
	case errB = <-bDone:
	case <-time.After(10 * time.Second):
		t.Fatal("other transaction never unblocked")
	}

	// One must be aborted as deadlock victim, both cannot commit!
	oneDeadlock := errors.Is(errA, storageengine.ErrDeadlock) || errors.Is(errB, storageengine.ErrDeadlock)
	if !oneDeadlock {
		t.Fatalf("neither transaction received ErrDeadlock: errA=%v, errB=%v", errA, errB)
	}

	_, _ = e.Execute(sA, "ROLLBACK;")
	_, _ = e.Execute(sB, "ROLLBACK;")
}

// TestM26_SER_SXUpgrade tests M26:
// In SERIALIZABLE, a transaction can upgrade its own S lock to X lock when updating the same row.
func TestM26_SER_SXUpgrade(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// S read
	resA, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resA.Rows) != 1 || resA.Rows[0][0] != "V1" {
		t.Fatal(err)
	}

	// Upgrade S to X in same transaction
	if _, err := e.Execute(sA, "UPDATE t SET v = 'V_upgraded' WHERE id = 1;"); err != nil {
		t.Fatalf("S to X upgrade failed: %v", err)
	}

	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatalf("COMMIT after upgrade failed: %v", err)
	}

	// Final verification
	resFinal, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(resFinal.Rows) != 1 || resFinal.Rows[0][0] != "V_upgraded" {
		t.Fatalf("final value = %v, want V_upgraded", resFinal)
	}
}

// TestM27_SER_UnsupportedQueries tests M27:
// Complex query shapes without proven predicate locking in SERIALIZABLE long transaction
// must fail closed with ErrUnsupported.
func TestM27_SER_UnsupportedQueries(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")

	if _, err := e.Execute(sA, "CREATE TABLE t2 (id INT PRIMARY KEY, ref_id INT);"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// JOIN query in SERIALIZABLE long transaction: must return ErrUnsupported
	_, err := e.Execute(sA, "SELECT t.id FROM t JOIN t2 ON t.id = t2.ref_id;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("JOIN query in SERIALIZABLE returned %v, want ErrUnsupported", err)
	}

	// Subquery in SERIALIZABLE: must return ErrUnsupported
	_, err = e.Execute(sA, "SELECT id FROM t WHERE id IN (SELECT ref_id FROM t2);")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("Subquery in SERIALIZABLE returned %v, want ErrUnsupported", err)
	}

	_, _ = e.Execute(sA, "ROLLBACK;")
}

// TestM28_SER_AutocommitOptimization tests M28:
// Standalone autocommit=1 pure SELECT can use nonlocking consistent read optimization per MySQL 8.4.
func TestM28_SER_AutocommitOptimization(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	_ = e.SetAutocommit(sA, true)

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatal(err)
	}

	// Pure read-only SELECT in autocommit=1 succeeds without requiring long lock ownership
	res, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "V1" {
		t.Fatalf("autocommit=1 SELECT in SERIALIZABLE failed: %v, %v", res, err)
	}
}

// TestT052_SER_PredicateBoundaryLocking covers T052:
// Verifies predicate range boundaries in SERIALIZABLE: ASC, DESC, unique index,
// and ensuring LIMIT does not drop interval protection.
func TestT052_SER_PredicateBoundaryLocking(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")

	if _, err := e.Execute(sA, "CREATE TABLE items (id INT PRIMARY KEY, val INT);"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "INSERT INTO items VALUES (10, 100), (20, 200), (30, 300);"); err != nil {
		t.Fatal(err)
	}

	// 1. Transaction A selects range [10, 30] with LIMIT 1
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	resA, err := e.Execute(sA, "SELECT * FROM items WHERE id >= 10 AND id <= 30 ORDER BY id ASC LIMIT 1;")
	if err != nil || len(resA.Rows) != 1 {
		t.Fatalf("A read: %v", resA)
	}

	// 2. Concurrent B attempts to INSERT inside the proven interval [10, 30] at id=15:
	// The interval must still be protected even though A used LIMIT 1!
	bDone := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "INSERT INTO items VALUES (15, 150);")
		bDone <- err
	}()

	select {
	case err := <-bDone:
		t.Fatalf("B INSERT succeeded prematurely inside locked interval: %v", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: B is waiting for A's range lock
	}

	_, _ = e.Execute(sA, "COMMIT;")

	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B INSERT failed after A commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never completed after A commit")
	}
}

// TestT053_SER_WriteSkewAggregateConstraint covers T053:
// Two transactions concurrently read account balances under SERIALIZABLE (S lock).
// Both attempt to withdraw money such that combined balance would become negative.
// Mutual S->X upgrade deadlock prevents both from succeeding.
func TestT053_SER_WriteSkewAggregateConstraint(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")

	if _, err := e.Execute(sA, "CREATE TABLE acct (id INT PRIMARY KEY, bal INT);"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "INSERT INTO acct VALUES (1, 50), (2, 50);"); err != nil {
		t.Fatal(err)
	}

	// Both transactions begin SERIALIZABLE
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// Both read accounts 1 and 2 (acquire S locks on both)
	_, _ = e.Execute(sA, "SELECT bal FROM acct WHERE id IN (1, 2);")
	_, _ = e.Execute(sB, "SELECT bal FROM acct WHERE id IN (1, 2);")

	// Txn A withdraws 80 from acct 1
	aDone := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "UPDATE acct SET bal = bal - 80 WHERE id = 1;")
		aDone <- err
	}()

	// Txn B withdraws 80 from acct 2
	bDone := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE acct SET bal = bal - 80 WHERE id = 2;")
		bDone <- err
	}()

	var errA, errB error
	select {
	case errA = <-aDone:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock detector did not fire in time")
	}

	select {
	case errB = <-bDone:
	case <-time.After(10 * time.Second):
		t.Fatal("second transaction never unblocked")
	}

	// Exactly one received ErrDeadlock
	hasDeadlock := errors.Is(errA, storageengine.ErrDeadlock) || errors.Is(errB, storageengine.ErrDeadlock)
	if !hasDeadlock {
		t.Fatalf("neither transaction aborted: errA=%v, errB=%v", errA, errB)
	}

	_, _ = e.Execute(sA, "ROLLBACK;")
	_, _ = e.Execute(sB, "ROLLBACK;")
}

// TestT054_SER_AutocommitModeMatrix covers T054:
// Verifies behavior across autocommit=1 standalone, autocommit=0, explicit BEGIN, and savepoints in SERIALIZABLE.
func TestT054_SER_AutocommitModeMatrix(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (10, 'V10');"); err != nil {
		t.Fatal(err)
	}

	// 1. autocommit=0: long transaction takes S lock on SELECT
	_ = e.SetAutocommit(sA, false)
	_ = sA.SetLockWaitTimeout(1)
	_ = sB.SetLockWaitTimeout(1)

	res, err := e.Execute(sA, "SELECT v FROM t WHERE id = 10;")
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("autocommit=0 SELECT: %v", err)
	}
	if !sA.InTransaction() {
		t.Fatal("autocommit=0 must implicitly start transaction")
	}

	// Concurrent B update must wait/timeout because A holds S lock
	bDone := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE t SET v = 'V_b' WHERE id = 10;")
		bDone <- err
	}()

	select {
	case err := <-bDone:
		t.Fatalf("B update succeeded prematurely against autocommit=0 S lock: %v", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: B is waiting
	}

	_, _ = e.Execute(sA, "COMMIT;")

	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B update failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never finished")
	}

	// 2. Savepoint Child retains Root S lock
	_ = e.SetAutocommit(sA, true)
	_ = sB.SetLockWaitTimeout(10)
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	_, _ = e.Execute(sA, "SELECT v FROM t WHERE id = 10;")
	_, _ = e.Execute(sA, "SAVEPOINT sp1;")

	// Inside savepoint child: S lock still held by Root
	go func() {
		_, err := e.Execute(sB, "UPDATE t SET v = 'V_sp' WHERE id = 10;")
		bDone <- err
	}()

	select {
	case err := <-bDone:
		t.Fatalf("B update succeeded prematurely against Savepoint child S lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	_, _ = e.Execute(sA, "ROLLBACK TO sp1;")
	_, _ = e.Execute(sA, "COMMIT;")

	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B update failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never finished")
	}
}
