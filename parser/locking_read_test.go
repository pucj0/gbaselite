package parser

import (
	"strings"
	"testing"
)

// locking_read_test.go is T030: the explicit lock mode and the options this release refuses.
//
// The policy that matters here is "no silent loss". A boolean plus a swallowed NOWAIT/SKIP LOCKED
// would let a caller believe it had a non-blocking or skip-on-conflict read while it actually got a
// blocking plain read, which is worse than refusing the statement.

func TestB02LockingReadModesAreDistinct(t *testing.T) {
	cases := []struct {
		sql       string
		wantMode  LockMode
		wantAlias string
	}{
		{sql: "SELECT v FROM t WHERE id = 1 FOR UPDATE", wantMode: LockModeUpdate},
		{sql: "SELECT v FROM t WHERE id = 1 FOR SHARE", wantMode: LockModeShare},
		{sql: "SELECT v FROM t WHERE id = 1 LOCK IN SHARE MODE", wantMode: LockModeShare, wantAlias: "LOCK IN SHARE MODE"},
		{sql: "SELECT v FROM t WHERE id = 1 for update", wantMode: LockModeUpdate},
		{sql: "SELECT v FROM t WHERE id = 1 FOR\n UPDATE", wantMode: LockModeUpdate},
	}
	for _, testCase := range cases {
		t.Run(testCase.sql, func(t *testing.T) {
			statement, err := Parse(testCase.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			query, ok := statement.(Select)
			if !ok {
				t.Fatalf("statement type = %T, want Select", statement)
			}
			if !query.Locking {
				t.Fatal("Locking = false for a locking read")
			}
			if query.LockMode != testCase.wantMode {
				t.Fatalf("LockMode = %q, want %q", query.LockMode, testCase.wantMode)
			}
			if query.LockingAlias != testCase.wantAlias {
				t.Fatalf("LockingAlias = %q, want %q", query.LockingAlias, testCase.wantAlias)
			}
			// The two modes must be distinguishable, which is the whole reason LockMode is not a
			// boolean: a shared read that reported UPDATE would take an exclusive lock.
			if query.LockMode != LockModeUpdate && query.LockMode != LockModeShare {
				t.Fatalf("LockMode %q is neither mode", query.LockMode)
			}
		})
	}
}

func TestB02PlainSelectIsNotLocking(t *testing.T) {
	for _, sql := range []string{
		"SELECT v FROM t WHERE id = 1",
		"SELECT v FROM t",
		"SELECT v FROM t ORDER BY v LIMIT 1",
	} {
		statement, err := Parse(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		query := statement.(Select)
		if query.Locking {
			t.Fatalf("%s: a plain SELECT reported Locking", sql)
		}
		if query.LockMode != "" {
			t.Fatalf("%s: a plain SELECT reported lock mode %q", sql, query.LockMode)
		}
	}
}

func TestB02UnsupportedLockingOptionsAreRefused(t *testing.T) {
	// Every one of these must fail at parse time. Accepting them and then blocking, or accepting
	// them and then skipping, would silently change what the caller asked for.
	refused := []struct {
		sql  string
		want string
	}{
		{sql: "SELECT v FROM t WHERE id = 1 FOR UPDATE NOWAIT", want: "NOWAIT"},
		{sql: "SELECT v FROM t WHERE id = 1 FOR UPDATE SKIP LOCKED", want: "SKIP LOCKED"},
		{sql: "SELECT v FROM t WHERE id = 1 FOR SHARE NOWAIT", want: "NOWAIT"},
		{sql: "SELECT v FROM t WHERE id = 1 FOR SHARE SKIP LOCKED", want: "SKIP LOCKED"},
		{sql: "SELECT v FROM t WHERE id = 1 LOCK IN SHARE MODE NOWAIT", want: "NOWAIT"},
		{sql: "SELECT v FROM t WHERE id = 1 FOR NO KEY UPDATE", want: "unsupported locking clause"},
		{sql: "SELECT v FROM t WHERE id = 1 FOR KEY SHARE", want: "unsupported locking clause"},
	}
	for _, testCase := range refused {
		t.Run(testCase.sql, func(t *testing.T) {
			_, err := Parse(testCase.sql)
			if err == nil {
				t.Fatalf("%s parsed successfully; the option must be refused", testCase.sql)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not name %q", err.Error(), testCase.want)
			}
		})
	}
}

func TestB02LockModeValidRejectsUnknownValues(t *testing.T) {
	if !LockModeUpdate.Valid() || !LockModeShare.Valid() {
		t.Fatal("a produced mode reported itself invalid")
	}
	for _, mode := range []LockMode{"", "UPDATE ", "update", "GAP"} {
		if mode.Valid() {
			t.Fatalf("LockMode(%q).Valid() = true", mode)
		}
	}
}
