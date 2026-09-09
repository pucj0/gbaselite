package mvcc

import (
	"bytes"
	"context"
	bolt "go.etcd.io/bbolt"
	"testing"
)

type maintenanceContext struct {
	context.Context
	store  *Store
	action func()
	ran    bool
}

func (c *maintenanceContext) Err() error {
	if !c.ran && c.store.apply.TryLock() {
		c.store.apply.Unlock()
		var count int
		_ = c.store.db.View(func(tx *bolt.Tx) error {
			k, _ := key("rows", []byte("k"))
			count = tx.Bucket(dataBucket).Bucket(k).Stats().KeyN
			return nil
		})
		if count < 12 {
			c.ran = true
			c.action()
		}
	}
	return nil
}
func TestMaintenanceYieldsBetweenVersionsAndPreservesSnapshot(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	put := func(value byte) {
		tx, err := s.Begin(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = tx.Put("rows", []byte("k"), bytes.Repeat([]byte{value}, 32<<10)); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		put(byte(i))
	}
	old, err := s.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback()
	ctx := &maintenanceContext{Context: context.Background(), store: s, action: func() { put(99) }}
	if err = s.CompactHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if !ctx.ran {
		t.Fatal("no opportunity to commit between batches")
	}
	v, ok, err := old.Get("rows", []byte("k"))
	if err != nil || !ok || v[0] != 11 {
		t.Fatal("snapshot lost", err)
	}
	latest, _, ok, err := s.Get(^uint64(0), "rows", []byte("k"))
	if err != nil || !ok || latest[0] != 99 {
		t.Fatal("concurrent write lost", err)
	}
}
