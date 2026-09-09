package server

import (
	"testing"
	"time"

	"gbaselite/executor"
)

func TestSessionTimeZoneControlsFractionalNowAndCurrentDate(t *testing.T) {
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{}
	readNow := func(zone string) time.Time {
		t.Helper()
		if _, err := ExecuteCompatible(engine, session, "SET time_zone='"+zone+"'"); err != nil {
			t.Fatal(err)
		}
		result, err := ExecuteCompatible(engine, session, "SELECT NOW(3)")
		if err != nil {
			t.Fatal(err)
		}
		value, err := time.Parse("2006-01-02 15:04:05.000", result.Rows[0][0].(string))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	utc := readNow("+00:00")
	shanghai := readNow("+08:00")
	if difference := shanghai.Sub(utc); difference < 8*time.Hour-time.Second || difference > 8*time.Hour+time.Second {
		t.Fatalf("NOW(3) time zone difference = %s, want 8h", difference)
	}

	readDate := func(zone string) time.Time {
		t.Helper()
		if _, err := ExecuteCompatible(engine, session, "SET time_zone='"+zone+"'"); err != nil {
			t.Fatal(err)
		}
		result, err := ExecuteCompatible(engine, session, "SELECT CURDATE()")
		if err != nil {
			t.Fatal(err)
		}
		return result.Rows[0][0].(time.Time)
	}
	west := readDate("-13:59")
	east := readDate("+14:00")
	if !east.After(west) {
		t.Fatalf("CURDATE extremes = west %s east %s", west, east)
	}
}
