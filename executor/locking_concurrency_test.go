package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_concurrency_test.go is the T064 suite: 100-run deterministic real SQL concurrency acceptance.

func TestT064SelectForUpdateAndConcurrentUpdate100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(1, 10)")
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sA, "SELECT * FROM items WHERE id=1 FOR UPDATE")

		barrier := newDMLWaitBarrier()
		barrier.arm(t)
		doneB := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(sB, "UPDATE items SET v = 99 WHERE id = 1")
			doneB <- err
		}()
		barrier.wait(t, "B waiting on A's FOR UPDATE")

		// Release A
		f.must(sA, "COMMIT")

		select {
		case err := <-doneB:
			if err != nil && !isStaleTargetConflict(err) && !errors.Is(err, storageengine.ErrConflict) {
				t.Fatalf("run %d: unexpected error for B: %v", run, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: B did not wake up after A commit", run)
		}

		_, _ = f.engine.Execute(sB, "ROLLBACK")
		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
	}
}

func TestT064ForShareAndConcurrentWrite100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(1, 10)")
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)
		sC := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sA, "SELECT * FROM items WHERE id=1 FOR SHARE")

		f.must(sB, "BEGIN")
		f.must(sB, "SELECT * FROM items WHERE id=1 FOR SHARE")

		barrier := newDMLWaitBarrier()
		barrier.arm(t)
		doneC := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(sC, "UPDATE items SET v = 20 WHERE id = 1")
			doneC <- err
		}()
		barrier.wait(t, "C waiting on shared reads")

		f.must(sA, "COMMIT")
		f.must(sB, "COMMIT")

		select {
		case err := <-doneC:
			if err != nil && !isStaleTargetConflict(err) && !errors.Is(err, storageengine.ErrConflict) {
				t.Fatalf("run %d: unexpected C outcome: %v", run, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: C did not wake up", run)
		}

		_, _ = f.engine.Execute(sC, "ROLLBACK")
		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
		f.engine.CloseSession(sC)
	}
}

func TestT064PlainSelectSnapshotIsolation100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(1, 10)")
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		r1 := f.must(sA, "SELECT v FROM items WHERE id=1")
		if got := t035Rows(t, r1); got != "10;" {
			t.Fatalf("run %d: initial read = %q, want 10;", run, got)
		}

		f.must(sB, "UPDATE items SET v = 99 WHERE id = 1")

		// SI check: A must still see 10
		r2 := f.must(sA, "SELECT v FROM items WHERE id=1")
		if got := t035Rows(t, r2); got != "10;" {
			t.Fatalf("run %d: SI violated: read %q after B commit, want 10;", run, got)
		}

		f.must(sA, "COMMIT")
		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
	}
}

func TestT064MissingKeyGapAndInsert100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(10, 10)")
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sA, "SELECT * FROM items WHERE id BETWEEN 15 AND 25 FOR UPDATE")

		barrier := newDMLWaitBarrier()
		barrier.arm(t)
		doneB := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(sB, "INSERT INTO items VALUES(20, 200)")
			doneB <- err
		}()
		barrier.wait(t, "B insert waiting on Gap")

		f.must(sA, "ROLLBACK")

		select {
		case err := <-doneB:
			if err != nil {
				t.Fatalf("run %d: B insert failed: %v", run, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: B did not unblock after Gap release", run)
		}

		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
	}
}

func TestT064UpdatePrimaryKeyMigration100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(1, 10)")
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sA, "UPDATE items SET id = 3 WHERE id = 1")

		barrier := newDMLWaitBarrier()
		barrier.arm(t)
		doneB := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(sB, "INSERT INTO items VALUES(3, 30)")
			doneB <- err
		}()
		barrier.wait(t, "B insert waiting on migrated key 3")

		f.must(sA, "COMMIT")

		select {
		case err := <-doneB:
			// B should fail with duplicate key
			if err == nil {
				t.Fatalf("run %d: expected duplicate key error for B, got nil", run)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: B did not wake up after A commit", run)
		}

		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
	}
}

