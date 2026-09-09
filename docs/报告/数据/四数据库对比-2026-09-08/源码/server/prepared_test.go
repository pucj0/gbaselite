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

func TestPreparedStatementProtocol(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
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
	for _, query := range []string{
		"CREATE DATABASE prepared_test",
		"USE prepared_test",
		"CREATE TABLE records(id BIGINT,name VARCHAR(64),score DOUBLE,note TEXT,created_at DATE)",
	} {
		if _, err := client.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	insert, err := client.Prepare("INSERT INTO records VALUES (?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Close()
	for _, values := range [][]any{
		{int64(1), "Alice", 88.5, nil, time.Date(2026, 7, 27, 12, 30, 0, 0, time.Local)},
		{int64(2), "Bob", 92.25, "ready", time.Date(2026, 7, 28, 8, 0, 0, 0, time.Local)},
	} {
		if _, err := insert.Exec(values...); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := client.Query("SELECT id,name,score,note,created_at FROM records WHERE id >= ? ORDER BY id DESC LIMIT ? OFFSET ?", int64(1), int64(2), int64(0))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type record struct {
		id      int64
		name    string
		score   float64
		note    sql.NullString
		created string
	}
	var records []record
	for rows.Next() {
		var item record
		if err := rows.Scan(&item.id, &item.name, &item.score, &item.note, &item.created); err != nil {
			t.Fatal(err)
		}
		records = append(records, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].id != 2 || records[0].name != "Bob" || records[1].id != 1 || records[1].note.Valid || records[0].created != "2026-07-28" {
		t.Fatalf("unexpected prepared rows: %#v", records)
	}

	if _, err := client.Exec("UPDATE records SET name=?,score=? WHERE id=?", "Alice Updated", 99.5, int64(1)); err != nil {
		t.Fatal(err)
	}
	var name string
	var score float64
	if err := client.QueryRow("SELECT name,score FROM records WHERE id=?", int64(1)).Scan(&name, &score); err != nil {
		t.Fatal(err)
	}
	if name != "Alice Updated" || score != 99.5 {
		t.Fatalf("got name=%q score=%v", name, score)
	}
	if _, err := client.Exec("INSERT INTO records SET id=?,name=?,score=?+?,note=?,created_at=?", int64(3), "Carol", 40, 2.5, "insert set", time.Date(2026, 7, 29, 0, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	if err := client.QueryRow("SELECT name,score FROM records WHERE id=?", int64(3)).Scan(&name, &score); err != nil {
		t.Fatal(err)
	}
	if name != "Carol" || score != 42.5 {
		t.Fatalf("prepared INSERT SET got name=%q score=%v", name, score)
	}

	for _, query := range []string{
		"CREATE TABLE workout_exercises(id BIGINT,workout_id BIGINT,PRIMARY KEY(id))",
		"CREATE TABLE workout_sets(id BIGINT,workout_exercise_id BIGINT,PRIMARY KEY(id))",
		"INSERT INTO workout_exercises VALUES (10,1),(20,2)",
		"INSERT INTO workout_sets VALUES (100,10),(200,20)",
	} {
		if _, err := client.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	deleted, err := client.Exec("DELETE FROM workout_sets WHERE workout_exercise_id IN (SELECT id FROM workout_exercises WHERE workout_id=?)", int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("prepared IN subquery affected=%d error=%v", affected, err)
	}
	updated, err := client.Exec("UPDATE records SET score=score+? WHERE id=?", 0.5, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("prepared expression UPDATE affected=%d error=%v", affected, err)
	}
	if err := client.QueryRow("SELECT score FROM records WHERE id=?", int64(1)).Scan(&score); err != nil || score != 100 {
		t.Fatalf("prepared expression UPDATE score=%v error=%v", score, err)
	}
	unionRows, err := client.Query("SELECT id FROM records WHERE id=? UNION ALL SELECT id FROM records WHERE id=? ORDER BY id DESC", int64(1), int64(2))
	if err != nil {
		t.Fatal(err)
	}
	var unionIDs []int64
	for unionRows.Next() {
		var id int64
		if err := unionRows.Scan(&id); err != nil {
			unionRows.Close()
			t.Fatal(err)
		}
		unionIDs = append(unionIDs, id)
	}
	if err := unionRows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(unionIDs) != 2 || unionIDs[0] != 2 || unionIDs[1] != 1 {
		t.Fatalf("prepared UNION rows=%v", unionIDs)
	}
}

func TestPreparedBooleanAndTinyIntComparisonCompatibility(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
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
	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/?timeout=3s")
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
	for _, query := range []string{
		"CREATE DATABASE prepared_boolean_test",
		"USE prepared_boolean_test",
		"CREATE TABLE options(id INT,enabled BOOLEAN,PRIMARY KEY(id))",
		"INSERT INTO options VALUES (1,TRUE),(2,FALSE),(3,TRUE)",
	} {
		if _, err := client.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for _, parameter := range []any{true, int64(1)} {
		var count int64
		if err := client.QueryRow("SELECT COUNT(*) FROM options WHERE enabled=?", parameter).Scan(&count); err != nil || count != 2 {
			t.Fatalf("enabled=%T(%v) count=%d error=%v", parameter, parameter, count, err)
		}
	}
	var count int64
	if err := client.QueryRow("SELECT COUNT(*) FROM options WHERE enabled IN (?,?)", int64(0), int64(1)).Scan(&count); err != nil || count != 3 {
		t.Fatalf("BOOLEAN IN count=%d error=%v", count, err)
	}
	if result, err := client.Exec("UPDATE options SET enabled=TRUE WHERE enabled=?", int64(1)); err != nil {
		t.Fatal(err)
	} else if affected, _ := result.RowsAffected(); affected != 2 {
		t.Fatalf("BOOLEAN UPDATE affected=%d", affected)
	}
}

func TestPreparedPlaceholderScanner(t *testing.T) {
	query := "SELECT '?' AS literal, value FROM t WHERE a=? AND note='x?y' /* ? */ AND b=? -- ?\n"
	if count := countPlaceholders(query); count != 2 {
		t.Fatalf("placeholder count = %d", count)
	}
	bound, err := bindPreparedQuery(query, []any{"O'Reilly", int64(7)})
	if err != nil {
		t.Fatal(err)
	}
	want := "a='O''Reilly'"
	if !strings.Contains(bound, want) || !strings.Contains(bound, "b=7") {
		t.Fatalf("unexpected bound query: %s", bound)
	}
}

func TestGoApplicationQueryCompatibility(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
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
	client, err := sql.Open("mysql", "root:123456@tcp("+listener.Addr().String()+")/?charset=utf8mb4&parseTime=true&timeout=3s")
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
	for _, query := range []string{
		"CREATE DATABASE app_query_test",
		"USE app_query_test",
		"CREATE TABLE IF NOT EXISTS auth_app(id VARCHAR(32),app_name VARCHAR(100),app_desc TEXT,public_key TEXT,private_key TEXT)",
		"CREATE TABLE IF NOT EXISTS auth_code(id VARCHAR(32),app_id VARCHAR(32),auth_code VARCHAR(36),auth_equ_count INT,use_info TEXT,expire_time DATE,create_time DATE,status VARCHAR(10),req_ip VARCHAR(64),req_place VARCHAR(255),stop_reason TEXT)",
		"CREATE TABLE IF NOT EXISTS auth_code_equ(id VARCHAR(32),code_id VARCHAR(32),auth_code VARCHAR(36),equ_id VARCHAR(255),status VARCHAR(10),expire_time DATE,last_time DATE,create_time DATE,req_ip VARCHAR(64),req_place VARCHAR(255))",
	} {
		if _, err := client.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if _, err := client.Exec("INSERT INTO auth_app VALUES (?,?,?,?,?)", "app-1", "Desktop", nil, "public", "private"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exec("INSERT INTO auth_code VALUES (?,?,?,?,?,?,?,?,?,?,?)", "code-1", "app-1", "AUTH-001", int64(3), nil, time.Date(2099, 12, 31, 0, 0, 0, 0, time.Local), time.Date(2026, 7, 27, 0, 0, 0, 0, time.Local), "10", "127.0.0.1", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, equID := range []string{"equ-1", "equ-2"} {
		if _, err := client.Exec("INSERT INTO auth_code_equ(id,code_id,equ_id,status) VALUES (?,?,?,?)", equID, "code-1", equID, "10"); err != nil {
			t.Fatal(err)
		}
	}

	query := `SELECT c.id,COALESCE(c.app_id,''),COALESCE(a.app_name,''),COALESCE(c.auth_code,''),
		COALESCE(c.status,''),COALESCE(c.req_place,''),COALESCE(e.used_count,0)
		FROM auth_code c
		LEFT JOIN auth_app a ON a.id=c.app_id
		LEFT JOIN (SELECT code_id,COUNT(*) AS used_count FROM auth_code_equ GROUP BY code_id) e ON e.code_id=c.id
		WHERE c.status=? AND c.auth_code LIKE ? ORDER BY c.create_time DESC LIMIT ? OFFSET ?`
	rows, err := client.Query(query, "10", "%AUTH%", int64(10), int64(0))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("expected an application query row: %v", rows.Err())
	}
	var id, appID, appName, authCode, status, place string
	var used int64
	if err := rows.Scan(&id, &appID, &appName, &authCode, &status, &place, &used); err != nil {
		t.Fatal(err)
	}
	if id != "code-1" || appID != "app-1" || appName != "Desktop" || authCode != "AUTH-001" || status != "10" || place != "" || used != 2 {
		t.Fatalf("unexpected application row: %q %q %q %q %q %q %d", id, appID, appName, authCode, status, place, used)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	tx, err := client.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var lockedID string
	if err := tx.QueryRow("SELECT id FROM auth_code WHERE auth_code=? LIMIT 1 FOR UPDATE", "AUTH-001").Scan(&lockedID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if lockedID != "code-1" {
		t.Fatalf("locked id = %q", lockedID)
	}
}
