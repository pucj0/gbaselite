package mvcc

import (
	"bytes"
	"context"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"testing"
)

func TestLayoutMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var snapshots []uint64
	for round := 0; round < 3; round++ {
		tx, err := s.Begin(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 300; i++ {
			k := []byte(fmt.Sprintf("%04d\x00x", i))
			if round == 2 && i%3 == 0 {
				err = tx.Delete("r", k)
			} else {
				err = tx.Put("r", k, bytes.Repeat([]byte{byte(round + 1)}, 1000))
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		n, err := tx.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, n)
	}
	target := filepath.Join(root, "flat")
	if err = s.ExportLayout(ctx, target, "flat"); err != nil {
		t.Fatal(err)
	}
	flat, err := OpenWithOptions(target, Options{LocalWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer flat.Close()
	check := func(other *Store) {
		t.Helper()
		for _, snap := range snapshots {
			for _, reverse := range []bool{false, true} {
				var want, got []string
				bounds := KeyRange{Reverse: reverse}
				if err := s.ScanRange(ctx, snap, "r", bounds, func(k, v []byte) error { want = append(want, string(k)+string(v)); return nil }); err != nil {
					t.Fatal(err)
				}
				if err := other.ScanRange(ctx, snap, "r", bounds, func(k, v []byte) error { got = append(got, string(k)+string(v)); return nil }); err != nil {
					t.Fatal(err)
				}
				if fmt.Sprint(want) != fmt.Sprint(got) {
					t.Fatalf("migration scan mismatch snapshot=%d reverse=%v", snap, reverse)
				}
			}
			for i := 0; i < 300; i++ {
				k := []byte(fmt.Sprintf("%04d\x00x", i))
				a, an, ao, err := s.Get(snap, "r", k)
				if err != nil {
					t.Fatal(err)
				}
				b, bn, bo, err := other.Get(snap, "r", k)
				if err != nil || an != bn || ao != bo || !bytes.Equal(a, b) {
					t.Fatalf("snapshot %d row %d mismatch: %v", snap, i, err)
				}
			}
		}
	}
	check(flat)
	reverse := filepath.Join(root, "nested")
	if err = flat.ExportLayout(ctx, reverse, "nested"); err != nil {
		t.Fatal(err)
	}
	nested, err := Open(reverse)
	if err != nil {
		t.Fatal(err)
	}
	check(nested)
	nested.Close()
	old, err := flat.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback()
	tx, err := flat.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Put("r", []byte("0001\x00x"), []byte("new")); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = flat.CompactHistory(ctx); err != nil {
		t.Fatal(err)
	}
	v, ok, err := old.Get("r", []byte("0001\x00x"))
	if err != nil || !ok || !bytes.Equal(v, bytes.Repeat([]byte{3}, 1000)) {
		t.Fatal("GC lost pinned snapshot", err)
	}
	if err = s.ExportLayout(ctx, target, "flat"); err == nil {
		t.Fatal("overwrote destination")
	}
	if err = flat.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v, _, ok, err = reopened.Get(^uint64(0), "r", []byte("0001\x00x"))
	if err != nil || !ok || string(v) != "new" {
		t.Fatal("reopen lost write", err)
	}
}
func TestIncompleteLayoutRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "migration.incomplete"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatal("opened incomplete migration")
	}
}

func TestLayoutMigrationRejectsOrphan(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket(flatVersionsBucket)
		if err != nil {
			return err
		}
		if err = tx.Bucket(metaBucket).Put(layoutKey, []byte{1}); err != nil {
			return err
		}
		return b.Put(append(flatPrefix([]byte("r\x00lost")), sequence(1)...), []byte{1, 'x'})
	})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err = s.ExportLayout(context.Background(), target, "nested"); err == nil {
		t.Fatal("silently dropped orphan")
	}
	if other, err := Open(target); err == nil {
		other.Close()
		t.Fatal("opened incomplete migration")
	}
}
