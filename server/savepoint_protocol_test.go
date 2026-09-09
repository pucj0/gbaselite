package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"gbaselite/executor"
	driver "github.com/go-sql-driver/mysql"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func TestSavepointsOverMySQLProtocol(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	db, err := sql.Open("mysql", "root:secret@tcp("+listener.Addr().String()+")/?timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		<-done
		engine.Close()
	}()
	for _, q := range []string{"CREATE DATABASE sp", "USE sp", "CREATE TABLE items(id INT PRIMARY KEY)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	prepared, err := tx.Prepare("SAVEPOINT prepared_point")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Exec(); err != nil {
		t.Fatal(err)
	}
	prepared.Close()
	for _, q := range []string{"INSERT INTO items VALUES(1)", "SAVEPOINT later", "ROLLBACK TO prepared_point"} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	assertCode := func(q string, code uint16) {
		t.Helper()
		_, err := tx.Exec(q)
		var driverErr *driver.MySQLError
		if !errors.As(err, &driverErr) || driverErr.Number != code {
			t.Fatalf("%s: want %d, got %v", q, code, err)
		}
	}
	assertCode("RELEASE SAVEPOINT later", 1305)
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM items").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback result: %d %v", count, err)
	}
	for i := 0; i < 31; i++ {
		if _, err := tx.Exec(fmt.Sprintf("SAVEPOINT p%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	assertCode("SAVEPOINT overflow", 1041)
	if _, err := tx.Exec("INSERT INTO items VALUES(2)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var id int
	if err := db.QueryRow("SELECT id FROM items").Scan(&id); err != nil || id != 2 {
		t.Fatalf("commit result: %d %v", id, err)
	}
}