func TestT064UniqueIndexContention100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, UNIQUE KEY uq_v(v))")
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sA, "INSERT INTO items VALUES(1, 100)")

		barrier := newDMLWaitBarrier()
		barrier.arm(t)
		doneB := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(sB, "INSERT INTO items VALUES(2, 100)")
			doneB <- err
		}()
		barrier.wait(t, "B waiting on unique reservation")

		f.must(sA, "COMMIT")

		select {
		case err := <-doneB:
			if err == nil {
				t.Fatalf("run %d: B expected duplicate key error, got nil", run)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: B did not unblock after unique reservation commit", run)
		}

		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
	}
}

func TestT064InsertVariantsIgnoreReplaceODKU100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, UNIQUE KEY uq_v(v))",
			"INSERT INTO items VALUES(1, 10)")
		s := t034Session(t, f, 50)

		// ODKU rewrites row under lock
		f.must(s, "INSERT INTO items VALUES(2, 10) ON DUPLICATE KEY UPDATE v = 20")
		r := f.must(s, "SELECT v FROM items WHERE id=1")
		if got := t035Rows(t, r); got != "20;" {
			t.Fatalf("run %d: ODKU expected 20, got %q", run, got)
		}

		// REPLACE replaces
		f.must(s, "REPLACE INTO items VALUES(1, 30)")
		r = f.must(s, "SELECT v FROM items WHERE id=1")
		if got := t035Rows(t, r); got != "30;" {
			t.Fatalf("run %d: REPLACE expected 30, got %q", run, got)
		}

		// IGNORE skips
		f.must(s, "INSERT IGNORE INTO items VALUES(1, 40)")
		r = f.must(s, "SELECT v FROM items WHERE id=1")
		if got := t035Rows(t, r); got != "30;" {
			t.Fatalf("run %d: IGNORE expected 30, got %q", run, got)
		}

		f.engine.CloseSession(s)
	}
}

func TestT064OnUpdateCascadeAndGap100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t,
			"CREATE TABLE parent(id BIGINT PRIMARY KEY)",
			"CREATE TABLE child(id BIGINT PRIMARY KEY, FOREIGN KEY(id) REFERENCES parent(id) ON UPDATE CASCADE)",
			"INSERT INTO parent VALUES(10)",
			"INSERT INTO child VALUES(10)",
		)
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sA, "SELECT * FROM child WHERE id BETWEEN 15 AND 25 FOR UPDATE")

		barrier := newDMLWaitBarrier()
		barrier.arm(t)
		doneB := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(sB, "UPDATE parent SET id = 20 WHERE id = 10")
			doneB <- err
		}()
		barrier.wait(t, "B cascade waiting on child Gap")

		f.must(sA, "ROLLBACK")

		select {
		case err := <-doneB:
			if err != nil {
				t.Fatalf("run %d: B cascade update failed: %v", run, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: B cascade did not unblock after child Gap release", run)
		}

		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
	}
}

