package server

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// T069/T070 at the protocol boundary: when a client connection goes away with an
// uncommitted transaction, the server must roll that transaction back and leave no
// entry behind in the storage engine transaction registry.
//
// The registry is read through the optional capability on the engine the server was
// opened with, so this test observes the same registry the commit path uses instead of
// a private copy, and it needs no physical backend import.

// requireActiveTransaction waits for the registry to show exactly one live root and
// returns it. Registration happens on the connection's own goroutine, so the wait is
// for an event the client already triggered, never a delay used to order transactions.
func requireActiveTransaction(t *testing.T, diagnostics storageengine.TransactionDiagnostics, want int) []storageengine.TransactionInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := make([]storageengine.TransactionInfo, 0, 2)
		for _, info := range diagnostics.ActiveTransactions() {
			if info.State == storageengine.TransactionActive {
				live = append(live, info)
			}
		}
		if len(live) == want {
			return live
		}
		if time.Now().After(deadline) {
			t.Fatalf("live transactions = %d, want %d: %+v", len(live), want, diagnostics.ActiveTransactions())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMySQLProtocolDisconnectReleasesTransactionRegistry(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, ok := engine.Backend.(storageengine.TransactionDiagnostics)
	if !ok {
		t.Fatal("the MVCC backend does not expose transaction diagnostics")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = databaseServer.Shutdown(shutdownContext)
		<-done
	}()

	ctx := context.Background()
	address := listener.Addr().String()
	// Pinned connections, so the client library cannot hide a closed session by
	// silently opening a replacement.
	open := func() (*sql.DB, *sql.Conn) {
		t.Helper()
		pool, err := sql.Open("mysql", "root:123456@tcp("+address+")/?charset=utf8mb4&timeout=3s&readTimeout=30s")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pool.Close() })
		connection, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return pool, connection
	}
	exec := func(connection *sql.Conn, query string) {
		t.Helper()
		if _, err := connection.ExecContext(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	adminPool, admin := open()
	defer admin.Close()
	defer adminPool.Close()
	workerPool, worker := open()
	defer workerPool.Close()

	for _, query := range []string{"CREATE DATABASE disc", "CREATE TABLE disc.items(id INT PRIMARY KEY,v INT)"} {
		exec(admin, query)
	}
	if live := requireActiveTransaction(t, diagnostics, 0); len(live) != 0 {
		t.Fatalf("registry was not empty before the transaction: %+v", live)
	}

	// An explicit transaction with an uncommitted write, then the socket goes away
	// without COMMIT or ROLLBACK.
	var workerID int64
	if err := worker.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&workerID); err != nil {
		t.Fatal(err)
	}
	exec(worker, "USE disc")
	exec(worker, "BEGIN")
	exec(worker, "INSERT INTO disc.items VALUES(1,10)")
	live := requireActiveTransaction(t, diagnostics, 1)
	if live[0].ParentID != "" || live[0].HasCommitTS {
		t.Fatalf("explicit transaction entry = %+v", live[0])
	}

	// KILL closes the victim connection from the inside, which is the same path a
	// dropped socket takes: the handler returns and its deferred cleanup runs.
	exec(admin, fmt.Sprintf("KILL %d", workerID))
	if err := worker.Close(); err != nil {
		t.Logf("closing the killed connection: %v", err)
	}
	requireActiveTransaction(t, diagnostics, 0)
	if stats := diagnostics.TransactionStats(); stats.ActiveRoot != 0 || stats.ActiveChildren != 0 || stats.HasOldestReadTS {
		t.Fatalf("disconnect left registry state behind: %+v", stats)
	}

	// The discarded write must not be visible, and the database must still work.
	var rows int
	if err := admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM disc.items").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("rows after disconnect rollback = %d, want 0", rows)
	}
	exec(admin, "INSERT INTO disc.items VALUES(2,20)")
	if err := admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM disc.items").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows after a later commit = %d, want 1", rows)
	}
	requireActiveTransaction(t, diagnostics, 0)
}
