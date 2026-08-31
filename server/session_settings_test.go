package server

import (
	"testing"
	"time"

	"gbaselite/executor"
)

func TestSessionCharacterSetsAndCollations(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}

	charsets, err := ExecuteCompatible(engine, session, "SHOW CHARACTER SET LIKE 'utf%'")
	if err != nil {
		t.Fatal(err)
	}
	if len(charsets.Rows) != 2 || charsets.Rows[0][0] != "utf8" || charsets.Rows[1][0] != "utf8mb4" {
		t.Fatalf("unexpected UTF character sets: %#v", charsets.Rows)
	}
	collations, err := ExecuteCompatible(engine, session, "SHOW COLLATION LIKE 'latin1%'")
	if err != nil {
		t.Fatal(err)
	}
	if len(collations.Rows) != 3 {
		t.Fatalf("unexpected latin1 collations: %#v", collations.Rows)
	}

	if _, err := ExecuteCompatible(engine, session, "SET NAMES latin1 COLLATE latin1_bin"); err != nil {
		t.Fatal(err)
	}
	variables, err := ExecuteCompatible(engine, session, "SELECT @@character_set_client, @@session.collation_connection")
	if err != nil {
		t.Fatal(err)
	}
	if variables.Rows[0][0] != "latin1" || variables.Rows[0][1] != "latin1_bin" {
		t.Fatalf("unexpected session character settings: %#v", variables.Rows)
	}
	if _, err := ExecuteCompatible(engine, session, "SET NAMES utf8mb4 COLLATE latin1_bin"); err == nil {
		t.Fatal("expected mismatched character set and collation to fail")
	}
}

func TestSessionCharacterSetRestoresDumpUserVariable(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	for _, query := range []string{
		"SET @saved_cs_client=@@character_set_client",
		"SET character_set_client=latin1",
		"SET character_set_client=@saved_cs_client",
		"SET character_set_client=NONE",
		"SET character_set_connection=NONE",
		"SET character_set_results=NONE",
		"SET collation_connection=NO",
	} {
		if _, err := ExecuteCompatible(engine, session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if session.CharacterSetClient != executor.DefaultCharacterSet {
		t.Fatalf("character_set_client = %q, want %q", session.CharacterSetClient, executor.DefaultCharacterSet)
	}

	other := &executor.Session{}
	if _, err := ExecuteCompatible(engine, other, "SET character_set_client=@saved_cs_client"); err == nil {
		t.Fatal("expected saved user variables to remain connection-local")
	}
	if other.CharacterSetClient != executor.DefaultCharacterSet {
		t.Fatalf("failed restore changed other session charset to %q", other.CharacterSetClient)
	}
}

func TestSessionCollationControlsStringPredicates(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	for _, query := range []string{
		"CREATE DATABASE collation_test",
		"USE collation_test",
		"CREATE TABLE labels(id INT,label VARCHAR(20))",
		"INSERT INTO labels VALUES (1,'Alpha'),(2,'alpha')",
	} {
		if _, err := ExecuteCompatible(engine, session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	caseInsensitive, err := ExecuteCompatible(engine, session, "SELECT id FROM labels WHERE label='alpha' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(caseInsensitive.Rows) != 2 {
		t.Fatalf("case-insensitive rows = %#v", caseInsensitive.Rows)
	}
	if _, err := ExecuteCompatible(engine, session, "SET collation_connection='utf8mb4_bin'"); err != nil {
		t.Fatal(err)
	}
	binary, err := ExecuteCompatible(engine, session, "SELECT id FROM labels WHERE label='alpha' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(binary.Rows) != 1 || binary.Rows[0][0] != int64(2) {
		t.Fatalf("binary collation rows = %#v", binary.Rows)
	}
	like, err := ExecuteCompatible(engine, session, "SELECT id FROM labels WHERE label LIKE 'a%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(like.Rows) != 1 || like.Rows[0][0] != int64(2) {
		t.Fatalf("binary LIKE rows = %#v", like.Rows)
	}
}

func TestSessionTimeZoneControlsCurrentTimestamp(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	readNow := func(zone string) time.Time {
		t.Helper()
		if _, err := ExecuteCompatible(engine, session, "SET SESSION time_zone='"+zone+"'"); err != nil {
			t.Fatal(err)
		}
		result, err := ExecuteCompatible(engine, session, "SELECT NOW()")
		if err != nil {
			t.Fatal(err)
		}
		value, err := time.Parse("2006-01-02 15:04:05", result.Rows[0][0].(string))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	utc := readNow("+00:00")
	shanghai := readNow("+08:00")
	difference := shanghai.Sub(utc)
	if difference < 8*time.Hour-time.Second || difference > 8*time.Hour+time.Second {
		t.Fatalf("time zone difference = %s, want 8h", difference)
	}
	variables, err := ExecuteCompatible(engine, session, "SHOW VARIABLES LIKE 'time_zone'")
	if err != nil {
		t.Fatal(err)
	}
	if len(variables.Rows) != 1 || variables.Rows[0][1] != "+08:00" {
		t.Fatalf("unexpected time_zone variable: %#v", variables.Rows)
	}
	if _, err := ExecuteCompatible(engine, session, "SET time_zone='+25:00'"); err == nil {
		t.Fatal("expected invalid time zone to fail")
	}
	if session.TimeZone != "+08:00" {
		t.Fatalf("invalid SET changed time zone to %q", session.TimeZone)
	}
}
