package server

import (
	"errors"
	"fmt"
	"testing"

	"gbaselite/storageengine"
)

// mysql_deadlock_error_code_test.go is the protocol half of the deadlock lifecycle: what the client is
// actually told.
//
// The two codes are the ones a MySQL client is written against. 1213 is "deadlock found, try
// restarting transaction" and 1205 is "lock wait timeout exceeded, try restarting transaction"; a
// client retries on both and treats anything else as a permanent failure. Getting them backwards, or
// letting either fall through to the generic 1064, turns a retryable contention into a syntax error as
// far as the caller is concerned.
//
// The interesting part is not the bare sentinel but the wrapping: the executor reports these errors
// through `fmt.Errorf("...: %w", ...)` and, where several causes are collected, through `errors.Join`.
// Both must keep the cause reachable, because the mapping is a chain test and not a string match.

// TestDeadlockAndLockWaitTimeoutErrorCodes pins the two retryable codes and the wrapping they must
// survive.
func TestDeadlockAndLockWaitTimeoutErrorCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want uint16
	}{
		{"bare deadlock", storageengine.ErrDeadlock, 1213},
		{"wrapped deadlock", fmt.Errorf("statement failed: %w", storageengine.ErrDeadlock), 1213},
		{"doubly wrapped deadlock", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", storageengine.ErrDeadlock)), 1213},
		{"joined deadlock", errors.Join(errors.New("first cause"), storageengine.ErrDeadlock), 1213},
		{"deadlock joined behind another error", errors.Join(storageengine.ErrDeadlock, errors.New("second cause")), 1213},

		{"bare lock wait timeout", storageengine.ErrLockWaitTimeout, 1205},
		{"wrapped lock wait timeout", fmt.Errorf("waiting for row lock: %w", storageengine.ErrLockWaitTimeout), 1205},
		{"joined lock wait timeout", errors.Join(errors.New("first cause"), storageengine.ErrLockWaitTimeout), 1205},

		// A stale-snapshot conflict is retryable in the same sense and shares the deadlock code, so a
		// client that retries on 1213 retries it too.
		{"conflict", fmt.Errorf("stale target: %w", storageengine.ErrConflict), 1213},

		// The two must not collapse into one code: a client that only retries deadlocks would never
		// retry a timeout, and the two failures have different remedies.
		{"an unrelated error is not remapped", errors.New("syntax error near SELECT"), 1064},
		{"an unrelated error that merely mentions locks", errors.New("unsupported lock clause"), 1064},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mysqlExecutionErrorCode(testCase.err); got != testCase.want {
				t.Fatalf("mysqlExecutionErrorCode(%v) = %d, want %d", testCase.err, got, testCase.want)
			}
		})
	}

	// A deadlock and a lock-wait timeout are different errors and must produce different codes even
	// when they arrive together: the deadlock is the one that was chosen as the victim.
	if deadlock, timeout := mysqlExecutionErrorCode(storageengine.ErrDeadlock), mysqlExecutionErrorCode(storageengine.ErrLockWaitTimeout); deadlock == timeout {
		t.Fatalf("a deadlock and a lock wait timeout both map to %d", deadlock)
	}
}
