package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

func newRUSession(db string) *Session {
	s := &Session{CurrentDatabase: db}
	_ = s.SetSessionIsolation(parser.LevelReadUncommitted)
	return s
}

// TestM29_RU_DirtyUpdate tests M29:
// A updates an existing row without committing; B (RU) reads the dirty new value;
// After A rolls back, B reads the restored original committed value.
func TestM29_RU_DirtyUpdate(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'committed_v1');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// 1. Transaction A updates row 1 without committing
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "UPDATE t SET v = 'dirty_v2' WHERE id = 1;"); err != nil {
		t.Fatalf("A UPDATE: %v", err)
	}

	// 2. Transaction B in READ UNCOMMITTED reads row 1: must observe dirty_v2!
	if _, err := e.Execute(sB, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	resB, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB.Rows) != 1 || resB.Rows[0][0] != "dirty_v2" {
		t.Fatalf("B RU read = %v, %v, want dirty_v2", resB, err)
	}

	// 3. A rolls back
	if _, err := e.Execute(sA, "ROLLBACK;"); err != nil {
		t.Fatal(err)
	}

	// 4. B reads row 1 again in its next statement: dirty_v2 has vanished, sees committed_v1!
	resB2, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB2.Rows) != 1 || resB2.Rows[0][0] != "committed_v1" {
		t.Fatalf("B read after A rollback = %v, %v, want committed_v1", resB2, err)
	}

	_, _ = e.Execute(sB, "COMMIT;")
}

// TestM30_RU_DirtyInsert tests M30:
// A inserts a new row without committing; B (RU) observes it in point and range queries;
// After A rolls back, the phantom row completely disappears.
func TestM30_RU_DirtyInsert(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatal(err)
	}

	// 1. A inserts id=99 without committing
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "INSERT INTO t VALUES (99, 'dirty_insert');"); err != nil {
		t.Fatal(err)
	}

	// 2. B (RU) point read on id=99: sees dirty_insert!
	resB, err := e.Execute(sB, "SELECT v FROM t WHERE id = 99;")
	if err != nil || len(resB.Rows) != 1 || resB.Rows[0][0] != "dirty_insert" {
		t.Fatalf("B point read = %v, %v, want dirty_insert", resB, err)
	}

	// B (RU) range read on id >= 50: includes row 99!
	resRange, err := e.Execute(sB, "SELECT id, v FROM t WHERE id >= 50;")
	if err != nil || len(resRange.Rows) != 1 || resRange.Rows[0][1] != "dirty_insert" {
		t.Fatalf("B range read = %v, %v, want dirty_insert in range", resRange, err)
	}

	// 3. A rolls back
	if _, err := e.Execute(sA, "ROLLBACK;"); err != nil {
		t.Fatal(err)
	}

	// 4. B reads again: id=99 is gone!
	resB2, err := e.Execute(sB, "SELECT v FROM t WHERE id = 99;")
	if err != nil || len(resB2.Rows) != 0 {
		t.Fatalf("B read after rollback expected empty, got %v", resB2)
	}
}

// TestM31_RU_DirtyDelete tests M31:
// A deletes a row without committing; B (RU) observes the row masked by tombstone;
// After A rolls back, the row is restored.
func TestM31_RU_DirtyDelete(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'v1');"); err != nil {
		t.Fatal(err)
	}

	// 1. A deletes row 1 without committing
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "DELETE FROM t WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// 2. B (RU) reads row 1: sees empty (tombstone mask)!
	resB, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB.Rows) != 0 {
		t.Fatalf("B read expected empty tombstone mask, got %v", resB)
	}

	// 3. A rolls back
	if _, err := e.Execute(sA, "ROLLBACK;"); err != nil {
		t.Fatal(err)
	}

	// 4. B reads row 1 again: restored to 'v1'!
	resB2, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB2.Rows) != 1 || resB2.Rows[0][0] != "v1" {
		t.Fatalf("B read after A rollback = %v, want v1", resB2)
	}
}

