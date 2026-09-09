package mvcc

import (
	"bytes"
	"context"
	"gbaselite/storageengine"
	bolt "go.etcd.io/bbolt"
)

// KeyRange bounds raw keys within one namespace. Nil bounds are unbounded.
// Stats, when supplied, belongs to this call and must not be shared concurrently.
type KeyRange = storageengine.KeyRange
type ScanStats = storageengine.ScanStats
type rangeEntry struct {
	key, value     []byte
	deleted, check bool
}
type rangePage struct {
	entries []rangeEntry
	after   []byte
	done    bool
}

func rangeCursor(c *bolt.Cursor, prefix []byte, r KeyRange, after []byte) []byte {
	var k []byte
	if !r.Reverse {
		start := prefix
		if r.Lower != nil {
			start = append(bytes.Clone(prefix), r.Lower...)
		}
		if after != nil {
			start = after
		}
		k, _ = c.Seek(start)
		if bytes.Equal(k, start) && (after != nil || r.Lower != nil && !r.LowerInclusive) {
			k, _ = c.Next()
		}
	} else {
		end := bytes.Clone(prefix)
		if r.Upper != nil {
			end = append(end, r.Upper...)
		} else {
			end[len(end)-1]++
		}
		if after != nil {
			end = after
		}
		k, _ = c.Seek(end)
		if k == nil {
			k, _ = c.Last()
		} else if bytes.Compare(k, end) > 0 || after != nil || r.Upper == nil || !r.UpperInclusive {
			k, _ = c.Prev()
		}
	}
	return k
}
func rangeContains(k, prefix []byte, r KeyRange) bool {
	if k == nil || !bytes.HasPrefix(k, prefix) {
		return false
	}
	suffix := k[len(prefix):]
	if r.Lower != nil {
		c := bytes.Compare(suffix, r.Lower)
		if c < 0 || c == 0 && !r.LowerInclusive {
			return false
		}
	}
	if r.Upper != nil {
		c := bytes.Compare(suffix, r.Upper)
		if c > 0 || c == 0 && !r.UpperInclusive {
			return false
		}
	}
	return true
}
func nextRangeKey(c *bolt.Cursor, reverse bool) []byte {
	if reverse {
		k, _ := c.Prev()
		return k
	}
	k, _ := c.Next()
	return k
}
func (s *Store) rangePage(ctx context.Context, snapshot uint64, prefix []byte, r KeyRange, after []byte) (rangePage, error) {
	page := rangePage{}
	s.gate.RLock()
	defer s.gate.RUnlock()
	err := s.db.View(func(tx *bolt.Tx) error {
		reader := newVisibilityReader(tx, snapshot)
		var flat *flatScanReader
		if reader.flat && !r.Reverse {
			flat = &flatScanReader{reader: &reader, cursor: tx.Bucket(flatVersionsBucket).Cursor()}
		}
		c := reader.data.Cursor()
		k := rangeCursor(c, prefix, r, after)
		size, examined := 0, 0
		for ; rangeContains(k, prefix, r); k = nextRangeKey(c, r.Reverse) {
			if err := ctx.Err(); err != nil {
				return err
			}
			examined++
			if r.Stats != nil {
				r.Stats.StoredKeys++
			}
			var v []byte
			var ok bool
			if flat != nil && !bytes.HasPrefix(k, rangeGuardPrefix) {
				v, _, ok = flat.visible(k)
			} else {
				v, _, ok = reader.visible(k)
			}
			if ok {
				page.entries = append(page.entries, rangeEntry{key: bytes.Clone(k[len(prefix):]), value: bytes.Clone(v)})
				size += len(k) + len(v)
			}
			if size >= MaxChunkBytes || examined >= 256 {
				page.after = bytes.Clone(k)
				return nil
			}
		}
		page.done = true
		return nil
	})
	return page, err
}

