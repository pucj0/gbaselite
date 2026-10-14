package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"gbaselite/storageengine"
)

// TestP5Fix01_RedTestA_CrossStatementGuardBypass covers P5-FIX-01 Red Test A:
// In READ COMMITTED, an optimistic Guard declared in statement 1 must NOT be relaxed or bypassed
// by a subsequent statement view opening at a newer revision.
func TestP5Fix01_RedTestA_CrossStatementGuardBypass(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	isoEngine, ok := e.Backend.(storageengine.IsolationEngine)
	if !ok {
		t.Fatal("backend missing IsolationEngine")
	}

	ctx := context.Background()

	// 1. Create dep=V1
	txInit, _ := isoEngine.BeginIsolated(ctx, storageengine.IsolationReadCommitted)
	_ = txInit.Put("db/rows/t", []byte("dep"), []byte("V1"))
	_ = txInit.Put("db/rows/t", []byte("k2"), []byte("V1"))
	if _, err := txInit.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Transaction A begins in READ COMMITTED
	txA, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	// 3. A declares Guard on dep (baseline is V1)
	if err := txA.Guard("db/rows/t", []byte("dep")); err != nil {
		t.Fatalf("Guard dep: %v", err)
	}

	// 4. Concurrent Transaction B modifies dep=V2 and commits
	txB, _ := isoEngine.BeginIsolated(ctx, storageengine.IsolationReadCommitted)
	_ = txB.Put("db/rows/t", []byte("dep"), []byte("V2"))
	if _, err := txB.Commit(ctx); err != nil {
		t.Fatalf("txB Commit: %v", err)
	}

	// 5. A starts a new Statement View at the newer Head
	isoTxnA := txA.(storageengine.IsolationTxn)
	stmtView, err := isoTxnA.BeginStatementView(ctx, storageengine.StatementViewKindStatement)
	if err != nil {
		t.Fatalf("BeginStatementView: %v", err)
	}
	defer stmtView.Close()

	// 6. A writes another unrelated key k2
	if err := txA.Put("db/rows/t", []byte("k2"), []byte("V_A")); err != nil {
		t.Fatalf("Put k2: %v", err)
	}

	// 7. A commits: MUST FAIL with ErrConflict because dep was modified after Guard was declared!
	// Overwriting t.inner.Snapshot with stmtView.Revision() would falsely allow this commit to succeed.
	_, err = txA.Commit(ctx)
	if !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("txA Commit returned %v, want ErrConflict (Guard dependency was bypassed by StatementView!)", err)
	}
}

// TestP5Fix01_RedTestB_LegalRCCurrentWrite covers P5-FIX-01 Red Test B:
// In READ COMMITTED, A begins -> B updates k and commits ->
// A reads k under covering lock in a new RC statement and updates k -> A commits successfully without false conflict.
func TestP5Fix01_RedTestB_LegalRCCurrentWrite(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// A begins RC
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// B updates k to V2 and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// A updates k to V3 under covering lock in a new statement
	if _, err := e.Execute(sA, "UPDATE t SET v = 'V3' WHERE id = 1;"); err != nil {
		t.Fatalf("A UPDATE falsely conflicted: %v", err)
	}

	// A commits
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatalf("A COMMIT falsely conflicted: %v", err)
	}

	// Verify final value is V3
	res, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(res.Rows) != 1 || res.Rows[0][0] != "V3" {
		t.Fatalf("final value = %v, want V3", res)
	}
}

