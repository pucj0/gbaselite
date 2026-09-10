// Package storageengine defines the backend-independent ordered-byte storage contract.
// SQL owns schema/row encodings; backends own durability, snapshots and conflicts.
package storageengine

import (
	"context"
	"errors"
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
