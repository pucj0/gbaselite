package storageengine

import (
	"context"
	"gbaselite/sqllayout"
)

type spaceHandle struct {
	tx    Txn
	space string
}
type tableHandle struct {
	spaceHandle
	id string
}

// TODO: remove relational convenience APIs from storageengine after all callers
// use Txn namespaces directly. Keep this bridge for existing backend contracts.
// BindTable is the legacy table-handle convenience API; namespace rules are
// owned by sqllayout, not by the backend transaction implementation.
func BindTable(tx Txn, id string) Table { return &tableHandle{spaceHandle{tx, sqllayout.Rows(id)}, id} }
func (t *tableHandle) Index(name string, kind IndexKind) Index {
	space := sqllayout.UniqueIndex(t.id, name)
	if kind == SecondaryIndex {
		space = sqllayout.SecondaryIndex(t.id, name)
	}
	return &spaceHandle{t.tx, space}
}
func (s *spaceHandle) Get(k []byte) ([]byte, bool, error) { return s.tx.Get(s.space, k) }
func (s *spaceHandle) Put(k, v []byte) error              { return s.tx.Put(s.space, k, v) }
func (s *spaceHandle) Delete(k []byte) error              { return s.tx.Delete(s.space, k) }
func (s *spaceHandle) Guard(k []byte) error               { return s.tx.Guard(s.space, k) }
func (s *spaceHandle) GuardRange(bounds KeyRange) error   { return s.tx.GuardRange(s.space, bounds) }
func (s *spaceHandle) Scan(ctx context.Context, r ScanRequest) (Iterator, error) {
	r.Space = s.space
	return s.tx.NewIterator(ctx, r)
}
