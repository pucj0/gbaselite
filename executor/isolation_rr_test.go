package executor

import (
	"testing"
)

func newRRSession(db string) *Session {
	s := &Session{CurrentDatabase: db}
	s.EnableIsolation()
	return s
}

// TestM08_RR_LazyFirstReadTiming verifies MySQL 8.4 RR Lazy Snapshot semantics (M08):
// In REPEATABLE READ, a transaction's consistent read view is NOT created at BEGIN.
// It is established at the FIRST consistent plain SELECT statement.
func TestM08_RR_LazyFirstReadTiming(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	// Initial: id=1, v='V1'
	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT V1: %v", err)
	}

	// Session A begins explicit transaction (RR)
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// Session B updates row 1 to V2 and commits BEFORE Session A's first SELECT
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE V2: %v", err)
	}

	// Session A performs its FIRST plain SELECT:
	// Under MySQL RR lazy view, it MUST see 'V2' (committed before first read), NOT 'V1' (snapshot at BEGIN)
	resA1, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil {
		t.Fatalf("A SELECT 1: %v", err)
	}
	if len(resA1.Rows) != 1 || resA1.Rows[0][0] != "V2" {
		t.Fatalf("A first SELECT returned %v, want 'V2' (failed MySQL RR lazy read view)", resA1.Rows)
	}

	// Session B updates row 1 to V3 and commits AFTER Session A's first SELECT
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V3' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE V3: %v", err)
	}

	// Session A performs its SECOND plain SELECT:
	// The view was fixed at first read, so it MUST STILL see 'V2', NOT 'V3'
	resA2, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil {
		t.Fatalf("A SELECT 2: %v", err)
	}
	if len(resA2.Rows) != 1 || resA2.Rows[0][0] != "V2" {
		t.Fatalf("A second SELECT returned %v, want 'V2' (repeatable read violated)", resA2.Rows)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestM09_RR_ConsistentViewReused verifies M09:
// A's first SELECT fixes view; B commits update; A's repeat SELECT sees same data.
func TestM09_RR_ConsistentViewReused(t *testing.T) {
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

	// A first read
	res1, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(res1.Rows) != 1 || res1.Rows[0][0] != "V1" {
		t.Fatalf("A read 1: %v, %v", res1, err)
	}

	// B updates and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// A repeat read: must still be V1
	res2, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(res2.Rows) != 1 || res2.Rows[0][0] != "V1" {
		t.Fatalf("A read 2 returned %v, want 'V1'", res2)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestM10_RR_PhantomFreeRange verifies M10:
// A's first empty range SELECT prevents phantoms from concurrent inserts.
func TestM10_RR_PhantomFreeRange(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// A first range query (empty)
	res1, err := e.Execute(sA, "SELECT * FROM t WHERE id >= 100 AND id <= 200;")
	if err != nil || len(res1.Rows) != 0 {
		t.Fatalf("A first query expected empty, got %v, err=%v", res1, err)
	}

	// B inserts matching row and commits
	if _, err := e.Execute(sB, "INSERT INTO t VALUES (150, 'phantom');"); err != nil {
		t.Fatalf("B INSERT: %v", err)
	}

	// A repeat range query: must still be empty (no phantom)
	res2, err := e.Execute(sA, "SELECT * FROM t WHERE id >= 100 AND id <= 200;")
	if err != nil || len(res2.Rows) != 0 {
		t.Fatalf("A repeat query saw phantom rows: %v", res2.Rows)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestM11_RR_OwnStagedWrites verifies M11:
// In RR, A's own writes (INSERT/UPDATE/DELETE) are visible despite the fixed view.
func TestM11_RR_OwnStagedWrites(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'orig');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// Fix view
	_, _ = e.Execute(sA, "SELECT * FROM t;")

	// A performs own UPDATE
	if _, err := e.Execute(sA, "UPDATE t SET v = 'own_update' WHERE id = 1;"); err != nil {
		t.Fatalf("A UPDATE: %v", err)
	}
	resUpdate, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(resUpdate.Rows) != 1 || resUpdate.Rows[0][0] != "own_update" {
		t.Fatalf("own UPDATE not visible: %v", resUpdate.Rows)
	}

	// A performs own INSERT
	if _, err := e.Execute(sA, "INSERT INTO t VALUES (2, 'own_insert');"); err != nil {
		t.Fatalf("A INSERT: %v", err)
	}
	resInsert, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 2;")
	if len(resInsert.Rows) != 1 || resInsert.Rows[0][0] != "own_insert" {
		t.Fatalf("own INSERT not visible: %v", resInsert.Rows)
	}

	// A performs own DELETE
	if _, err := e.Execute(sA, "DELETE FROM t WHERE id = 1;"); err != nil {
		t.Fatalf("A DELETE: %v", err)
	}
	resDelete, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(resDelete.Rows) != 0 {
		t.Fatalf("own DELETE still visible: %v", resDelete.Rows)
	}

	// B still sees original committed state (B cannot see A's uncommitted writes)
	resB, _ := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if len(resB.Rows) != 1 || resB.Rows[0][0] != "orig" {
		t.Fatalf("B unexpectedly saw A's uncommitted writes: %v", resB.Rows)
	}

	_, _ = e.Execute(sA, "ROLLBACK;")
}

// TestM12_RR_CurrentReadSeparation verifies M12:
// Plain SELECT uses consistent snapshot; SELECT FOR UPDATE uses current read under lock.
func TestM12_RR_CurrentReadSeparation(t *testing.T) {
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
	res1, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(res1.Rows) != 1 || res1.Rows[0][0] != "V1" {
		t.Fatalf("A read 1: %v", res1.Rows)
	}

	// B updates to V2 and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'V2' WHERE id = 1;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// A executes SELECT FOR UPDATE: MUST see current committed V2 under lock
	resLock, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1 FOR UPDATE;")
	if err != nil {
		t.Fatalf("A SELECT FOR UPDATE: %v", err)
	}
	if len(resLock.Rows) != 1 || resLock.Rows[0][0] != "V2" {
		t.Fatalf("A FOR UPDATE returned %v, want current 'V2'", resLock.Rows)
	}

	// Subsequent plain SELECT in A still adheres to consistent view (sees V1)
	resPlain, err := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if err != nil {
		t.Fatalf("A plain SELECT: %v", err)
	}
	if len(resPlain.Rows) != 1 || resPlain.Rows[0][0] != "V1" {
		t.Fatalf("A plain SELECT after FOR UPDATE returned %v, want consistent 'V1'", resPlain.Rows)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestM11_RR_SavepointRollbackOwnWrites verifies T033:
// SAVEPOINT and ROLLBACK TO inside an RR transaction correctly rolls back own staged writes
// while maintaining consistent view visibility.
func TestM11_RR_SavepointRollbackOwnWrites(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}

	// 1. Establish consistent view
	res1, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(res1.Rows) != 1 || res1.Rows[0][0] != "V1" {
		t.Fatalf("initial read: %v", res1.Rows)
	}

	// 2. Set savepoint
	if _, err := e.Execute(sA, "SAVEPOINT sp1;"); err != nil {
		t.Fatalf("SAVEPOINT: %v", err)
	}

	// 3. Mutate inside savepoint
	if _, err := e.Execute(sA, "UPDATE t SET v = 'sp1_modified' WHERE id = 1;"); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if _, err := e.Execute(sA, "INSERT INTO t VALUES (2, 'sp1_row2');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	resMut, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(resMut.Rows) != 1 || resMut.Rows[0][0] != "sp1_modified" {
		t.Fatalf("staged write not visible: %v", resMut.Rows)
	}

	// 4. Rollback to savepoint
	if _, err := e.Execute(sA, "ROLLBACK TO sp1;"); err != nil {
		t.Fatalf("ROLLBACK TO: %v", err)
	}

	// Row 1 must be restored to V1, Row 2 must disappear
	resRestored1, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(resRestored1.Rows) != 1 || resRestored1.Rows[0][0] != "V1" {
		t.Fatalf("row 1 after ROLLBACK TO = %v, want 'V1'", resRestored1.Rows)
	}

	resRestored2, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 2;")
	if len(resRestored2.Rows) != 0 {
		t.Fatalf("row 2 after ROLLBACK TO still visible: %v", resRestored2.Rows)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestM10_RR_AbsentPointLookupConsistent verifies T034:
// Looking up an absent point in RR stays absent even after concurrent insert commits.
func TestM10_RR_AbsentPointLookupConsistent(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// A looks up absent key 999
	res1, err := e.Execute(sA, "SELECT * FROM t WHERE id = 999;")
	if err != nil || len(res1.Rows) != 0 {
		t.Fatalf("expected empty result, got %v, err=%v", res1, err)
	}

	// B inserts key 999 and commits
	if _, err := e.Execute(sB, "INSERT INTO t VALUES (999, 'concurrent');"); err != nil {
		t.Fatalf("B INSERT: %v", err)
	}

	// A repeats lookup: must still be absent (phantom-free point lookup)
	res2, err := e.Execute(sA, "SELECT * FROM t WHERE id = 999;")
	if err != nil || len(res2.Rows) != 0 {
		t.Fatalf("A repeat point lookup saw concurrent insert: %v", res2.Rows)
	}

	_, _ = e.Execute(sA, "COMMIT;")
}

// TestT035_RR_CommitValidationBaselines verifies T035:
// Writing to a stale target without covering lock conflicts; reading visibility revision does not bypass write conflicts.
func TestT035_RR_CommitValidationBaselines(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRRSession("db")
	sB := newRRSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (10, 'base');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// A begins transaction (lazy)
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatalf("A BEGIN: %v", err)
	}

	// B updates row 10 and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'b_committed' WHERE id = 10;"); err != nil {
		t.Fatalf("B UPDATE: %v", err)
	}

	// A performs first read: sees 'b_committed' (lazy view)
	resA1, err := e.Execute(sA, "SELECT v FROM t WHERE id = 10;")
	if err != nil || len(resA1.Rows) != 1 || resA1.Rows[0][0] != "b_committed" {
		t.Fatalf("A first read = %v, want 'b_committed'", resA1.Rows)
	}

	// B updates row 10 again and commits
	if _, err := e.Execute(sB, "UPDATE t SET v = 'b_second' WHERE id = 10;"); err != nil {
		t.Fatalf("B UPDATE 2: %v", err)
	}

	// A repeat read still sees 'b_committed'
	resA2, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 10;")
	if len(resA2.Rows) != 1 || resA2.Rows[0][0] != "b_committed" {
		t.Fatalf("A repeat read = %v, want 'b_committed'", resA2.Rows)
	}

	// Now A attempts UPDATE row 10:
	// A's update attempts to write row 10 based on its stale view ('b_committed' vs now 'b_second').
	// A's DML uses current read under lock: it detects row 10 was replaced by newer committed version ('b_second').
	// It must conflict and reject!
	_, err = e.Execute(sA, "UPDATE t SET v = 'a_overwrite' WHERE id = 10;")
	if err == nil {
		_, err = e.Execute(sA, "COMMIT;")
	}
	if err == nil {
		t.Fatalf("A was allowed to overwrite B's concurrent commit without conflict!")
	}

	rollbackSessionTransaction(sA)

	// B verifies final value in database is 'b_second'
	resFinal, _ := e.Execute(sB, "SELECT v FROM t WHERE id = 10;")
	if len(resFinal.Rows) != 1 || resFinal.Rows[0][0] != "b_second" {
		t.Fatalf("final value = %v, want 'b_second'", resFinal.Rows)
	}
}
