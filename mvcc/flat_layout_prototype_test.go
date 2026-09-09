package mvcc

// This prototype is deliberately test-only. Production databases retain their
// nested layout until range scans, restore and upgrade recovery are validated.
import (
	"bytes"
	"encoding/binary"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"path/filepath"
	"testing"
	"time"
)

var prototypeFlat = []byte("flat-versions")

func prototypePrefix(k []byte) []byte {
	p := make([]byte, 0, len(k)+2)
	for _, v := range k {
		p = append(p, v)
		if v == 0 {
			p = append(p, 255)
		}
	}
	return append(p, 0, 0)
}
func prototypeVisible(tx *bolt.Tx, k []byte, snapshot uint64) ([]byte, uint64, bool) {
	prefix := prototypePrefix(k)
	seek := append(bytes.Clone(prefix), sequence(snapshot)...)
	c := tx.Bucket(prototypeFlat).Cursor()
	v, p := c.Seek(seek)
	if v == nil {
		v, p = c.Last()
	} else if bytes.Compare(v, seek) > 0 {
		v, p = c.Prev()
	}
	for ; v != nil && len(v) == len(prefix)+8 && bytes.HasPrefix(v, prefix); v, p = c.Prev() {
		n := number(v[len(prefix):])
		if tx.Bucket(commitsBucket).Get(sequence(n)) == nil {
			continue
		}
		if len(p) == 0 || p[0] == 0 {
			return nil, n, false
		}
		return p[1:], n, true
	}
	return nil, 0, false
}
func TestFlatVersionLayoutPrototype(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	keys := [][]byte{[]byte("r"), []byte("r\x00"), []byte("r\x00\xff"), []byte("ra")}
	err = s.db.Update(func(tx *bolt.Tx) error {
		flat, err := tx.CreateBucket(prototypeFlat)
		if err != nil {
			return err
		}
		for _, k := range keys {
			row, err := tx.Bucket(dataBucket).CreateBucket(k)
			if err != nil {
				return err
			}
			for n := uint64(1); n <= 31; n++ {
				value := append([]byte{1}, byte(n))
				if n%7 == 0 {
					value = []byte{0}
				}
				if err = row.Put(sequence(n), value); err != nil {
					return err
				}
				if err = flat.Put(append(prototypePrefix(k), sequence(n)...), value); err != nil {
					return err
				}
				if n%3 != 0 {
					if err = tx.Bucket(commitsBucket).Put(sequence(n), []byte{1}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.db.View(func(tx *bolt.Tx) error {
		for _, k := range append(keys, []byte("missing")) {
			for n := uint64(0); n < 40; n++ {
				a, av, ao := visible(tx, k, n)
				b, bv, bo := prototypeVisible(tx, k, n)
				if !bytes.Equal(a, b) || av != bv || ao != bo {
					return fmt.Errorf("key %x snapshot %d mismatch", k, n)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func BenchmarkVersionLayoutPrototype(b *testing.B) {
	for _, flat := range []bool{false, true} {
		name := "nested"
		if flat {
			name = "flat"
		}
		b.Run(name, func(b *testing.B) {
			db, err := bolt.Open(filepath.Join(b.TempDir(), "prototype.db"), 0600, &bolt.Options{NoFreelistSync: true})
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			const rows = 10000
			start := time.Now()
			for base := 0; base < rows; base += 128 {
				err = db.Update(func(tx *bolt.Tx) error {
					root, err := tx.CreateBucketIfNotExists(dataBucket)
					if err != nil {
						return err
					}
					f, err := tx.CreateBucketIfNotExists(prototypeFlat)
					if err != nil {
						return err
					}
					commits, err := tx.CreateBucketIfNotExists(commitsBucket)
					if err != nil {
						return err
					}
					for i := base; i < base+128 && i < rows; i++ {
						var k [8]byte
						binary.BigEndian.PutUint64(k[:], uint64(i))
						var bucket *bolt.Bucket
						if !flat {
							bucket, err = root.CreateBucket(k[:])
							if err != nil {
								return err
							}
						}
						for n := uint64(1); n <= 3; n++ {
							value := append([]byte{1}, bytes.Repeat([]byte{byte(n)}, 128)...)
							if flat {
								err = f.Put(append(prototypePrefix(k[:]), sequence(n)...), value)
							} else {
								err = bucket.Put(sequence(n), value)
							}
							if err != nil {
								return err
							}
							if err = commits.Put(sequence(n), []byte{1}); err != nil {
								return err
							}
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
			seed := time.Since(start)
			var size int64
			_ = db.View(func(tx *bolt.Tx) error { size = tx.Size(); return nil })
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var k [8]byte
				binary.BigEndian.PutUint64(k[:], uint64(i%rows))
				err = db.View(func(tx *bolt.Tx) error {
					var n uint64
					var ok bool
					if flat {
						_, n, ok = prototypeVisible(tx, k[:], 2)
					} else {
						_, n, ok = visible(tx, k[:], 2)
					}
					if !ok || n != 2 {
						return fmt.Errorf("wrong snapshot")
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(seed.Nanoseconds())/rows, "seed-ns/row")
			b.ReportMetric(float64(size)/rows, "db-bytes/row")
		})
	}
}
