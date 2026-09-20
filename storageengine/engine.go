// Package storageengine defines the backend-independent ordered-byte storage contract.
// SQL owns schema/row encodings; backends own durability, snapshots and conflicts.
package storageengine

import (
	"context"
	"errors"
	"time"
)

// MaxValueBytes is the portable SQL record budget, independent of physical pages.
const MaxValueBytes = 48 << 10

var (
	ErrConflict      = errors.New("row version conflict; retry transaction")
	ErrClosed        = errors.New("storage engine or transaction closed")
	ErrWriteSetLimit = errors.New("transaction write set exceeds configured budget")
	ErrNotLeader     = errors.New("node is not the leader")
	ErrUnsupported   = errors.New("storage engine capability is not supported")
)

type ScanStats struct{ StoredKeys, StagedKeys uint64 }

// KeyRange uses bytewise ordering. Nil means unbounded; empty non-nil is a bound.
// Stats is call-local and must not be shared by concurrent scans.
type KeyRange struct {
	Lower, Upper                   []byte
	LowerInclusive, UpperInclusive bool
	Reverse                        bool
	Stats                          *ScanStats
}

// ScanRequest selects one opaque namespace. Limit=0 means unlimited.
// Unordered permits a faster full scan; it cannot be combined with bounds/reverse.
type ScanRequest struct {
	Space     string
	Range     KeyRange
	Limit     int
	Unordered bool
}

// Iterator is single-consumer and must be closed, including on early exit.
// Key/Value are borrowed until the next Next/Close. Errors are reported by Err.
// Scans see the transaction snapshot plus prior writes; don't mutate that Txn
// while iterating. A separate child write transaction is allowed.
type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Err() error
	Close() error
}

// Txn provides snapshot isolation, read-your-writes and optimistic conflict checks.
// Child commits merge atomically into their parent; rollback discards only the child.
// Commit/Rollback end the transaction and require all its iterators to be closed.
// Get returns owned bytes. Put/Delete do not retain caller-owned slices.
// Guard/GuardRange add optimistic dependencies, validated at commit, including
// inserts/deletes after Snapshot. GuardRange copies its bounds; zero KeyRange
// guards the whole space. Reverse/Stats do not affect conflict membership.
// These are validation dependencies, not blocking gap/next-key locks.
type Txn interface {
	ID() string
	Snapshot() uint64
	Get(space string, key []byte) ([]byte, bool, error)
	Put(space string, key, value []byte) error
	Delete(space string, key []byte) error
	Guard(space string, key []byte) error
	GuardRange(space string, bounds KeyRange) error
	NewIterator(context.Context, ScanRequest) (Iterator, error)
	Scan(context.Context, string, func([]byte, []byte) error) error
	ScanRange(context.Context, string, KeyRange, func([]byte, []byte) error) error
	Table(id string) Table
	Child() (Txn, error)
	Commit(context.Context) (uint64, error)
	Rollback() error
}

// Engine owns storage lifetime. Close requires callers to close transactions first.
// Counter reservations are durable and outside SQL rollback (gaps are allowed).
// Revision reads are for committed catalog metadata, not a new SQL snapshot.
// Engine is the minimal transaction/lifetime contract. Optional capabilities
// must be detected explicitly; Begin and Txn.Snapshot semantics are unchanged.
type Engine interface {
	Begin(context.Context) (Txn, error)
	Close() error
}
type RevisionReader interface {
	Head() (uint64, error)
	CatalogHead() (uint64, error)
	NewIterator(context.Context, uint64, ScanRequest) (Iterator, error)
	Scan(context.Context, uint64, string, func([]byte, []byte) error) error
}
type CounterAllocator interface {
	AdvanceCounter(context.Context, string, uint64) error
	ReserveCounter(context.Context, string, uint64) (uint64, error)
}
type ReplicatedEngine interface {
	Barrier(context.Context) error
	Replica() Replica
}
type Availability interface{ AvailabilityError() error }

// FullEngine names the pre-capability contract for existing full-featured users.
// Implementations of that contract remain valid Engines without adapters.
type FullEngine interface {
	Engine
	RevisionReader
	CounterAllocator
	ReplicatedEngine
	Availability
}