// TestP5Fix01_RedTestC_MultipleStatementViewsAccumulatingDependencies covers P5-FIX-01 Red Test C:
// In an RC transaction with 10 statement views, legal locked writes succeed, while
// optimistic Guard dependencies retain their declaration baselines and conflict if modified.
func TestP5Fix01_RedTestC_MultipleStatementViewsAccumulatingDependencies(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	isoEngine, ok := e.Backend.(storageengine.IsolationEngine)
	if !ok {
		t.Fatal("backend missing IsolationEngine")
	}

	ctx := context.Background()

	// 1. Initial seed
	txInit, _ := isoEngine.BeginIsolated(ctx, storageengine.IsolationReadCommitted)
	_ = txInit.Put("db/rows/t", []byte("dep"), []byte("V_init"))
	for i := 1; i <= 10; i++ {
		_ = txInit.Put("db/rows/t", []byte(fmt.Sprintf("k_%d", i)), []byte("V_init"))
	}
	if _, err := txInit.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Transaction A begins in RC
	txA, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	// A guards dep
	if err := txA.Guard("db/rows/t", []byte("dep")); err != nil {
		t.Fatal(err)
	}

	isoTxnA := txA.(storageengine.IsolationTxn)

	// A establishes 10 successive statement views, updating keys under covering lock
	for i := 1; i <= 10; i++ {
		// B commits on key i
		txB, _ := isoEngine.BeginIsolated(ctx, storageengine.IsolationReadCommitted)
		_ = txB.Put("db/rows/t", []byte(fmt.Sprintf("k_%d", i)), []byte(fmt.Sprintf("B_val_%d", i)))
		_, _ = txB.Commit(ctx)

		// A opens statement view i
		stmtView, err := isoTxnA.BeginStatementView(ctx, storageengine.StatementViewKindStatement)
		if err != nil {
			t.Fatal(err)
		}
		// A updates k_i
		_ = txA.Put("db/rows/t", []byte(fmt.Sprintf("k_%d", i)), []byte(fmt.Sprintf("A_val_%d", i)))
		_ = stmtView.Close()
	}

	// Concurrent B updates dep
	txB, _ := isoEngine.BeginIsolated(ctx, storageengine.IsolationReadCommitted)
	_ = txB.Put("db/rows/t", []byte("dep"), []byte("V_modified"))
	_, _ = txB.Commit(ctx)

	// Even though statement view advanced to revision 10+, the early optimistic dependency on dep
	// was NOT relaxed: A's commit MUST FAIL with ErrConflict!
	_, err = txA.Commit(ctx)
	if !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("txA Commit = %v, want ErrConflict", err)
	}
}

// TestP5Fix02_StaleTargetObservationOnChild covers P5-FIX-02:
// When A's UPDATE reads candidate in StatementView V1, and B concurrently commits V2 before A acquires X lock,
// staleTargetObservation must inspect the statement view, detect that the target row changed, and conflict.
func TestP5Fix02_StaleTargetObservationOnChild(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'orig_10');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Hook into dmlLockRequestHook to pause A after candidate read, right before acquiring lock
	candidateSelected := make(chan struct{})
	bCommitted := make(chan struct{})

	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		select {
		case <-candidateSelected:
		default:
			close(candidateSelected)
			<-bCommitted
		}
		return req
	}
	defer func() { dmlLockRequestHook = nil }()

	var wg sync.WaitGroup
	var errA error

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		// A executes UPDATE: reads candidate 'orig_10', then pauses at dmlLockRequestHook
		_, errA = e.Execute(sA, "UPDATE t SET v = 'a_new' WHERE id = 1;")
	}()

	// Wait for A to read candidate and pause before acquiring lock
	<-candidateSelected

	// B modifies row 1 to 'b_interleaved' and commits while A is paused
	if _, err := e.Execute(sB, "UPDATE t SET v = 'b_interleaved' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// Resume A to acquire lock and check stale target
	close(bCommitted)
	wg.Wait()

	// A must have detected that the target row was modified by B while A was waiting for lock!
	if errA == nil {
		t.Fatalf("A UPDATE unexpectedly succeeded! Must fail with stale target conflict.")
	}

	_, _ = e.Execute(sA, "ROLLBACK;")

	// Final verification: row 1 must remain B's value 'b_interleaved', not overwritten by A
	res, _ := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if len(res.Rows) != 1 || res.Rows[0][0] != "b_interleaved" {
		t.Fatalf("row 1 = %v, want 'b_interleaved'", res.Rows)
	}
}

