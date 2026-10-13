package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gbaselite/storageengine"
)

// locking_diagnostics_test.go is the T062 test suite verifying SHOW ENGINE diagnostics via SQL.

func TestT062ShowEngineDiagnosticsAdminAndUserPermissions(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	adminSession := t034Session(t, f, 50)
	adminSession.Username = "root"
	adminSession.Host = "%"

	// 1. Admin can query SHOW ENGINE LOCKS, LOCK WAITS, DEADLOCKS, STATUS
	resLocks := f.must(adminSession, "SHOW ENGINE LOCKS")
	if len(resLocks.Columns) != 12 {
		t.Fatalf("expected 12 columns for SHOW ENGINE LOCKS, got %d", len(resLocks.Columns))
	}

	resWaits := f.must(adminSession, "SHOW ENGINE LOCK WAITS")
	if len(resWaits.Columns) != 12 {
		t.Fatalf("expected 12 columns for SHOW ENGINE LOCK WAITS, got %d", len(resWaits.Columns))
	}

	resDeadlocks := f.must(adminSession, "SHOW ENGINE DEADLOCKS")
	if len(resDeadlocks.Columns) != 8 {
		t.Fatalf("expected 8 columns for SHOW ENGINE DEADLOCKS, got %d", len(resDeadlocks.Columns))
	}

	resStatus := f.must(adminSession, "SHOW ENGINE INNODB STATUS")
	if len(resStatus.Columns) != 3 {
		t.Fatalf("expected 3 columns for SHOW ENGINE INNODB STATUS, got %d", len(resStatus.Columns))
	}

	resLockStatus := f.must(adminSession, "SHOW ENGINE LOCKS STATUS")
	if len(resLockStatus.Columns) != 7 {
		t.Fatalf("expected 7 columns for SHOW ENGINE LOCKS STATUS, got %d", len(resLockStatus.Columns))
	}

	// 2. Non-admin user is rejected
	f.must(adminSession, "CREATE USER 'alice'@'%' IDENTIFIED BY 'password'")
	userSession := t034Session(t, f, 50)
	userSession.Username = "alice"
	userSession.Host = "%"

	for _, cmd := range []string{
		"SHOW ENGINE LOCKS",
		"SHOW ENGINE LOCK WAITS",
		"SHOW ENGINE DEADLOCKS",
		"SHOW ENGINE INNODB STATUS",
	} {
		_, err := f.engine.Execute(userSession, cmd)
		if err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Fatalf("expected access denied for command %q, got: %v", cmd, err)
		}
	}
}

func TestT062ShowEngineDiagnosticsContentAndAnonymousFields(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1, 10)")
	sA := t034Session(t, f, 50)
	sB := t034Session(t, f, 50)

	// Session A holds lock on row 1
	f.must(sA, "BEGIN")
	f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")

	// Session B waits on row 1
	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	doneB := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(sB, "UPDATE items SET v = 12 WHERE id = 1")
		doneB <- err
	}()
	barrier.wait(t, "Session B waiting on row 1")

	// Query SHOW ENGINE LOCKS
	resLocks := f.must(sA, "SHOW ENGINE LOCKS")
	if len(resLocks.Rows) == 0 {
		t.Fatal("expected granted locks in SHOW ENGINE LOCKS, got none")
	}

	// Verify privacy: none of the returned rows should contain raw table names, keys, or spaces
	for _, row := range resLocks.Rows {
		for _, col := range row {
			colStr := col.(string)
			if strings.Contains(colStr, "items") || strings.Contains(colStr, "secret") || strings.Contains(colStr, "row/") {
				t.Fatalf("leaked raw resource name in SHOW ENGINE LOCKS: %q", colStr)
			}
		}
	}

	// Query SHOW ENGINE LOCK WAITS
	resWaits := f.must(sA, "SHOW ENGINE LOCK WAITS")
	if len(resWaits.Rows) == 0 {
		t.Fatal("expected waiting request in SHOW ENGINE LOCK WAITS, got none")
	}

	// Session A commits
	f.must(sA, "COMMIT")

	select {
	case err := <-doneB:
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("expected ErrConflict for Session B after row version change, got: %v", err)
		}
	}
	_, _ = f.engine.Execute(sB, "ROLLBACK")

	// After commit, LOCKS and LOCK WAITS should return 0 rows
	resLocksAfter := f.must(sA, "SHOW ENGINE LOCKS")
	if len(resLocksAfter.Rows) != 0 {
		t.Fatalf("expected 0 locks after commit, got %d", len(resLocksAfter.Rows))
	}
}

