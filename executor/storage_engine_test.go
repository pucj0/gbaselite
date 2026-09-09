package executor

import (
	"bytes"
	"context"
	"fmt"
	"gbaselite/storageengine"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This test-only backend has no MVCC/bbolt dependency. It deliberately reuses
// iterator buffers, exposing operators that accidentally retain borrowed bytes.
// Conflict detection is conservative at database revision granularity.
type memoryBackend struct {
	data         map[string]map[string][]byte
	history      map[uint64]map[string]map[string][]byte
	head, serial uint64
	counters     map[string]uint64
	closed       bool
}
type memoryTxn struct {
	engine   *memoryBackend
	parent   *memoryTxn
	data     map[string]map[string][]byte
	id       string
	snapshot uint64
	closed   bool
}

func cloneMemory(in map[string]map[string][]byte) map[string]map[string][]byte {
	out := make(map[string]map[string][]byte)
	for s, rows := range in {
		out[s] = map[string][]byte{}
		for k, v := range rows {
			out[s][k] = bytes.Clone(v)
		}
	}
	return out
}
func (e *memoryBackend) Begin(ctx context.Context) (storageengine.Txn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.closed {
		return nil, storageengine.ErrClosed
	}
	e.serial++
	return &memoryTxn{engine: e, data: cloneMemory(e.data), snapshot: e.head, id: fmt.Sprintf("memory-%d", e.serial)}, nil
}
func (e *memoryBackend) Head() (uint64, error)             { return e.head, e.AvailabilityError() }
func (e *memoryBackend) CatalogHead() (uint64, error)      { return e.Head() }
func (e *memoryBackend) Barrier(ctx context.Context) error { return ctx.Err() }
func (e *memoryBackend) Replica() storageengine.Replica    { return nil }
func (e *memoryBackend) Close() error                      { e.closed = true; return nil }
func (e *memoryBackend) AvailabilityError() error {
	if e.closed {
		return storageengine.ErrClosed
	}
	return nil
}
func (e *memoryBackend) AdvanceCounter(ctx context.Context, k string, n uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.counters == nil {
		e.counters = map[string]uint64{}
	}
	if n > e.counters[k] {
		e.counters[k] = n
	}
	return nil
}
func (e *memoryBackend) ReserveCounter(ctx context.Context, k string, n uint64) (uint64, error) {
	first := e.counters[k] + 1
	return first, e.AdvanceCounter(ctx, k, first+n-1)
}
func (e *memoryBackend) NewIterator(ctx context.Context, revision uint64, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	if revision > e.head {
		return nil, storageengine.ErrConflict
	}
	data := e.data
	if revision != e.head {
		var ok bool
		data, ok = e.history[revision]
		if !ok {
			return nil, storageengine.ErrConflict
		}
	}
	return newMemoryIterator(ctx, data, r), nil
}
func (e *memoryBackend) Scan(ctx context.Context, rev uint64, s string, f func([]byte, []byte) error) error {
	it, err := e.NewIterator(ctx, rev, storageengine.ScanRequest{Space: s})
	if err != nil {
		return err
	}
	return storageengine.Consume(it, f)
}
func (t *memoryTxn) ID() string       { return t.id }
func (t *memoryTxn) Snapshot() uint64 { return t.snapshot }
func (t *memoryTxn) check() error {
	if t.closed {
		return storageengine.ErrClosed
	}
	return t.engine.AvailabilityError()
}
func (t *memoryTxn) Get(s string, k []byte) ([]byte, bool, error) {
	if err := t.check(); err != nil {
		return nil, false, err
	}
	v, ok := t.data[s][string(k)]
	return bytes.Clone(v), ok, nil
}
func (t *memoryTxn) Put(s string, k, v []byte) error {
	if err := t.check(); err != nil {
		return err
	}
	if t.data[s] == nil {
		t.data[s] = map[string][]byte{}
	}
	t.data[s][string(k)] = bytes.Clone(v)
	return nil
}
func (t *memoryTxn) Delete(s string, k []byte) error {
	if err := t.check(); err != nil {
		return err
	}
	delete(t.data[s], string(k))
	return nil
}
func (t *memoryTxn) Guard(s string, k []byte) error      { return t.check() }
func (t *memoryTxn) GuardRange(s string) error           { return t.check() }
func (t *memoryTxn) Table(id string) storageengine.Table { return storageengine.BindTable(t, id) }
func (t *memoryTxn) Child() (storageengine.Txn, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	t.engine.serial++
	return &memoryTxn{engine: t.engine, parent: t, data: cloneMemory(t.data), snapshot: t.snapshot, id: fmt.Sprintf("memory-%d", t.engine.serial)}, nil
}
func (t *memoryTxn) Commit(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := t.check(); err != nil {
		return 0, err
	}
	t.closed = true
	if t.parent != nil {
		if err := t.parent.check(); err != nil {
			return 0, err
		}
		t.parent.data = t.data
		return 0, nil
	}
	if t.snapshot != t.engine.head {
		return 0, storageengine.ErrConflict
	}
	if t.engine.history == nil {
		t.engine.history = map[uint64]map[string]map[string][]byte{}
	}
	t.engine.history[t.engine.head] = cloneMemory(t.engine.data)
	t.engine.data = t.data
	t.engine.head++
	return t.engine.head, nil
}
func (t *memoryTxn) Rollback() error { t.closed = true; return nil }
func (t *memoryTxn) NewIterator(ctx context.Context, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	return newMemoryIterator(ctx, t.data, r), nil
}
func (t *memoryTxn) Scan(ctx context.Context, s string, f func([]byte, []byte) error) error {
	it, err := t.NewIterator(ctx, storageengine.ScanRequest{Space: s, Unordered: true})
	if err != nil {
		return err
	}
	return storageengine.Consume(it, f)
}
func (t *memoryTxn) ScanRange(ctx context.Context, s string, r storageengine.KeyRange, f func([]byte, []byte) error) error {
	it, err := t.NewIterator(ctx, storageengine.ScanRequest{Space: s, Range: r})
	if err != nil {
		return err
	}
	return storageengine.Consume(it, f)
}

type memoryIterator struct {
	ctx        context.Context
	entries    [][2][]byte
	position   int
	key, value []byte
	err        error
	closed     bool
}

func newMemoryIterator(ctx context.Context, data map[string]map[string][]byte, r storageengine.ScanRequest) *memoryIterator {
	it := &memoryIterator{ctx: ctx}
	for k, v := range data[r.Space] {
		key := []byte(k)
		lo, hi := bytes.Compare(key, r.Range.Lower), bytes.Compare(key, r.Range.Upper)
		if r.Range.Lower != nil && (lo < 0 || lo == 0 && !r.Range.LowerInclusive) {
			continue
		}
		if r.Range.Upper != nil && (hi > 0 || hi == 0 && !r.Range.UpperInclusive) {
			continue
		}
		it.entries = append(it.entries, [2][]byte{key, bytes.Clone(v)})
	}
	sort.Slice(it.entries, func(i, j int) bool {
		c := bytes.Compare(it.entries[i][0], it.entries[j][0])
		if r.Range.Reverse {
			return c > 0
		}
		return c < 0
	})
	if r.Limit > 0 && len(it.entries) > r.Limit {
		it.entries = it.entries[:r.Limit]
	}
	return it
}
func (i *memoryIterator) Next() bool {
	if i.closed {
		return false
	}
	if i.err = i.ctx.Err(); i.err != nil {
		return false
	}
	if i.position >= len(i.entries) {
		return false
	}
	e := i.entries[i.position]
	i.position++
	i.key = append(i.key[:0], e[0]...)
	i.value = append(i.value[:0], e[1]...)
	return true
}
func (i *memoryIterator) Key() []byte   { return i.key }
func (i *memoryIterator) Value() []byte { return i.value }
func (i *memoryIterator) Err() error    { return i.err }
func (i *memoryIterator) Close() error  { i.closed = true; i.entries = nil; return nil }

func TestSQLRunsOnIndependentStorageEngine(t *testing.T) {
	backend := &memoryBackend{data: map[string]map[string][]byte{}, counters: map[string]uint64{}}
	called := false
	e, err := OpenWithOptions(t.TempDir(), "root", "test-only", OpenOptions{BackendFactory: func(_ string, _ storageengine.Options) (storageengine.Engine, error) {
		called = true
		return backend, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if !called {
		t.Fatal("factory not used")
	}
	s := &Session{}
	run := func(s *Session, q string) *Result {
		t.Helper()
		r, err := e.Execute(s, q)
		if err != nil {
			t.Fatal(q, err)
		}
		return r
	}
	for _, q := range []string{"CREATE DATABASE portable", "USE portable", "CREATE TABLE items(id BIGINT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE,grp INT,KEY group_idx(grp))", "INSERT INTO items(v,grp) VALUES(10,1),(20,1),(30,2)", "BEGIN", "UPDATE items SET v=99 WHERE id=1", "ROLLBACK"} {
		run(s, q)
	}
	if got := fmt.Sprint(run(s, "SELECT id,v FROM items ORDER BY id DESC LIMIT 2").Rows); got != "[[3 30] [2 20]]" {
		t.Fatal(got)
	}
	if got := fmt.Sprint(run(s, "SELECT SUM(v),COUNT(*) FROM items").Rows); got != "[[60 3]]" {
		t.Fatal("borrowed batch bytes", got)
	}
	if got := fmt.Sprint(run(s, "SELECT id,v FROM items WHERE grp=1 ORDER BY id").Rows); got != "[[1 10] [2 20]]" {
		t.Fatal("secondary", got)
	}
	run(s, "BEGIN")
	run(s, "UPDATE items SET v=11 WHERE id=1")
	if _, err = e.Execute(s, "INSERT INTO items(v,grp) VALUES(40,1),(20,1)"); err == nil {
		t.Fatal("unique violation accepted")
	}
	run(s, "COMMIT")
	if got := fmt.Sprint(run(s, "SELECT SUM(v),COUNT(*) FROM items").Rows); got != "[[61 3]]" {
		t.Fatal("statement rollback", got)
	}
	old := &Session{CurrentDatabase: "portable"}
	defer e.CloseSession(old)
	run(old, "BEGIN")
	run(s, "UPDATE items SET v=12 WHERE id=1")
	if got := fmt.Sprint(run(old, "SELECT v FROM items WHERE id=1").Rows); got != "[[11]]" {
		t.Fatal("snapshot", got)
	}
	run(old, "ROLLBACK")
	run(s, "CREATE TABLE children(id INT PRIMARY KEY,parent_id BIGINT,FOREIGN KEY(parent_id) REFERENCES items(id))")
	run(s, "INSERT INTO children VALUES(1,2)")
	if _, err = e.Execute(s, "DELETE FROM items WHERE id=2"); err == nil {
		t.Fatal("foreign key protection lost")
	}
	if got := fmt.Sprint(run(s, "SELECT items.v FROM items INNER JOIN children ON items.id=children.parent_id").Rows); got != "[[20]]" {
		t.Fatal("join", got)
	}
	if _, err = e.Execute(s, "BACKUP MVCC TO 'not-created'"); err != storageengine.ErrUnsupported {
		t.Fatalf("optional capability: %v", err)
	}
}

func TestSQLPackagesDoNotImportPhysicalBackends(t *testing.T) {
	for _, dir := range []string{"executor", "server", "parser", "planner", "sql", "physical"} {
		root := filepath.Join("..", dir)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := goparser.ParseFile(token.NewFileSet(), path, nil, goparser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				name, _ := strconv.Unquote(imp.Path.Value)
				if strings.Contains(name, "bbolt") || name == "gbaselite/mvcc" || name == "gbaselite/replication" || strings.HasPrefix(name, "gbaselite/storageengine/mvccadapter") {
					t.Errorf("%s directly imports physical backend %s", path, name)
				}
				if name == "gbaselite/enginefactory" && filepath.Base(path) != "open_options.go" {
					t.Errorf("backend composition leaked into operator %s", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
