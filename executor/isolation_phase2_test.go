package executor

import (
	"errors"
	"testing"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

func newIsolationTestEngine(t *testing.T) (*Engine, func()) {
	t.Helper()
	dir := t.TempDir()
	e, err := OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	s := &Session{}
	if _, err := e.Execute(s, "CREATE DATABASE db;"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := e.Execute(s, "USE db;"); err != nil {
		t.Fatalf("USE db: %v", err)
	}
	if _, err := e.Execute(s, "CREATE TABLE t (id INT PRIMARY KEY, v VARCHAR(20));"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	return e, func() { _ = e.Close() }
}

// TestB03SessionIsolationDefault tests that a fresh session starts with RR default (M01).
func TestB03SessionIsolationDefault(t *testing.T) {
	s := &Session{}
	s.InitializeSettings()
	if got := s.SessionDefaultIsolation(); got != parser.LevelRepeatableRead {
		t.Fatalf("SessionDefaultIsolation() = %v, want %v", got, parser.LevelRepeatableRead)
	}
	if _, ok := s.PendingNextIsolation(); ok {
		t.Fatalf("PendingNextIsolation() unexpectedly present")
	}
	if _, ok := s.ActiveIsolation(); ok {
		t.Fatalf("ActiveIsolation() unexpectedly present")
	}
	if got := s.EffectiveIsolation(); got != parser.LevelRepeatableRead {
		t.Fatalf("EffectiveIsolation() = %v, want %v", got, parser.LevelRepeatableRead)
	}
}

// TestB03SetSessionIsolation tests setting session default and clearing pendingNext (M01).
func TestB03SetSessionIsolation(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()
	s := &Session{CurrentDatabase: "db"}

	// Set session to READ COMMITTED
	if _, err := e.Execute(s, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;"); err != nil {
		t.Fatalf("SET SESSION: %v", err)
	}
	if s.SessionDefaultIsolation() != parser.LevelReadCommitted {
		t.Fatalf("SessionDefaultIsolation = %v, want READ COMMITTED", s.SessionDefaultIsolation())
	}

	// Set pending next
	if _, err := e.Execute(s, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ;"); err != nil {
		t.Fatalf("SET TRANSACTION: %v", err)
	}
	if pending, ok := s.PendingNextIsolation(); !ok || pending != parser.LevelRepeatableRead {
		t.Fatalf("PendingNextIsolation = (%v, %v), want (REPEATABLE READ, true)", pending, ok)
	}

	// New SET SESSION must clear pending next
	if _, err := e.Execute(s, "SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE;"); err != nil {
		t.Fatalf("SET SESSION 2: %v", err)
	}
	if s.SessionDefaultIsolation() != parser.LevelSerializable {
		t.Fatalf("SessionDefaultIsolation = %v, want SERIALIZABLE", s.SessionDefaultIsolation())
	}
	if _, ok := s.PendingNextIsolation(); ok {
		t.Fatalf("PendingNextIsolation should have been cleared by SET SESSION")
	}
}

// TestB03SetNextTransactionIsolation tests that next-transaction override affects only the next transaction (M02).
func TestB03SetNextTransactionIsolation(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()
	s := &Session{CurrentDatabase: "db"}

	// Session default is RR
	if _, err := e.Execute(s, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ;"); err != nil {
		t.Fatalf("SET TRANSACTION: %v", err)
	}
	if pending, ok := s.PendingNextIsolation(); !ok || pending != parser.LevelRepeatableRead {
		t.Fatalf("PendingNextIsolation = (%v, %v), want (REPEATABLE READ, true)", pending, ok)
	}

	// Begin consumes pendingNext
	if _, err := e.Execute(s, "BEGIN;"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if active, ok := s.ActiveIsolation(); !ok || active != parser.LevelRepeatableRead {
		t.Fatalf("ActiveIsolation = (%v, %v), want (REPEATABLE READ, true)", active, ok)
	}
	if _, ok := s.PendingNextIsolation(); ok {
		t.Fatalf("PendingNextIsolation was not consumed on BEGIN")
	}

	// Commit clears active
	if _, err := e.Execute(s, "COMMIT;"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if _, ok := s.ActiveIsolation(); ok {
		t.Fatalf("ActiveIsolation should be cleared after COMMIT")
	}
}

// TestB03IsolationActiveTransaction tests active transaction restrictions (M03).
func TestB03IsolationActiveTransaction(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()
	s := &Session{CurrentDatabase: "db"}

	if _, err := e.Execute(s, "BEGIN;"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}

	// In active transaction, SET TRANSACTION is rejected (SQLSTATE 25001)
	if _, err := e.Execute(s, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED;"); err == nil {
		t.Fatalf("SET TRANSACTION inside active transaction unexpectedly succeeded")
	}

	// In active transaction, SET SESSION is accepted, but active isolation is unchanged
	activeBefore, _ := s.ActiveIsolation()
	if _, err := e.Execute(s, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;"); err != nil {
		t.Fatalf("SET SESSION inside active transaction failed: %v", err)
	}
	if s.SessionDefaultIsolation() != parser.LevelReadCommitted {
		t.Fatalf("SessionDefaultIsolation = %v, want READ COMMITTED", s.SessionDefaultIsolation())
	}
	activeAfter, _ := s.ActiveIsolation()
	if activeAfter != activeBefore {
		t.Fatalf("ActiveIsolation changed from %v to %v during active transaction", activeBefore, activeAfter)
	}

	rollbackSessionTransaction(s)
}

// TestB03IsolationAutocommit tests autocommit consumption and autocommit=0 implicit start (M04).
func TestB03IsolationAutocommit(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()
	s := &Session{CurrentDatabase: "db"}

	// Autocommit single statement consumes pendingNext
	if _, err := e.Execute(s, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ;"); err != nil {
		t.Fatalf("SET TRANSACTION: %v", err)
	}
	if _, err := e.Execute(s, "SELECT * FROM t;"); err != nil {
		t.Fatalf("SELECT * FROM t: %v", err)
	}
	if _, ok := s.PendingNextIsolation(); ok {
		t.Fatalf("pendingNext was not consumed by single autocommit statement")
	}

	// Autocommit=0 implicit transaction
	if err := e.SetAutocommit(s, false); err != nil {
		t.Fatalf("SetAutocommit: %v", err)
	}
	if _, err := e.Execute(s, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ;"); err != nil {
		t.Fatalf("SET TRANSACTION: %v", err)
	}
	if _, err := e.Execute(s, "INSERT INTO t VALUES (1, 'a');"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if !s.InTransaction() {
		t.Fatalf("expected active implicit transaction with autocommit=0")
	}
	if active, ok := s.ActiveIsolation(); !ok || active != parser.LevelRepeatableRead {
		t.Fatalf("ActiveIsolation = (%v, %v), want (REPEATABLE READ, true)", active, ok)
	}
	if _, ok := s.PendingNextIsolation(); ok {
		t.Fatalf("pendingNext should be consumed by implicit transaction begin")
	}

	if _, err := e.Execute(s, "ROLLBACK;"); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
	if _, ok := s.ActiveIsolation(); ok {
		t.Fatalf("ActiveIsolation should be cleared after ROLLBACK")
	}
}

// TestB03IsolationResetConnection tests ResetConnection cleans up isolation state (M05).
func TestB03IsolationResetConnection(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()
	s := &Session{CurrentDatabase: "db"}

	_ = s.SetSessionIsolation(parser.LevelSerializable)
	_ = s.SetNextTransactionIsolation(parser.LevelReadUncommitted)

	e.ResetConnection(s)

	if s.SessionDefaultIsolation() != parser.LevelRepeatableRead {
		t.Fatalf("ResetConnection did not restore sessionDefault to RR, got %v", s.SessionDefaultIsolation())
	}
	if _, ok := s.PendingNextIsolation(); ok {
		t.Fatalf("ResetConnection did not clear pendingNext")
	}
	if _, ok := s.ActiveIsolation(); ok {
		t.Fatalf("ResetConnection did not clear activeIsolation")
	}
}

// TestB03IsolationCrossSession tests that sessions do not leak mutable isolation state (M01).
func TestB03IsolationCrossSession(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()
	s1 := &Session{CurrentDatabase: "db"}
	s2 := &Session{CurrentDatabase: "db"}

	if _, err := e.Execute(s1, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;"); err != nil {
		t.Fatalf("SET SESSION on s1: %v", err)
	}

	if s1.SessionDefaultIsolation() != parser.LevelReadCommitted {
		t.Fatalf("s1 default = %v, want READ COMMITTED", s1.SessionDefaultIsolation())
	}
	if s2.SessionDefaultIsolation() != parser.LevelRepeatableRead {
		t.Fatalf("s2 default unexpectedly changed to %v, want REPEATABLE READ", s2.SessionDefaultIsolation())
	}
}

// TestB03IsolationUnsupportedCapability verifies T017 fail-closed capability gate.
func TestB03IsolationUnsupportedCapability(t *testing.T) {
	e, cleanup := newIsolationTestEngine(t)
	defer cleanup()
	s := &Session{CurrentDatabase: "db"}

	for _, level := range []string{"READ UNCOMMITTED", "READ COMMITTED", "SERIALIZABLE"} {
		t.Run("Explicit_"+level, func(t *testing.T) {
			if _, err := e.Execute(s, "SET TRANSACTION ISOLATION LEVEL "+level+";"); err != nil {
				t.Fatalf("SET TRANSACTION %s: %v", level, err)
			}
			// Starting transaction must fail closed with ErrUnsupported in Phase 2
			_, err := e.Execute(s, "BEGIN;")
			if err == nil || !errors.Is(err, storageengine.ErrUnsupported) {
				t.Fatalf("BEGIN with %s returned %v, want ErrUnsupported", level, err)
			}
			// Must not leak active transaction
			if s.InTransaction() {
				t.Fatalf("transaction was started despite unsupported isolation level")
			}
			// pendingNext is preserved because Begin failed
			if _, ok := s.PendingNextIsolation(); !ok {
				t.Fatalf("pendingNext was prematurely consumed despite failed begin")
			}
			// Clean up for next test
			_ = s.SetSessionIsolation(parser.LevelRepeatableRead)
		})

		t.Run("Autocommit_"+level, func(t *testing.T) {
			if _, err := e.Execute(s, "SET TRANSACTION ISOLATION LEVEL "+level+";"); err != nil {
				t.Fatalf("SET TRANSACTION %s: %v", level, err)
			}
			_, err := e.Execute(s, "SELECT * FROM t;")
			if err == nil || !errors.Is(err, storageengine.ErrUnsupported) {
				t.Fatalf("autocommit SELECT with %s returned %v, want ErrUnsupported", level, err)
			}
			if s.InTransaction() {
				t.Fatalf("transaction was started despite unsupported isolation level")
			}
			if _, ok := s.PendingNextIsolation(); !ok {
				t.Fatalf("pendingNext was prematurely consumed despite failed query")
			}
			_ = s.SetSessionIsolation(parser.LevelRepeatableRead)
		})
	}
}