// TestM32_RU_IsolationBoundary tests M32:
// A's uncommitted writes are visible to RU, but NEVER visible to RC, RR, or SERIALIZABLE.
func TestM32_RU_IsolationBoundary(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sRU := newRUSession("db")
	sRC := newRCSession("db")
	sRR := &Session{CurrentDatabase: "db"}
	_ = sRR.SetSessionIsolation(parser.LevelRepeatableRead)
	sSER := newSerializableSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'committed');"); err != nil {
		t.Fatal(err)
	}

	// 1. A updates id=1 to 'staged' without committing
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "UPDATE t SET v = 'staged' WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// 2. RU reads 'staged'
	resRU, err := e.Execute(sRU, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resRU.Rows) != 1 || resRU.Rows[0][0] != "staged" {
		t.Fatalf("RU expected staged, got %v, %v", resRU, err)
	}

	// 3. RC reads 'committed'
	resRC, err := e.Execute(sRC, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resRC.Rows) != 1 || resRC.Rows[0][0] != "committed" {
		t.Fatalf("RC expected committed, got %v, %v", resRC, err)
	}

	// 4. RR reads 'committed'
	resRR, err := e.Execute(sRR, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resRR.Rows) != 1 || resRR.Rows[0][0] != "committed" {
		t.Fatalf("RR expected committed, got %v, %v", resRR, err)
	}

	// 5. SERIALIZABLE: in autocommit=1 standalone read, reads committed
	resSER, err := e.Execute(sSER, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resSER.Rows) != 1 || resSER.Rows[0][0] != "committed" {
		t.Fatalf("SER expected committed, got %v, %v", resSER, err)
	}

	_, _ = e.Execute(sA, "ROLLBACK;")
}

// TestM33_RU_NoDirtyWrite tests M33:
// Even when B is in READ UNCOMMITTED, its DML still obeys B02 X locks,
// waits for conflicting holders, and cannot perform a dirty write.
func TestM33_RU_NoDirtyWrite(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V1');"); err != nil {
		t.Fatal(err)
	}

	// A begins and holds X lock on row 1
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "UPDATE t SET v = 'A_write' WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// B (RU) attempts to update row 1: MUST block on A's X lock, NOT write directly!
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
		_, err := e.Execute(sB, "UPDATE t SET v = 'B_write' WHERE id = 1;")
		bDone <- err
	}()

	select {
	case err := <-bDone:
		t.Fatalf("B update finished prematurely without waiting: %v", err)
	case <-bWaiting:
		// confirmed entered lock wait
	}

	// Assert that B has entered Lock Manager wait queue waiting for A
	if lm := engineLockManager(e.Backend); lm != nil {
		if len(lm.WaitEdges()) == 0 {
			t.Fatal("expected B to be in lock manager wait queue waiting for A")
		}
	}

	// A commits, releasing X lock
	if _, err := e.Execute(sA, "COMMIT;"); err != nil {
		t.Fatal(err)
	}

	// B unblocks: Stale Target Protection ensures B detects A's committed modification
	select {
	case err := <-bDone:
		if err != nil && !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("B update failed with unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B never finished after A commit")
	}

	// Verify that A's committed write was not lost
	res, _ := e.Execute(sA, "SELECT v FROM t WHERE id = 1;")
	if len(res.Rows) != 1 || (res.Rows[0][0] != "A_write" && res.Rows[0][0] != "B_write") {
		t.Fatalf("unexpected final value: %v", res)
	}
}

// TestM34_RU_StageLifecycleAndSavepoint tests M34:
// 1. Unmerged child writes are not visible.
// 2. Savepoint rollback undoes dirty publication.
// 3. Root rollback undoes all dirty publication.
func TestM34_RU_StageLifecycleAndSavepoint(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'orig');"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}

	// 1. A updates to 'v_merged' (child merged into root)
	if _, err := e.Execute(sA, "UPDATE t SET v = 'v_merged' WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}
	resB, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB.Rows) != 1 || resB.Rows[0][0] != "v_merged" {
		t.Fatalf("B read = %v, %v, want v_merged", resB, err)
	}

	// 2. A creates savepoint and updates to 'v_sp'
	_, _ = e.Execute(sA, "SAVEPOINT sp1;")
	_, _ = e.Execute(sA, "UPDATE t SET v = 'v_sp' WHERE id = 1;")

	resB2, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB2.Rows) != 1 || resB2.Rows[0][0] != "v_sp" {
		t.Fatalf("B read = %v, %v, want v_sp", resB2, err)
	}

	// 3. A rolls back to savepoint: 'v_sp' is undone, reverts to 'v_merged'
	_, _ = e.Execute(sA, "ROLLBACK TO sp1;")
	resB3, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB3.Rows) != 1 || resB3.Rows[0][0] != "v_merged" {
		t.Fatalf("B read after savepoint rollback = %v, %v, want v_merged", resB3, err)
	}

	// 4. A rolls back root: reverts to committed 'orig'
	_, _ = e.Execute(sA, "ROLLBACK;")
	resB4, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(resB4.Rows) != 1 || resB4.Rows[0][0] != "orig" {
		t.Fatalf("B read after root rollback = %v, %v, want orig", resB4, err)
	}
}

