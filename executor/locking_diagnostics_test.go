package executor

import (
	"errors"
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
	if len(resLocks.Columns) != 9 {
		t.Fatalf("expected 9 columns for SHOW ENGINE LOCKS, got %d", len(resLocks.Columns))
	}

	resWaits := f.must(adminSession, "SHOW ENGINE LOCK WAITS")
	if len(resWaits.Columns) != 9 {
		t.Fatalf("expected 9 columns for SHOW ENGINE LOCK WAITS, got %d", len(resWaits.Columns))
	}

	resDeadlocks := f.must(adminSession, "SHOW ENGINE DEADLOCKS")
	if len(resDeadlocks.Columns) != 7 {
		t.Fatalf("expected 7 columns for SHOW ENGINE DEADLOCKS, got %d", len(resDeadlocks.Columns))
	}

	resStatus := f.must(adminSession, "SHOW ENGINE INNODB STATUS")
	if len(resStatus.Columns) != 3 {
		t.Fatalf("expected 3 columns for SHOW ENGINE INNODB STATUS, got %d", len(resStatus.Columns))
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

type mockUnsupportedBackend struct {
	storageengine.Engine
}
