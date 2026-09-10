package mvcc

import (
	"context"
	"errors"
	"testing"
)

func TestRangeGuardDetectsPhantoms(t *testing.T) {
	for _, wal := range []bool{false, true} {
		s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: wal})
		if err != nil {
			t.Fatal(err)
		}
		old, err := s.Begin(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = old.GuardRange("rows", KeyRange{}); err != nil {
			t.Fatal(err)
		}
		writer, err := s.Begin(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = writer.Put("rows", []byte("new"), []byte("v")); err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err = old.Commit(context.Background()); !errors.Is(err, ErrConflict) {
			t.Fatal("missed inserted row", wal, err)
		}
		s.Close()
	}
}
