package server

import (
	"context"
	"database/sql"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"gbaselite/executor"
	_ "github.com/go-sql-driver/mysql"
)

// locking_diagnostics_test.go is the T062 test suite verifying SHOW ENGINE diagnostics
// over real TCP MySQL protocol client connections.

func TestT062ServerShowEngineDiagnosticsProtocol(t *testing.T) {
	dir := t.TempDir()
	e, err := executor.OpenWithOptions(dir, "root", "password", executor.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &MySQLServer{Engine: e, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()

	addr := l.Addr().String()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	}()

	// 1. Connect as root administrator over TCP
	dbRoot, err := sql.Open("mysql", "root:password@tcp("+addr+")/?timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	defer dbRoot.Close()
	dbRoot.SetMaxOpenConns(2)

	// Setup database and table
	if _, err := dbRoot.Exec("CREATE DATABASE test"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbRoot.Exec("USE test"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbRoot.Exec("CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbRoot.Exec("INSERT INTO items VALUES(1, 10)"); err != nil {
		t.Fatal(err)
	}

	// 2. SHOW ENGINE LOCKS: verify columns, types, rows
	rowsLocks, err := dbRoot.Query("SHOW ENGINE LOCKS")
	if err != nil {
		t.Fatalf("SHOW ENGINE LOCKS failed: %v", err)
	}
	colsLocks, err := rowsLocks.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if len(colsLocks) != 12 {
		t.Fatalf("expected 12 columns for SHOW ENGINE LOCKS, got %d: %v", len(colsLocks), colsLocks)
	}
	expectedLocksCols := []string{"GrantID", "RootID", "ScopeID", "Generation", "Term", "Mode", "ResourceKind", "OpaqueResourceID", "GrantedAt", "SnapshotRevision", "Truncated", "NextCursor"}
	for i, col := range expectedLocksCols {
		if colsLocks[i] != col {
			t.Fatalf("column %d: expected %s, got %s", i, col, colsLocks[i])
		}
	}
	rowsLocks.Close()

	// 3. SHOW ENGINE LOCK WAITS: verify columns
	rowsWaits, err := dbRoot.Query("SHOW ENGINE LOCK WAITS")
	if err != nil {
		t.Fatalf("SHOW ENGINE LOCK WAITS failed: %v", err)
	}
	colsWaits, err := rowsWaits.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if len(colsWaits) != 12 {
		t.Fatalf("expected 12 columns for SHOW ENGINE LOCK WAITS, got %d: %v", len(colsWaits), colsWaits)
	}
	expectedWaitsCols := []string{"RequestID", "RootID", "ScopeID", "RequestedMode", "OpaqueResourceID", "BlockerRootIDs", "Reason", "WaitMilliseconds", "Deadline", "SnapshotRevision", "Truncated", "NextCursor"}
	for i, col := range expectedWaitsCols {
		if colsWaits[i] != col {
			t.Fatalf("waits column %d: expected %s, got %s", i, col, colsWaits[i])
		}
	}
	rowsWaits.Close()

	// 4. SHOW ENGINE DEADLOCKS: verify columns
	rowsDeadlocks, err := dbRoot.Query("SHOW ENGINE DEADLOCKS")
	if err != nil {
		t.Fatalf("SHOW ENGINE DEADLOCKS failed: %v", err)
	}
	colsDeadlocks, err := rowsDeadlocks.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if len(colsDeadlocks) != 8 {
		t.Fatalf("expected 8 columns for SHOW ENGINE DEADLOCKS, got %d: %v", len(colsDeadlocks), colsDeadlocks)
	}
	expectedDeadlockCols := []string{"DetectedOrdinal", "GraphRevision", "VictimRootID", "CycleRootIDs", "AnonymousIDs", "Modes", "Truncated", "NextCursor"}
	for i, col := range expectedDeadlockCols {
		if colsDeadlocks[i] != col {
			t.Fatalf("deadlocks column %d: expected %s, got %s", i, col, colsDeadlocks[i])
		}
	}
	rowsDeadlocks.Close()

	// 5. SHOW ENGINE LOCKS STATUS: returns metadata row even when lock table is empty!
	var eng, view, snapRev, total, trunc, nextCursor, limit string
	err = dbRoot.QueryRow("SHOW ENGINE LOCKS STATUS").Scan(&eng, &view, &snapRev, &total, &trunc, &nextCursor, &limit)
	if err != nil {
		t.Fatalf("SHOW ENGINE LOCKS STATUS failed: %v", err)
	}
	if view != "LOCKS" || snapRev == "" || nextCursor == "" {
		t.Fatalf("unexpected empty metadata: view=%s, rev=%s, cursor=%s", view, snapRev, nextCursor)
	}

	// 6. SHOW ENGINE INNODB STATUS: verify 3-column MySQL standard
	rowsStatus, err := dbRoot.Query("SHOW ENGINE INNODB STATUS")
	if err != nil {
		t.Fatalf("SHOW ENGINE INNODB STATUS failed: %v", err)
	}
	colsStatus, err := rowsStatus.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if len(colsStatus) != 3 {
		t.Fatalf("expected 3 columns for SHOW ENGINE INNODB STATUS, got %d: %v", len(colsStatus), colsStatus)
	}
	var statusType, statusName, statusText string
	if rowsStatus.Next() {
		if err := rowsStatus.Scan(&statusType, &statusName, &statusText); err != nil {
			t.Fatal(err)
		}
		if statusType != "INNODB" || !strings.Contains(statusText, "LOCK STATS:") || !strings.Contains(statusText, "NextCursor=") {
			t.Fatalf("unexpected status content: type=%s, text=%s", statusType, statusText)
		}
	}
	rowsStatus.Close()

	// 7. Test active held locks reporting and privacy
	tx, err := dbRoot.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE items SET v = 99 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}

	// Read SHOW ENGINE LOCKS while tx holds lock
	dbRoot2, err := sql.Open("mysql", "root:password@tcp("+addr+")/test?timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	defer dbRoot2.Close()

	rowsActive, err := dbRoot2.Query("SHOW ENGINE LOCKS")
	if err != nil {
		t.Fatalf("SHOW ENGINE LOCKS during active tx failed: %v", err)
	}
	foundActive := false
	for rowsActive.Next() {
		foundActive = true
		vals := make([]sql.NullString, 12)
		ptrs := make([]any, 12)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rowsActive.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		// Verify privacy: no raw table name, SQL, or secret leaked
		for _, v := range vals {
			if v.Valid {
				s := v.String
				if strings.Contains(s, "items") || strings.Contains(s, "secret") || strings.Contains(s, "row/") {
					t.Fatalf("leaked raw identity in TCP MySQL protocol response: %s", s)
				}
			}
		}
	}
	rowsActive.Close()
	if !foundActive {
		t.Fatal("expected at least one active lock row in SHOW ENGINE LOCKS")
	}

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// 7. Non-admin user rejected with error packet over TCP
	if _, err := dbRoot.Exec("CREATE USER 'charlie'@'%' IDENTIFIED BY 'charpass'"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbRoot.Exec("GRANT SELECT ON test.* TO 'charlie'@'%'"); err != nil {
		t.Fatal(err)
	}

	dbCharlie, err := sql.Open("mysql", "charlie:charpass@tcp("+addr+")/test?timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	defer dbCharlie.Close()

	for _, cmd := range []string{
		"SHOW ENGINE LOCKS",
		"SHOW ENGINE LOCK WAITS",
		"SHOW ENGINE DEADLOCKS",
		"SHOW ENGINE INNODB STATUS",
	} {
		_, err := dbCharlie.Query(cmd)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "access denied") {
			t.Fatalf("expected access denied for charlie on %s, got: %v", cmd, err)
		}
	}

	// 8. Unknown engine returns error packet
	if _, err := dbRoot.Query("SHOW ENGINE UNKNOWN_FOO LOCKS"); err == nil || !strings.Contains(err.Error(), "unknown engine") {
		t.Fatalf("expected unknown engine error over TCP, got: %v", err)
	}

	// 9. Standard statements work normally without regression
	var count int
	if err := dbRoot.QueryRow("SELECT COUNT(*) FROM items").Scan(&count); err != nil || count != 1 {
		t.Fatalf("SELECT COUNT(*) failed: %v, count=%d", err, count)
	}
	rowsTables, err := dbRoot.Query("SHOW TABLES")
	if err != nil {
		t.Fatal(err)
	}
	rowsTables.Close()
}
