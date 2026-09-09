package mvcc

import (
	bolt "go.etcd.io/bbolt"
	"testing"
)

func TestVisibilityCacheIsViewLocalAndHandlesCollisions(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := key("row", []byte("a"))
	b, _ := key("row", []byte("b"))
	err = s.db.Update(func(tx *bolt.Tx) error {
		for _, k := range [][]byte{a, b} {
			bucket, e := tx.Bucket(dataBucket).CreateBucket(k)
			if e != nil {
				return e
			}
			if e = bucket.Put(sequence(1), []byte{1, 'o'}); e != nil {
				return e
			}
			if e = bucket.Put(sequence(17), []byte{1, 'n'}); e != nil {
				return e
			}
		}
		return tx.Bucket(commitsBucket).Put(sequence(1), []byte{1})
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(snapshot, want uint64, text string) {
		t.Helper()
		err := s.db.View(func(tx *bolt.Tx) error {
			r := newVisibilityReader(tx, snapshot)
			for i := 0; i < 4; i++ {
				for _, k := range [][]byte{a, b} {
					v, n, ok := r.visible(k)
					if !ok || n != want || string(v) != text {
						t.Fatalf("snapshot %d got %d %q %v", snapshot, n, v, ok)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check(100, 1, "o") // 1 and 17 collide; unpublished 17 must remain invisible.
	if err = s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(commitsBucket).Put(sequence(17), []byte{1}) }); err != nil {
		t.Fatal(err)
	}
	check(100, 17, "n") // A fresh physical view must not reuse a cached false marker.
	check(1, 1, "o")    // Publication must not change an old logical snapshot.
	if err = s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(dataBucket).Bucket(a).Put(sequence(33), []byte{0}); err != nil {
			return err
		}
		return tx.Bucket(commitsBucket).Put(sequence(33), []byte{1})
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.db.View(func(tx *bolt.Tx) error {
		r := newVisibilityReader(tx, 100)
		_, n, ok := r.visible(a)
		if ok || n != 33 {
			t.Fatal("tombstone", n, ok)
		}
		v, n, ok := r.visible(b)
		if !ok || n != 17 || string(v) != "n" {
			t.Fatal("collision after tombstone", n, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