func TestT062ShowEngineDiagnosticsUnknownAndUnsupportedEngine(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	adminSession := t034Session(t, f, 50)

	// Unknown engine
	_, err := f.engine.Execute(adminSession, "SHOW ENGINE UNKNOWN_FOO LOCKS")
	if err == nil || !strings.Contains(err.Error(), "unknown engine") {
		t.Fatalf("expected unknown engine error, got: %v", err)
	}

	// Supported engine qualifiers
	for _, eng := range []string{"INNODB", "GB_LOCKS", "MVCC"} {
		res := f.must(adminSession, "SHOW ENGINE "+eng+" LOCKS")
		if res == nil {
			t.Fatalf("SHOW ENGINE %s LOCKS returned nil", eng)
		}
	}

	// Backend without LockDiagnostics capability returns ErrUnsupported
	fMock := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY)")
	fMock.engine.Backend = &mockUnsupportedBackend{fMock.engine.Backend}
	_, err = fMock.engine.Execute(adminSession, "SHOW ENGINE LOCKS")
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported on backend without LockDiagnostics, got: %v", err)
	}
}

func TestT062ShowEngineStatusTotalAccuracyUnderHighLoad(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	adminSession := t034Session(t, f, 50)
	adminSession.Username = "root"
	adminSession.Host = "%"

	// 1. Empty state: Total is 0, Truncated is false
	resEmpty := f.must(adminSession, "SHOW ENGINE LOCKS STATUS")
	if len(resEmpty.Rows) != 1 {
		t.Fatalf("expected 1 status row for empty locks, got %d", len(resEmpty.Rows))
	}
	// Columns: Engine, View, SnapshotRevision, Total, Truncated, NextCursor, Limit
	if resEmpty.Rows[0][3].(string) != "0" {
		t.Fatalf("expected Total 0 for empty lock table, got %v", resEmpty.Rows[0][3])
	}
	if resEmpty.Rows[0][4].(string) != "false" {
		t.Fatalf("expected Truncated false for empty lock table, got %v", resEmpty.Rows[0][4])
	}

	// 2. Insert 300 rows and hold locks on them in a transaction (> 256)
	f.must(adminSession, "BEGIN")
	for i := 1; i <= 300; i++ {
		f.must(adminSession, fmt.Sprintf("INSERT INTO items VALUES(%d, %d)", i, i*10))
	}

	// 3. Query SHOW ENGINE LOCKS: ReturnedRows should be clamped to 256
	resLocks := f.must(adminSession, "SHOW ENGINE LOCKS")
	if len(resLocks.Rows) != 256 {
		t.Fatalf("expected 256 returned rows for SHOW ENGINE LOCKS, got %d", len(resLocks.Rows))
	}

	// 4. Query SHOW ENGINE LOCKS STATUS: Total must report >= 300 and Truncated == true
	resStatus := f.must(adminSession, "SHOW ENGINE LOCKS STATUS")
	if len(resStatus.Rows) != 1 {
		t.Fatalf("expected 1 row for SHOW ENGINE LOCKS STATUS, got %d", len(resStatus.Rows))
	}
	totalStr := resStatus.Rows[0][3].(string)
	truncStr := resStatus.Rows[0][4].(string)
	if truncStr != "true" {
		t.Fatalf("expected Truncated true when > 256 locks exist, got %s", truncStr)
	}
	totalNum := 0
	fmt.Sscanf(totalStr, "%d", &totalNum)
	if totalNum < 300 {
		t.Fatalf("expected Total >= 300, got %d", totalNum)
	}

	// 5. Query with LIMIT 1: Total remains accurate, ReturnedRows is 1
	resLimit1 := f.must(adminSession, "SHOW ENGINE LOCKS LIMIT 1")
	if len(resLimit1.Rows) != 1 {
		t.Fatalf("expected 1 row for LIMIT 1, got %d", len(resLimit1.Rows))
	}

	f.must(adminSession, "ROLLBACK")
}

type mockUnsupportedBackend struct {
	storageengine.Engine
}
