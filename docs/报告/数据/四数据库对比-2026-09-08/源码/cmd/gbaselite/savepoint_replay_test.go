package main

import (
	"gbaselite/executor"
	"gbaselite/journal"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSavepointDurableRecoveryAndBinlogReplay(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	logPath := filepath.Join(dir, "binlog.jsonl")
	e, err := executor.Open(source, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	binlog, err := journal.OpenBinlog(logPath, journal.DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	e.SetBinlog(binlog)
	session := &executor.Session{}
	for _, q := range []string{
		"CREATE DATABASE sp", "USE sp", "CREATE TABLE items(id INT AUTO_INCREMENT PRIMARY KEY,value INT UNIQUE)",
		"INSERT INTO items(value) VALUES(10)", "BEGIN", "SAVEPOINT a",
		"INSERT INTO items(value) VALUES(20)", "SAVEPOINT b", "INSERT INTO items(value) VALUES(30)",
		"ROLLBACK TO a", "INSERT INTO items(value) VALUES(40)", "SAVEPOINT again",
		"INSERT INTO items(value) VALUES(50)", "ROLLBACK TO again", "COMMIT",
		"BEGIN", "CREATE TABLE created(id INT AUTO_INCREMENT PRIMARY KEY,value INT)", "SAVEPOINT inner_point",
		"INSERT INTO created(value) VALUES(111)", "ROLLBACK TO inner_point", "INSERT INTO created(value) VALUES(222)", "COMMIT",
	} {
		if _, err := e.Execute(session, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := binlog.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"VALUES(20)", "VALUES(30)", "VALUES(50)", "VALUES(111)"} {
		if strings.Contains(string(content), removed) {
			t.Fatalf("discarded SQL retained: %s", removed)
		}
	}
	if !strings.Contains(string(content), `"version":1`) || !strings.Contains(string(content), `"version":2`) {
		t.Fatal("expected mixed legacy and counter records")
	}
	recovered := filepath.Join(dir, "replayed")
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  path: '"+recovered+"'\nauth:\n  username: root\n  password: secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runReplayBinlog([]string{"--config", cfg, "--input", logPath}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{source, recovered} {
		engine, err := executor.Open(path, "root", "secret")
		if err != nil {
			t.Fatal(err)
		}
		s := &executor.Session{CurrentDatabase: "sp"}
		for _, q := range []string{"INSERT INTO items(value) VALUES(60)", "INSERT INTO created(value) VALUES(333)"} {
			if _, err := engine.Execute(s, q); err != nil {
				t.Fatal(err)
			}
		}
		for q, want := range map[string][][]any{
			"SELECT id,value FROM items ORDER BY id":   {{int64(1), int64(10)}, {int64(4), int64(40)}, {int64(6), int64(60)}},
			"SELECT id,value FROM created ORDER BY id": {{int64(2), int64(222)}, {int64(3), int64(333)}},
		} {
			got, err := engine.Execute(s, q)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Rows, want) {
				t.Fatalf("%s: %v != %v", path, got.Rows, want)
			}
		}
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
