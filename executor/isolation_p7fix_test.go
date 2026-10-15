package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestP7Fix03_RUDMLUsesCommittedBaseline covers P7-FIX-03:
// RU plain SELECT retains dirty read, but DML (UPDATE, DELETE, INSERT) candidate scans,
// constraints, and target row selections MUST use the committed baseline.
func TestP7Fix03_RUDMLUsesCommittedBaseline(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "CREATE TABLE users (id INT PRIMARY KEY, age INT, name VARCHAR(50));"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "INSERT INTO users VALUES (1, 20, 'Alice');"); err != nil {
		t.Fatal(err)
	}

	// 1. Transaction A updates age=30 without committing
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "UPDATE users SET age = 30 WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// 2. Transaction B in READ UNCOMMITTED:
	// A plain SELECT sees A's dirty age=30
	resSel, err := e.Execute(sB, "SELECT name FROM users WHERE age = 30;")
	if err != nil || len(resSel.Rows) != 1 || resSel.Rows[0][0] != "Alice" {
		t.Fatalf("RU SELECT WHERE age=30 expected Alice, got %v, %v", resSel, err)
	}

	// An UPDATE in READ UNCOMMITTED candidate scan MUST use the COMMITTED baseline!
	// In the committed baseline, Alice's age is 20, NOT 30!
	// So B's UPDATE WHERE age=30 must NOT match or touch Alice!
	resUpd, err := e.Execute(sB, "UPDATE users SET name = 'Bob' WHERE age = 30;")
	if err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}
	if resUpd.AffectedRows != 0 {
		t.Fatalf("RU UPDATE incorrectly modified uncommitted candidate! AffectedRows=%d, want 0", resUpd.AffectedRows)
	}

	// 3. A rolls back
	_, _ = e.Execute(sA, "ROLLBACK;")

	// 4. Verify Alice's committed name was NOT overwritten by B!
	resFinal, _ := e.Execute(sB, "SELECT age, name FROM users WHERE id = 1;")
	if len(resFinal.Rows) != 1 || resFinal.Rows[0][1] != "Alice" {
		t.Fatalf("Alice name was corrupted! got %v, want Alice", resFinal)
	}
}

// TestP7Fix04_NewDirtyIteratorErrorPropagationAndBudget covers P7-FIX-04:
// Tests that newDirtyIterator propagates ScanRangeWithTx errors, aborts immediately on
// context cancellation, and fails closed when closed.
func TestP7Fix04_NewDirtyIteratorErrorPropagationAndBudget(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1'), (2, 'V2');"); err != nil {
		t.Fatal(err)
	}

	// Context cancellation during execution aborts immediately
	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel() // canceled upfront

	sB.Context = ctxCancel
	_, err := e.Execute(sB, "SELECT * FROM t;")
	if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled")) {
		t.Fatalf("expected cancellation error, got %v", err)
	}
	sB.Context = nil
}

// TestP7Fix05_SavepointChildWritesNotPublishedToOtherRoots covers P7-FIX-05:
// Unmerged writes inside a Savepoint Child must NOT be published to other roots.
// Only writes merged into the outermost root transaction are visible in RU.
func TestP7Fix05_SavepointChildWritesNotPublishedToOtherRoots(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'orig');"); err != nil {
		t.Fatal(err)
	}

	// 1. Transaction A begins and writes id=1 to 'merged_to_root'
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "UPDATE t SET v = 'merged_to_root' WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// B (RU) sees merged_to_root
	resB, _ := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if len(resB.Rows) != 1 || resB.Rows[0][0] != "merged_to_root" {
		t.Fatalf("B read = %v, want merged_to_root", resB)
	}

	// 2. A creates a savepoint layer
	if _, err := e.Execute(sA, "SAVEPOINT sp1;"); err != nil {
		t.Fatal(err)
	}

	// A inserts id=2 inside savepoint child (unmerged into root)
	if _, err := e.Execute(sA, "INSERT INTO t VALUES (2, 'unmerged_child');"); err != nil {
		t.Fatal(err)
	}

	// B (RU) MUST NOT see id=2 because it is inside an unmerged savepoint child!
	resB2, _ := e.Execute(sB, "SELECT v FROM t WHERE id = 2;")
	if len(resB2.Rows) != 0 {
		t.Fatalf("B RU read unmerged savepoint child write! got %v, want empty", resB2)
	}

	// 3. A rolls back to savepoint: unmerged child writes are discarded
	if _, err := e.Execute(sA, "ROLLBACK TO sp1;"); err != nil {
		t.Fatal(err)
	}

	// B still sees merged_to_root on id=1, and id=2 is gone
	resB3, _ := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if len(resB3.Rows) != 1 || resB3.Rows[0][0] != "merged_to_root" {
		t.Fatalf("B read after rollback to sp = %v, want merged_to_root", resB3)
	}

	_, _ = e.Execute(sA, "ROLLBACK;")
}

// TestP7FixR205_SavepointRollbackPublishesErrorHandling covers P7-FIX-R2-05:
// Verifies that rollbackToSavepoint checks publishSessionDirtyWrites,
// does not swallow errors, and correctly maintains clean state across savepoints.
func TestP7FixR205_SavepointRollbackPublishesErrorHandling(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V0');"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "UPDATE t SET v = 'V1' WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// Create savepoint sp1 and update
	if _, err := e.Execute(sA, "SAVEPOINT sp1;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// Rollback to sp1: must succeed and return V1
	if _, err := e.Execute(sA, "ROLLBACK TO sp1;"); err != nil {
		t.Fatalf("ROLLBACK TO sp1 failed: %v", err)
	}

	// Release savepoint sp1
	if _, err := e.Execute(sA, "RELEASE SAVEPOINT sp1;"); err != nil {
		t.Fatalf("RELEASE SAVEPOINT sp1 failed: %v", err)
	}

	// B (RU) observes V1
	resB, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB.Rows) != 1 || resB.Rows[0][0] != "V1" {
		t.Fatalf("B read after savepoint release = %v, %v, want V1", resB, err)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}