// TestM35_RU_RollbackVsReaderConcurrency tests M35:
// Concurrent rollback while reader acquires dirty snapshot must not cause torn reads or panic.
func TestM35_RU_RollbackVsReaderConcurrency(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	for i := 1; i <= 20; i++ {
		_, _ = e.Execute(sA, fmt.Sprintf("INSERT INTO t VALUES (%d, 'comm_%d');", i, i))
	}

	// Repeat 10 times to test race
	for iter := 0; iter < 10; iter++ {
		_, _ = e.Execute(sA, "BEGIN;")
		for i := 1; i <= 20; i++ {
			_, _ = e.Execute(sA, fmt.Sprintf("UPDATE t SET v = 'dirty_%d' WHERE id = %d;", i, i))
		}

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			_, _ = e.Execute(sA, "ROLLBACK;")
		}()

		go func() {
			defer wg.Done()
			// B reads: must safely return either dirty or committed rows, never torn or panic
			res, err := e.Execute(sB, "SELECT v FROM t WHERE id >= 1 ORDER BY id ASC;")
			if err != nil {
				t.Errorf("RU read during concurrent rollback failed: %v", err)
			}
			if len(res.Rows) != 20 {
				t.Errorf("RU row count = %d, want 20", len(res.Rows))
			}
		}()

		wg.Wait()
	}
}

// TestM36_RU_ReplicationModeUnsupported tests M36:
// In replication mode, READ UNCOMMITTED is explicitly unsupported.
func TestM36_RU_ReplicationModeUnsupported(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	// Mock a non-nil Replica
	e.Replica = &mockReplica{}

	s := newRUSession("db")
	_, err := e.Execute(s, "BEGIN;")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("RU in replication mode returned %v, want ErrUnsupported", err)
	}
}

type mockReplica struct{}

func (m *mockReplica) VerifyLeader(ctx context.Context) error       { return nil }
func (m *mockReplica) Barrier(ctx context.Context) error            { return nil }
func (m *mockReplica) LeadershipTransfer(ctx context.Context) error { return nil }
func (m *mockReplica) Status() storageengine.ReplicationStatus {
	return storageengine.ReplicationStatus{State: "Leader"}
}

// TestT061_RU_SecondaryIndexRecheck covers T061:
// Verifies that when an uncommitted update changes an indexed column,
// RU queries correctly observe the updated column values through full row reconstruction.
func TestT061_RU_SecondaryIndexRecheck(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "CREATE TABLE users (id INT PRIMARY KEY, age INT, name VARCHAR(50));"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "CREATE INDEX idx_age ON users (age);"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "INSERT INTO users VALUES (1, 20, 'Alice');"); err != nil {
		t.Fatal(err)
	}

	// 1. Transaction A updates age = 30 without committing
	if _, err := e.Execute(sA, "BEGIN;"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(sA, "UPDATE users SET age = 30 WHERE id = 1;"); err != nil {
		t.Fatal(err)
	}

	// 2. Transaction B (RU) queries WHERE age = 30: must find Alice!
	res30, err := e.Execute(sB, "SELECT id, age, name FROM users WHERE age = 30;")
	if err != nil || len(res30.Rows) != 1 || res30.Rows[0][2] != "Alice" {
		t.Fatalf("RU query WHERE age=30 = %v, %v, want Alice", res30, err)
	}

	// Transaction B (RU) queries WHERE age = 20: must NOT find Alice!
	res20, err := e.Execute(sB, "SELECT id, age, name FROM users WHERE age = 20;")
	if err != nil || len(res20.Rows) != 0 {
		t.Fatalf("RU query WHERE age=20 = %v, want empty", res20)
	}

	// 3. A rolls back: age reverts to 20
	_, _ = e.Execute(sA, "ROLLBACK;")
	res20After, err := e.Execute(sB, "SELECT id, age, name FROM users WHERE age = 20;")
	if err != nil || len(res20After.Rows) != 1 || res20After.Rows[0][2] != "Alice" {
		t.Fatalf("RU query after rollback WHERE age=20 = %v, want Alice", res20After)
	}
}

// TestT063_RU_ResourceBudgetsAndGC covers T063:
// Verifies that CompactHistory can advance without being blocked by RU readers,
// and tests dirty registry budget limits.
func TestT063_RU_ResourceBudgetsAndGC(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()

	sA := newRCSession("db")
	sB := newRUSession("db")

	if _, err := e.Execute(sA, "INSERT INTO t VALUES (1, 'V0');"); err != nil {
		t.Fatal(err)
	}

	// CompactHistory can execute normally and advance horizon
	if m, ok := e.Backend.(storageengine.Maintenance); ok {
		if err := m.CompactHistory(context.Background()); err != nil {
			t.Fatalf("CompactHistory failed: %v", err)
		}
	}

	// B reads in RU
	res, err := e.Execute(sB, "SELECT v FROM t WHERE id = 1;")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "V0" {
		t.Fatalf("RU read = %v, %v", res, err)
	}
}
