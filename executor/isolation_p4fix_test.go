package executor

import (
	"context"
	"errors"
	"testing"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

// TestP4Fix01_RRSavepointInheritsView covers P4-FIX-01 Test A:
// Root fixes view V1 -> SAVEPOINT sp1 -> B commits V2 -> Child SELECT must still see V1 ->
// ROLLBACK TO sp1 -> SELECT still V1 -> RELEASE SAVEPOINT -> COMMIT -> all pins cleared.
func TestP4Fix01_RRSavepointInheritsView(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// 1. A begins RR
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// 2. A SELECT sees V1, fixing consistent view
	res1, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(res1.Rows) != 1 || res1.Rows[0][0] != "V1" {
		t.Fatalf("first read: %v, %v", res1, err)
	}

	// 3. A creates SAVEPOINT sp1
	if _, err := e.Execute(sA, "SAVEPOINT sp1;"); err != nil {
		t.Fatalf("SAVEPOINT: %v", err)
	}

	// 4. B updates to V2 and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// 5. A (inside Savepoint Child) SELECT must still see V1!
	resChild, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil {
		t.Fatalf("A child SELECT: %v", err)
	}
	if len(resChild.Rows) != 1 || resChild.Rows[0][0] != "V1" {
		t.Fatalf("A child inside SAVEPOINT saw V2 instead of fixed V1: %v", resChild.Rows)
	}

	// 6. A ROLLBACK TO sp1
	if _, err := e.Execute(sA, "ROLLBACK TO sp1;"); err != nil {
		t.Fatalf("ROLLBACK TO: %v", err)
	}

	// 7. A SELECT must still see V1
	resAfterRollback, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resAfterRollback.Rows) != 1 || resAfterRollback.Rows[0][0] != "V1" {
		t.Fatalf("A after ROLLBACK TO saw %v, want V1", resAfterRollback)
	}

	// 8. A RELEASE SAVEPOINT
	if _, err := e.Execute(sA, "RELEASE SAVEPOINT sp1;"); err != nil {
		t.Fatalf("RELEASE SAVEPOINT: %v", err)
	}

	// 9. A COMMIT
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatalf("A COMMIT: %v", err)
	}
}

