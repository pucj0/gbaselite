package server

import (
	"testing"

	"gbaselite/executor"
)

func TestCaseInsensitiveCollationDoesNotUseExactTextIndexLookup(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	for _, query := range []string{
		"CREATE DATABASE collation_index",
		"USE collation_index",
		"CREATE TABLE labels(id INT,label VARCHAR(20),UNIQUE KEY uq_label(label))",
		"INSERT INTO labels VALUES (1,'Alpha')",
	} {
		if _, err := ExecuteCompatible(engine, session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := ExecuteCompatible(engine, session, "SELECT id FROM labels WHERE label='alpha'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != int64(1) {
		t.Fatalf("case-insensitive indexed lookup = %#v", result.Rows)
	}
	if _, err := ExecuteCompatible(engine, session, "SET collation_connection=utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	result, err = ExecuteCompatible(engine, session, "SELECT id FROM labels WHERE label='alpha'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 0 {
		t.Fatalf("binary indexed lookup = %#v", result.Rows)
	}
}
