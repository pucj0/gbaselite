package mvcc

import (
	"context"
	"strings"
	"testing"
)

func TestInvalidRestorePreservesActiveTransaction(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = tx.Put("r", []byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	generation := s.generation.Load()
	if err = s.Restore(strings.NewReader("not a bbolt snapshot")); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if s.generation.Load() != generation {
		t.Fatal("invalid restore changed generation")
	}
	if _, err = tx.Commit(context.Background()); err != nil {
		t.Fatal("healthy transaction invalidated", err)
	}
	v, _, ok, err := s.Get(^uint64(0), "r", []byte("k"))
	if err != nil || !ok || string(v) != "v" {
		t.Fatal(string(v), err)
	}
}