func AdvanceCounter(ctx context.Context, e Engine, key string, floor uint64) error {
	c, ok := e.(CounterAllocator)
	if !ok {
		return ErrUnsupported
	}
	return c.AdvanceCounter(ctx, key, floor)
}
func ReserveCounter(ctx context.Context, e Engine, key string, count uint64) (uint64, error) {
	c, ok := e.(CounterAllocator)
	if !ok {
		return 0, ErrUnsupported
	}
	return c.ReserveCounter(ctx, key, count)
}

// LeaderVerifier is an optional capability of a Replica, independent of Engine.
// It confirms quorum-backed leadership without establishing an FSM read barrier.
type LeaderVerifier interface {
	VerifyLeader(context.Context) error
}

type Replica interface {
	Barrier(context.Context) error
	Status() ReplicationStatus
}
type ReplicationStatus struct {
	ID, State, Leader, LeaderID string
	Applied                     uint64
}
type Peer struct{ ID, Address string }
type ReplicationOptions struct {
	ID, Bind, Advertise, Directory string
	Peers                          []Peer
	Bootstrap                      bool
	TLSCert, TLSKey, TLSCA         string
}
type Options struct {
	LocalWAL              bool
	TransactionWriteBytes int64
	Replication           *ReplicationOptions
}

// Factory is the composition seam. Operators never select a concrete backend.
type Factory func(directory string, options Options) (Engine, error)
type BackupManifest struct {
	Format int
	SHA256 string
	Bytes  int64
	Head   uint64
}

// Maintenance is optional. SQL must return ErrUnsupported when it is absent.
type Maintenance interface {
	Backup(context.Context, string) (BackupManifest, error)
	RestoreBackup(context.Context, string) error
	CompactHistory(context.Context) error
	Compact(context.Context, string) error
}
type Diagnostics interface{ PendingBytes() (int64, error) }

// TransactionState is the neutral, backend-independent lifecycle state a storage
// engine reports through TransactionDiagnostics. It is a diagnostics classification
// only: it carries no promise about visibility, conflict resolution or durability
// beyond what Txn.Commit already reports, and SQL does not branch on it.
type TransactionState string

const (
	// TransactionActive accepts reads and writes and has not ended.
	TransactionActive TransactionState = "ACTIVE"
	// TransactionCommitting is a root transaction whose durable publication is in
	// progress. The commit point has not been reached, so callers must not treat it
	// as committed and must not read a commit sequence for it.
	TransactionCommitting TransactionState = "COMMITTING"
	// TransactionCommitted is a root transaction that ended successfully.
	//
	// A data-writing transaction reaches it with HasCommitTS=true, a durable
	// publication marker and installed data versions. A dependency-only
	// transaction (Guard/GuardRange and no Put/Delete) also reaches it with
	// HasCommitTS=true and an advanced head, but installs no data version, because a
	// guard is a validation dependency rather than a value. A read-only or empty
	// root reaches it with HasCommitTS=false: it allocated no commit sequence,
	// published no marker and did not advance the head. The sequence itself must
	// therefore never be treated as the evidence of a commit, and HasCommitTS must
	// never be read as "this transaction changed data".
	TransactionCommitted TransactionState = "COMMITTED"
	// TransactionAborted is an ended transaction whose writes were discarded, whether
	// it was rolled back or ended by conflict, cancellation, or a failed commit.
	TransactionAborted TransactionState = "ABORTED"
	// TransactionMerged is a child transaction whose writes were merged into its
	// parent. It has no durable commit sequence of its own.
	TransactionMerged TransactionState = "MERGED"
	// TransactionUnknown is the fail-closed classification: a backend that cannot
	// classify a transaction must report this instead of claiming it is ACTIVE.
	TransactionUnknown TransactionState = "UNKNOWN"
)

