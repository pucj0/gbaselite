package mvcc

import (
	"bytes"
	"context"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"path/filepath"
	"testing"
)

func flatScanFixture(t testing.TB, rows, history int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	err = s.db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucket(flatVersionsBucket); err != nil {
			return err
		}
		if err := tx.Bucket(metaBucket).Put(layoutKey, []byte{1}); err != nil {
			return err
		}
		for n := 1; n <= history; n++ {
			if n%3 != 0 {
				if err := tx.Bucket(commitsBucket).Put(sequence(uint64(n)), []byte{1}); err != nil {
					return err
				}
			}
		}
		for i := 0; i < rows; i++ {
			// Directory-only rows, embedded zeros, prefix-related keys and long values.
			k := []byte(fmt.Sprintf("r\x00%06d", i/3))
			k = append(k, bytes.Repeat([]byte{0}, i%3)...)
			if err := tx.Bucket(dataBucket).Put(k, []byte{1}); err != nil {
				return err
			}
			if i%17 == 0 {
				continue
			}
			for n := 1; n <= history; n++ {
				value := append([]byte{1}, bytes.Repeat([]byte{byte(n)}, 300)...)
				if (i+n)%7 == 0 {
					value = []byte{0}
				}
				if err := putVersion(tx, k, sequence(uint64(n)), value); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFlatSequentialVisibility(t *testing.T) {
	for _, history := range []int{1, 3, 31} {
		s := flatScanFixture(t, 600, history)
		for _, snapshot := range []uint64{0, 1, 2, 3, 4, 5, 7, 16, 30, 31, 32, ^uint64(0)} {
			err := s.db.View(func(tx *bolt.Tx) error {
				reader := newVisibilityReader(tx, snapshot)
				scan := flatScanReader{reader: &reader, cursor: tx.Bucket(flatVersionsBucket).Cursor()}
				c := tx.Bucket(dataBucket).Cursor()
				for k, _ := c.First(); k != nil; k, _ = c.Next() {
					want, wn, wo := flatVisible(tx, k, snapshot)
					got, gn, goLive := scan.visible(k)
					if wn != gn || wo != goLive || !bytes.Equal(want, got) {
						t.Fatalf("snapshot=%d key=%x got=(%d,%v) want=(%d,%v)", snapshot, k, gn, goLive, wn, wo)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, bounds := range []KeyRange{{}, {Lower: []byte("000050"), Upper: []byte("000150"), LowerInclusive: true}, {Lower: []byte("000050"), Upper: []byte("000150"), UpperInclusive: true}} {
				var want, got []string
				err = s.db.View(func(tx *bolt.Tx) error {
					c := tx.Bucket(dataBucket).Cursor()
					for k, _ := c.First(); k != nil; k, _ = c.Next() {
						if !rangeContains(k, []byte("r\x00"), bounds) {
							continue
						}
						if v, _, ok := flatVisible(tx, k, snapshot); ok {
							want = append(want, string(k[2:])+":"+string(v))
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				err = s.ScanRange(context.Background(), snapshot, "r", bounds, func(k, v []byte) error { got = append(got, string(k)+":"+string(v)); return nil })
				if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("paged snapshot=%d: err=%v got=%d want=%d", snapshot, err, len(got), len(want))
				}
			}
		}
	}

}

func BenchmarkFlatSequentialVisibility(b *testing.B) {
	for _, history := range []int{1, 3, 31} {
		b.Run(fmt.Sprintf("history%d", history), func(b *testing.B) {
			s := flatScanFixture(b, 10000, history)
			for _, sequential := range []bool{false, true} {
				b.Run(fmt.Sprintf("sequential%v", sequential), func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						err := s.db.View(func(tx *bolt.Tx) error {
							c := tx.Bucket(dataBucket).Cursor()
							var reader visibilityReader
							var scan flatScanReader
							count := 0
							for k, _ := c.First(); k != nil; k, _ = c.Next() {
								if count%256 == 0 {
									reader = newVisibilityReader(tx, uint64(history))
									scan = flatScanReader{reader: &reader, cursor: tx.Bucket(flatVersionsBucket).Cursor()}
								}
								if sequential {
									scan.visible(k)
								} else {
									reader.visible(k)
								}
								count++
							}
							return nil
						})
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func TestFlatScanSnapshotAndOverlay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source, err := Open(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	seed, err := source.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if err = seed.Put("r", []byte(fmt.Sprintf("%04d", i)), []byte("old")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = source.ExportLayout(ctx, filepath.Join(root, "flat"), "flat"); err != nil {
		t.Fatal(err)
	}
	flat, err := Open(filepath.Join(root, "flat"))
	if err != nil {
		t.Fatal(err)
	}
	defer flat.Close()
	old, err := flat.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback()
	count := 0
	err = old.ScanRange(ctx, "r", KeyRange{}, func(k, v []byte) error {
		if string(v) != "old" {
			t.Fatalf("snapshot saw %q", v)
		}
		count++
		if count == 1 {
			writer, e := flat.Begin(ctx, nil)
			if e != nil {
				return e
			}
			defer writer.Rollback()
			if e = writer.Put("r", []byte("0599"), []byte("new")); e != nil {
				return e
			}
			if e = writer.Delete("r", []byte("0500")); e != nil {
				return e
			}
			_, e = writer.Commit(ctx)
			return e
		}
		return nil
	})
	if err != nil || count != 600 {
		t.Fatalf("old scan %d: %v", count, err)
	}
	own, err := flat.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer own.Rollback()
	if err = own.Put("r", []byte("0599"), []byte("own")); err != nil {
		t.Fatal(err)
	}
	if err = own.Delete("r", []byte("0001")); err != nil {
		t.Fatal(err)
	}
	if err = own.Put("r", []byte("0600"), []byte("added")); err != nil {
		t.Fatal(err)
	}
	count = 0
	err = own.ScanRange(ctx, "r", KeyRange{}, func(k, v []byte) error {
		count++
		want := "old"
		switch string(k) {
		case "0001", "0500":
			t.Fatalf("deleted key %s", k)
		case "0599":
			want = "own"
		case "0600":
			want = "added"
		}
		if string(v) != want {
			t.Fatalf("overlay %s=%s want %s", k, v, want)
		}
		return nil
	})
	if err != nil || count != 599 {
		t.Fatalf("overlay scan %d: %v", count, err)
	}
}
