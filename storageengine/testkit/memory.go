// Package testkit provides contract fixtures. It is never a runtime backend choice.
package testkit

import (
	"bytes"
	"context"
	"fmt"
	"gbaselite/storageengine"
	"sort"
	"sync"
	"sync/atomic"
)

// NewMemory returns an independent ephemeral backend for contract and SQL tests.
func NewMemory() storageengine.Engine {
	return &memoryBackend{data: map[string]map[string][]byte{}, history: map[uint64]map[string]map[string][]byte{}, counters: map[string]uint64{}, versions: map[string]map[string]uint64{}}
}

type memoryRange struct {
	space  string
	bounds storageengine.KeyRange
}

func markMemory(m *map[string]map[string]bool, s, k string, v bool) {
	if *m == nil {
		*m = map[string]map[string]bool{}
	}
	if (*m)[s] == nil {
		(*m)[s] = map[string]bool{}
	}
	(*m)[s][k] = v
}
func validateMemoryScan(ctx context.Context, r storageengine.ScanRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.Limit < 0 || r.Unordered && (r.Range.Lower != nil || r.Range.Upper != nil || r.Range.Reverse) {
		return fmt.Errorf("invalid scan request")
	}
	return nil
}

// This test-only backend has no MVCC/bbolt dependency. It deliberately reuses
// iterator buffers, exposing operators that accidentally retain borrowed bytes.
// Write and guard conflicts use per-key committed revisions, including tombstones.
type memoryBackend struct {
	mu           sync.Mutex
	versions     map[string]map[string]uint64
	data         map[string]map[string][]byte
	history      map[uint64]map[string]map[string][]byte
	head, serial uint64
	counters     map[string]uint64
	closed       atomic.Bool
}
type memoryTxn struct {
	writes   map[string]map[string]bool
	guards   map[string]map[string]bool
	ranges   []memoryRange
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.closed.Load() {
		return nil, storageengine.ErrClosed
	}
	e.serial++
	return &memoryTxn{engine: e, data: cloneMemory(e.data), snapshot: e.head, id: fmt.Sprintf("memory-%d", e.serial)}, nil
}
func (e *memoryBackend) Head() (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.head, e.AvailabilityError()
}
func (e *memoryBackend) CatalogHead() (uint64, error)      { return e.Head() }
func (e *memoryBackend) Barrier(ctx context.Context) error { return ctx.Err() }
func (e *memoryBackend) Replica() storageengine.Replica    { return nil }
func (e *memoryBackend) Close() error                      { e.closed.Store(true); return nil }
func (e *memoryBackend) AvailabilityError() error {
	if e.closed.Load() {
		return storageengine.ErrClosed
	}
	return nil
}
func (e *memoryBackend) AdvanceCounter(ctx context.Context, k string, n uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.AvailabilityError(); err != nil {
		return err
	}
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := e.AvailabilityError(); err != nil {
		return 0, err
	}
	if n == 0 || e.counters[k] > ^uint64(0)-n {
		return 0, fmt.Errorf("invalid counter reservation")
	}
	first := e.counters[k] + 1
	e.counters[k] += n
	return first, nil
}
func (e *memoryBackend) NewIterator(ctx context.Context, revision uint64, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.AvailabilityError(); err != nil {
		return nil, err
	}
	if err := validateMemoryScan(ctx, r); err != nil {
		return nil, err
	}
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
	markMemory(&t.writes, s, string(k), true)
	return nil
}
func (t *memoryTxn) Delete(s string, k []byte) error {
	if err := t.check(); err != nil {
		return err
	}
	delete(t.data[s], string(k))
	markMemory(&t.writes, s, string(k), false)
	return nil
}
func (t *memoryTxn) Guard(s string, k []byte) error {
	if err := t.check(); err != nil {
		return err
	}
	markMemory(&t.guards, s, string(k), true)
	return nil
}
func (t *memoryTxn) GuardRange(s string, r storageengine.KeyRange) error {
	if err := t.check(); err != nil {
		return err
	}
	t.ranges = append(t.ranges, memoryRange{s, r.CloneBounds()})
	return nil
}
func (t *memoryTxn) Table(id string) storageengine.Table { return storageengine.BindTable(t, id) }
func (t *memoryTxn) Child() (storageengine.Txn, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	t.engine.mu.Lock()
	defer t.engine.mu.Unlock()
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
		for space, keys := range t.writes {
			for k, live := range keys {
				if live {
					if err := t.parent.Put(space, []byte(k), t.data[space][k]); err != nil {
						return 0, err
					}
				} else {
					if err := t.parent.Delete(space, []byte(k)); err != nil {
						return 0, err
					}
				}
			}
		}
		for space, keys := range t.guards {
			for k := range keys {
				markMemory(&t.parent.guards, space, k, true)
			}
		}
		t.parent.ranges = append(t.parent.ranges, t.ranges...)
		return 0, nil
	}
	e := t.engine
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, dependencies := range []map[string]map[string]bool{t.writes, t.guards} {
		for space, keys := range dependencies {
			for k := range keys {
				if e.versions[space][k] > t.snapshot {
					return 0, storageengine.ErrConflict
				}
			}
		}
	}
	for _, dep := range t.ranges {
		for k, revision := range e.versions[dep.space] {
			if revision > t.snapshot && dep.bounds.Contains([]byte(k)) {
				return 0, storageengine.ErrConflict
			}
		}
	}
	if len(t.writes) == 0 && len(t.guards) == 0 && len(t.ranges) == 0 {
		return e.head, nil
	}
	e.history[e.head] = cloneMemory(e.data)
	e.head++
	for space, keys := range t.writes {
		if e.data[space] == nil {
			e.data[space] = map[string][]byte{}
		}
		if e.versions[space] == nil {
			e.versions[space] = map[string]uint64{}
		}
		for k, live := range keys {
			if live {
				e.data[space][k] = bytes.Clone(t.data[space][k])
			} else {
				delete(e.data[space], k)
			}
			e.versions[space][k] = e.head
		}
	}
	return e.head, nil
}
func (t *memoryTxn) Rollback() error { t.closed = true; return nil }
func (t *memoryTxn) NewIterator(ctx context.Context, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	if err := validateMemoryScan(ctx, r); err != nil {
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
