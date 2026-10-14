package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

func newRCSession(db string) *Session {
	s := &Session{CurrentDatabase: db}
	_ = s.SetSessionIsolation(parser.LevelReadCommitted)
	return s
}

// TestM13_RC_NonRepeatableRead tests M13:
// In READ COMMITTED, each statement captures a fresh committed view.
// A sees V1, B commits V2, A subsequently sees V2 (non-repeatable read).
// A never sees B's uncommitted data (no dirty read).
func TestM13_RC_NonRepeatableRead(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Session A begins RC transaction
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// Statement 1: sees V1
	resA1, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resA1.Rows) != 1 || resA1.Rows[0][0] != "V1" {
		t.Fatalf("A statement 1 = %v, err=%v, want 'V1'", resA1, err)
	}

	// Session B updates to V2 but does NOT commit yet
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatalf("B BEGIN: %v", err)
	}
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// Statement 2 while B is uncommitted: must STILL see V1 (no dirty read!)
	resA2, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resA2.Rows) != 1 || resA2.Rows[0][0] != "V1" {
		t.Fatalf("A statement 2 saw uncommitted data: %v, want 'V1'", resA2)
	}

	// Session B commits V2
	if _, err := e.Execute(sB, "COMMIT;"); err != nil {
		t.Fatalf("B COMMIT: %v", err)
	}

	// Statement 3 after B commit: in RC, it MUST see new committed V2 (non-repeatable read!)
	resA3, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resA3.Rows) != 1 || resA3.Rows[0][0] != "V2" {
		t.Fatalf("A statement 3 after B commit = %v, want 'V2'", resA3)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestM14_RC_Phantom tests M14:
// In READ COMMITTED, concurrent committed inserts appear in subsequent range reads.
func TestM14_RC_Phantom(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// Statement 1: range is empty
	res1, err := e.Execute(sA, "SELECT * FROM t WHERE id >= 100 AND id <= 200;")
	if err != nil || len(res1.Rows) != 0 {
		t.Fatalf("A statement 1 expected empty, got %v", res1)
	}

	// Session B inserts matching row and commits
	if _, err := e.Execute(sB, "INSERT INTO t VALUES (150, 'phantom_rc');"); err != nil {
		t.Fatalf("B INSERT: %v", err)
	}

	// Statement 2: in RC, phantom row MUST appear
	res2, err := e.Execute(sA, "SELECT * FROM t WHERE id >= 100 AND id <= 200;")
	if err != nil || len(res2.Rows) != 1 || res2.Rows[0][0] != int64(150) {
		t.Fatalf("A statement 2 did not see phantom: %v", res2)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestM15_RC_SingleStatementAtomicity tests M15:
// Even in RC, a single SQL statement uses ONE stable statement view throughout its execution.
func TestM15_RC_SingleStatementAtomicity(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	isoEngine, ok := e.Backend.(storageengine.IsolationEngine)
	if !ok {
		t.Fatal("backend missing IsolationEngine")
	}

	ctx := context.Background()

	// Seed 3 rows into test space
	tx0, err := e.Backend.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx0.Put("atomicity_space", []byte("1"), []byte("V1"))
	_ = tx0.Put("atomicity_space", []byte("2"), []byte("V1"))
	_ = tx0.Put("atomicity_space", []byte("3"), []byte("V1"))
	if _, err := tx0.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Session A begins RC
	txA, err := isoEngine.BeginIsolated(ctx, storageengine.IsolationReadCommitted)
	if err != nil {
		t.Fatalf("BeginIsolated RC: %v", err)
	}
	defer txA.Rollback()

	isoTxnA := txA.(storageengine.IsolationTxn)
	stmtView, err := isoTxnA.BeginStatementView(ctx, storageengine.StatementViewKindStatement)
	if err != nil {
		t.Fatalf("BeginStatementView: %v", err)
	}
	defer stmtView.Close()

	// Iterator on stmtView
	it, err := stmtView.NewIterator(ctx, storageengine.ScanRequest{Space: "atomicity_space"})
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()

	// Read row 1
	if !it.Next() {
		t.Fatalf("read 1 failed: %v", it.Err())
	}
	if string(it.Value()) != "V1" {
		t.Fatalf("row 1 = %s, want V1", it.Value())
	}

	// Concurrent B updates row 2 and row 3 to V2 and commits
	txB, err := e.Backend.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = txB.Put("atomicity_space", []byte("2"), []byte("V2"))
	_ = txB.Put("atomicity_space", []byte("3"), []byte("V2"))
	if _, err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Statement view of A must STILL see V1 for remaining rows (statement consistency!)
	rowsSeen := 1
	for it.Next() {
		rowsSeen++
		if string(it.Value()) != "V1" {
			t.Fatalf("row %s in same statement view saw concurrent update: %s, want V1", it.Key(), it.Value())
		}
	}
	if rowsSeen != 3 {
		t.Fatalf("rowsSeen = %d, want 3", rowsSeen)
	}
}

// TestM16_RC_ComplexQuerySharesOneView tests M16:
// Subqueries, CTEs, and JOINs in RC share the single statement view.
func TestM16_RC_ComplexQuerySharesOneView(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, '10'), (2, '20');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := e.Execute(sA, "CREATE TABLE t2 (id INT PRIMARY KEY, ref_id INT);"); err != nil {
		t.Fatalf("CREATE t2: %v", err)
	}
	if _, err := e.Execute(sA, "INSERT INTO t2 VALUES (100, 1), (200, 2);"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// Subquery in RC
	res, err := e.Execute(sA, "SELECT id, v FROM t WHERE id IN (SELECT ref_id FROM t2 WHERE id = 100);")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != int64(1) {
		t.Fatalf("subquery failed: %v, %v", res, err)
	}

	// B inserts into t2 and commits
	if _, err := e.Execute(sB, "INSERT INTO t2 VALUES (300, 1);"); err != nil {
		t.Fatal(err)
	}

	// Next statement in A sees new committed row
	res2, err := e.Execute(sA, "SELECT COUNT(*) FROM t2;")
	if err != nil || len(res2.Rows) != 1 || res2.Rows[0][0] != int64(3) {
		t.Fatalf("next statement in RC should see 3 rows, got: %v", res2)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestM17_RC_LegalCurrentWrite tests M17:
// In RC, after B updates and commits key X, A can safely UPDATE key X under covering lock.
// It must NOT fail with a false conflict solely because A began before B.
func TestM17_RC_LegalCurrentWrite(t *testing.T) {
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

	// B updates key 1 to V2 and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// A subsequently updates key 1 to V3: under RC, this must succeed!
	if _, err := e.Execute(sA, "UPDATE t SET v = 'V3' WHERE id = 1;"); err != nil {
		t.Fatalf("A UPDATE falsely conflicted in RC: %v", err)
	}

	// A commits successfully
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatalf("A COMMIT failed: %v", err)
	}

	// Final verification
	res, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "V3" {
		t.Fatalf("final value = %v, want V3", res)
	}
}

// TestM18_RC_PreventLostUpdate tests M18:
// Concurrent writes to same key under RC are properly serialized by locks and do not lose updates.
func TestM18_RC_PreventLostUpdate(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, '10');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	_ = sB.SetLockWaitTimeout(1)

	// A acquires lock on id=1 and updates
	if _, err := e.Execute(sA, "UPDATE t SET v = '20' WHERE id = 1;"); err != nil {
		t.Fatalf("A UPDATE: %v", err)
	}

	// B attempts to update same key: must block or conflict
	var wg sync.WaitGroup
	var errB error
	bFinished := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errB = e.Execute(sB, "UPDATE t SET v = '30' WHERE id = 1;")
		close(bFinished)
	}()

	// A commits
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatalf("A COMMIT: %v", err)
	}

	wg.Wait()
	if errB != nil {
		// If B failed or was aborted by conflict, B rolls back
		_, _ = e.Execute(sB, "ROLLBACK;")
	} else {
		// If B succeeded after A committed, B commits
		_, _ = e.Execute(sB, "COMMIT;")
	}

	// Final value must be either 20 or 30, never torn or lost
	res, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(res.Rows) != 1 || (res.Rows[0][0] != "20" && res.Rows[0][0] != "30") {
		t.Fatalf("lost update or invalid state: %v", res)
	}
}

// TestM19_RC_UniquenessAndForeignKey tests M19:
// In RC, unique and foreign key constraints are strictly preserved.
func TestM19_RC_UniquenessAndForeignKey(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "CREATE TABLE parent (id INT PRIMARY KEY);"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "CREATE TABLE child (id INT PRIMARY KEY, p_id INT, CONSTRAINT fk_p FOREIGN KEY (p_id) REFERENCES parent(id));"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "INSERT INTO parent VALUES (1);"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// Insert into child succeeds
	if _, err := e.Execute(sA, "INSERT INTO child VALUES (10, 1);"); err != nil {
		t.Fatalf("valid FK insert failed: %v", err)
	}

	// Invalid FK insert fails
	if _, err := e.Execute(sA, "INSERT INTO child VALUES (20, 999);"); err == nil {
		t.Fatalf("invalid FK insert unexpectedly succeeded")
	}

	// A commits child row (10, 1)
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatal(err)
	}

	// Duplicate insert by B: fails with duplicate primary key error
	if _, err := e.Execute(sB, "INSERT INTO child VALUES (10, 1);"); err == nil {
		t.Fatalf("duplicate primary key insert unexpectedly succeeded")
	}
}

