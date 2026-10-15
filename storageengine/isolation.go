package storageengine

import (
	"context"
	"errors"
)

var (
	// ErrViewClosed reports an operation on an already closed read view.
	ErrViewClosed = errors.New("read view is closed")
	// ErrViewInvalidated reports that a read view is no longer usable because of a database restore or generation change.
	ErrViewInvalidated = errors.New("read view invalidated by generation change or restore")
	// ErrViewBudgetLimit reports that allocating a read view would exceed the configured budget.
	ErrViewBudgetLimit = errors.New("read view budget limit exceeded")
)

// IsolationLevel represents the transaction isolation level.
type IsolationLevel string

const (
	IsolationReadUncommitted IsolationLevel = "READ UNCOMMITTED"
	IsolationReadCommitted   IsolationLevel = "READ COMMITTED"
	IsolationRepeatableRead  IsolationLevel = "REPEATABLE READ"
	IsolationSerializable    IsolationLevel = "SERIALIZABLE"
)

// Valid reports whether the isolation level is one of the four valid levels.
func (l IsolationLevel) Valid() bool {
	switch l {
	case IsolationReadUncommitted, IsolationReadCommitted, IsolationRepeatableRead, IsolationSerializable:
		return true
	default:
		return false
	}
}

// StatementViewKind identifies the nature and scope of a read view.
type StatementViewKind uint8

const (
	// StatementViewKindStatement is a single SQL statement view (refreshed per statement in READ COMMITTED).
	StatementViewKindStatement StatementViewKind = iota
	// StatementViewKindConsistent is a transaction-wide consistent view (pinned on first consistent read in REPEATABLE READ).
	StatementViewKindConsistent
	// StatementViewKindUncommitted is an uncommitted staged view (READ UNCOMMITTED).
	StatementViewKindUncommitted
)

// ViewState is the lifecycle state of one ReadView.
type ViewState string

const (
	ViewStateActive      ViewState = "ACTIVE"
	ViewStateClosed      ViewState = "CLOSED"
	ViewStateInvalidated ViewState = "INVALIDATED"
)

// ReadView represents an immutable snapshot read view for one statement or transaction.
// It is single-consumer and safe for concurrent read access according to the storage contract.
type ReadView interface {
	ID() string
	Revision() uint64
	Generation() uint64
	RootID() string
	Kind() StatementViewKind
	State() ViewState
	Get(space string, key []byte) ([]byte, bool, error)
	NewIterator(context.Context, ScanRequest) (Iterator, error)
	Close() error
}

// IsolationTxn is an optional capability on a Txn that can produce and manage isolated read views.
type IsolationTxn interface {
	IsolationLevel() IsolationLevel
	BeginStatementView(ctx context.Context, kind StatementViewKind) (ReadView, error)
	BeginConsistentView(ctx context.Context) (ReadView, error)
	ActiveView() (ReadView, bool)
}

// DirtyPublisher is an optional capability on a Txn for publishing uncommitted dirty writes.
type DirtyPublisher interface {
	PublishDirtyWrites() error
	WithdrawDirtyWrites()
}

// IsolationEngine is an optional capability on an Engine for creating transactions with explicit isolation levels.
type IsolationEngine interface {
	BeginIsolated(ctx context.Context, level IsolationLevel) (Txn, error)
}

// BeginIsolated creates an isolated transaction if the engine supports IsolationEngine, or returns ErrUnsupported.
func BeginIsolated(ctx context.Context, e Engine, level IsolationLevel) (Txn, error) {
	iso, ok := e.(IsolationEngine)
	if !ok {
		return nil, ErrUnsupported
	}
	return iso.BeginIsolated(ctx, level)
}

type internalReaderKey struct{}

// WithInternalReader marks a context as originating from a verified query planner/executor,
// allowing internal schema/candidate scans on SERIALIZABLE transactions.
func WithInternalReader(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalReaderKey{}, true)
}

// IsInternalReader reports whether ctx was marked with WithInternalReader.
func IsInternalReader(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(internalReaderKey{}).(bool)
	return v
}
