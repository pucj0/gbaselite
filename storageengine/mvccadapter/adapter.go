// Package mvccadapter implements the storage contract using the existing MVCC
// store, bbolt persistence, and optional Raft proposer. No physical handles escape.
package mvccadapter

import (
	"context"
	"errors"
	"gbaselite/mvcc"
	"gbaselite/replication"
	"gbaselite/storageengine"
	"os"
	"path/filepath"
)

type engine struct {
	store    *mvcc.Store
	proposer mvcc.Proposer
	node     *replication.Node
}
type txn struct{ inner *mvcc.Tx }

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
	return &txn{t}, nil
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

func (r replica) VerifyLeader(ctx context.Context) error { return r.node.VerifyLeader(ctx) }
func (r replica) Barrier(ctx context.Context) error      { return r.node.Barrier(ctx) }
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
func (e *engine) PendingBytes() (int64, error)                     { return e.store.PendingBytes() }
func (t *txn) ID() string                                          { return t.inner.ID }
func (t *txn) Snapshot() uint64                                    { return t.inner.Snapshot }
func (t *txn) Get(s string, k []byte) ([]byte, bool, error)        { return t.inner.Get(s, k) }
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
	return &txn{c}, nil
}
func (t *txn) Commit(ctx context.Context) (uint64, error) { return t.inner.Commit(ctx) }
func (t *txn) Rollback() error                            { return t.inner.Rollback() }
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
	return newIterator(ctx, r, func(y func([]byte, []byte) error) error {
		if r.Unordered {
			return t.inner.Scan(ctx, r.Space, y)
		}
		return t.inner.ScanRange(ctx, r.Space, r.Range, y)
	})
}
func (e *engine) NewIterator(ctx context.Context, rev uint64, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	return newIterator(ctx, r, func(y func([]byte, []byte) error) error {
		if r.Unordered {
			return e.store.Scan(ctx, rev, r.Space, y)
		}
		return e.store.ScanRange(ctx, rev, r.Space, r.Range, y)
	})
}
