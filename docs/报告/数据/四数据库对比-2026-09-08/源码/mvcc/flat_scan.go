package mvcc

import (
	"bytes"
	bolt "go.etcd.io/bbolt"
)

// flatScanReader belongs to one physical read transaction and consumes directory
// keys in ascending order. Returned values remain borrowed from that transaction.
// Short histories walk adjacent leaves; long histories retain snapshot seeks.
type flatScanReader struct {
	seekOnly                 bool
	reader                   *visibilityReader
	cursor                   *bolt.Cursor
	prefix, current, payload []byte
	started                  bool
}

func (r *flatScanReader) visible(k []byte) ([]byte, uint64, bool) {
	if r.seekOnly {
		return flatVisible(r.reader.tx, k, r.reader.snapshot)
	}
	r.prefix = r.prefix[:0]
	for _, b := range k {
		r.prefix = append(r.prefix, b)
		if b == 0 {
			r.prefix = append(r.prefix, 255)
		}
	}
	r.prefix = append(r.prefix, 0, 0)
	if !r.started || r.current != nil && bytes.Compare(r.current, r.prefix) < 0 {
		r.current, r.payload = r.cursor.Seek(r.prefix)
		r.started = true
	}
	var value []byte
	var version uint64
	var live bool
	examined := 0
	for len(r.current) == len(r.prefix)+8 && bytes.HasPrefix(r.current, r.prefix) {
		n := number(r.current[len(r.prefix):])
		if n > r.reader.snapshot {
			r.seekOnly = true
			break
		}
		// Limit extra history walking before falling back to the existing seek path.
		if examined == 4 {
			value, version, live = flatVisible(r.reader.tx, k, r.reader.snapshot)
			r.seekOnly = true
			break
		}
		examined++
		entry := &r.reader.cache[n%uint64(len(r.reader.cache))]
		if !entry.known || entry.version != n {
			entry.version, entry.known = n, true
			entry.committed = r.reader.commits.Get(r.current[len(r.prefix):]) != nil
		}
		if entry.committed {
			version = n
			live = len(r.payload) > 0 && r.payload[0] != 0
			value = nil
			if live {
				value = r.payload[1:]
			}
		}
		r.current, r.payload = r.cursor.Next()
	}
	return value, version, live
}