func TestT064RawTxnAndSQLSharedResource100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(1, 10)")
		sA := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")

		table := t026LoadTable(t, f.engine, sA, "items")
		rowKey, _ := primaryKeyBytes(table, t026Row(t, table, int64(1), int64(10)))

		// Raw write on the same key must wait for SQL lock
		rawTx, err := f.engine.Backend.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		lm := engineLockManager(f.engine.Backend)
		doneRaw := make(chan error, 1)
		go func() {
			doneRaw <- rawTx.Put(fmt.Sprintf("row/%s", table.ID), rowKey, []byte("raw_val"))
		}()

		waitDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(waitDeadline) {
			if len(lm.WaitEdges()) > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if len(lm.WaitEdges()) == 0 {
			t.Fatalf("run %d: raw write did not establish wait edge on held SQL lock", run)
		}

		f.must(sA, "COMMIT")

		select {
		case err := <-doneRaw:
			if err != nil {
				t.Fatalf("run %d: raw put failed after SQL commit: %v", run, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: raw write did not wake up after SQL commit", run)
		}

		_ = rawTx.Rollback()
		f.engine.CloseSession(sA)
	}
}

func TestT064DeadlockTimeoutCancel100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(1, 10),(2, 20)")
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sB, "BEGIN")

		f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")
		f.must(sB, "UPDATE items SET v = 22 WHERE id = 2")

		barrier := newDMLWaitBarrier()
		barrier.arm(t)
		doneA := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(sA, "UPDATE items SET v = 222 WHERE id = 2")
			doneA <- err
		}()
		barrier.wait(t, "A waiting on row 2")

		// B updates row 1 -> triggers cycle -> deadlock victim selected
		_, errB := f.engine.Execute(sB, "UPDATE items SET v = 111 WHERE id = 1")
		if errB == nil || (!errors.Is(errB, storageengine.ErrDeadlock) && !errors.Is(errB, storageengine.ErrConflict)) {
			t.Fatalf("run %d: expected deadlock error on B, got %v", run, errB)
		}

		_, _ = f.engine.Execute(sB, "ROLLBACK")

		select {
		case err := <-doneA:
			if err != nil {
				t.Fatalf("run %d: A failed after deadlock victim B rollback: %v", run, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: A did not wake up after victim B rollback", run)
		}

		_, _ = f.engine.Execute(sA, "ROLLBACK")
		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
	}
}

func TestT064SavepointCommitRollbackDisconnect100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(1, 10),(2, 20)")
		sA := t034Session(t, f, 50)

		f.must(sA, "BEGIN")
		f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")
		f.must(sA, "SAVEPOINT sp1")
		f.must(sA, "UPDATE items SET v = 22 WHERE id = 2")
		f.must(sA, "ROLLBACK TO SAVEPOINT sp1")

		// Row 1 update preserved, row 2 rolled back
		r := f.must(sA, "SELECT v FROM items WHERE id=1")
		if got := t035Rows(t, r); got != "11;" {
			t.Fatalf("run %d: row 1 expected 11, got %q", run, got)
		}
		r = f.must(sA, "SELECT v FROM items WHERE id=2")
		if got := t035Rows(t, r); got != "20;" {
			t.Fatalf("run %d: row 2 expected 20, got %q", run, got)
		}

		// Disconnect releases all locks
		f.engine.CloseSession(sA)

		lm := engineLockManager(f.engine.Backend)
		if lm.LockTableSize() != 0 || lm.Stats().ActiveOwners != 0 {
			t.Fatalf("run %d: residue after disconnect: size=%d", run, lm.LockTableSize())
		}
	}
}

func TestT064StaleDMLTargetRootRetry100Runs(t *testing.T) {
	for run := 0; run < 100; run++ {
		f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
			"INSERT INTO items VALUES(1, 10),(2, 20)")
		sA := t034Session(t, f, 50)
		sB := t034Session(t, f, 50)

		f.must(sB, "BEGIN")
		f.must(sB, "UPDATE items SET v = 200 WHERE id = 2")

		f.must(sA, "BEGIN")
		f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")

		barrier := newDMLWaitBarrier()
		barrier.arm(t)
		doneB := make(chan error, 1)
		go func() {
			_, err := f.engine.Execute(sB, "UPDATE items SET v = 111 WHERE id = 1")
			doneB <- err
		}()
		barrier.wait(t, "B waiting on A's id=1")

		f.must(sA, "COMMIT")

		select {
		case err := <-doneB:
			if !isStaleTargetConflict(err) && !errors.Is(err, storageengine.ErrConflict) {
				t.Fatalf("run %d: expected conflict error for B, got: %v", run, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: B did not wake up after A commit", run)
		}

		// Subsequent COMMIT must be refused!
		_, errCommit := f.engine.Execute(sB, "COMMIT")
		if !isStaleTargetConflict(errCommit) && !errors.Is(errCommit, storageengine.ErrConflict) {
			t.Fatalf("run %d: expected COMMIT refusal, got: %v", run, errCommit)
		}

		_, _ = f.engine.Execute(sB, "ROLLBACK")
		f.engine.CloseSession(sA)
		f.engine.CloseSession(sB)
	}
}
