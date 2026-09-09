package mvcc

import (
	"bytes"
	"context"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"testing"
)

func TestLocalStreamRejectsMalformedTailBeforeInstall(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedLargeTest(t, s)
	tx := largeUpdateTest(t, s)
	defer tx.Rollback()
	if err = tx.flushWrites(); err != nil {
		t.Fatal(err)
	}
	before := s.localSeq
	if err = tx.stage.Update(func(b *bolt.Tx) error { return b.Bucket([]byte("writes")).Put([]byte("zz\x00tail"), []byte{1, 4}) }); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Commit(context.Background()); err == nil {
		t.Fatal("accepted malformed tail")
	}
	if s.localSeq != before {
		t.Fatal("reserved sequence before full validation")
	}
	if err = s.db.View(func(b *bolt.Tx) error {
		m := b.Bucket(metaBucket)
		if number(m.Get([]byte("allocated"))) > before {
			t.Fatal("installed before full validation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		v, _, ok, err := s.Get(^uint64(0), "rows", []byte(fmt.Sprintf("%03d", i)))
		if err != nil || !ok || !bytes.Equal(v, []byte("old")) {
			t.Fatal(i, err)
		}
	}
}

func TestLocalStreamBorrowedBatchesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	seedLargeTest(t, s)
	tx, err := s.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 100; i++ {
		k := []byte(fmt.Sprintf("%03d", i))
		if i%7 == 0 {
			err = tx.Delete("rows", k)
		} else {
			err = tx.Put("rows", k, bytes.Repeat([]byte{byte(i)}, 4000+i))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Guard("rows", []byte("guard")); err != nil {
		t.Fatal(err)
	}
	if err = tx.Put("catalog", []byte("tail"), []byte("definition")); err != nil {
		t.Fatal(err)
	}
	index, err := tx.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		v, _, ok, err := s.Get(^uint64(0), "rows", []byte(fmt.Sprintf("%03d", i)))
		if err != nil || ok != (i%7 != 0) {
			t.Fatal(i, ok, err)
		}
		if ok && !bytes.Equal(v, bytes.Repeat([]byte{byte(i)}, 4000+i)) {
			t.Fatal("borrowed value corrupted", i)
		}
	}
	if _, _, ok, err := s.Get(^uint64(0), "rows", []byte("guard")); err != nil || ok {
		t.Fatal("guard became row", err)
	}
	if v, _, ok, err := s.Get(^uint64(0), "catalog", []byte("tail")); err != nil || !ok || string(v) != "definition" {
		t.Fatal("catalog", err)
	}
	if err = s.db.View(func(b *bolt.Tx) error {
		if number(b.Bucket(metaBucket).Get([]byte("catalog_head"))) != index {
			t.Fatal("catalog publication")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
