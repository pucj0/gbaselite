package server

import (
	"context"
	"database/sql"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func TestMVCCRejectsSavepointsOverMySQLProtocol(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "secret")
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
		t.Fatalf("prepared savepoint rejected: %v", err)
	}
	prepared.Close()
	for _, q := range []string{"SAVEPOINT p", "ROLLBACK TO p", "RELEASE SAVEPOINT p"} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatalf("savepoint rejected: %s: %v", q, err)
		}
	}
	if _, err := tx.Exec("INSERT INTO items VALUES(2)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var id int
	if err := db.QueryRow("SELECT id FROM items").Scan(&id); err != nil || id != 2 {
		t.Fatalf("rejected statement poisoned transaction: %d %v", id, err)
	}
}
