package mvcc

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestSnapshotRowsAndIndependentWriters(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	seed, _ := s.Begin(ctx, nil)
	seed.Put("rows", []byte("a"), []byte("0"))
	seed.Put("rows", []byte("b"), []byte("0"))
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Begin(ctx, nil)
	b, _ := s.Begin(ctx, nil)
	old, _ := s.Begin(ctx, nil)
	defer old.Rollback()
	a.Put("rows", []byte("a"), []byte("1"))
	b.Put("rows", []byte("b"), []byte("2"))
	if _, err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Commit(ctx); err != nil {
		t.Fatal("different rows conflicted", err)
	}
	v, _, _ := old.Get("rows", []byte("a"))
	if string(v) != "0" {
		t.Fatal("snapshot changed")
	}
	a, _ = s.Begin(ctx, nil)
	b, _ = s.Begin(ctx, nil)
	a.Put("rows", []byte("a"), []byte("3"))
	b.Put("rows", []byte("a"), []byte("4"))
	a.Commit(ctx)
	if _, err := b.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("lost update accepted", err)
	}
}
func TestLargeWriteStreamsAndReopens(t *testing.T) {
	directory := t.TempDir()
	s, _ := Open(directory)
	ctx := context.Background()
	tx, _ := s.Begin(ctx, nil)
	for i := 0; i < 2000; i++ {
		if err := tx.Put("rows", []byte(fmt.Sprintf("%08d", i)), make([]byte, 1024)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	read, _ := s.Begin(ctx, nil)
	defer read.Rollback()
	count := 0
	if err := read.Scan(ctx, "rows", func(k, v []byte) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 2000 {
		t.Fatal(count)
	}
}
func TestStatementChildRollbackAndMerge(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	ctx := context.Background()
	tx, _ := s.Begin(ctx, nil)
	defer tx.Rollback()
	tx.Put("x", []byte("a"), []byte("1"))
	child, _ := tx.Child()
	child.Put("x", []byte("a"), []byte("2"))
	child.Rollback()
	v, _, _ := tx.Get("x", []byte("a"))
	if string(v) != "1" {
		t.Fatal(string(v))
	}
	child, _ = tx.Child()
	child.Put("x", []byte("b"), []byte("3"))
	if _, err := child.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	count := 0
	tx.Scan(ctx, "x", func(k, v []byte) error { count++; return nil })
	if count != 2 {
		t.Fatal(count)
	}
}
