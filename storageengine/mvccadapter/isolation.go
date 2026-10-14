package mvccadapter

import (
	"context"
	"fmt"
	"sync"

	"gbaselite/mvcc"
	"gbaselite/storageengine"
)

var _ storageengine.IsolationEngine = (*engine)(nil)
var _ storageengine.IsolationTxn = (*txn)(nil)
var _ storageengine.ReadView = (*mvccReadView)(nil)

type mvccReadView struct {
	token       *mvcc.ViewToken
	kind        storageengine.StatementViewKind
	txn         *txn
	isChildView bool
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

func (v *mvccReadView) NewIterator(ctx context.Context, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	switch v.State() {
	case storageengine.ViewStateClosed:
		return nil, storageengine.ErrViewClosed
	case storageengine.ViewStateInvalidated:
		return nil, storageengine.ErrViewInvalidated
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
	// In Phase 5, REPEATABLE READ and READ COMMITTED are executable by the storage backend.
	// RU and SERIALIZABLE must return ErrUnsupported without registering a root transaction.
	if level != storageengine.IsolationRepeatableRead && level != storageengine.IsolationReadCommitted {
		return nil, fmt.Errorf("%w: isolation level %s is not supported yet", storageengine.ErrUnsupported, level)
	}
	t, err := e.store.BeginLazy(ctx, e.proposer)
	if err != nil {
		return nil, err
	}
	return &txn{inner: t, engine: e, isolationLevel: level}, nil
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
	if kind == storageengine.StatementViewKindUncommitted {
		return nil, fmt.Errorf("%w: uncommitted statement view is not supported yet", storageengine.ErrUnsupported)
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
	if t.isolationLevel == storageengine.IsolationReadCommitted && t.inner != nil {
		t.inner.Snapshot = token.Revision()
		t.engine.store.UpdateRootReadTS(t.inner.ID, token.Revision())
	}
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
	return nil, false
}