// ScanRange releases its bbolt read transaction after each bounded page, before
// calling the consumer. Logical snapshot visibility is retained across pages.
func (s *Store) ScanRange(ctx context.Context, snapshot uint64, space string, r KeyRange, yield func([]byte, []byte) error) error {
	prefix, err := key(space, nil)
	if err != nil {
		return err
	}
	if _, err = key(space, r.Lower); err != nil {
		return err
	}
	if _, err = key(space, r.Upper); err != nil {
		return err
	}
	var after []byte
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		p, err := s.rangePage(ctx, snapshot, prefix, r, after)
		if err != nil {
			return err
		}
		for _, entry := range p.entries {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = yield(entry.key, entry.value); err != nil {
				return err
			}
		}
		if p.done {
			return nil
		}
		after = p.after
	}
}
func (t *Tx) stageRangePage(ctx context.Context, prefix []byte, r KeyRange, after []byte) (rangePage, error) {
	page := rangePage{}
	if t.stage == nil {
		page.done = true
		return page, nil
	}
	err := t.stage.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("writes")).Cursor()
		k := rangeCursor(c, prefix, r, after)
		size, count := 0, 0
		for ; rangeContains(k, prefix, r); k = nextRangeKey(c, r.Reverse) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if r.Stats != nil {
				r.Stats.StagedKeys++
			}
			op, err := decodeStagedOp(k, tx.Bucket([]byte("writes")).Get(k))
			if err != nil {
				return err
			}
			page.entries = append(page.entries, rangeEntry{key: bytes.Clone(k[len(prefix):]), value: op.Value, deleted: op.Delete, check: op.Check})
			size += len(k) + len(op.Value)
			count++
			if size >= MaxChunkBytes || count >= 256 {
				page.after = bytes.Clone(k)
				return nil
			}
		}
		page.done = true
		return nil
	})
	return page, err
}

type rangeIterator struct {
	read     func([]byte) (rangePage, error)
	page     rangePage
	position int
	loaded   bool
}

func (it *rangeIterator) peek() (*rangeEntry, error) {
	for !it.loaded || it.position == len(it.page.entries) {
		if it.loaded && it.page.done {
			return nil, nil
		}
		p, err := it.read(it.page.after)
		if err != nil {
			return nil, err
		}
		it.page = p
		it.loaded = true
		it.position = 0
	}
	return &it.page.entries[it.position], nil
}

// ScanRange merges committed keys and every transaction overlay in key order.
// Newest non-guard writes win. Deletes suppress older entries; guards fall
// through. Memory is bounded by one page per transaction layer, never the table.
func (t *Tx) ScanRange(ctx context.Context, space string, r KeyRange, yield func([]byte, []byte) error) (err error) {
	defer func() {
		if t.generation != t.store.generation.Load() {
			err = ErrConflict
		}
	}()
	if t.closed || t.generation != t.store.generation.Load() {
		return ErrClosed
	}
	prefix, err := key(space, nil)
	if err != nil {
		return err
	}
	if _, err = key(space, r.Lower); err != nil {
		return err
	}
	if _, err = key(space, r.Upper); err != nil {
		return err
	}
	var iters []*rangeIterator
	for layer := t; layer != nil; layer = layer.parent {
		if layer.closed {
			return ErrClosed
		}
		if err := layer.flushWrites(); err != nil {
			return err
		}
		if layer.stage != nil {
			current := layer
			iters = append(iters, &rangeIterator{read: func(after []byte) (rangePage, error) { return current.stageRangePage(ctx, prefix, r, after) }})
		}
	}
	iters = append(iters, &rangeIterator{read: func(after []byte) (rangePage, error) { return t.store.rangePage(ctx, t.Snapshot, prefix, r, after) }})
	heads := make([]*rangeEntry, len(iters))
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		var selected []byte
		for i, it := range iters {
			heads[i], err = it.peek()
			if err != nil {
				return err
			}
			if heads[i] == nil {
				continue
			}
			cmp := bytes.Compare(heads[i].key, selected)
			if selected == nil || !r.Reverse && cmp < 0 || r.Reverse && cmp > 0 {
				selected = heads[i].key
			}
		}
		if selected == nil {
			return nil
		}
		var winner *rangeEntry
		for i, head := range heads {
			if head == nil || !bytes.Equal(head.key, selected) {
				continue
			}
			if winner == nil && !head.check {
				winner = head
			}
			iters[i].position++
		}
		if winner != nil && !winner.deleted {
			if err = yield(winner.key, winner.value); err != nil {
				return err
			}
		}
	}
}
