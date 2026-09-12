package executor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"gbaselite/storageengine"
	"gbaselite/storageengine/testkit"
)

var errInjectedCommit = errors.New("injected commit failure")

// commitFailingBackend fails the Nth commit after arm. Autocommit statements
// commit twice in order: the statement child transaction, then the statement
// transaction, so arm(1) exercises the child commit and arm(2) the outer commit.
type commitFailingBackend struct {
	storageengine.Engine
	failAt  atomic.Int64
	commits atomic.Int64
}

func (b *commitFailingBackend) arm(at int64) {
	b.commits.Store(0)
	b.failAt.Store(at)
}

func (b *commitFailingBackend) disarm() { b.failAt.Store(0) }

// Optional storage capabilities are not forwarded by Go embedding, so the
// counter allocator used by INSERT must be delegated explicitly.
func (b *commitFailingBackend) AdvanceCounter(ctx context.Context, key string, floor uint64) error {
	allocator, ok := b.Engine.(storageengine.CounterAllocator)
	if !ok {
		return storageengine.ErrUnsupported
	}
	return allocator.AdvanceCounter(ctx, key, floor)
}

func (b *commitFailingBackend) ReserveCounter(ctx context.Context, key string, count uint64) (uint64, error) {
	allocator, ok := b.Engine.(storageengine.CounterAllocator)
	if !ok {
		return 0, storageengine.ErrUnsupported
	}
	return allocator.ReserveCounter(ctx, key, count)
}

func (b *commitFailingBackend) Begin(ctx context.Context) (storageengine.Txn, error) {
	tx, err := b.Engine.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &commitFailingTxn{Txn: tx, backend: b}, nil
}

type commitFailingTxn struct {
	storageengine.Txn
	backend *commitFailingBackend
}

func (t *commitFailingTxn) Child() (storageengine.Txn, error) {
	child, err := t.Txn.Child()
	if err != nil {
		return nil, err
	}
	return &commitFailingTxn{Txn: child, backend: t.backend}, nil
}

func (t *commitFailingTxn) Commit(ctx context.Context) (uint64, error) {
	if at := t.backend.failAt.Load(); at != 0 && t.backend.commits.Add(1) == at {
		return 0, errInjectedCommit
	}
	return t.Txn.Commit(ctx)
}

func TestMVCCInsertLastInsertIDRequiresStatementCommit(t *testing.T) {
	for _, c := range []struct {
		name   string
		failAt int64
		query  string
	}{
		{"insert_values_child_commit", 1, "INSERT INTO t(v) VALUES(5)"},
		{"insert_select_child_commit", 1, "INSERT INTO t(v) SELECT v FROM src"},
		{"insert_values_outer_commit", 2, "INSERT INTO t(v) VALUES(5)"},
		{"insert_select_outer_commit", 2, "INSERT INTO t(v) SELECT v FROM src"},
	} {
		t.Run(c.name, func(t *testing.T) {
			backend := &commitFailingBackend{Engine: testkit.NewMemory()}
			e, err := OpenWithOptions(t.TempDir(), "root", "test", OpenOptions{BackendFactory: func(string, storageengine.Options) (storageengine.Engine, error) {
				return backend, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			s := &Session{}
			run := func(q string) *Result {
				t.Helper()
				r, err := e.Execute(s, q)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return r
			}
			run("CREATE DATABASE committed")
			run("USE committed")
			run("CREATE TABLE t(id INT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE)")
			run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
			run("INSERT INTO src VALUES(1,7)")
			s.LastInsertID = 42
			backend.arm(c.failAt)
			if _, err := e.Execute(s, c.query); !errors.Is(err, errInjectedCommit) {
				t.Fatalf("%s: err=%v, want injected commit failure", c.query, err)
			}
			backend.disarm()
			if s.LastInsertID != 42 {
				t.Fatalf("%s published LastInsertID=%d without a landed commit", c.query, s.LastInsertID)
			}
			if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[0]]" {
				t.Fatalf("%s left rows behind: %s", c.query, got)
			}
			inserted := run("INSERT INTO t(v) SELECT v FROM src")
			if inserted.LastInsertID == 0 || s.LastInsertID != inserted.LastInsertID {
				t.Fatalf("committed statement last id = %d session = %d", inserted.LastInsertID, s.LastInsertID)
			}
		})
	}
}
