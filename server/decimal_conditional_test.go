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

func TestDecimalConditionalPreparedProtocol(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &MySQLServer{Engine: engine, Logger: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	db, err := sql.Open("mysql", "root:secret@tcp("+listener.Addr().String()+")/?charset=utf8mb4&timeout=3s")
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
	}()
	for _, query := range []string{"CREATE DATABASE decimal_wire", "USE decimal_wire", "CREATE TABLE numbers(id INT PRIMARY KEY,n DECIMAL(25,2),f DOUBLE)", "INSERT INTO numbers VALUES(1,9007199254740993.01,2.5),(2,0.25,3.5)"} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(query, err)
		}
	}
	statement, err := db.Prepare("SELECT IF(id=1,0,n),IF(id=1,n,0),IF(id=1,n,f) FROM numbers WHERE id=?")
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	var first, second string
	var mixed float64
	if err := statement.QueryRow(2).Scan(&first, &second, &mixed); err != nil {
		t.Fatal(err)
	}
	if first != "0.25" || second != "0" || mixed != 3.5 {
		t.Fatalf("binary conditional results %s %s %v", first, second, mixed)
	}
	if err := statement.QueryRow(1).Scan(&first, &second, &mixed); err != nil {
		t.Fatal(err)
	}
	if second != "9007199254740993.01" || mixed == 0 {
		t.Fatalf("binary exact/mixed results %s %s %v", first, second, mixed)
	}
}
