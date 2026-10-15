package mvccadapter

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"

	"gbaselite/mvcc"
	"gbaselite/storageengine"
)

var _ storageengine.IsolationEngine = (*engine)(nil)
var _ storageengine.IsolationTxn = (*txn)(nil)
var _ storageengine.ReadView = (*mvccReadView)(nil)

type mvccReadView struct {
	token         *mvcc.ViewToken
	kind          storageengine.StatementViewKind
	txn           *txn
	isChildView   bool
	dirtySnapshot *mvcc.DirtySnapshot
}

type dirtyRow struct {
	key   []byte
	value []byte
}

type inMemoryIterator struct {
	rows  []dirtyRow
	idx   int
	curr  dirtyRow
	valid bool
}

func (it *inMemoryIterator) Next() bool {
	if it.idx < len(it.rows) {
		it.curr = it.rows[it.idx]
		it.idx++
		it.valid = true
		return true
	}
	it.valid = false
	return false
}

func (it *inMemoryIterator) Key() []byte {
	if !it.valid {
		return nil
	}
	return it.curr.key
}

func (it *inMemoryIterator) Value() []byte {
	if !it.valid {
		return nil
	}
	return it.curr.value
}

func (it *inMemoryIterator) Valid() bool { return it.valid }
func (it *inMemoryIterator) Err() error  { return nil }
func (it *inMemoryIterator) Close() error {
	it.rows = nil
	it.curr = dirtyRow{}
	it.valid = false
	return nil
}

func (v *mvccReadView) ID() string                            { return v.token.ID() }
func (v *mvccReadView) Revision() uint64                      { return v.token.Revision() }
func (v *mvccReadView) Generation() uint64                    { return v.token.Generation() }
func (v *mvccReadView) RootID() string                        { return v.token.RootID() }
func (v *mvccReadView) Kind() storageengine.StatementViewKind { return v.kind }

func (v *mvccReadView) State() storageengine.ViewState {
	return v.token.State()
}

func (v *mvccReadView) Get(space string, key []byte) ([]byte, bool, error) {
	switch v.State() {
	case storageengine.ViewStateClosed:
		return nil, false, storageengine.ErrViewClosed
	case storageengine.ViewStateInvalidated:
		return nil, false, storageengine.ErrViewInvalidated
	}
	if v.txn != nil && v.txn.root().isolationLevel == storageengine.IsolationSerializable && !isMetadataSpace(space) {
		return nil, false, fmt.Errorf("%w: ReadView.Get on user data in SERIALIZABLE is unsupported", storageengine.ErrUnsupported)
	}
	if v.kind == storageengine.StatementViewKindUncommitted {
		// 1. Reader's own writes take highest priority
		if v.txn != nil && v.txn.inner != nil {
			obs, err := v.txn.inner.ReadCurrentUnlocked(context.Background(), space, key)
			if err == nil && obs.Source == mvcc.CurrentOwnOverlay {
				if obs.Tombstone {
					return nil, false, nil
				}
				return obs.Value, true, nil
			}
		}
		// 2. Foreign sampled dirty version
		if v.dirtySnapshot != nil {
			if dirty, ok := v.dirtySnapshot.Get(space, key); ok {
				if dirty.Deleted {
					return nil, false, nil
				}
				return dirty.Value, true, nil
			}
		}
	}
	var tx *mvcc.Tx
	if v.txn != nil {
		tx = v.txn.inner
	}
	return v.token.GetWithTx(space, key, tx)
}

type borrowedIterator struct {
	storageengine.Iterator
	cleanup func()
}

func (b *borrowedIterator) Close() error {
	defer b.cleanup()
	return b.Iterator.Close()
}