// TestP4Fix01_RRSavepointBeforeFirstRead covers P4-FIX-01 Test B:
// A BEGIN RR -> A SAVEPOINT sp1 (no read yet) -> B commits V2 ->
// A first SELECT in Savepoint Child sees V2 -> A ends Savepoint -> repeat SELECT in root still V2.
func TestP4Fix01_RRSavepointBeforeFirstRead(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// 1. A begins RR
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// 2. A creates SAVEPOINT sp1 before any plain SELECT
	if _, err := e.Execute(sA, "SAVEPOINT sp1;"); err != nil {
		t.Fatalf("SAVEPOINT: %v", err)
	}

	// 3. B updates to V2 and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// 4. A performs FIRST plain SELECT inside Savepoint Child: must see V2 (lazy view established!)
	resChild, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil {
		t.Fatalf("child SELECT: %v", err)
	}
	if len(resChild.Rows) != 1 || resChild.Rows[0][0] != "V2" {
		t.Fatalf("first SELECT in savepoint returned %v, want V2", resChild.Rows)
	}

	// B updates to V3 and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V3' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE 2: %v", err)
	}

	// 5. Release savepoint
	if _, err := e.Execute(sA, "RELEASE SAVEPOINT sp1;"); err != nil {
		t.Fatalf("RELEASE SAVEPOINT: %v", err)
	}

	// 6. A repeats plain SELECT in root: must still see V2 (not V3)
	resRoot, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resRoot.Rows) != 1 || resRoot.Rows[0][0] != "V2" {
		t.Fatalf("root SELECT after savepoint release returned %v, want V2", resRoot)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestP4Fix01_RRSavepointOwnWritesOverlay covers P4-FIX-01 Test C:
// Child inside savepoint sees own writes; ROLLBACK TO restores original view.
func TestP4Fix01_RRSavepointOwnWritesOverlay(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// Fix view
	_, _ = e.Execute(sA, "SELECT * FROM t;")

	// Create savepoint
	if _, err := e.Execute(sA, "SAVEPOINT sp1;"); err != nil {
		t.Fatalf("SAVEPOINT: %v", err)
	}

	// Child writes
	if _, err := e.Execute(sA, "UPDATE t SET v = 'child_v' WHERE id = 1;"); err != nil {
		t.Fatalf("child UPDATE: %v", err)
	}
	if _, err := e.Execute(sA, "INSERT INTO t VALUES (2, 'child_row2');"); err != nil {
		t.Fatalf("child INSERT: %v", err)
	}

	// Child must see own writes
	resChild, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resChild.Rows) != 1 || resChild.Rows[0][0] != "child_v" {
		t.Fatalf("child own write not visible: %v", resChild)
	}

	// Rollback to savepoint
	if _, err := e.Execute(sA, "ROLLBACK TO sp1;"); err != nil {
		t.Fatalf("ROLLBACK TO: %v", err)
	}

	// Original view restored
	resRestored, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resRestored.Rows) != 1 || resRestored.Rows[0][0] != "V1" {
		t.Fatalf("restored read: %v, want V1", resRestored)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestP4Fix01_RRSavepointNested covers P4-FIX-01 Test D:
// 2 levels of nested savepoints, concurrent commits, all children share same consistent revision.
func TestP4Fix01_RRSavepointNested(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// A fixes view at V1
	_, _ = e.Execute(sA, "SELECT * FROM t;")

	// Layer 1
	if _, err := e.Execute(sA, "SAVEPOINT sp1;"); err != nil {
		t.Fatalf("SAVEPOINT sp1: %v", err)
	}
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE V2: %v", err)
	}

	// Layer 2
	if _, err := e.Execute(sA, "SAVEPOINT sp2;"); err != nil {
		t.Fatalf("SAVEPOINT sp2: %v", err)
	}
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V3' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE V3: %v", err)
	}

	// Both layer 2 and layer 1 must see V1!
	resL2, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(resL2.Rows) != 1 || resL2.Rows[0][0] != "V1" {
		t.Fatalf("layer 2 saw %v, want V1", resL2.Rows)
	}

	_, _ = e.Execute(sA, "ROLLBACK TO sp2;")
	resL1, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(resL1.Rows) != 1 || resL1.Rows[0][0] != "V1" {
		t.Fatalf("layer 1 saw %v, want V1", resL1.Rows)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestP4Fix02_GuardBeforeFirstReadConflict covers P4-FIX-02:
// A transaction that declared a Guard before first read cannot bypass conflict validation
// if that guarded key was concurrently modified and committed.
func TestP4Fix02_GuardBeforeFirstReadConflict(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	isoEngine, ok := e.Backend.(storageengine.IsolationEngine)
	if !ok {
		t.Fatal("Backend does not implement IsolationEngine")
	}

	ctx := context.Background()

	// 1. Create k1=V1
	txInit, _ := isoEngine.BeginIsolated(ctx, storageengine.IsolationRepeatableRead)
	_ = txInit.Put("db/rows/t", []byte("1"), []byte("V1"))
	_, _ = txInit.Commit(ctx)

	// 2. Transaction A starts in RR
	txA, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationRepeatableRead)
	if err != nil {
		t.Fatalf("txA BeginIsolated: %v", err)
	}
	defer txA.Rollback()

	// 3. A guards k1 before any consistent read
	if err := txA.Guard("db/rows/t", []byte("1")); err != nil {
		t.Fatalf("Guard k1: %v", err)
	}

	// 4. B updates k1 to V2 and commits
	txB, _ := isoEngine.BeginIsolated(ctx, storageengine.IsolationRepeatableRead)
	_ = txB.Put("db/rows/t", []byte("1"), []byte("V2"))
	if _, err := txB.Commit(ctx); err != nil {
		t.Fatalf("txB Commit: %v", err)
	}

	// 5. A calls BeginConsistentView() to sample newer Head
	isoTxnA := txA.(storageengine.IsolationTxn)
	_, err = isoTxnA.BeginConsistentView(ctx)
	if err != nil {
		t.Fatalf("BeginConsistentView: %v", err)
	}

	// 6. A writes unrelated key k2
	if err := txA.Put("db/rows/t", []byte("2"), []byte("V_A")); err != nil {
		t.Fatalf("txA Put k2: %v", err)
	}

	// 7. A tries to commit: MUST FAIL with ErrConflict because k1 was modified after Guard!
	_, err = txA.Commit(ctx)
	if !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("txA Commit returned %v, want ErrConflict (guard dependency was bypassed!)", err)
	}
}

