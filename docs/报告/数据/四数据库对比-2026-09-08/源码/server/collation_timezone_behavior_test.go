package server

import (
	"testing"
	"time"

	"gbaselite/executor"
)

func TestSessionCollationControlsOrderingDistinctAndGrouping(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	for _, query := range []string{
		"CREATE DATABASE collation_sets",
		"USE collation_sets",
		"CREATE TABLE labels(label VARCHAR(20))",
		"INSERT INTO labels VALUES ('b'),('A'),('a')",
	} {
		if _, err := ExecuteCompatible(engine, session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	distinctCI, err := ExecuteCompatible(engine, session, "SELECT DISTINCT label FROM labels ORDER BY label")
	if err != nil {
		t.Fatal(err)
	}
	if len(distinctCI.Rows) != 2 || distinctCI.Rows[0][0] != "A" || distinctCI.Rows[1][0] != "b" {
		t.Fatalf("case-insensitive DISTINCT/ORDER BY = %#v", distinctCI.Rows)
	}
	groupedCI, err := ExecuteCompatible(engine, session, "SELECT label,COUNT(*) AS total FROM labels GROUP BY label ORDER BY label")
	if err != nil {
		t.Fatal(err)
	}
	if len(groupedCI.Rows) != 2 || groupedCI.Rows[0][1] != int64(2) {
		t.Fatalf("case-insensitive GROUP BY = %#v", groupedCI.Rows)
	}

	if _, err := ExecuteCompatible(engine, session, "SET collation_connection=utf8mb4_bin"); err != nil {
		t.Fatal(err)
	}
	distinctBinary, err := ExecuteCompatible(engine, session, "SELECT DISTINCT label FROM labels ORDER BY label")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "a", "b"}
	if len(distinctBinary.Rows) != len(want) {
		t.Fatalf("binary DISTINCT/ORDER BY = %#v", distinctBinary.Rows)
	}
	for index, value := range want {
		if distinctBinary.Rows[index][0] != value {
			t.Fatalf("binary row %d = %#v, want %q", index, distinctBinary.Rows[index], value)
		}
	}
}

func TestSessionTimeZoneControlsTimestampDefaults(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	for _, query := range []string{
		"CREATE DATABASE timezone_defaults",
		"USE timezone_defaults",
		"CREATE TABLE events(id INT,created DATETIME DEFAULT CURRENT_TIMESTAMP)",
		"SET time_zone='+00:00'",
		"INSERT INTO events SET id=1",
		"SET time_zone='+08:00'",
		"INSERT INTO events SET id=2",
	} {
		if _, err := ExecuteCompatible(engine, session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	result, err := ExecuteCompatible(engine, session, "SELECT created FROM events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("timestamp rows = %#v", result.Rows)
	}
	utc := result.Rows[0][0].(time.Time)
	shanghai := result.Rows[1][0].(time.Time)
	difference := shanghai.Sub(utc)
	if difference < 8*time.Hour-time.Second || difference > 8*time.Hour+time.Second {
		t.Fatalf("default timestamp difference = %s, want 8h", difference)
	}
}
