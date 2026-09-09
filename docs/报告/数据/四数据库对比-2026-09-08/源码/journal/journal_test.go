package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedactSQL(t *testing.T) {
	query := "INSERT INTO `audit_log` VALUES (123, 'secret', \"other\", NULL); -- private"
	redacted := RedactSQL(query)
	if strings.Contains(redacted, "secret") || strings.Contains(redacted, "other") || strings.Contains(redacted, "123") {
		t.Fatalf("sensitive literals were not redacted: %s", redacted)
	}
	if !strings.Contains(redacted, "`audit_log`") || !strings.Contains(redacted, "VALUES (?, ?, ?, NULL)") {
		t.Fatalf("SQL shape was not preserved: %s", redacted)
	}
	if Operation("CREATE TABLE users(id INT)") != "CREATE TABLE" || Operation("SELECT 1") != "SELECT" {
		t.Fatal("operation classification failed")
	}
}

func TestAuditJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log, err := OpenAudit(path, DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	event := AuditEvent{ConnectionID: 7, Username: "app", RemoteIP: "127.0.0.1", Operation: "UPDATE", Result: "success", AffectedRows: 2, SQL: "UPDATE t SET secret=?"}
	if err := log.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AuditEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(content))), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ConnectionID != 7 || decoded.AffectedRows != 2 || decoded.Timestamp.IsZero() {
		t.Fatalf("audit event = %#v", decoded)
	}
}

func TestBinlogAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binlog.jsonl")
	log, err := OpenBinlog(path, DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	firstTime := time.Now().UTC().Add(-time.Minute)
	if err := log.Append(BinlogRecord{Timestamp: firstTime, Statements: []BinlogStatement{{Database: "app", SQL: "INSERT INTO t VALUES (1)", AffectedRows: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(BinlogRecord{Statements: []BinlogStatement{{Database: "app", SQL: "UPDATE t SET id=2", AffectedRows: 1}}}); err != nil {
		t.Fatal(err)
	}
	if log.Sequence() != 2 {
		t.Fatalf("sequence = %d", log.Sequence())
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	var sequences []uint64
	count, last, err := ReadBinlog(path, ReplayOptions{AfterSequence: 1}, func(record BinlogRecord) error {
		sequences = append(sequences, record.Sequence)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || last != 2 || len(sequences) != 1 || sequences[0] != 2 {
		t.Fatalf("count=%d last=%d sequences=%v", count, last, sequences)
	}
}

func TestAuditRetentionPrunesExpiredRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	now := time.Now().UTC()
	records := []AuditEvent{
		{Timestamp: now.Add(-48 * time.Hour), Operation: "OLD", Result: "success"},
		{Timestamp: now.Add(-time.Hour), Operation: "RECENT", Result: "success"},
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	log, err := OpenAudit(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), `"operation":"OLD"`) || !strings.Contains(string(content), `"operation":"RECENT"`) {
		t.Fatalf("retained audit log = %s", content)
	}
}

func TestZeroRetentionKeepsAuditRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	content := fmt.Sprintf("{\"timestamp\":%q,\"operation\":\"OLD\",\"result\":\"success\"}\n", time.Now().UTC().Add(-400*24*time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := OpenAudit(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(retained) != content {
		t.Fatalf("permanent audit retention changed the log: %s", retained)
	}
}

func TestBinlogRetentionPreservesSequenceAfterAllRecordsExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binlog.jsonl")
	log, err := OpenBinlog(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(BinlogRecord{Timestamp: time.Now().UTC().Add(-48 * time.Hour), Statements: []BinlogStatement{{SQL: "CREATE DATABASE expired"}}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	log, err = OpenBinlog(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if log.Sequence() != 1 {
		t.Fatalf("sequence after retention = %d", log.Sequence())
	}
	if err := log.Append(BinlogRecord{Statements: []BinlogStatement{{SQL: "CREATE DATABASE retained"}}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	var sequences []uint64
	count, last, err := ReadBinlog(path, ReplayOptions{}, func(record BinlogRecord) error {
		sequences = append(sequences, record.Sequence)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || last != 2 || len(sequences) != 1 || sequences[0] != 2 {
		t.Fatalf("count=%d last=%d sequences=%v", count, last, sequences)
	}
	sequence, err := LastBinlogSequence(path)
	if err != nil || sequence != 2 {
		t.Fatalf("last sequence = %d, %v", sequence, err)
	}
}

func TestRetentionDaysValidation(t *testing.T) {
	for _, days := range []int{-1, 366} {
		if _, err := OpenAudit(filepath.Join(t.TempDir(), "audit.jsonl"), days); err == nil {
			t.Fatalf("audit retention %d was accepted", days)
		}
		if _, err := OpenBinlog(filepath.Join(t.TempDir(), "binlog.jsonl"), days); err == nil {
			t.Fatalf("binlog retention %d was accepted", days)
		}
	}
}
