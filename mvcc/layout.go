package mvcc

import (
	"bytes"
	"errors"
	bolt "go.etcd.io/bbolt"
)

var flatVersionsBucket = []byte("flat_versions_v1")
var layoutKey = []byte("version_layout")

func flatLayout(tx *bolt.Tx) bool {
	return bytes.Equal(tx.Bucket(metaBucket).Get(layoutKey), []byte{1})
}
func validateVersionLayout(tx *bolt.Tx) error {
	v := tx.Bucket(metaBucket).Get(layoutKey)
	if len(v) == 0 {
		return nil
	}
	if !bytes.Equal(v, []byte{1}) || tx.Bucket(flatVersionsBucket) == nil {
		return errors.New("unsupported/incomplete MVCC version layout")
	}
	return nil
}
func flatPrefix(k []byte) []byte {
	p := make([]byte, 0, len(k)+2)
	for _, v := range k {
		p = append(p, v)
		if v == 0 {
			p = append(p, 255)
		}
	}
	return append(p, 0, 0)
}
func putVersion(tx *bolt.Tx, k, version, value []byte) error {
	root := tx.Bucket(dataBucket)
	if flatLayout(tx) {
		if root.Get(k) == nil {
			if err := root.Put(k, []byte{1}); err != nil {
				return err
			}
		}
		return tx.Bucket(flatVersionsBucket).Put(append(flatPrefix(k), version...), value)
	}
	b, err := root.CreateBucketIfNotExists(k)
	if err != nil {
		return err
	}
	return b.Put(version, value)
}
func flatVisible(tx *bolt.Tx, k []byte, snapshot uint64) ([]byte, uint64, bool) {
	prefix := flatPrefix(k)
	seek := append(bytes.Clone(prefix), sequence(snapshot)...)
	c := tx.Bucket(flatVersionsBucket).Cursor()
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
