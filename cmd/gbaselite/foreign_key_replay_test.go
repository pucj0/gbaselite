package main

import (
	"gbaselite/executor"
	"gbaselite/journal"
	"os"
	"path/filepath"
	"testing"
)

func TestReplayForeignKeySessionState(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "binlog.jsonl")
	b, err := journal.OpenBinlog(logPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []journal.BinlogStatement{
		{SQL: "CREATE DATABASE tew"},
		{Database: "tew", SQL: "CREATE TABLE child(id INT PRIMARY KEY,pid INT,FOREIGN KEY(pid) REFERENCES parent(id))", ForeignKeyChecksDisabled: true},
		{Database: "tew", SQL: "INSERT INTO child VALUES(1,42)", ForeignKeyChecksDisabled: true},
		{Database: "tew", SQL: "CREATE TABLE parent(id INT PRIMARY KEY)"},
		{Database: "tew", SQL: "INSERT INTO parent VALUES(42)"},
	} {
		if err := b.Append(journal.BinlogRecord{Statements: []journal.BinlogStatement{s}}); err != nil {
			t.Fatal(err)
		}
	}
	b.Close()
	cfg := filepath.Join(dir, "config.yaml")
	data := filepath.Join(dir, "data")
	if err := os.WriteFile(cfg, []byte("storage:\n  path: '"+data+"'\nauth:\n  username: root\n  password: secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runReplayBinlog([]string{"--config", cfg, "--input", logPath}); err != nil {
		t.Fatal(err)
	}
	e, err := executor.Open(data, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &executor.Session{CurrentDatabase: "tew"}
	if _, err := e.Execute(s, "INSERT INTO child VALUES(2,99)"); err == nil {
		t.Fatal("checks lost after replay")
	}
}
