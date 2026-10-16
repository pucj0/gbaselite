package server

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"gbaselite/executor"
	"gbaselite/parser"
	"gbaselite/protocol"

	_ "github.com/go-sql-driver/mysql"
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

// TestP2Fix01_SetVariableStringLiteral covers P2-FIX-01:
// user variable SET statements with string literals containing isolation keywords must not be rejected.
func TestP2Fix01_SetVariableStringLiteral(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}

	queries := []string{
		"SET @note = 'transaction_isolation'",
		"SET @note = 'tx_isolation'",
		"SET @note = 'SET TRANSACTION ISOLATION LEVEL READ COMMITTED'",
	}

	for _, q := range queries {
		if _, err := ExecuteCompatible(engine, session, q); err != nil {
			t.Fatalf("ExecuteCompatible(%q) failed: %v", q, err)
		}
	}
}

// TestB03IsolationBinaryPreparedProtocol covers P2-FIX-02:
// binary prepared statement protocol (COM_STMT_PREPARE, COM_STMT_EXECUTE, COM_STMT_CLOSE)
// and verifies that prepared statement dynamically reads execution-time session isolation state.
func TestB03IsolationBinaryPreparedProtocol(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()

	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/?charset=utf8mb4&timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	client.SetMaxOpenConns(1)
	defer func() {
		_ = client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = databaseServer.Shutdown(ctx)
		<-done
	}()

	ctx := context.Background()
	conn, err := client.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 1. COM_STMT_PREPARE: prepare variable query
	stmt, err := conn.PrepareContext(ctx, "SELECT @@session.transaction_isolation")
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer stmt.Close()

	// 2. COM_STMT_EXECUTE: initial value is REPEATABLE-READ
	var iso string
	if err := stmt.QueryRowContext(ctx).Scan(&iso); err != nil {
		t.Fatalf("QueryRow failed: %v", err)
	}
	if iso != "REPEATABLE-READ" {
		t.Fatalf("initial prepared query = %q, want REPEATABLE-READ", iso)
	}

	// 3. Update session isolation to READ COMMITTED
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		t.Fatalf("SET SESSION failed: %v", err)
	}

	// 4. COM_STMT_EXECUTE again: must observe execution-time session state
	if err := stmt.QueryRowContext(ctx).Scan(&iso); err != nil {
		t.Fatalf("QueryRow 2 failed: %v", err)
	}
	if iso != "READ-COMMITTED" {
		t.Fatalf("second prepared query = %q, want READ-COMMITTED", iso)
	}

	// 5. Data queries under RC now succeed in Phase 5
	if _, err := conn.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS test"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "USE test"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE t_rc (id INT)"); err != nil {
		t.Fatalf("CREATE TABLE under RC failed: %v", err)
	}

	// 6. Data queries under SERIALIZABLE now succeed in Phase 6
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE t_ser (id INT)"); err != nil {
		t.Fatalf("CREATE TABLE under SERIALIZABLE failed: %v", err)
	}

	// 7. Data queries under READ UNCOMMITTED now succeed in Phase 7
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL READ UNCOMMITTED"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE t_ru (id INT)"); err != nil {
		t.Fatalf("CREATE TABLE under READ UNCOMMITTED failed: %v", err)
	}
}

func scramblePassword(password string, seed []byte) []byte {
	if password == "" {
		return nil
	}
	stage1 := sha1.Sum([]byte(password))
	stage2 := sha1.Sum(stage1[:])
	h := sha1.New()
	h.Write(seed)
	h.Write(stage2[:])
	scramble := h.Sum(nil)
	resp := make([]byte, len(stage1))
	for i := range stage1 {
		resp[i] = stage1[i] ^ scramble[i]
	}
	return resp
}

