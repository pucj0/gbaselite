package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gbaselite/internal/mysqlcompat"
)

const (
	DefaultCharacterSet = "utf8mb4"
	DefaultCollation    = "utf8mb4_general_ci"
	DefaultTimeZone     = "SYSTEM"
)

// DefaultLockWaitTimeout is the lock-wait budget every session starts with.
//
// It is declared here, next to the other session defaults, rather than read from the
// lock manager: the executor must not depend on that package. The contract fixes the
// session default and the manager's own fallback at the same 50 seconds, so the two
// constants agree by specification, and task T035 pins that agreement with a test.
const DefaultLockWaitTimeout = 50 * time.Second

// Session lock-wait budget bounds, in whole seconds.
//
// The low bound excludes 0 because a zero-second budget could never wait for a lock
// at all, and the high bound keeps the value inside the range MySQL documents for
// innodb_lock_wait_timeout. The bounds are exported so a future protocol layer can
// render the same range without re-deriving it.
const (
	MinLockWaitTimeoutSeconds int64 = 1
	MaxLockWaitTimeoutSeconds int64 = 86400
)

// innodbLockWaitVariable is the session variable the helpers below own.
const innodbLockWaitVariable = "innodb_lock_wait_timeout"

// LockWaitTimeout reports the session's lock-wait budget.
//
// A session that never set the variable, and a session whose value was cleared by
// ResetConnection, reports the contract default. Callers therefore never have to
// distinguish "unset" from "50 seconds": the value they read is the value the lock
// path will use.
func (s *Session) LockWaitTimeout() time.Duration {
	if s == nil || s.lockWaitTimeout <= 0 {
		return DefaultLockWaitTimeout
	}
	return s.lockWaitTimeout
}

// SetLockWaitTimeout validates and stores a whole-second lock-wait budget.
//
// It is the single validation point for `SET SESSION innodb_lock_wait_timeout`: the
// SET path and any future caller share one rule, so a value that is refused on one
// path cannot be accepted on another.
func (s *Session) SetLockWaitTimeout(seconds int64) error {
	if seconds < MinLockWaitTimeoutSeconds || seconds > MaxLockWaitTimeoutSeconds {
		return fmt.Errorf("%s requires an integer between %d and %d seconds, got %d",
			innodbLockWaitVariable, MinLockWaitTimeoutSeconds, MaxLockWaitTimeoutSeconds, seconds)
	}
	if s == nil {
		return fmt.Errorf("%s requires a session", innodbLockWaitVariable)
	}
	s.lockWaitTimeout = time.Duration(seconds) * time.Second
	return nil
}

// lockWaitSessionContextKey carries the Session itself into the statement context.
//
// The lock-acquisition path (executor/dml_locking.go) opens a lock scope from a
// storageengine.Txn, so it has no Session, and the neutral locking API accepts no
// deadline: the only wait bound that can travel down to the lock manager is the
// context's. Attaching the live Session — instead of a copy of one number — is what
// keeps that bound correct: the lock path always reads the session's *current* value,
// so a session that is reset or reused cannot leave a stale budget behind.
type lockWaitSessionContextKey struct{}

// attachLockWaitSession makes this session's lock-wait budget reachable from the
// statement context.
//
// It is idempotent: an already attached session is left alone, so the context chain
// does not grow once per statement. A nil Context is replaced by a Background-based
// one, which is exactly what the statement path already substitutes for nil.
func (s *Session) attachLockWaitSession() {
	if s == nil {
		return
	}
	if s.Context != nil {
		if current, ok := s.Context.Value(lockWaitSessionContextKey{}).(*Session); ok && current == s {
			return
		}
	}
	parent := s.Context
	if parent == nil {
		parent = context.Background()
	}
	s.Context = context.WithValue(parent, lockWaitSessionContextKey{}, s)
}

// lockWaitTimeoutForContext reports the lock-wait budget of the session attached to
// ctx, or the contract default when no session is attached (a hand-built context in a
// storage-level test, for example). It never returns a non-positive budget.
func lockWaitTimeoutForContext(ctx context.Context) time.Duration {
	if ctx != nil {
		if session, ok := ctx.Value(lockWaitSessionContextKey{}).(*Session); ok && session != nil {
			return session.LockWaitTimeout()
		}
	}
	return DefaultLockWaitTimeout
}