// TransactionInfo is one transaction's diagnostics snapshot. Fields describe the
// transaction at the instant the snapshot was taken, so a caller must not read the
// combination as an ordered history: State may already be terminal while CommitTS is
// absent, and vice versa is never legal.
//
// Counters are process-local observations, not durable state, and they are never
// keys or values: no SQL text, key bytes or row data appears here.
type TransactionInfo struct {
	// ID is the transaction identifier reported by Txn.ID; empty when unavailable.
	ID string
	// ParentID is the owning transaction for a child, empty for a root.
	ParentID string

	// StartTS is the head the transaction began at; ReadTS is the snapshot it reads.
	// They are equal for a normal begin. A child inherits its parent's snapshot
	// instead of taking a new one, so it reports the parent's values and pins no
	// history of its own.
	StartTS uint64
	ReadTS  uint64

	// CommitTS is the commit sequence, valid only when HasCommitTS is true. Sequence
	// 0 is a legal value, so the boolean rather than the value reports presence.
	//
	// HasCommitTS reports that this root transaction has a durable commit revision.
	// It does not promise that the transaction installed a data version: a
	// dependency-only commit has a revision and no version. Use Writes/WriteBytes to
	// ask whether the transaction changed data.
	CommitTS    uint64
	HasCommitTS bool

	// State is the lifecycle classification. Generation identifies the database
	// generation the transaction belongs to, and StartedAt is when it registered.
	State      TransactionState
	Generation uint64
	StartedAt  time.Time

	// Observation counters for work this transaction performed. RowsObserved and
	// BytesObserved are bounded summaries: the keys and values a transaction read are
	// never retained here.
	PointReads    uint64
	RangeReads    uint64
	RowsObserved  uint64
	BytesObserved uint64

	// Writes and WriteBytes describe the current logical data write set, including
	// bytes spooled to staging storage; they are not a durable size, and they are not
	// a call history: replacing a key changes its contribution instead of adding one.
	// A guard is a dependency rather than a write, so it contributes to neither
	// counter, and Writes=0 with HasCommitTS=true is a legal combination.
	Writes     uint64
	WriteBytes int64

	// PointDependencies and RangeDependencies count the optimistic validation
	// dependencies from Guard and GuardRange. They are dependencies, not locks, and
	// ordinary reads never appear here.
	PointDependencies uint64
	RangeDependencies uint64

	// AbortReason is a bounded category such as "conflict" or "canceled". It never
	// contains keys, values or SQL text.
	AbortReason string
}

// TransactionStats is the aggregate diagnostics view of one engine.
type TransactionStats struct {
	// ActiveRoot and ActiveChildren count registered transactions that have not
	// reached a terminal state.
	ActiveRoot     uint64
	ActiveChildren uint64
	// Committed, Aborted and Conflicts are monotonic process-lifetime counters of
	// terminal outcomes; they are not limited to the currently open database.
	Committed uint64
	Aborted   uint64
	Conflicts uint64

	// OldestReadTS is the GC horizon: the smallest read timestamp still pinned by a
	// registered root transaction, so history at or after it must be retained. The
	// boolean is required because 0 is a legal timestamp and cannot act as "unset".
	OldestReadTS    uint64
	HasOldestReadTS bool
}

// TransactionDiagnostics is an optional Engine capability exposing read-only
// transaction diagnostics. It is deliberately separate from Engine, Txn,
// Diagnostics and FullEngine: a backend may implement none, one, or several, and
// callers must detect each capability explicitly and fall back to ErrUnsupported
// when it is absent. Absence is not an error and never changes transaction
// semantics.
//
// Both methods are safe to call concurrently with Begin, Commit and Rollback, and
// neither blocks on transaction progress. They observe; they do not control.
type TransactionDiagnostics interface {
	// ActiveTransactions returns the transactions currently registered in the
	// backend registry. The slice is owned by the caller, is never nil, and has no
	// guaranteed order.
	//
	// "Registered" is not the same as State == ACTIVE: an entry that reached a
	// terminal state but has not been unregistered yet is still returned for that
	// short window. A caller that wants live transactions only must filter on State.
	ActiveTransactions() []TransactionInfo
	// TransactionStats returns the aggregate counters and the current GC horizon.
	//
	// ActiveRoot and ActiveChildren count only non-terminal registered transactions,
	// so len(ActiveTransactions()) may briefly exceed their sum between a terminal
	// transition and the unregister that follows it.
	TransactionStats() TransactionStats
}
