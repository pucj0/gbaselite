package mvcc

import (
	"bytes"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"testing"
)

func TestVersionLookupMatchesHistoricalVisibility(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	k, _ := key("rows", []byte("many"))
	empty, _ := key("rows", []byte("empty"))
	if err = s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.Bucket(dataBucket).CreateBucket(k)
		if err != nil {
			return err
		}
		if _, err = tx.Bucket(dataBucket).CreateBucket(empty); err != nil {
			return err
		}
		for n := uint64(1); n <= 300; n++ {
			value := append([]byte{1}, bytes.Repeat([]byte(fmt.Sprintf("%03d", n)), 30)...)
			if n%11 == 0 {
				value = []byte{0}
			}
			if err = b.Put(sequence(n), value); err != nil {
				return err
			}
			if n%3 != 0 {
				if err = tx.Bucket(commitsBucket).Put(sequence(n), []byte{1}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.db.View(func(tx *bolt.Tx) error {
		for snapshot := uint64(0); snapshot <= 401; snapshot++ {
			var want uint64
			for n := uint64(1); n <= 300 && n <= snapshot; n++ {
				if n%3 != 0 {
					want = n
				}
			}
			exists := want != 0 && want%11 != 0
			var value []byte
			if exists {
				value = bytes.Repeat([]byte(fmt.Sprintf("%03d", want)), 30)
			}
			r := newVisibilityReader(tx, snapshot)
			for _, cached := range []bool{false, true} {
				var v []byte
				var n uint64
				var ok bool
				if cached {
					v, n, ok = r.visible(k)
				} else {
					v, n, ok = visible(tx, k, snapshot)
				}
				if n != want || ok != exists || !bytes.Equal(v, value) {
					t.Fatalf("snapshot=%d cached=%v got=(%d,%v) want=(%d,%v)", snapshot, cached, n, ok, want, exists)
				}
			}
			if _, n, ok := r.visible(empty); n != 0 || ok {
				t.Fatal("empty bucket became visible")
			}
			if _, n, ok := visible(tx, []byte("rows\x00missing"), snapshot); n != 0 || ok {
				t.Fatal("missing row became visible")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