func inRange(key []byte, r storageengine.KeyRange) bool {
	if r.Lower != nil {
		cmp := bytes.Compare(key, r.Lower)
		if cmp < 0 || (cmp == 0 && !r.LowerInclusive) {
			return false
		}
	}
	if r.Upper != nil {
		cmp := bytes.Compare(key, r.Upper)
		if cmp > 0 || (cmp == 0 && !r.UpperInclusive) {
			return false
		}
	}
	return true
}

const (
	maxDirtyScanRows  = 65536
	maxDirtyScanBytes = 64 * 1024 * 1024
)

type dirtyBudgetTracker struct {
	rows     int
	bytes    int64
	maxRows  int
	maxBytes int64
}

func (b *dirtyBudgetTracker) addOrUpdate(oldRow *dirtyRow, newKey, newVal []byte) error {
	var oldCost int64
	newCost := int64(len(newKey) + len(newVal) + 64)
	if oldRow != nil {
		oldCost = int64(len(oldRow.key) + len(oldRow.value) + 64)
	} else {
		if b.rows+1 > b.maxRows {
			return fmt.Errorf("%w: read uncommitted scan materialization row limit exceeded (%d > %d)", storageengine.ErrViewBudgetLimit, b.rows+1, b.maxRows)
		}
	}
	netBytes := b.bytes - oldCost + newCost
	if netBytes > b.maxBytes {
		return fmt.Errorf("%w: read uncommitted scan materialization byte limit exceeded (%d > %d)", storageengine.ErrViewBudgetLimit, netBytes, b.maxBytes)
	}
	if oldRow == nil {
		b.rows++
	}
	b.bytes = netBytes
	return nil
}

func (b *dirtyBudgetTracker) delete(oldRow *dirtyRow) {
	if oldRow != nil {
		cost := int64(len(oldRow.key) + len(oldRow.value) + 64)
		b.bytes -= cost
		b.rows--
	}
}