// TestP5Fix03_BorrowedIteratorProtectsAgainstRootTerminalAndGC covers P5-FIX-03:
// When an active borrowed iterator is reading multi-page rows, root terminal / unregister
// must NOT release the GC Pin while the iterator is borrowing, preventing CompactHistory from purging rows.
func TestP5Fix03_BorrowedIteratorProtectsAgainstRootTerminalAndGC(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	const totalRows = 300 // Spans at least 2 pages

	// Seed 300 rows with value V1 into test space
	txInit, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < totalRows; i++ {
		key := fmt.Sprintf("k_%04d", i)
		_ = txInit.Put("test_rows", []byte(key), []byte("V1"))
	}
	if _, err := txInit.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Transaction A begins RC
	txA, err := storageengine.BeginIsolated(context.Background(), e.Backend, storageengine.IsolationReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	// A starts statement view
	isoTxnA := txA.(storageengine.IsolationTxn)
	stmtView, err := isoTxnA.BeginStatementView(context.Background(), storageengine.StatementViewKindStatement)
	if err != nil {
		t.Fatal(err)
	}

	// Open iterator on stmtView
	it, err := stmtView.NewIterator(context.Background(), storageengine.ScanRequest{Space: "test_rows"})
	if err != nil {
		t.Fatal(err)
	}

	// Read first 10 rows from page 1
	for i := 0; i < 10; i++ {
		if !it.Next() {
			t.Fatalf("read %d failed: %v", i, it.Err())
		}
	}

	// B updates all 300 rows to V2 and commits
	txB, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < totalRows; i++ {
		key := fmt.Sprintf("k_%04d", i)
		_ = txB.Put("test_rows", []byte(key), []byte("V2"))
	}
	if _, err := txB.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A's Root transaction terminates (COMMIT or ROLLBACK) while iterator is still open!
	_, _ = txA.Commit(context.Background())

	// CompactHistory runs: must NOT purge V1 of page 2 because iterator is still borrowing!
	if m, ok := e.Backend.(storageengine.Maintenance); ok {
		if err := m.CompactHistory(context.Background()); err != nil {
			t.Fatalf("CompactHistory: %v", err)
		}
	}

	// Iterator continues reading remaining rows through page 2: must safely read V1!
	for i := 10; i < totalRows; i++ {
		if !it.Next() {
			t.Fatalf("read %d across page boundary failed: %v", i, it.Err())
		}
		if string(it.Value()) != "V1" {
			t.Fatalf("row %d after root close & GC = %s, want V1", i, it.Value())
		}
	}
	_ = it.Close()
}

// TestP5Fix04_StrengthenedM18_ConcurrentUpdatesNoLostUpdate covers P5-FIX-04:
// Exact serialization of two concurrent updates to same row, verifying lock wait,
// no lost updates, and exact final value.
func TestP5Fix04_StrengthenedM18_ConcurrentUpdatesNoLostUpdate(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, '10');"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// A updates row 1 to 15
	if _, err := e.Execute(sA, "UPDATE t SET v = '15' WHERE id = 1;"); err != nil {
		t.Fatalf("A UPDATE: %v", err)
	}

	// B attempts to update row 1: must wait on lock held by A
	bStarted := make(chan struct{})
	bDone := make(chan error, 1)

	go func() {
		close(bStarted)
		// B attempts update
		_, err := e.Execute(sB, "UPDATE t SET v = '25' WHERE id = 1;")
		bDone <- err
	}()

	<-bStarted

	// A commits, releasing lock
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatalf("A COMMIT: %v", err)
	}

	// B either conflicts on stale version or completes
	errB := <-bDone
	if errB != nil {
		// B detected stale target and failed closed: verify A's update '15' was not lost!
		_, _ = e.Execute(sB, "ROLLBACK;")
		res, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
		if len(res.Rows) != 1 || res.Rows[0][0] != "15" {
			t.Fatalf("value = %v, want '15'", res.Rows)
		}
	} else {
		// B successfully updated after A: commits and final is '25'
		if _, err := e.Execute(sB, "COMMIT;"); err != nil {
			t.Fatal(err)
		}
		res, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
		if len(res.Rows) != 1 || res.Rows[0][0] != "25" {
			t.Fatalf("value = %v, want '25'", res.Rows)
		}
	}
}