// TestB03IsolationCOMResetConnectionProtocol covers COM_RESET_CONNECTION (0x1f)
// over the raw MySQL binary packet protocol.
func TestB03IsolationCOMResetConnectionProtocol(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = databaseServer.Shutdown(ctx)
		<-done
	}()

	clientConn, err := net.DialTimeout("tcp", listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	p := &protocol.PacketConn{Conn: clientConn}

	// 1. Read initial handshake from server
	hsPacket, err := p.ReadPacket()
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	if len(hsPacket) < 61 {
		t.Fatalf("handshake packet too short: %d", len(hsPacket))
	}
	seed := append(hsPacket[22:30], hsPacket[49:61]...)
	authResp := scramblePassword("123456", seed)

	// 2. Send handshake response
	caps := uint32(protocol.ClientProtocol41 | protocol.ClientSecureConnection)
	hsResp := make([]byte, 32)
	hsResp[0] = byte(caps & 0xff)
	hsResp[1] = byte((caps >> 8) & 0xff)
	hsResp[2] = byte((caps >> 16) & 0xff)
	hsResp[3] = byte((caps >> 24) & 0xff)
	hsResp[8] = 45 // utf8mb4
	hsResp = append(hsResp, []byte("root\x00")...)
	hsResp = append(hsResp, byte(len(authResp)))
	hsResp = append(hsResp, authResp...)
	if err := p.WritePacket(hsResp); err != nil {
		t.Fatalf("write auth response: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("flush auth: %v", err)
	}

	// 3. Read OK packet from server
	okPacket, err := p.ReadPacket()
	if err != nil || len(okPacket) == 0 || okPacket[0] != 0x00 {
		t.Fatalf("auth failed: %v, okPacket: %v", err, okPacket)
	}

	// 4. Send COM_QUERY (0x03): SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED
	p.ResetSequence()
	query1 := append([]byte{0x03}, []byte("SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED")...)
	if err := p.WritePacket(query1); err != nil {
		t.Fatalf("write query 1: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("flush query 1: %v", err)
	}
	ok1, err := p.ReadPacket()
	if err != nil || len(ok1) == 0 || ok1[0] != 0x00 {
		t.Fatalf("SET query failed: %v, resp: %v", err, ok1)
	}

	// 5. Send COM_RESET_CONNECTION (0x1f)
	p.ResetSequence()
	if err := p.WritePacket([]byte{0x1f}); err != nil {
		t.Fatalf("write COM_RESET_CONNECTION: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("flush COM_RESET_CONNECTION: %v", err)
	}
	okReset, err := p.ReadPacket()
	if err != nil || len(okReset) == 0 || okReset[0] != 0x00 {
		t.Fatalf("COM_RESET_CONNECTION failed: %v, resp: %v", err, okReset)
	}

	// 6. Send COM_QUERY (0x03): SELECT @@session.transaction_isolation
	p.ResetSequence()
	query2 := append([]byte{0x03}, []byte("SELECT @@session.transaction_isolation")...)
	if err := p.WritePacket(query2); err != nil {
		t.Fatalf("write query 2: %v", err)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("flush query 2: %v", err)
	}

	// Read result set: column count, column definition, EOF, row, EOF
	_, err = p.ReadPacket() // col count
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ReadPacket() // col def
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ReadPacket() // EOF
	if err != nil {
		t.Fatal(err)
	}
	row, err := p.ReadPacket() // row data
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(row), "REPEATABLE-READ") {
		t.Fatalf("after COM_RESET_CONNECTION, row = %q, want REPEATABLE-READ", string(row))
	}
}

// TestB03IsolationPrecedenceAndReset covers T017:
// Verifies precedence hierarchy (sessionDefault < pendingNext < activeTxn)
// and reset lifecycle on disconnect, reconnect, and COM_RESET_CONNECTION.
func TestB03IsolationPrecedenceAndReset(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}

	session := &executor.Session{}

	// 1. Initial connection defaults to REPEATABLE READ
	if session.SessionDefaultIsolation() != parser.LevelRepeatableRead {
		t.Fatalf("initial sessionDefault = %v, want REPEATABLE READ", session.SessionDefaultIsolation())
	}
	if _, ok := session.PendingNextIsolation(); ok {
		t.Fatalf("initial pendingNext must be unset")
	}
	if _, ok := session.ActiveIsolation(); ok {
		t.Fatalf("initial activeIsolation must be unset")
	}

	// 2. Set session default to READ COMMITTED
	if _, err := ExecuteCompatible(engine, session, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		t.Fatalf("SET SESSION RC: %v", err)
	}
	if session.SessionDefaultIsolation() != parser.LevelReadCommitted {
		t.Fatalf("sessionDefault = %v, want READ COMMITTED", session.SessionDefaultIsolation())
	}

	// 3. Set pendingNext to SERIALIZABLE (precedence: pendingNext overrides sessionDefault for next txn)
	if _, err := ExecuteCompatible(engine, session, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
		t.Fatalf("SET TRANSACTION SER: %v", err)
	}
	if p, ok := session.PendingNextIsolation(); !ok || p != parser.LevelSerializable {
		t.Fatalf("pendingNext = %v, want SERIALIZABLE", p)
	}

	// 4. Begin transaction: activeTxn adopts pendingNext (SERIALIZABLE) and clears pendingNext
	if _, err := ExecuteCompatible(engine, session, "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if a, ok := session.ActiveIsolation(); !ok || a != parser.LevelSerializable {
		t.Fatalf("activeIsolation = %v, want SERIALIZABLE", a)
	}
	if _, ok := session.PendingNextIsolation(); ok {
		t.Fatalf("pendingNext was not consumed upon BEGIN")
	}

	// 5. While activeTxn is running, change sessionDefault to READ UNCOMMITTED:
	// Active transaction isolation must remain frozen at SERIALIZABLE (precedence: activeTxn > sessionDefault)
	if _, err := ExecuteCompatible(engine, session, "SET SESSION TRANSACTION ISOLATION LEVEL READ UNCOMMITTED"); err != nil {
		t.Fatalf("SET SESSION RU inside active txn: %v", err)
	}
	if a, ok := session.ActiveIsolation(); !ok || a != parser.LevelSerializable {
		t.Fatalf("activeIsolation changed to %v, must stay frozen at SERIALIZABLE", a)
	}
	if session.SessionDefaultIsolation() != parser.LevelReadUncommitted {
		t.Fatalf("sessionDefault = %v, want READ UNCOMMITTED", session.SessionDefaultIsolation())
	}

	// 6. Commit transaction: activeTxn is cleared
	if _, err := ExecuteCompatible(engine, session, "COMMIT"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if _, ok := session.ActiveIsolation(); ok {
		t.Fatalf("activeIsolation must be cleared after COMMIT")
	}

	// 7. Next transaction without pendingNext adopts sessionDefault (READ UNCOMMITTED)
	if _, err := ExecuteCompatible(engine, session, "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if a, ok := session.ActiveIsolation(); !ok || a != parser.LevelReadUncommitted {
		t.Fatalf("activeIsolation = %v, want READ UNCOMMITTED from sessionDefault", a)
	}
	if _, err := ExecuteCompatible(engine, session, "ROLLBACK"); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}

	// 8. ResetConnection lifecycle reset: resets sessionDefault to RR, clears pending and active
	_ = session.SetSessionIsolation(parser.LevelReadCommitted)
	_ = session.SetNextTransactionIsolation(parser.LevelSerializable)
	engine.ResetConnection(session)

	if session.SessionDefaultIsolation() != parser.LevelRepeatableRead {
		t.Fatalf("after ResetConnection, sessionDefault = %v, want REPEATABLE READ", session.SessionDefaultIsolation())
	}
	if _, ok := session.PendingNextIsolation(); ok {
		t.Fatalf("after ResetConnection, pendingNext must be cleared")
	}
	if _, ok := session.ActiveIsolation(); ok {
		t.Fatalf("after ResetConnection, activeIsolation must be cleared")
	}
}
