// Package mvccadapter implements the storage contract using the existing MVCC
// store, bbolt persistence, and optional Raft proposer. No physical handles escape.
package mvccadapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gbaselite/lockmanager"
	"gbaselite/mvcc"
	"gbaselite/replication"
	"gbaselite/storageengine"
)

type engine struct {
	store    *mvcc.Store
	proposer mvcc.Proposer
	node     *replication.Node
	// locks is the B02 lock manager this engine owns. It is nil when the engine runs without
	// locking, in which case the optional LockingTxn capability is absent and a caller that
	// requires locking fails closed.
	locks *lockmanager.Manager
}
type txn struct {
	inner          *mvcc.Tx
	engine         *engine
	isolationLevel storageengine.IsolationLevel
	activeView     *mvccReadView
	consistentView *mvccReadView
	rootTxn        *txn
}

func (t *txn) root() *txn {
	if t.rootTxn != nil {
		return t.rootTxn
	}
	return t
}

func isMetadataSpace(s string) bool {
	return s == "catalog" || s == "views" || strings.HasPrefix(s, "meta") || strings.HasPrefix(s, "fkref/")
}

var _ storageengine.Engine = (*engine)(nil)
var _ storageengine.Maintenance = (*engine)(nil)
var _ storageengine.Txn = (*txn)(nil)