func (s *Session) InitializeSettings() {
	if s == nil {
		return
	}
	if s.CharacterSetClient == "" {
		s.CharacterSetClient = DefaultCharacterSet
	}
	if s.CharacterSetConnection == "" {
		s.CharacterSetConnection = DefaultCharacterSet
	}
	if s.CharacterSetResults == "" {
		s.CharacterSetResults = DefaultCharacterSet
	}
	if s.CollationConnection == "" {
		s.CollationConnection = DefaultCollation
	}
	if s.TimeZone == "" {
		s.TimeZone = DefaultTimeZone
	}
	if s.ServerTimeZone == "" {
		s.ServerTimeZone = DefaultTimeZone
	}
	if s.UserVariables == nil {
		s.UserVariables = make(map[string]string)
	}
	// Make the lock-wait budget readable from the statement context the lock paths
	// receive. Doing it here — on the one path every statement already runs through —
	// is what lets dml_locking.go honour the setting without a Session parameter.
	s.attachLockWaitSession()
}

func (s *Session) SetNames(charsetName, collationName string) error {
	charset, err := mysqlcompat.ResolveCharset(charsetName)
	if err != nil {
		return err
	}
	if collationName == "" {
		collationName = charset.DefaultCollation
	}
	collation, err := mysqlcompat.ResolveCollation(collationName)
	if err != nil {
		return err
	}
	if collation.Charset != charset.Name {
		return fmt.Errorf("collation %q is not valid for character set %q", collation.Name, charset.Name)
	}
	s.CharacterSetClient = charset.Name
	s.CharacterSetConnection = charset.Name
	s.CharacterSetResults = charset.Name
	s.CollationConnection = collation.Name
	return nil
}

func (s *Session) SetCharacterSet(variable, value string) error {
	s.InitializeSettings()
	variable = strings.ToLower(strings.TrimSpace(variable))
	if variable == "collation_connection" {
		collation, err := mysqlcompat.ResolveCollation(value)
		if err != nil {
			return err
		}
		s.CollationConnection = collation.Name
		s.CharacterSetConnection = collation.Charset
		return nil
	}
	charset, err := mysqlcompat.ResolveCharset(value)
	if err != nil {
		return err
	}
	switch variable {
	case "character_set_client":
		s.CharacterSetClient = charset.Name
	case "character_set_connection":
		s.CharacterSetConnection = charset.Name
		s.CollationConnection = charset.DefaultCollation
	case "character_set_results":
		s.CharacterSetResults = charset.Name
	default:
		return fmt.Errorf("unsupported character set variable %q", variable)
	}
	return nil
}

func (s *Session) SetTimeZone(value string) error {
	name, location, err := mysqlcompat.ParseTimeZone(value)
	if err != nil {
		return err
	}
	s.TimeZone = name
	s.timeLocation = location
	return nil
}

func (s *Session) Location() *time.Location {
	if s == nil {
		return time.Local
	}
	s.InitializeSettings()
	if s.timeLocation == nil {
		_, location, err := mysqlcompat.ParseTimeZone(s.TimeZone)
		if err == nil {
			s.timeLocation = location
		}
	}
	if s.timeLocation == nil {
		return time.Local
	}
	return s.timeLocation
}

func (s *Session) Compare(left, right any) int {
	if result, ok := compareCollated(left, right); ok {
		return result
	}
	if leftText, ok := left.(string); ok {
		if rightText, rightOK := right.(string); rightOK {
			s.InitializeSettings()
			return mysqlcompat.CompareStrings(leftText, rightText, s.CollationConnection)
		}
	}
	return compareAny(left, right)
}

func (s *Session) Now() time.Time {
	return time.Now().In(s.Location())
}

// ResetConnection preserves authentication and connection identity, but discards
// transactional state and all per-borrower settings.
func (e *Engine) ResetConnection(s *Session) {
	e.CloseSession(s)
	*s = Session{Context: s.Context, CurrentDatabase: s.CurrentDatabase, StreamResults: s.StreamResults, Username: s.Username, Host: s.Host, RemoteIP: s.RemoteIP, RemotePort: s.RemotePort, ConnectionID: s.ConnectionID, SecureTransport: s.SecureTransport, TLSVersion: s.TLSVersion, TLSCipher: s.TLSCipher, JournalSessionID: s.JournalSessionID, ServerTimeZone: s.ServerTimeZone}
	s.InitializeSettings()
	if s.ServerTimeZone != "" {
		_ = s.SetTimeZone(s.ServerTimeZone)
	}
}

