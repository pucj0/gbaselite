package mvccadapter

import (
	"context"
	"fmt"
	"gbaselite/mvcc"
	"gbaselite/storageengine"
)

var _ storageengine.IsolationEngine = (*engine)(nil)
var _ storageengine.IsolationTxn = (*txn)(nil)
var _ storageengine.ReadView = (*mvccReadView)(nil)

type mvccReadView struct {
	token *mvcc.ViewToken
	kind  storageengine.StatementViewKind
	txn   *txn
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
	return v.token.Get(space, key)
}

func (v *mvccReadView) NewIterator(ctx context.Context, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	return newIterator(ctx, r, func(yield func([]byte, []byte) error) error {
		bounds := mvcc.KeyRange{
			Lower:          r.Range.Lower,
			Upper:          r.Range.Upper,
			LowerInclusive: r.Range.LowerInclusive,
			UpperInclusive: r.Range.UpperInclusive,
			Reverse:        r.Range.Reverse,
		}
		return v.token.ScanRange(ctx, r.Space, bounds, yield)
	})
}

func (v *mvccReadView) Close() error {
	if v.txn != nil && v.txn.activeView == v {
		v.txn.activeView = nil
	}
	return v.token.Close()
}

func (e *engine) BeginIsolated(ctx context.Context, level storageengine.IsolationLevel) (storageengine.Txn, error) {
	if !level.Valid() {
		return nil, fmt.Errorf("invalid isolation level: %q", level)
	}
	// In Phase 3, only REPEATABLE READ is executable by the storage backend.
	// RU, RC, and SERIALIZABLE must return ErrUnsupported without registering a root transaction.
	if level != storageengine.IsolationRepeatableRead {
		return nil, fmt.Errorf("%w: isolation level %s is not supported yet", storageengine.ErrUnsupported, level)
	}
	t, err := e.store.Begin(ctx, e.proposer)
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
	return v, nil
}

func (t *txn) BeginConsistentView(ctx context.Context) (storageengine.ReadView, error) {
	if t.inner == nil || t.inner.Closed() {
		if t.inner != nil && t.inner.DeadlockAborted() {
			return nil, storageengine.ErrDeadlock
		}
		return nil, storageengine.ErrClosed
	}
	if t.consistentView != nil && t.consistentView.State() == storageengine.ViewStateActive {
		t.activeView = t.consistentView
		return t.consistentView, nil
	}
	token, err := t.engine.store.BeginViewTokenAt(ctx, t.inner, t.inner.Snapshot)
	if err != nil {
		return nil, err
	}
	v := &mvccReadView{token: token, kind: storageengine.StatementViewKindConsistent, txn: t}
	t.consistentView = v
	t.activeView = v
	return v, nil
}

func (t *txn) ActiveView() (storageengine.ReadView, bool) {
	if t.activeView != nil && t.activeView.State() == storageengine.ViewStateActive {
		return t.activeView, true
	}
	return nil, false
}