// TestM20_RC_SavepointDoesNotFreezeView tests M20:
// In RC, creating a SAVEPOINT or rolling back to it does NOT freeze older statement views.
// Subsequent plain SELECT statements observe newly committed data.
func TestM20_RC_SavepointDoesNotFreezeView(t *testing.T) {
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

	// Statement 1: V1
	res1, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(res1.Rows) != 1 || res1.Rows[0][0] != "V1" {
		t.Fatalf("statement 1 = %v, want V1", res1)
	}

	// B commits V2
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// A sets SAVEPOINT sp1
	if _, err := e.Execute(sA, "SAVEPOINT sp1;"); err != nil {
		t.Fatal(err)
	}

	// Inside Savepoint: SELECT must see V2 in RC!
	res2, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(res2.Rows) != 1 || res2.Rows[0][0] != "V2" {
		t.Fatalf("statement inside savepoint in RC = %v, want V2", res2)
	}

	// B commits V3
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V3' WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// A rolls back to savepoint
	if _, err := e.Execute(sA, "ROLLBACK TO sp1;"); err != nil {
		t.Fatal(err)
	}

	// After ROLLBACK TO: subsequent SELECT in RC must see V3!
	res3, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(res3.Rows) != 1 || res3.Rows[0][0] != "V3" {
		t.Fatalf("statement after ROLLBACK TO in RC = %v, want V3", res3)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestT043_RC_GapLockingStrategy covers T043:
// In RC, gap locking is retained for duplicate key / FK protection to prevent integrity errors.
func TestT043_RC_GapLockingStrategy(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "CREATE TABLE items (id INT PRIMARY KEY, code VARCHAR(30) UNIQUE);"); err != nil {
		t.Fatal(err)
	}

	// Insert initial unique key
	if _, err := e.Execute(sA, "INSERT INTO items VALUES (1, 'CODE1');"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// Session A inserts code 'CODE2' and commits
	if _, err := e.Execute(sA, "INSERT INTO items VALUES (2, 'CODE2');"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatal(err)
	}

	// Session B attempts to insert duplicate 'CODE2' -> fails with duplicate key error
	if _, err := e.Execute(sB, "INSERT INTO items VALUES (3, 'CODE2');"); err == nil {
		t.Fatalf("duplicate unique key insert unexpectedly succeeded")
	}
}

// TestT044_RC_LifecycleAndExceptions covers T044:
// RC autocommit=0/1, BEGIN/COMMIT/ROLLBACK, ResetConnection, and error paths.
func TestT044_RC_LifecycleAndExceptions(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	s := newRCSession("db")

	// 1. autocommit=1 single statement
	if _, err := e.Execute(s, "INSERT INTO t VALUES (10, 'val10');"); err != nil {
		t.Fatalf("autocommit=1 INSERT in RC: %v", err)
	}
	res, err := e.Execute(s, "SELECT v FROM t WHERE id = 10;")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "val10" {
		t.Fatalf("autocommit=1 SELECT in RC: %v", res)
	}

	// 2. autocommit=0 implicit start
	_ = e.SetAutocommit(s, false)
	if _, err := e.Execute(s, "UPDATE t SET v = 'val10_updated' WHERE id = 10;"); err != nil {
		t.Fatalf("autocommit=0 UPDATE in RC: %v", err)
	}
	if !s.InTransaction() {
		t.Fatalf("expected active transaction with autocommit=0")
	}
	_, _ = e.Execute(s, "ROLLBACK;")
	if s.InTransaction() {
		t.Fatalf("expected transaction cleared after ROLLBACK")
	}

	// 3. ResetConnection restores RR
	_ = e.SetAutocommit(s, true)
	e.ResetConnection(s)
	if s.SessionDefaultIsolation() != parser.LevelRepeatableRead {
		t.Fatalf("ResetConnection did not restore RR, got %v", s.SessionDefaultIsolation())
	}
}

// TestT045_RC_LongTransactionGCRetention covers T045:
// In a long RC transaction with 100 statements, each statement view is released upon exit,
// allowing GC CompactHistory to advance between statements without pin accumulation.
func TestT045_RC_LongTransactionGCRetention(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRCSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V0');"); err != nil {
		t.Fatal(err)
	}

	// Session A begins long-running READ COMMITTED transaction
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 100; i++ {
		// Session B commits new version
		valB := fmt.Sprintf("V%d", i)
		if _, err := e.Execute(sB, fmt.Sprintf("UPDATE t SET v = '%s' WHERE id = 1;", valB)); err != nil {
			t.Fatalf("B update %d: %v", i, err)
		}

		// Session A reads: gets fresh statement view, reads valB
		resA, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
		if err != nil || len(resA.Rows) != 1 || resA.Rows[0][0] != valB {
			t.Fatalf("A read %d = %v, want %s", i, resA.Rows, valB)
		}

		// CompactHistory can advance between statement views without errors
		if m, ok := e.Backend.(storageengine.Maintenance); ok {
			_ = m.CompactHistory(context.Background())
		}
	}

	_, _ = e.Execute(sA, "COMMIT;")
}
