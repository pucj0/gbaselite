package server

import (
	"testing"

	"gbaselite/executor"
)

func TestSessionCollationControlsHashEligibleJoin(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	for _, query := range []string{
		"CREATE DATABASE collation_join",
		"USE collation_join",
		"CREATE TABLE left_labels(id INT,label VARCHAR(20))",
		"CREATE TABLE right_labels(id INT,label VARCHAR(20))",
		"INSERT INTO left_labels VALUES (1,'Alpha')",
		"INSERT INTO right_labels VALUES (2,'alpha')",
	} {
		if _, err := ExecuteCompatible(engine, session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := ExecuteCompatible(engine, session, "SELECT l.id,r.id FROM left_labels l JOIN right_labels r ON l.label=r.label")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("case-insensitive JOIN = %#v", result.Rows)
	}
	if _, err := ExecuteCompatible(engine, session, "SET collation_connection=utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	result, err = ExecuteCompatible(engine, session, "SELECT l.id,r.id FROM left_labels l JOIN right_labels r ON l.label=r.label")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 0 {
		t.Fatalf("binary JOIN = %#v", result.Rows)
	}
}
