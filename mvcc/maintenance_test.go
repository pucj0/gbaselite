package mvcc

import (
	"bytes"
	"context"
	bolt "go.etcd.io/bbolt"
	"testing"
)

func TestSnapshotRestoreAndHistoryRetention(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	put := func(value string) {
		tx, err := s.Begin(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = tx.Put("rows", []byte("k"), []byte(value)); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	put("old")
	old, err := s.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback()
	put("new")
	if err = s.CompactHistory(ctx); err != nil {
		t.Fatal(err)
	}
	v, ok, err := old.Get("rows", []byte("k"))
	if err != nil || !ok || string(v) != "old" {
		t.Fatalf("pinned history: %s %v", v, err)
	}
	old.Rollback()
	if err = s.CompactHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if size, err := s.PendingBytes(); err != nil || size != 0 {
		t.Fatalf("pending retained: %d %v", size, err)
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if _, err = snapshot.WriteTo(&data); err != nil {
		t.Fatal(err)
	}
	snapshot.Rollback()
	target, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err = target.Restore(bytes.NewReader(data.Bytes())); err != nil {
		t.Fatal(err)
	}
	v, _, ok, err = target.Get(^uint64(0), "rows", []byte("k"))
	if err != nil || !ok || string(v) != "new" {
		t.Fatalf("restored: %s %v", v, err)
	}
	tx, err := target.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = target.Restore(bytes.NewReader(data.Bytes())); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tx.Get("rows", []byte("k")); err == nil {
		t.Fatal("old transaction survived restore")
	}
	tx.Rollback()
}

func TestUnpublishedRowsRemainInvisibleAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ops := []Op{{Space: "rows", Key: []byte("a"), Value: []byte("one")}, {Space: "rows", Key: []byte("b"), Value: []byte("two")}}
	if _, err = s.Apply(1, Command{Kind: "stage", ID: "replay", Ops: ops}); err != nil {
		t.Fatal(err)
	}
	if err = s.db.Update(func(tx *bolt.Tx) error {
		k, _ := key("rows", []byte("a"))
		b, err := tx.Bucket(dataBucket).CreateBucket(k)
		if err != nil {
			return err
		}
		return b.Put(sequence(2), append([]byte{1}, []byte("one")...))
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, ok, err := s.Get(^uint64(0), "rows", []byte("a")); err != nil || ok {
		t.Fatalf("uncommitted row became visible: %v %v", ok, err)
	}
	result, err := s.Apply(2, Command{Kind: "commit", ID: "replay"})
	if err != nil || result.Err() != nil {
		t.Fatalf("replay %+v %v", result, err)
	}
	for _, op := range ops {
		value, _, ok, err := s.Get(^uint64(0), op.Space, op.Key)
		if err != nil || !ok || !bytes.Equal(value, op.Value) {
			t.Fatalf("replay value %s %v", value, err)
		}
	}
}
