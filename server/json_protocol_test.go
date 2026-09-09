package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"gbaselite/executor"
	driver "github.com/go-sql-driver/mysql"
)

func TestJSONOverMySQLProtocol(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "secret")
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
	for _, q := range []string{`CREATE DATABASE json_test`, `USE json_test`, `CREATE TABLE docs(id INT PRIMARY KEY,body JSON)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	insert, err := db.Prepare(`INSERT INTO docs VALUES(?,JSON_OBJECT('name',?,'nested',JSON_ARRAY(?,NULL),'big',?))`)
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Close()
	for i, name := range []string{"中文😀", "second"} {
		if _, err := insert.Exec(i+1, name, int64(2), int64(9223372036854775807)); err != nil {
			t.Fatal(err)
		}
	}
	query, err := db.Prepare(`SELECT JSON_OBJECT('doc',body),JSON_UNQUOTE(JSON_EXTRACT(body,?)),JSON_LENGTH(body),JSON_EXTRACT(body,'$.missing'),JSON_EXTRACT(body,'$.nested[1]') FROM docs WHERE id=?`)
	if err != nil {
		t.Fatal(err)
	}
	defer query.Close()
	var raw, name, jn string
	var missing sql.NullString
	var length int
	if err := query.QueryRow("$.name", 1).Scan(&raw, &name, &length, &missing, &jn); err != nil {
		t.Fatal(err)
	}
	if name != "中文😀" || length != 3 || missing.Valid || jn != "null" {
		t.Fatalf("bad decoded values %q %d %+v %s", name, length, missing, jn)
	}
	var doc map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	inner, ok := doc["doc"].(map[string]any)
	if !ok || inner["big"] != json.Number("9223372036854775807") {
		t.Fatalf("lost nested JSON/integer: %s", raw)
	}
	if _, err := db.Exec(`UPDATE docs SET body=JSON_SET(body,?,JSON_ARRAY(?)) WHERE id=?`, "$.nested", "updated", 1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT JSON_UNQUOTE(JSON_EXTRACT(body,'$.nested[0]')) FROM docs WHERE id=1`).Scan(&name); err != nil || name != "updated" {
		t.Fatalf("update: %s %v", name, err)
	}
	// Reuse the statement with different parameter types and values.
	if err := query.QueryRow("$.name", 2).Scan(&raw, &name, &length, &missing, &jn); err != nil || name != "second" {
		t.Fatalf("reuse: %s %v", name, err)
	}
	for _, tc := range []struct {
		q    string
		code uint16
	}{
		{`SELECT JSON_OBJECT('key')`, 1582}, {`SELECT JSON_OBJECT(NULL,1)`, 3158},
		{`SELECT JSON_EXTRACT('{','$')`, 3141}, {`SELECT JSON_EXTRACT('{}','bad')`, 3143},
		{`SELECT JSON_REMOVE('{}','$')`, 3153}, {`SELECT JSON_CONTAINS_PATH('{}','bad','$.a')`, 3154},
	} {
		var value any
		err := db.QueryRow(tc.q).Scan(&value)
		var me *driver.MySQLError
		if !errors.As(err, &me) || me.Number != tc.code {
			t.Errorf("%s: want %d, got %v", tc.q, tc.code, err)
		}
	}
	bad, err := db.Prepare(`SELECT JSON_EXTRACT(?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if err := bad.QueryRow("{", "$").Scan(&raw); err == nil {
		t.Fatal("prepared invalid document accepted")
	}
	if err := bad.QueryRow(`{"a":1}`, "$.a").Scan(&raw); err != nil || raw != "1" {
		t.Fatalf("reuse after error: %q %v", raw, err)
	}
}
