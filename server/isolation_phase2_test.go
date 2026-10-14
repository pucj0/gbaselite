package server

import (
	"strings"
	"testing"
	"time"

	"gbaselite/executor"
)

// TestB03IsolationVariables covers T015 / M44: SELECT @@... and SHOW VARIABLES.
func TestB03IsolationVariables(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}

	// 1. Initial state: REPEATABLE-READ
	for _, query := range []string{
		"SELECT @@session.transaction_isolation",
		"SELECT @@global.transaction_isolation",
		"SELECT @@transaction_isolation",
		"SELECT @@tx_isolation",
		"SELECT @@session.tx_isolation",
	} {
		res, err := ExecuteCompatible(engine, session, query)
		if err != nil {
			t.Fatalf("%s failed: %v", query, err)
		}
		if len(res.Rows) != 1 || res.Rows[0][0] != "REPEATABLE-READ" {
			t.Fatalf("%s = %v, want REPEATABLE-READ", query, res.Rows)
		}
	}

	// SHOW VARIABLES initial
	showRes, err := ExecuteCompatible(engine, session, "SHOW VARIABLES LIKE 'transaction_isolation'")
	if err != nil {
		t.Fatalf("SHOW VARIABLES: %v", err)
	}
	if len(showRes.Rows) != 1 || showRes.Rows[0][1] != "REPEATABLE-READ" {
		t.Fatalf("SHOW VARIABLES = %v, want REPEATABLE-READ", showRes.Rows)
	}

	// 2. Modify session default to READ COMMITTED
	if _, err := ExecuteCompatible(engine, session, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		t.Fatalf("SET SESSION: %v", err)
	}

	// @@session and @@transaction_isolation and @@tx_isolation must now report READ-COMMITTED
	for _, query := range []string{
		"SELECT @@session.transaction_isolation",
		"SELECT @@transaction_isolation",
		"SELECT @@tx_isolation",
	} {
		res, err := ExecuteCompatible(engine, session, query)
		if err != nil {
			t.Fatalf("%s failed: %v", query, err)
		}
		if len(res.Rows) != 1 || res.Rows[0][0] != "READ-COMMITTED" {
			t.Fatalf("%s = %v, want READ-COMMITTED", query, res.Rows)
		}
	}

	// @@global must remain REPEATABLE-READ
	globalRes, err := ExecuteCompatible(engine, session, "SELECT @@global.transaction_isolation")
	if err != nil {
		t.Fatalf("SELECT @@global: %v", err)
	}
	if len(globalRes.Rows) != 1 || globalRes.Rows[0][0] != "REPEATABLE-READ" {
		t.Fatalf("SELECT @@global = %v, want REPEATABLE-READ", globalRes.Rows)
	}

	// SHOW VARIABLES must report READ-COMMITTED for session, REPEATABLE-READ for global
	showSession, err := ExecuteCompatible(engine, session, "SHOW SESSION VARIABLES LIKE 'transaction_isolation'")
	if err != nil {
		t.Fatalf("SHOW SESSION VARIABLES: %v", err)
	}
	if len(showSession.Rows) != 1 || showSession.Rows[0][1] != "READ-COMMITTED" {
		t.Fatalf("SHOW SESSION VARIABLES = %v, want READ-COMMITTED", showSession.Rows)
	}

	showGlobal, err := ExecuteCompatible(engine, session, "SHOW GLOBAL VARIABLES LIKE 'transaction_isolation'")
	if err != nil {
		t.Fatalf("SHOW GLOBAL VARIABLES: %v", err)
	}
	if len(showGlobal.Rows) != 1 || showGlobal.Rows[0][1] != "REPEATABLE-READ" {
		t.Fatalf("SHOW GLOBAL VARIABLES = %v, want REPEATABLE-READ", showGlobal.Rows)
	}

	// 3. SET NEXT does NOT leak into sessionDefault
	if _, err := ExecuteCompatible(engine, session, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
		t.Fatalf("SET NEXT: %v", err)
	}
	res, err := ExecuteCompatible(engine, session, "SELECT @@session.transaction_isolation")
	if err != nil {
		t.Fatalf("SELECT @@session: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "READ-COMMITTED" {
		t.Fatalf("pendingNext leaked into session: %v", res.Rows)
	}
}

// TestB03IsolationProtocolParity covers T014 / M07: Engine.Execute vs ExecuteCompatible parity.
func TestB03IsolationProtocolParity(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}

	s1 := &executor.Session{}
	s2 := &executor.Session{}

	// Both methods produce exact same state transition
	if _, err := engine.Execute(s1, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;"); err != nil {
		t.Fatalf("engine.Execute: %v", err)
	}
	if _, err := ExecuteCompatible(engine, s2, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED;"); err != nil {
		t.Fatalf("ExecuteCompatible: %v", err)
	}

	if s1.SessionDefaultIsolation() != s2.SessionDefaultIsolation() {
		t.Fatalf("s1 (%v) != s2 (%v)", s1.SessionDefaultIsolation(), s2.SessionDefaultIsolation())
	}
}

