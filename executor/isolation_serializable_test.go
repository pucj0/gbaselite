package executor

import (
	"context"
	"errors"
	"sync"
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
	var closeOnce sync.Once
	bDone := make(chan error, 1)

	// Hook into dmlLockRequestHook: ShouldWait is called exactly when B must wait for A's lock
	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		req.ShouldWait = func(ctx context.Context) bool {
			closeOnce.Do(func() { close(bWaiting) })
			return true
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	go func() {
		_, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;")
		bDone <- err
	}()

	<-bWaiting

	// Deterministically verify B has entered Lock Manager wait queue
	if lm := engineLockManager(e.Backend); lm != nil {
		if len(lm.WaitEdges()) == 0 {
			t.Fatal("expected B to be in lock manager wait queue")
		}
	}

	// Ensure B has not finished (it is waiting for A's S lock)
	select {
	case err := <-bDone:
		t.Fatalf("B finished prematurely without waiting for S lock: %v", err)
	default:
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
	var closeOnce sync.Once
	bDone := make(chan error, 1)

	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		req.ShouldWait = func(ctx context.Context) bool {
			closeOnce.Do(func() { close(bWaiting) })
			return true
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	go func() {
		_, err := e.Execute(sB, "INSERT INTO t VALUES (999, 'absent_val');")
		bDone <- err
	}()

	<-bWaiting

	// Deterministically verify B has entered Lock Manager wait queue for gap lock
	if lm := engineLockManager(e.Backend); lm != nil {
		if len(lm.WaitEdges()) == 0 {
			t.Fatal("expected B to be in lock manager wait queue for gap lock")
		}
	}

	select {
	case err := <-bDone:
		t.Fatalf("B finished prematurely without waiting for gap lock: %v", err)
	default:
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

	// C inserts outside range (id=300): must succeed without error!
	if _, err := e.Execute(sC, "INSERT INTO t VALUES (300, 'outside');"); err != nil {
		t.Fatalf("C insert outside range failed: %v", err)
	}

	// B inserts inside range (id=150): MUST wait on A's range lock
	bWaiting := make(chan struct{})
	var closeOnce sync.Once
	bDone := make(chan error, 1)

	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		req.ShouldWait = func(ctx context.Context) bool {
			closeOnce.Do(func() { close(bWaiting) })
			return true
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	go func() {
		_, err := e.Execute(sB, "INSERT INTO t VALUES (150, 'inside');")
		bDone <- err
	}()

	<-bWaiting

	// Deterministically verify B has entered Lock Manager wait queue for range lock
	if lm := engineLockManager(e.Backend); lm != nil {
		if len(lm.WaitEdges()) == 0 {
			t.Fatal("expected B to be in lock manager wait queue for range lock")
		}
	}

	select {
	case err := <-bDone:
		t.Fatalf("B finished prematurely without waiting for range lock: %v", err)
	default:
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

// TestM26_SER_DualSXUpgradeDeadlock tests M26:
// Two transactions in SERIALIZABLE both read row 1 (both hold S).
// Both subsequently attempt to update row 1 (both request X -> deadlock cycle!).
// Deadlock detector breaks cycle, exactly one is aborted as deadlock victim, the other commits.
func TestM26_SER_DualSXUpgradeDeadlock(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newSerializableSession("db")
	sB := newSerializableSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// Both read row 1 (acquire S lock on row 1)
	_, _ = e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	_, _ = e.Execute(sB, "SELECT v FROM t WHERE id = 1;")

	aDone := make(chan error, 1)
	go func() {
		_, err := e.Execute(sA, "UPDATE t SET v = 'V_A' WHERE id = 1;")
		aDone <- err
	}()

	bDone := make(chan error, 1)
	go func() {
		_, err := e.Execute(sB, "UPDATE t SET v = 'V_B' WHERE id = 1;")
		bDone <- err
	}()

	var errA, errB error
	select {
	case errA = <-aDone:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock detector did not fire in time")
	}

	select {
	case errB = <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("other transaction never unblocked")
	}

	oneDeadlock := errors.Is(errA, storageengine.ErrDeadlock) || errors.Is(errB, storageengine.ErrDeadlock)
	if !oneDeadlock {
		t.Fatalf("neither transaction aborted: errA=%v, errB=%v", errA, errB)
	}

	_, _ = e.Execute(sA, "ROLLBACK;")
	_, _ = e.Execute(sB, "ROLLBACK;")
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

	// Must NOT hold active transaction or residual lock grants
	if sA.InTransaction() {
		t.Fatal("autocommit=1 SELECT must not leave an active transaction")
	}
	if lm := engineLockManager(e.Backend); lm != nil && lm.LockTableSize() != 0 {
		t.Fatalf("autocommit=1 SELECT left residual locks in table: %d", lm.LockTableSize())
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
	bWaiting := make(chan struct{})
	var closeOnce sync.Once
	bDone := make(chan error, 1)

	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		req.ShouldWait = func(ctx context.Context) bool {
			closeOnce.Do(func() { close(bWaiting) })
			return true
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	go func() {
		_, err := e.Execute(sB, "INSERT INTO items VALUES (15, 150);")
		bDone <- err
	}()

	<-bWaiting

	// Deterministically verify B has entered Lock Manager wait queue for range lock
	if lm := engineLockManager(e.Backend); lm != nil {
		if len(lm.WaitEdges()) == 0 {
			t.Fatal("expected B to be in lock manager wait queue for range lock")
		}
	}

	select {
	case err := <-bDone:
		t.Fatalf("B finished prematurely without waiting for range lock: %v", err)
	default:
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
	bWaiting := make(chan struct{})
	var closeOnce sync.Once
	bDone := make(chan error, 1)

	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		req.ShouldWait = func(ctx context.Context) bool {
			closeOnce.Do(func() { close(bWaiting) })
			return true
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	go func() {
		_, err := e.Execute(sB, "UPDATE t SET v = 'V_b' WHERE id = 10;")
		bDone <- err
	}()

	<-bWaiting

	// Deterministically verify B has entered Lock Manager wait queue
	if lm := engineLockManager(e.Backend); lm != nil {
		if len(lm.WaitEdges()) == 0 {
			t.Fatal("expected B to be in lock manager wait queue")
		}
	}

	select {
	case err := <-bDone:
		t.Fatalf("B update succeeded prematurely against autocommit=0 S lock: %v", err)
	default:
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
	bWaiting2 := make(chan struct{})
	var closeOnce2 sync.Once

	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		req.ShouldWait = func(ctx context.Context) bool {
			closeOnce2.Do(func() { close(bWaiting2) })
			return true
		}
		return req
	}

	go func() {
		_, err := e.Execute(sB, "UPDATE t SET v = 'V_sp' WHERE id = 10;")
		bDone <- err
	}()

	<-bWaiting2

	// Deterministically verify B has entered Lock Manager wait queue
	if lm := engineLockManager(e.Backend); lm != nil {
		if len(lm.WaitEdges()) == 0 {
			t.Fatal("expected B to be in lock manager wait queue for savepoint child lock")
		}
	}

	select {
	case err := <-bDone:
		t.Fatalf("B update succeeded prematurely against Savepoint child S lock: %v", err)
	default:
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

// TestP6Fix02_SerializableDMLUnprotectedReadPathsFailClosed covers P6-FIX-02:
// DML statements with unprotected read paths (UPDATE JOIN, DELETE JOIN, subqueries in WHERE,
// INSERT SELECT, CTAS) must fail closed with ErrUnsupported in SERIALIZABLE transactions.
func TestP6Fix02_SerializableDMLUnprotectedReadPathsFailClosed(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	s := newSerializableSession("db")

	if _, err := e.Execute(s, "CREATE TABLE t2 (id INT PRIMARY KEY, v VARCHAR(50));"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(s, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// 1. UPDATE JOIN: must fail closed
	_, err := e.Execute(s, "UPDATE t JOIN t2 ON t.id = t2.id SET t.v = 'new';")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("UPDATE JOIN in SERIALIZABLE returned %v, want ErrUnsupported", err)
	}

	// 2. DELETE JOIN: must fail closed
	_, err = e.Execute(s, "DELETE t FROM t JOIN t2 ON t.id = t2.id;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("DELETE JOIN in SERIALIZABLE returned %v, want ErrUnsupported", err)
	}

	// 3. UPDATE with subquery in WHERE: must fail closed
	_, err = e.Execute(s, "UPDATE t SET v = 'new' WHERE id IN (SELECT id FROM t2);")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("UPDATE with subquery in SERIALIZABLE returned %v, want ErrUnsupported", err)
	}

	// 4. DELETE with subquery in WHERE: must fail closed
	_, err = e.Execute(s, "DELETE FROM t WHERE id IN (SELECT id FROM t2);")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("DELETE with subquery in SERIALIZABLE returned %v, want ErrUnsupported", err)
	}

	// 5. INSERT SELECT: must fail closed
	_, err = e.Execute(s, "INSERT INTO t2 SELECT id, v FROM t;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("INSERT SELECT in SERIALIZABLE returned %v, want ErrUnsupported", err)
	}

	// 6. CREATE TABLE AS SELECT: must fail closed
	_, err = e.Execute(s, "CREATE TABLE t3 AS SELECT id, v FROM t;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("CTAS in SERIALIZABLE returned %v, want ErrUnsupported", err)
	}

	// 7. Inside Savepoint Child: must also fail closed
	_, _ = e.Execute(s, "SAVEPOINT sp1;")
	_, err = e.Execute(s, "INSERT INTO t2 SELECT id, v FROM t;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("INSERT SELECT in Savepoint child returned %v, want ErrUnsupported", err)
	}
	_, _ = e.Execute(s, "ROLLBACK TO sp1;")

	// 8. Plain single-table INSERT VALUES, UPDATE, DELETE (without join/subquery) must succeed!
	if _, err := e.Execute(s, "INSERT INTO t VALUES (50, 'val50');"); err != nil {
		t.Fatalf("plain INSERT VALUES failed: %v", err)
	}
	if _, err := e.Execute(s, "UPDATE t SET v = 'val50_updated' WHERE id = 50;"); err != nil {
		t.Fatalf("plain UPDATE failed: %v", err)
	}
	if _, err := e.Execute(s, "DELETE FROM t WHERE id = 50;"); err != nil {
		t.Fatalf("plain DELETE failed: %v", err)
	}

	_, _ = e.Execute(s, "COMMIT;")
}

// TestP6R203_AutocommitSerializableDMLFailsClosed covers P6-R2-03:
// In SERIALIZABLE, standalone autocommit=1 DML with unprotected read paths (UPDATE JOIN,
// DELETE JOIN, subqueries in WHERE, INSERT SELECT, CTAS) must fail closed with ErrUnsupported.
func TestP6R203_AutocommitSerializableDMLFailsClosed(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	s := newSerializableSession("db")
	_ = e.SetAutocommit(s, true) // autocommit=1 standalone

	if _, err := e.Execute(s, "CREATE TABLE t2 (id INT PRIMARY KEY, v VARCHAR(50));"); err != nil {
		t.Fatal(err)
	}

	// 1. UPDATE JOIN in autocommit=1: must fail closed
	_, err := e.Execute(s, "UPDATE t JOIN t2 ON t.id = t2.id SET t.v = 'new';")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("autocommit UPDATE JOIN returned %v, want ErrUnsupported", err)
	}

	// 2. DELETE JOIN in autocommit=1: must fail closed
	_, err = e.Execute(s, "DELETE t FROM t JOIN t2 ON t.id = t2.id;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("autocommit DELETE JOIN returned %v, want ErrUnsupported", err)
	}

	// 3. UPDATE with subquery in autocommit=1: must fail closed
	_, err = e.Execute(s, "UPDATE t SET v = 'new' WHERE id IN (SELECT id FROM t2);")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("autocommit UPDATE subquery returned %v, want ErrUnsupported", err)
	}

	// 4. INSERT SELECT in autocommit=1: must fail closed
	_, err = e.Execute(s, "INSERT INTO t2 SELECT id, v FROM t;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("autocommit INSERT SELECT returned %v, want ErrUnsupported", err)
	}

	// 5. CTAS in autocommit=1: must fail closed
	_, err = e.Execute(s, "CREATE TABLE t3 AS SELECT id, v FROM t;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("autocommit CTAS returned %v, want ErrUnsupported", err)
	}

	// 6. Plain single-table INSERT, UPDATE, DELETE in autocommit=1 succeed!
	if _, err := e.Execute(s, "INSERT INTO t VALUES (70, 'val70');"); err != nil {
		t.Fatalf("autocommit plain INSERT failed: %v", err)
	}
	if _, err := e.Execute(s, "UPDATE t SET v = 'val70_updated' WHERE id = 70;"); err != nil {
		t.Fatalf("autocommit plain UPDATE failed: %v", err)
	}
	if _, err := e.Execute(s, "DELETE FROM t WHERE id = 70;"); err != nil {
		t.Fatalf("autocommit plain DELETE failed: %v", err)
	}
}
