package executor

import (
	"path/filepath"
	"testing"

	"gbaselite/journal"
)

func TestBinlogRecordsCommittedChangesOnly(t *testing.T) {
	binlogPath := filepath.Join(t.TempDir(), "binlog.jsonl")
	binlog, err := journal.OpenBinlog(binlogPath, journal.DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := Open(t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	engine.SetBinlog(binlog)
	session := &Session{Username: "root", Host: "%", RemoteIP: "127.0.0.1", ConnectionID: 9}
	for _, query := range []string{
		"CREATE DATABASE app",
		"BEGIN",
		"CREATE TABLE app.rolled_back(id INT)",
		"ROLLBACK",
		"BEGIN",
		"CREATE TABLE app.items(id INT)",
		"INSERT INTO app.items VALUES (1)",
		"COMMIT",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if _, err := engine.Execute(session, "INSERT INTO missing VALUES (1)"); err == nil {
		t.Fatal("expected failed statement")
	}
	if err := binlog.Close(); err != nil {
		t.Fatal(err)
	}
	var records []journal.BinlogRecord
	count, last, err := journal.ReadBinlog(binlogPath, journal.ReplayOptions{}, func(record journal.BinlogRecord) error {
		records = append(records, record)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || last != 2 || len(records) != 2 {
		t.Fatalf("count=%d last=%d records=%#v", count, last, records)
	}
	if len(records[0].Statements) != 1 || records[0].Statements[0].SQL != "CREATE DATABASE app" {
		t.Fatalf("autocommit record = %#v", records[0])
	}
	if len(records[1].Statements) != 2 || records[1].ConnectionID != 9 || records[1].RemoteIP != "127.0.0.1" {
		t.Fatalf("transaction record = %#v", records[1])
	}
	for _, record := range records {
		for _, statement := range record.Statements {
			if statement.SQL == "CREATE TABLE app.rolled_back(id INT)" {
				t.Fatal("rolled back statement was written to binlog")
			}
		}
	}
}

func TestDisconnectDropsPendingBinlogStatements(t *testing.T) {
	binlogPath := filepath.Join(t.TempDir(), "binlog.jsonl")
	binlog, err := journal.OpenBinlog(binlogPath, journal.DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := Open(t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	engine.SetBinlog(binlog)
	session := &Session{}
	for _, query := range []string{"BEGIN", "CREATE DATABASE disconnected"} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	engine.CloseSession(session)
	if binlog.Sequence() != 0 {
		t.Fatalf("sequence = %d", binlog.Sequence())
	}
	if err := binlog.Close(); err != nil {
		t.Fatal(err)
	}
}