func Open(directory string, options storageengine.Options) (storageengine.Engine, error) {
	for _, marker := range []string{"store.gob", "store.pages", "store.wal", "store.checkpoint", "store.gob.tmp", "store.pages.tmp", "store.wal.tmp", "store.checkpoint.tmp"} {
		if _, err := os.Stat(filepath.Join(directory, "databases", marker)); err == nil {
			return nil, errors.New("legacy data requires migrate-legacy --source <old-directory> --target <new-directory>; source is preserved")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if options.Replication == nil {
		if _, err := os.Stat(filepath.Join(directory, "replication", "raft.db")); err == nil {
			return nil, errors.New("replicated data cannot be opened as a standalone node")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if options.LocalWAL && options.Replication != nil {
		return nil, errors.New("local WAL requires standalone storage")
	}
	raw, err := mvcc.OpenWithOptions(filepath.Join(directory, "versioned"), mvcc.Options{WriteSetLimitBytes: options.TransactionWriteBytes, LocalWAL: options.LocalWAL})
	if err != nil {
		return nil, err
	}
	e := &engine{store: raw, proposer: raw}
	// One lock manager per engine. It is created here and attached before any transaction can
	// begin, so every transaction of this engine shares one lock domain and a second manager can
	// never be attached to the same store.
	limits := lockmanager.BudgetLimits{
		MemoryBytes:  options.LockMemoryBytes,
		RootRequests: options.LockMaxRequestsPerRoot,
		Waiters:      options.LockMaxWaiters,
		GraphWork:    options.LockGraphWorkLimit,
	}
	locks, lockErr := lockmanager.NewManager(limits, 1, nil)
	if lockErr != nil {
		raw.Close()
		return nil, lockErr
	}
	if _, attachErr := raw.AttachLocks(locks); attachErr != nil {
		locks.Close()
		raw.Close()
		return nil, attachErr
	}
	e.locks = locks
	if c := options.Replication; c != nil {
		o := replication.Options{ID: c.ID, Bind: c.Bind, Advertise: c.Advertise, Directory: filepath.Join(directory, "replication"), Bootstrap: c.Bootstrap, TLSCert: c.TLSCert, TLSKey: c.TLSKey, TLSCA: c.TLSCA}
		for _, p := range c.Peers {
			o.Peers = append(o.Peers, replication.Peer{ID: p.ID, Address: p.Address})
		}
		e.node, err = replication.Open(raw, o)
		if err != nil {
			raw.Close()
			return nil, err
		}
		e.proposer = e.node
	}
	return e, nil
}
func (e *engine) Begin(ctx context.Context) (storageengine.Txn, error) {
	t, err := e.store.Begin(ctx, e.proposer)
	if err != nil {
		return nil, err
	}
	return &txn{inner: t, engine: e}, nil
}
func (e *engine) Head() (uint64, error)             { return e.store.Head() }
func (e *engine) CatalogHead() (uint64, error)      { return e.store.CatalogHead() }
func (e *engine) Barrier(ctx context.Context) error { return e.proposer.Barrier(ctx) }
func (e *engine) AvailabilityError() error          { return e.store.AvailabilityError() }
func (e *engine) Close() error {
	var err error
	if e.node != nil {
		err = e.node.Close()
	}
	// The lock manager is invalidated and woken before the store closes, so a waiter is never left
	// blocked on a manager whose store has gone away.
	e.store.CloseLocks()
	if e.locks != nil {
		e.locks.Close()
	}
	return errors.Join(err, e.store.Close())
}
func (e *engine) AdvanceCounter(ctx context.Context, name string, floor uint64) error {
	r, err := e.proposer.Propose(ctx, mvcc.Command{Kind: "advance", Counter: name, Floor: floor})
	if err != nil {
		return err
	}
	return r.Err()
}
func (e *engine) ReserveCounter(ctx context.Context, name string, count uint64) (uint64, error) {
	r, err := e.proposer.Propose(ctx, mvcc.Command{Kind: "reserve", Counter: name, Count: count})
	if err != nil {
		return 0, err
	}
	return r.Number, r.Err()
}
func (e *engine) Replica() storageengine.Replica {
	if e.node == nil {
		return nil
	}
	return replica{e.node}
}

type replica struct{ node *replication.Node }

var _ storageengine.LeaderVerifier = replica{}
var _ storageengine.LeadershipTransferrer = replica{}

func (r replica) VerifyLeader(ctx context.Context) error       { return r.node.VerifyLeader(ctx) }
func (r replica) Barrier(ctx context.Context) error            { return r.node.Barrier(ctx) }
func (r replica) LeadershipTransfer(ctx context.Context) error { return r.node.LeadershipTransfer(ctx) }
func (r replica) Node() *replication.Node                      { return r.node }
func (r replica) Status() storageengine.ReplicationStatus {
	s := r.node.Status()
	return storageengine.ReplicationStatus{ID: s.ID, State: s.State, Leader: s.Leader, LeaderID: s.LeaderID, Applied: s.Applied}
}
func (e *engine) Backup(ctx context.Context, path string) (storageengine.BackupManifest, error) {
	m, err := e.store.Backup(ctx, path)
	return storageengine.BackupManifest{Format: m.Format, SHA256: m.SHA256, Bytes: m.Bytes, Head: m.Head}, err
}
func (e *engine) RestoreBackup(ctx context.Context, path string) error {
	return e.store.RestoreBackup(ctx, path)
}
func (e *engine) CompactHistory(ctx context.Context) error { return e.store.CompactHistory(ctx) }
func (e *engine) Compact(ctx context.Context, path string) error {
	return e.store.ExportLayout(ctx, path, "flat")
}
func (e *engine) PendingBytes() (int64, error) { return e.store.PendingBytes() }
func (t *txn) ID() string                      { return t.inner.ID }
func (t *txn) Snapshot() uint64                { return t.inner.Snapshot }
func (t *txn) Get(s string, k []byte) ([]byte, bool, error) {
	if t.activeView != nil && t.activeView.State() == storageengine.ViewStateActive {
		return t.activeView.Get(s, k)
	}
	if t.root().isolationLevel == storageengine.IsolationSerializable && !isMetadataSpace(s) {
		return t.serializableGet(s, k)
	}
	return t.inner.Get(s, k)
}

func (t *txn) serializableGet(s string, k []byte) ([]byte, bool, error) {
	locking, ok := t.LockingTxn()
	if !ok {
		return nil, false, fmt.Errorf("%w: serializable Get requires locking capability", storageengine.ErrUnsupported)
	}
	ctx := context.Background()
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = locking.CloseLockScope(ctx, scope) }()

	tableID := s
	if idx := strings.Index(s, "/"); idx != -1 {
		tableID = s[idx+1:]
	}
	grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIS, Kind: storageengine.LockComponentTableIntent, Space: s, TableID: tableID},
			{Mode: storageengine.LockModeS, Kind: storageengine.LockComponentRecord, Space: s, TableID: tableID, Key: k},
		},
	})
	if err != nil {
		return nil, false, err
	}
	obs, err := locking.ReadCurrent(ctx, grant, s, k)
	if err != nil {
		return nil, false, err
	}
	if !obs.Exists {
		// Absent record: protect insertion position via gap lock
		prev, next, err := locking.KeysAround(ctx, s, k)
		if err != nil {
			return nil, false, err
		}
		_, err = locking.Acquire(ctx, scope, storageengine.LockRequest{
			Components: []storageengine.LockComponent{
				{Mode: storageengine.LockModeIS, Kind: storageengine.LockComponentTableIntent, Space: s, TableID: tableID},
				{Mode: storageengine.LockModeGap, Kind: storageengine.LockComponentInterval, Space: s, TableID: tableID, Lower: prev, Upper: next},
			},
		})
		if err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	return obs.Value, true, nil
}
func (t *txn) Put(s string, k, v []byte) error                     { return t.inner.Put(s, k, v) }
func (t *txn) Delete(s string, k []byte) error                     { return t.inner.Delete(s, k) }
func (t *txn) Guard(s string, k []byte) error                      { return t.inner.Guard(s, k) }
func (t *txn) GuardRange(s string, r storageengine.KeyRange) error { return t.inner.GuardRange(s, r) }
func (t *txn) Table(id string) storageengine.Table                 { return storageengine.BindTable(t, id) }
func (t *txn) Child() (storageengine.Txn, error) {
	c, err := t.inner.Child()
	if err != nil {
		return nil, err
	}
	root := t.root()
	return &txn{
		inner:          c,
		engine:         t.engine,
		isolationLevel: root.isolationLevel,
		rootTxn:        root,
	}, nil
}
func (t *txn) Commit(ctx context.Context) (uint64, error) {
	if t.rootTxn == nil {
		if t.consistentView != nil {
			_ = t.consistentView.Close()
			t.consistentView = nil
		}
		if t.activeView != nil {
			_ = t.activeView.Close()
			t.activeView = nil
		}
	} else {
		t.activeView = nil
	}
	return t.inner.Commit(ctx)
}
func (t *txn) Rollback() error {
	if t.rootTxn == nil {
		if t.consistentView != nil {
			_ = t.consistentView.Close()
			t.consistentView = nil
		}
		if t.activeView != nil {
			_ = t.activeView.Close()
			t.activeView = nil
		}
	} else {
		t.activeView = nil
	}
	return t.inner.Rollback()
}
func (t *txn) Scan(ctx context.Context, s string, f func([]byte, []byte) error) error {
	it, err := t.NewIterator(ctx, storageengine.ScanRequest{Space: s, Unordered: true})
	if err != nil {
		return err
	}
	return storageengine.Consume(it, f)
}
func (t *txn) ScanRange(ctx context.Context, s string, r storageengine.KeyRange, f func([]byte, []byte) error) error {
	it, err := t.NewIterator(ctx, storageengine.ScanRequest{Space: s, Range: r})
	if err != nil {
		return err
	}
	return storageengine.Consume(it, f)
}
func (e *engine) Scan(ctx context.Context, revision uint64, s string, f func([]byte, []byte) error) error {
	it, err := e.NewIterator(ctx, revision, storageengine.ScanRequest{Space: s, Unordered: true})
	if err != nil {
		return err
	}
	return storageengine.Consume(it, f)
}
func (t *txn) NewIterator(ctx context.Context, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	if t.activeView != nil && t.activeView.State() == storageengine.ViewStateActive {
		return t.activeView.NewIterator(ctx, r)
	}
	if t.root().isolationLevel == storageengine.IsolationSerializable && !isMetadataSpace(r.Space) {
		return nil, fmt.Errorf("%w: raw unguided NewIterator on SERIALIZABLE transaction without LockPlan is unsupported; use LockingTxn or SQL executor", storageengine.ErrUnsupported)
	}
	return newIterator(ctx, r, func(y func([]byte, []byte) error) error {
		if r.Unordered {
			return t.inner.Scan(ctx, r.Space, y)
		}
		return t.inner.ScanRange(ctx, r.Space, r.Range, y)
	})
}
func (t *txn) Closed() bool {
	if t.inner == nil {
		return true
	}
	return t.inner.Closed()
}
func (e *engine) NewIterator(ctx context.Context, rev uint64, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	return newIterator(ctx, r, func(y func([]byte, []byte) error) error {
		if r.Unordered {
			return e.store.Scan(ctx, rev, r.Space, y)
		}
		return e.store.ScanRange(ctx, rev, r.Space, r.Range, y)
	})
}
func (e *engine) LockManager() *lockmanager.Manager {
	return e.locks
}
