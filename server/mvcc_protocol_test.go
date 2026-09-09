package server

import (
	"context"
	"database/sql"
	"gbaselite/executor"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func TestMVCCMySQLProtocol(t *testing.T) {
	e, err := openTestEngineWithOptions(t, t.TempDir(), "root", "secret", executor.OpenOptions{StorageMode: "mvcc"})
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
	db, err := sql.Open("mysql", "root:secret@tcp("+l.Addr().String()+")/?timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		<-done
	}()
	for _, q := range []string{"CREATE DATABASE test", "USE test", "CREATE TABLE items(id BIGINT PRIMARY KEY AUTO_INCREMENT, amount DECIMAL(20,2), doc JSON)"} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("INSERT INTO items(amount,doc) VALUES(?,JSON_OBJECT('n',?))", "9007199254740993.25", int64(7)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var amount, doc string
	if err = db.QueryRow("SELECT amount,JSON_EXTRACT(doc,'$.n') FROM items WHERE id=?", 1).Scan(&amount, &doc); err != nil {
		t.Fatal(err)
	}
	if amount != "9007199254740993.25" || doc != "7" {
		t.Fatalf("values %s %s", amount, doc)
	}
	var explainID int64
	var selectType, access, extra string
	var table, partitions, possible, key, keyLen, ref sql.NullString
	var estimated sql.NullInt64
	var filtered sql.NullFloat64
	if err = db.QueryRow("EXPLAIN SELECT id FROM items WHERE id=?", 1).Scan(&explainID, &selectType, &table, &partitions, &access, &possible, &key, &keyLen, &ref, &estimated, &filtered, &extra); err != nil {
		t.Fatal(err)
	}
	if access != "const" || key.String != "PRIMARY" || !estimated.Valid || estimated.Int64 != 1 || filtered.Valid {
		t.Fatal("invalid EXPLAIN wire result", access, key, estimated, filtered)
	}
	rows, err := db.Query("SHOW REPLICATION STATUS")
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
}
