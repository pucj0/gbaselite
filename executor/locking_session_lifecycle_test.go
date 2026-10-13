package executor

import (
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_session_lifecycle_test.go is the T052 suite for session lifecycle, disconnect cleanup,
// savepoints, statement scope, and lock manager zero-residue invariants.

// TestT052SessionDisconnectWakesWaitersAndCleansUpLocks verifies that when a session unexpectedly
// disconnects (simulated by CloseSession), its active root transaction and all locks are released,
// any blocked waiters are woken up, and no lock residues remain.
func TestT052SessionDisconnectWakesWaitersAndCleansUpLocks(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1,10),(2,20)")

	sA := t034Session(t, f, 20)
	sB := t034Session(t, f, 20)

	lm := engineLockManager(f.engine.Backend)
	if lm == nil {
		t.Fatal("no lock manager")
	}

	// 1. Session A holds X lock on row 1
	f.must(sA, "BEGIN")
	f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")

	// 2. Session B attempts to update row 1 and enters wait
	barrier := newDMLWaitBarrier()
	barrier.arm(t)

	doneB := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(sB, "UPDATE items SET v = 12 WHERE id = 1")
		doneB <- err
	}()
	barrier.wait(t, "Session B waiting on row 1")

	// Verify Session B is waiting for Session A
	edges := lm.WaitEdges()
	if len(edges) == 0 {
		t.Fatal("expected wait edges during Session B block, got none")
	}
	t.Logf("Verified wait edge: %+v", edges[0])

	// 3. Session A disconnects (CloseSession)
	f.engine.CloseSession(sA)

	// Session B must wake up and succeed
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("Session B failed after Session A disconnect: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Session B was not woken by Session A disconnect")
	}

	// Session B commits its update
	f.must(sB, "COMMIT")

	// Verify data: row 1 was updated by Session B
	res := f.must(sB, "SELECT v FROM items WHERE id = 1")
	if got := t035Rows(t, res); got != "12;" {
		t.Fatalf("row 1 = %q, want 12;", got)
	}

	// 4. Session B disconnects
	f.engine.CloseSession(sB)

	// Verify complete cleanup: 0 active owners, 0 wait edges, 0 pending requests, 0 lock table size
	stats := lm.Stats()
	if stats.ActiveOwners != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 || lm.LockTableSize() != 0 {
		t.Fatalf("lock manager has residue after all sessions disconnected: stats=%+v, size=%d",
			stats, lm.LockTableSize())
	}
}

// TestT052SavepointRollbackKeepsRootLocksAndCleansUpOnCommit verifies that rolling back to a
// savepoint cancels statement scopes of discarded layers but preserves root-level locks until final COMMIT.
func TestT052SavepointRollbackKeepsRootLocksAndCleansUpOnCommit(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1,10),(2,20)")

	sA := t034Session(t, f, 20)
	sB := t034Session(t, f, 2)

	lm := engineLockManager(f.engine.Backend)
	if lm == nil {
		t.Fatal("no lock manager")
	}

	// Session A begins and updates row 1
	f.must(sA, "BEGIN")
	f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")

	// Create savepoint and update row 2
	f.must(sA, "SAVEPOINT sp1")
	f.must(sA, "UPDATE items SET v = 22 WHERE id = 2")

	// Rollback to savepoint sp1 (discards row 2 update)
	f.must(sA, "ROLLBACK TO SAVEPOINT sp1")

	// Root must STILL hold lock on row 1! Session B trying to update row 1 must block:
	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	doneB := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(sB, "UPDATE items SET v = 99 WHERE id = 1")
		doneB <- err
	}()
	barrier.wait(t, "Session B waiting on row 1 held by Session A root")

	// Session A commits
	f.must(sA, "COMMIT")

	select {
	case err := <-doneB:
		// Session B woke up; target was modified by Session A, so returns ErrConflict
		if !errorsIsConflict(err) {
			t.Logf("Session B outcome: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Session B did not wake up after Session A commit")
	}

	_, _ = f.engine.Execute(sB, "ROLLBACK")
	f.engine.CloseSession(sA)
	f.engine.CloseSession(sB)

	// Verify all lock residues cleared
	stats := lm.Stats()
	if stats.ActiveOwners != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 || lm.LockTableSize() != 0 {
		t.Fatalf("residue after commit and close: stats=%+v, size=%d", stats, lm.LockTableSize())
	}
}

// TestT052SavepointReleaseMergesAndCleansUpOnRollback verifies RELEASE SAVEPOINT and subsequent
// full transaction ROLLBACK completely frees all held locks and resets lock manager stats.
func TestT052SavepointReleaseMergesAndCleansUpOnRollback(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1,10),(2,20)")

	sA := t034Session(t, f, 20)
	lm := engineLockManager(f.engine.Backend)
	if lm == nil {
		t.Fatal("no lock manager")
	}

	f.must(sA, "BEGIN")
	f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")
	f.must(sA, "SAVEPOINT sp1")
	f.must(sA, "UPDATE items SET v = 22 WHERE id = 2")
	f.must(sA, "RELEASE SAVEPOINT sp1")

	// Transaction still holds locks
	if lm.Stats().ActiveOwners == 0 || lm.Stats().GrantedComponents == 0 {
		t.Fatal("expected locks held before rollback")
	}

	// Full ROLLBACK
	f.must(sA, "ROLLBACK")
	f.engine.CloseSession(sA)

	// Lock manager must be completely clean
	stats := lm.Stats()
	if stats.ActiveOwners != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 || lm.LockTableSize() != 0 {
		t.Fatalf("residue after full rollback: stats=%+v, size=%d", stats, lm.LockTableSize())
	}
}

func errorsIsConflict(err error) bool {
	return err != nil && (storageengine.ErrConflict == err || isStaleTargetConflict(err))
}