// handleSessionLockWaitTimeoutSet applies
// `SET [SESSION|LOCAL] innodb_lock_wait_timeout = <whole seconds>` and reports
// whether the statement was about this variable.
//
// It returns handled=false for every other statement, so `SET PASSWORD`, `SET NAMES`
// and every ordinary statement keep the parser path they had before. GLOBAL is
// refused instead of silently ignored: the budget is per-session state, so accepting
// a global form without a global store would tell the user their change applied while
// nothing changed.
func handleSessionLockWaitTimeoutSet(session *Session, query string) (*Result, bool, error) {
	global, value, matched := parseSessionLockWaitTimeoutSet(query)
	if !matched {
		return nil, false, nil
	}
	if session == nil {
		return nil, true, fmt.Errorf("%s requires a session", innodbLockWaitVariable)
	}
	if global {
		return nil, true, fmt.Errorf(
			"SET GLOBAL %s is not supported; use SET SESSION %s with an integer between %d and %d seconds",
			innodbLockWaitVariable, innodbLockWaitVariable, MinLockWaitTimeoutSeconds, MaxLockWaitTimeoutSeconds)
	}
	if strings.Contains(value, ",") {
		return nil, true, fmt.Errorf("%s requires a single whole-second integer between %d and %d",
			innodbLockWaitVariable, MinLockWaitTimeoutSeconds, MaxLockWaitTimeoutSeconds)
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		// A decimal, a negative sign, a word or an empty value all land here. The
		// message names the variable and the legal range so the user can act on it.
		return nil, true, fmt.Errorf("%s requires an integer between %d and %d seconds, got %q",
			innodbLockWaitVariable, MinLockWaitTimeoutSeconds, MaxLockWaitTimeoutSeconds, value)
	}
	if err := session.SetLockWaitTimeout(seconds); err != nil {
		return nil, true, err
	}
	session.attachLockWaitSession()
	return &Result{Message: "session lock wait timeout changed"}, true, nil
}

// parseSessionLockWaitTimeoutSet recognises the statement shape and returns the
// scope and the raw right-hand value. matched=false means "this is not a statement
// about innodb_lock_wait_timeout", which is the answer for every other SET form.
func parseSessionLockWaitTimeoutSet(query string) (global bool, value string, matched bool) {
	text := strings.TrimSpace(query)
	if len(text) < len("SET")+1 || !strings.EqualFold(text[:len("SET")], "SET") || !isSettingSpace(text[len("SET")]) {
		// "SETUP" and "SETTINGS" are different words; a bare "SET" has no assignment.
		return false, "", false
	}
	rest := strings.TrimSpace(text[len("SET"):])
	if rest == "" || strings.HasPrefix(strings.ToUpper(rest), "PASSWORD") {
		return false, "", false
	}
	rest, global = trimSettingScope(rest)
	name, tail := cutSettingVariable(rest)
	name = strings.Trim(strings.TrimSpace(name), "`")
	if !strings.EqualFold(name, innodbLockWaitVariable) {
		return false, "", false
	}
	tail = strings.TrimSpace(tail)
	switch {
	case strings.HasPrefix(tail, "="):
		tail = strings.TrimSpace(tail[1:])
	case len(tail) >= 3 && strings.EqualFold(tail[:2], "TO") && isSettingSpace(tail[2]):
		tail = strings.TrimSpace(tail[2:])
	default:
		// The variable was named but the assignment is malformed; returning the empty
		// value makes the caller report the range error instead of falling through to a
		// generic parse error that never mentions the variable.
		return global, "", true
	}
	return global, trimSettingQuotes(tail), true
}

// trimSettingScope removes the optional SESSION/LOCAL/GLOBAL scope prefix, reporting
// whether the statement asked for the global form.
func trimSettingScope(rest string) (string, bool) {
	upper := strings.ToUpper(rest)
	for _, candidate := range []struct {
		prefix string
		global bool
	}{
		{"@@GLOBAL.", true},
		{"@@SESSION.", false},
		{"@@LOCAL.", false},
		{"GLOBAL.", true},
		{"GLOBAL ", true},
		{"SESSION ", false},
		{"LOCAL ", false},
		{"@@", false},
	} {
		if strings.HasPrefix(upper, candidate.prefix) {
			return strings.TrimSpace(rest[len(candidate.prefix):]), candidate.global
		}
	}
	return rest, false
}

// cutSettingVariable splits the variable name from the assignment at the first '='
// or whitespace, so the name never swallows the value.
func cutSettingVariable(rest string) (string, string) {
	for index := 0; index < len(rest); index++ {
		switch rest[index] {
		case '=', ' ', '\t', '\n', '\r':
			return rest[:index], rest[index:]
		}
	}
	return rest, ""
}

// trimSettingQuotes removes one layer of quoting around a setting value.
func trimSettingQuotes(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		first := value[0]
		if (first == '\'' || first == '"' || first == '`') && value[len(value)-1] == first {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// isSettingSpace reports whether a byte separates setting keywords.
func isSettingSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r'
}