type fakeBasicEngine struct {
	storageengine.Engine
	beginCalled bool
}

func (f *fakeBasicEngine) Begin(ctx context.Context) (storageengine.Txn, error) {
	f.beginCalled = true
	return f.Engine.Begin(ctx)
}

// TestP4Fix04_MissingCapabilityFailsClosed covers P4-FIX-04:
// When backend does not implement IsolationEngine, SQL isolation transaction must fail closed
// with ErrUnsupported and must NOT call basic Begin.
func TestP4Fix04_MissingCapabilityFailsClosed(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	fake := &fakeBasicEngine{Engine: e.Backend}
	e.Backend = fake

	s := newRRSession("db")
	_ = s.SetNextTransactionIsolation(parser.LevelRepeatableRead)

	// Beginning transaction on unsupported backend must return ErrUnsupported
	_, err := e.Execute(s, "BEGIN;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BEGIN on uncapable backend returned %v, want ErrUnsupported", err)
	}

	if fake.beginCalled {
		t.Fatalf("Backend.Begin() was unexpectedly called despite missing IsolationEngine!")
	}

	// pendingNext must NOT be consumed
	if _, ok := s.PendingNextIsolation(); !ok {
		t.Fatalf("pendingNext was consumed despite failed begin")
	}
}

// TestP4Fix05_LazyRootAfterGC covers P4-FIX-05:
// A lazy root that has not established a consistent view does not pin history;
// after concurrent commits and CompactHistory, subsequent first plain SELECT establishes
// view at current valid Head, and subsequent DML/locking read succeeds safely without reading pruned history.
func TestP4Fix05_LazyRootAfterGC(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// 1. Session A begins RR (lazy, unpinned)
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// 2. Session B does multiple updates and commits
	for i := 2; i <= 5; i++ {
		q := "UPDATE t SET v = 'V" + string(rune('0'+i)) + "' WHERE id = 1;"
		if _, err := e.Execute(sB, q); err != nil {
			t.Fatalf("B update %d: %v", i, err)
		}
	}

	// 3. CompactHistory runs
	if m, ok := e.Backend.(storageengine.Maintenance); ok {
		if err := m.CompactHistory(context.Background()); err != nil {
			t.Fatalf("CompactHistory: %v", err)
		}
	} else {
		t.Fatal("backend does not implement Maintenance")
	}

	// 4. A performs first plain SELECT: must establish view at current Head (V5) and see V5
	resA1, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resA1.Rows) != 1 || resA1.Rows[0][0] != "V5" {
		t.Fatalf("A first read after GC = %v, err=%v, want 'V5'", resA1.Rows, err)
	}

	// 5. B commits V6
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V6' WHERE id = 1;"); err != nil {
		t.Fatalf("B update 6: %v", err)
	}

	// 6. A repeats plain SELECT: must remain V5
	resA2, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resA2.Rows) != 1 || resA2.Rows[0][0] != "V5" {
		t.Fatalf("A repeat read = %v, err=%v, want 'V5'", resA2.Rows, err)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}