func (v *mvccReadView) newDirtyIterator(ctx context.Context, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch v.State() {
	case storageengine.ViewStateClosed:
		return nil, storageengine.ErrViewClosed
	case storageengine.ViewStateInvalidated:
		return nil, storageengine.ErrViewInvalidated
	}
	if err := v.token.BeginBorrow(); err != nil {
		return nil, err
	}
	defer v.token.EndBorrow()

	bounds := mvcc.KeyRange{
		Lower:          r.Range.Lower,
		Upper:          r.Range.Upper,
		LowerInclusive: r.Range.LowerInclusive,
		UpperInclusive: r.Range.UpperInclusive,
		Reverse:        r.Range.Reverse,
	}

	var tx *mvcc.Tx
	if v.txn != nil {
		tx = v.txn.inner
	}

	tracker := dirtyBudgetTracker{
		maxRows:  maxDirtyScanRows,
		maxBytes: maxDirtyScanBytes,
	}

	combined := make(map[string]dirtyRow)

	err := v.token.ScanRangeWithTx(ctx, r.Space, bounds, tx, func(k, val []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tracker.addOrUpdate(nil, k, val); err != nil {
			return err
		}
		combined[string(k)] = dirtyRow{key: bytes.Clone(k), value: bytes.Clone(val)}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Overlay foreign sampled dirty entries
	if v.dirtySnapshot != nil {
		for _, d := range v.dirtySnapshot.SpaceEntries(r.Space) {
			if inRange(d.Key, r.Range) {
				sk := string(d.Key)
				var oldRow *dirtyRow
				if existing, ok := combined[sk]; ok {
					oldRow = &existing
				}
				if d.Deleted {
					if oldRow != nil {
						tracker.delete(oldRow)
						delete(combined, sk)
					}
				} else {
					if err := tracker.addOrUpdate(oldRow, d.Key, d.Value); err != nil {
						return nil, err
					}
					combined[sk] = dirtyRow{key: bytes.Clone(d.Key), value: bytes.Clone(d.Value)}
				}
			}
		}
	}

	// Overlay reader's own uncommitted writes
	if tx != nil {
		ownWrites, err := tx.CollectDirtyVersions()
		if err != nil {
			return nil, err
		}
		for _, op := range ownWrites {
			if op.Space == r.Space && inRange(op.Key, r.Range) {
				sk := string(op.Key)
				var oldRow *dirtyRow
				if existing, ok := combined[sk]; ok {
					oldRow = &existing
				}
				if op.Deleted {
					if oldRow != nil {
						tracker.delete(oldRow)
						delete(combined, sk)
					}
				} else {
					if err := tracker.addOrUpdate(oldRow, op.Key, op.Value); err != nil {
						return nil, err
					}
					combined[sk] = dirtyRow{key: bytes.Clone(op.Key), value: bytes.Clone(op.Value)}
				}
			}
		}
	}

	rows := make([]dirtyRow, 0, len(combined))
	for _, row := range combined {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		cmp := bytes.Compare(rows[i].key, rows[j].key)
		if r.Range.Reverse {
			return cmp > 0
		}
		return cmp < 0
	})

	return &inMemoryIterator{rows: rows}, nil
}

func (v *mvccReadView) NewIterator(ctx context.Context, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	switch v.State() {
	case storageengine.ViewStateClosed:
		return nil, storageengine.ErrViewClosed
	case storageengine.ViewStateInvalidated:
		return nil, storageengine.ErrViewInvalidated
	}
	if v.txn != nil && v.txn.root().isolationLevel == storageengine.IsolationSerializable && !isMetadataSpace(r.Space) {
		return nil, fmt.Errorf("%w: ReadView.NewIterator on user data in SERIALIZABLE is unsupported", storageengine.ErrUnsupported)
	}
	if v.kind == storageengine.StatementViewKindUncommitted && v.dirtySnapshot != nil {
		return v.newDirtyIterator(ctx, r)
	}
	if err := v.token.BeginBorrow(); err != nil {
		return nil, err
	}
	var endOnce sync.Once
	endBorrow := func() {
		endOnce.Do(func() {
			v.token.EndBorrow()
		})
	}
	it, err := newIterator(ctx, r, func(yield func([]byte, []byte) error) error {
		defer endBorrow()
		bounds := mvcc.KeyRange{
			Lower:          r.Range.Lower,
			Upper:          r.Range.Upper,
			LowerInclusive: r.Range.LowerInclusive,
			UpperInclusive: r.Range.UpperInclusive,
			Reverse:        r.Range.Reverse,
		}
		var tx *mvcc.Tx
		if v.txn != nil {
			tx = v.txn.inner
		}
		return v.token.ScanRangeWithTx(ctx, r.Space, bounds, tx, yield)
	})
	if err != nil {
		endBorrow()
		return nil, err
	}
	return &borrowedIterator{Iterator: it, cleanup: endBorrow}, nil
}

func (v *mvccReadView) Close() error {
	if v.txn != nil && v.txn.activeView == v {
		v.txn.activeView = nil
	}
	if v.isChildView {
		return nil
	}
	return v.token.Close()
}

func (e *engine) BeginIsolated(ctx context.Context, level storageengine.IsolationLevel) (storageengine.Txn, error) {
	if !level.Valid() {
		return nil, fmt.Errorf("invalid isolation level: %q", level)
	}
	if level == storageengine.IsolationReadUncommitted && (e.node != nil || e.Replica() != nil) {
		return nil, fmt.Errorf("%w: READ UNCOMMITTED is not supported in replication mode", storageengine.ErrUnsupported)
	}
	t, err := e.store.BeginLazy(ctx, e.proposer)
	if err != nil {
		return nil, err
	}
	return &txn{inner: t, engine: e, isolationLevel: level, ctx: ctx}, nil
}

func (t *txn) IsolationLevel() storageengine.IsolationLevel {
	if t.isolationLevel == "" {
		return storageengine.IsolationRepeatableRead
	}
	return t.isolationLevel
}

func (t *txn) BeginStatementView(ctx context.Context, kind storageengine.StatementViewKind) (storageengine.ReadView, error) {
	if t.inner == nil || t.inner.Closed() {
		if t.inner != nil && t.inner.DeadlockAborted() {
			return nil, storageengine.ErrDeadlock
		}
		return nil, storageengine.ErrClosed
	}
	if t.root().isolationLevel == storageengine.IsolationSerializable {
		return nil, fmt.Errorf("%w: StatementView on SERIALIZABLE transaction is unsupported; use LockingTxn or SQL executor", storageengine.ErrUnsupported)
	}
	if kind == storageengine.StatementViewKindUncommitted {
		if t.root().isolationLevel != storageengine.IsolationReadUncommitted {
			return nil, fmt.Errorf("%w: StatementViewKindUncommitted is only supported in READ UNCOMMITTED", storageengine.ErrUnsupported)
		}
		token, err := t.engine.store.BeginViewToken(ctx, t.inner)
		if err != nil {
			return nil, err
		}
		var snap *mvcc.DirtySnapshot
		if t.engine.store.DirtyRegistry() != nil {
			rootID := ""
			if t.root().inner != nil {
				rootID = t.root().inner.ID
			}
			snap, err = t.engine.store.DirtyRegistry().Sample(rootID)
			if err != nil {
				_ = token.Close()
				return nil, err
			}
		}
		v := &mvccReadView{token: token, kind: kind, txn: t, dirtySnapshot: snap}
		t.activeView = v
		return v, nil
	}
	if kind > storageengine.StatementViewKindUncommitted {
		return nil, fmt.Errorf("invalid statement view kind: %d", kind)
	}
	token, err := t.engine.store.BeginViewToken(ctx, t.inner)
	if err != nil {
		return nil, err
	}
	v := &mvccReadView{token: token, kind: kind, txn: t}
	t.activeView = v
	return v, nil
}

func (t *txn) BeginConsistentView(ctx context.Context) (storageengine.ReadView, error) {
	root := t.root()
	if root.inner == nil || root.inner.Closed() {
		if root.inner != nil && root.inner.DeadlockAborted() {
			return nil, storageengine.ErrDeadlock
		}
		return nil, storageengine.ErrClosed
	}
	if root.isolationLevel == storageengine.IsolationSerializable || root.isolationLevel == storageengine.IsolationReadUncommitted {
		return nil, fmt.Errorf("%w: ConsistentView on %s transaction is unsupported", storageengine.ErrUnsupported, root.isolationLevel)
	}

	rootView, err := root.ensureRootConsistentView(ctx)
	if err != nil {
		return nil, err
	}

	if t == root {
		t.activeView = rootView
		return rootView, nil
	}

	childView := &mvccReadView{
		token:       rootView.token,
		kind:        storageengine.StatementViewKindConsistent,
		txn:         t,
		isChildView: true,
	}
	t.activeView = childView
	return childView, nil
}

func (t *txn) ensureRootConsistentView(ctx context.Context) (*mvccReadView, error) {
	if t.consistentView != nil && t.consistentView.State() == storageengine.ViewStateActive {
		return t.consistentView, nil
	}
	token, err := t.engine.store.BeginViewToken(ctx, t.inner)
	if err != nil {
		return nil, err
	}
	v := &mvccReadView{token: token, kind: storageengine.StatementViewKindConsistent, txn: t}
	t.consistentView = v
	if t.inner != nil {
		t.engine.store.UpdateRootReadTS(t.inner.ID, token.Revision())
	}
	return v, nil
}

func (t *txn) ActiveView() (storageengine.ReadView, bool) {
	if t.activeView != nil && t.activeView.State() == storageengine.ViewStateActive {
		return t.activeView, true
	}
	root := t.root()
	if root != nil && root != t && root.activeView != nil && root.activeView.State() == storageengine.ViewStateActive {
		return root.activeView, true
	}
	return nil, false
}
