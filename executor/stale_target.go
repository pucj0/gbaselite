package executor

import (
	"errors"

	"gbaselite/storageengine"
)

// stale_target.go owns the cause of the one conflict that must end a transaction rather than a
// statement.
//
// Every DML statement selects its candidates from the transaction's fixed snapshot (FR-017) and then
// verifies, under the row's lock, that the version it selected is still the version that exists
// (FR-018). When it is not, the statement cannot proceed and reports a retryable conflict. The
// transaction as a whole, however, is a unit of work: its *other* statements were computed from the
// same snapshot that has just been proven too old for a row this transaction must write. Publishing
// those statements' writes would commit a partial unit of work whose remaining parts were abandoned
// with an error, which is exactly what "retry the transaction" is meant to prevent — and it is what
// T037 means by "the early conflict ends the root".
//
// So the conflict needs an identity. It has to stay a storageengine.ErrConflict for every existing
// caller (the protocol maps it to 1213, and `errors.Is` checks are all over the DML tests), while
// being distinguishable from the conflicts that must NOT end the root:
//
//   - a lock wait timeout (ErrLockWaitTimeout) is a statement-level bound and leaves the transaction
//     usable, so it must never reach this path;
//   - a user cancellation or deadline is the caller's own decision about one statement;
//   - an ErrConflict raised by the *lock* layer (a request that could not wait) is a lock refusal,
//     not a statement about a row's version.
//
// staleDMLTargetError is that identity. Its message is byte-for-byte the message ErrConflict already
// produces, so no client-visible text changes.

// staleDMLTargetError marks the retryable conflict a DML statement raises when a row it must write
// was replaced by a newer committed version after the statement's snapshot.
type staleDMLTargetError struct{}

// Error reports the message storageengine.ErrConflict already reports, so the identity is extra
// information for this package and not a change to what a client sees.
func (staleDMLTargetError) Error() string { return storageengine.ErrConflict.Error() }

// Unwrap keeps every existing `errors.Is(err, storageengine.ErrConflict)` true, which is what the
// protocol mapping and the DML acceptance tests rely on.
func (staleDMLTargetError) Unwrap() error { return storageengine.ErrConflict }

// staleTargetConflict reports that a statement's candidate describes a version of a row that another
// root has already replaced, so the transaction must be retried rather than patched up.
func staleTargetConflict() error { return staleDMLTargetError{} }

// isStaleTargetConflict reports whether one error is that conflict, through any wrapping.
func isStaleTargetConflict(err error) bool {
	var target staleDMLTargetError
	return errors.As(err, &target)
}
