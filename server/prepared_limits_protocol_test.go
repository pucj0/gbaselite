package server

import (
	"context"
	"database/sql"
	"errors"
	"gbaselite/executor"
	driver "github.com/go-sql-driver/mysql"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func TestPreparedStatementLimitOverTCP(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "test")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0), MaxPreparedStatements: 1, MaxPreparedBytes: 64}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	client, err := sql.Open("mysql", "root:test@tcp("+listener.Addr().String()+")/?timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	client.SetMaxOpenConns(1)
	defer func() {
		client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		<-done
	}()
	first, err := client.Prepare("SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if statement, err := client.Prepare("SELECT 2"); err == nil {
		statement.Close()
		t.Fatal("server ignored statement limit")
	} else {
		var mysqlErr *driver.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1461 {
			t.Fatalf("wrong protocol error: %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := client.Prepare("SELECT 2")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var result int
	if err := second.QueryRow().Scan(&result); err != nil || result != 2 {
		t.Fatalf("connection unusable after rejection: %d %v", result, err)
	}
}
