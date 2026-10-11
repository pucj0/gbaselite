package server

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"gbaselite/executor"
	"gbaselite/storageengine"
)

// session_settings_lock_wait_test.go is the server-side half of T035.
//
// The executor owns the value and its range, but the wire path does not reach the executor: the
// handler intercepts every `SET` statement before it. So the session variable is only really
// supported when this layer accepts it, stores it through the same validating setter, and reports it
// back — and when a lock-wait timeout reaches the client as 1205 rather than as a syntax error.

// TestT035SessionSetAcceptsLockWaitTimeout pins the accepted values and the default.
func TestT035SessionSetAcceptsLockWaitTimeout(t *testing.T) {
	session := &executor.Session{}
	session.InitializeSettings()
	if got := session.LockWaitTimeout(); got != 50*time.Second {
		t.Fatalf("default lock wait = %v, want 50s", got)
	}
	// The default is reported through SHOW/SELECT @@ as well, so a client can read what it will get.
	if value := sessionSettingValue(t, session, "innodb_lock_wait_timeout"); value != "50" {
		t.Fatalf("reported default = %q, want 50", value)
	}

	for _, seconds := range []int64{1, 7, 50, 86400} {
		query := fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", seconds)
		if _, err := executeSessionSet(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got := session.LockWaitTimeout(); got != time.Duration(seconds)*time.Second {
			t.Fatalf("%s stored %v, want %ds", query, got, seconds)
		}
		if value := sessionSettingValue(t, session, "innodb_lock_wait_timeout"); value != fmt.Sprint(seconds) {
			t.Fatalf("%s reported %q, want %d", query, value, seconds)
		}
	}

	// DEFAULT restores the contract default instead of being rejected or stored as a duration.
	if _, err := executeSessionSet(session, "SET SESSION innodb_lock_wait_timeout = DEFAULT"); err != nil {
		t.Fatalf("SET ... DEFAULT: %v", err)
	}
	if got := session.LockWaitTimeout(); got != 50*time.Second {
		t.Fatalf("DEFAULT stored %v, want 50s", got)
	}
}

// TestT035SessionSetRejectsIllegalLockWaitTimeout pins every rejected shape, and that a rejection
// leaves the session's value untouched.
func TestT035SessionSetRejectsIllegalLockWaitTimeout(t *testing.T) {
	rejected := []string{
		"SET SESSION innodb_lock_wait_timeout = 0",
		"SET SESSION innodb_lock_wait_timeout = -1",
		"SET SESSION innodb_lock_wait_timeout = 1.5",
		"SET SESSION innodb_lock_wait_timeout = 86401",
		"SET SESSION innodb_lock_wait_timeout = abc",
		"SET GLOBAL innodb_lock_wait_timeout = 60",
	}
	for _, query := range rejected {
		session := &executor.Session{}
		session.InitializeSettings()
		if _, err := executeSessionSet(session, query); err == nil {
			t.Fatalf("%s was accepted", query)
		}
		if got := session.LockWaitTimeout(); got != 50*time.Second {
			t.Fatalf("%s changed the session value to %v", query, got)
		}
	}

	// A statement that changes nothing is not an error: the assignment list may contain settings this
	// build does not have, which MySQL also ignores.
	session := &executor.Session{}
	session.InitializeSettings()
	if _, err := executeSessionSet(session, "SET SESSION some_unknown_variable = 1"); err != nil {
		t.Fatalf("an unknown session variable must be ignored, got %v", err)
	}
}

// TestT035LockWaitTimeoutMapsTo1205 proves the client-visible error code.
//
// A lock-wait timeout that arrived as 1064 would tell the caller its statement was malformed and
// invite it to rewrite perfectly valid SQL; 1205 is the retryable lock-wait timeout clients expect.
func TestT035LockWaitTimeoutMapsTo1205(t *testing.T) {
	err := fmt.Errorf("%w: session innodb_lock_wait_timeout expired", storageengine.ErrLockWaitTimeout)
	if code := mysqlExecutionErrorCode(err); code != 1205 {
		t.Fatalf("a lock-wait timeout mapped to %d, want 1205", code)
	}
	// The mapping must be reached through the wrapping the executor applies, so it cannot depend on
	// the error being returned bare.
	if !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatal("the wrapped lock-wait timeout does not unwrap to the sentinel")
	}
	// An unrelated error still falls through to the existing mapping (sanity check that 1205 is not
	// the new default).
	if code := mysqlExecutionErrorCode(errors.New("syntax error")); code == 1205 {
		t.Fatal("an unrelated error mapped to the lock-wait timeout code")
	}
}

// sessionSettingValue reads one reported session variable through the same table SHOW/SELECT use.
func sessionSettingValue(t *testing.T, session *executor.Session, name string) string {
	t.Helper()
	for _, row := range sessionVariableValues(session, false) {
		if len(row) == 2 && row[0] == name {
			return fmt.Sprint(row[1])
		}
	}
	t.Fatalf("%s is not reported as a session variable", name)
	return ""
}