// TestB03IsolationPreparedExecution covers T016: prepared statement reads execution-time session state.
func TestB03IsolationPreparedExecution(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}

	prepared := "SELECT @@session.transaction_isolation"

	// Execution 1: default RR
	res1, err := ExecuteCompatible(engine, session, prepared)
	if err != nil {
		t.Fatalf("exec 1: %v", err)
	}
	if res1.Rows[0][0] != "REPEATABLE-READ" {
		t.Fatalf("want REPEATABLE-READ, got %v", res1.Rows[0][0])
	}

	// Change session isolation
	if _, err := ExecuteCompatible(engine, session, "SET SESSION transaction_isolation = 'READ-COMMITTED'"); err != nil {
		t.Fatalf("SET SESSION: %v", err)
	}

	// Execution 2: must see updated session isolation
	res2, err := ExecuteCompatible(engine, session, prepared)
	if err != nil {
		t.Fatalf("exec 2: %v", err)
	}
	if res2.Rows[0][0] != "READ-COMMITTED" {
		t.Fatalf("want READ-COMMITTED, got %v", res2.Rows[0][0])
	}
}

// TestB03IsolationExistingSettingsRegression covers non-regression of existing server settings.
func TestB03IsolationExistingSettingsRegression(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}

	// SET NAMES
	if _, err := ExecuteCompatible(engine, session, "SET NAMES utf8mb4 COLLATE utf8mb4_general_ci"); err != nil {
		t.Fatalf("SET NAMES: %v", err)
	}

	// SET time_zone
	if _, err := ExecuteCompatible(engine, session, "SET time_zone = '+08:00'"); err != nil {
		t.Fatalf("SET time_zone: %v", err)
	}

	// SET foreign_key_checks
	if _, err := ExecuteCompatible(engine, session, "SET foreign_key_checks = 0"); err != nil {
		t.Fatalf("SET foreign_key_checks: %v", err)
	}

	// SET innodb_lock_wait_timeout
	if _, err := ExecuteCompatible(engine, session, "SET SESSION innodb_lock_wait_timeout = 25"); err != nil {
		t.Fatalf("SET innodb_lock_wait_timeout: %v", err)
	}
	if session.LockWaitTimeout() != 25*time.Second {
		t.Fatalf("LockWaitTimeout = %v, want 25s", session.LockWaitTimeout())
	}

	// Legal query containing literal 'TRANSACTION ISOLATION' must NOT be rejected
	if _, err := ExecuteCompatible(engine, session, "CREATE DATABASE testdb"); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := ExecuteCompatible(engine, session, "USE testdb"); err != nil {
		t.Fatalf("USE: %v", err)
	}
	if _, err := ExecuteCompatible(engine, session, "CREATE TABLE logs (msg VARCHAR(100))"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := ExecuteCompatible(engine, session, "INSERT INTO logs VALUES ('TRANSACTION ISOLATION test message')"); err != nil {
		t.Fatalf("INSERT with 'TRANSACTION ISOLATION' literal unexpectedly failed: %v", err)
	}
	res, err := ExecuteCompatible(engine, session, "SELECT msg FROM logs WHERE msg LIKE '%TRANSACTION ISOLATION%'")
	if err != nil {
		t.Fatalf("SELECT with 'TRANSACTION ISOLATION' literal unexpectedly failed: %v", err)
	}
	if len(res.Rows) != 1 || !strings.Contains(res.Rows[0][0].(string), "TRANSACTION ISOLATION") {
		t.Fatalf("unexpected query result: %v", res.Rows)
	}

	// Invalid isolation commands must fail cleanly through ExecuteCompatible
	for _, badSQL := range []string{
		"SET GLOBAL TRANSACTION ISOLATION LEVEL REPEATABLE READ",
		"SET GLOBAL transaction_isolation = 'REPEATABLE-READ'",
		"SET @@GLOBAL.transaction_isolation = 'SERIALIZABLE'",
		"SET PERSIST transaction_isolation = 'READ-COMMITTED'",
		"SET TRANSACTION READ ONLY",
		"SET TRANSACTION ISOLATION LEVEL SNAPSHOT",
		"SET transaction_isolation = 'INVALID_LEVEL'",
	} {
		if _, err := ExecuteCompatible(engine, session, badSQL); err == nil {
			t.Fatalf("ExecuteCompatible(%q) unexpectedly succeeded, want error", badSQL)
		}
	}
}
