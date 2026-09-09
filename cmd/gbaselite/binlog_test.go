package main

import (
	"os"
	"path/filepath"
	"testing"

	"gbaselite/journal"
)

func TestReplayBinlogCommand(t *testing.T) {
	directory := t.TempDir()
	dataPath := filepath.Join(directory, "data")
	binlogPath := filepath.Join(directory, "binlog.jsonl")
	configPath := filepath.Join(directory, "config.yaml")
	configContents := "storage:\n  path: '" + dataPath + "'\nauth:\n  username: root\n  password: secret\n"
	if err := os.WriteFile(configPath, []byte(configContents), 0o600); err != nil {
		t.Fatal(err)
	}
	binlog, err := journal.OpenBinlog(binlogPath, journal.DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []journal.BinlogRecord{
		{Statements: []journal.BinlogStatement{{SQL: "CREATE DATABASE recovered"}}},
		{Statements: []journal.BinlogStatement{
			{Database: "recovered", SQL: "CREATE TABLE items(id INT,label VARCHAR(32))"},
			{Database: "recovered", SQL: "INSERT INTO items VALUES (1,'restored')"},
		}},
	} {
		if err := binlog.Append(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := binlog.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runReplayBinlog([]string{"--config", configPath, "--input", binlogPath}); err == nil {
		t.Fatal("legacy runtime replay accepted")
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatal("rejected replay created data")
	}

}

func TestReplayBinlogCheckOnlyDoesNotOpenData(t *testing.T) {
	directory := t.TempDir()
	binlogPath := filepath.Join(directory, "binlog.jsonl")
	binlog, err := journal.OpenBinlog(binlogPath, journal.DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	if err := binlog.Append(journal.BinlogRecord{Statements: []journal.BinlogStatement{{SQL: "CREATE DATABASE must_not_run"}}}); err != nil {
		t.Fatal(err)
	}
	if err := binlog.Close(); err != nil {
		t.Fatal(err)
	}

	missingConfig := filepath.Join(directory, "missing-config.yaml")
	if err := runReplayBinlog([]string{"--config", missingConfig, "--input", binlogPath, "--check-only"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(directory, "data"),
		filepath.Join(directory, "databases"),
		filepath.Join(directory, "users"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("check-only unexpectedly created %s: %v", path, err)
		}
	}
}
