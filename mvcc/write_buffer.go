package mvcc

import (
	"bytes"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"math"
	"sort"
)

const writeBufferBytes = 128 << 10

// The temporary write-set buffer is bounded independently of transaction size.
// Encoded bytes are owned here; callers may reuse their input keys/values.
func (t *Tx) flushWrites() error {
	if len(t.buffered) == 0 {
		return nil
	}
	if err := t.ensureStage(); err != nil {
		return err
	}
	keys := make([]string, 0, len(t.buffered))
	for k := range t.buffered {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if err := t.stage.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("writes"))
		for _, k := range keys {
			if err := b.Put([]byte(k), t.buffered[k]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	t.buffered = nil
	t.bufferBytes = 0
	return nil
}
func (t *Tx) stagedValue(k []byte) ([]byte, error) {
	if v, ok := t.buffered[string(k)]; ok {
		return v, nil
	}
	if t.stage == nil {
		return nil, nil
	}
	var value []byte
	err := t.stage.View(func(tx *bolt.Tx) error { value = bytes.Clone(tx.Bucket([]byte("writes")).Get(k)); return nil })
	return value, err
}
func (t *Tx) bufferWrite(op Op, k, value []byte) error {
	previous, err := t.stagedValue(k)
	if err != nil {
		return err
	}
	if op.Check && previous != nil {
		old, err := decodeStagedOp(k, previous)
		if err != nil {
			return err
		}
		if !old.Check {
			return nil
		}
	}
	delta := int64(len(k) + len(value))
	if previous != nil {
		delta -= int64(len(k) + len(previous))
	}
	if delta > 0 && t.stagedBytes > math.MaxInt64-delta {
		return fmt.Errorf("%w: accounting overflow", ErrWriteSetLimit)
	}
	if limit := t.store.writeSetLimit; limit > 0 && t.stagedBytes+delta > limit {
		return fmt.Errorf("%w (%d bytes); resources.transaction_write_mb=0 disables this budget", ErrWriteSetLimit, limit)
	}
	cost := int64(len(k) + len(value) + 64)
	oldCost := int64(0)
	if v, ok := t.buffered[string(k)]; ok {
		oldCost = int64(len(k) + len(v) + 64)
	}
	if t.bufferBytes-oldCost+cost > writeBufferBytes {
		if err = t.flushWrites(); err != nil {
			return err
		}
		oldCost = 0
	}
	// An operation that exceeds this budget is written directly.
	// Store that single bounded operation directly instead of enlarging the buffer.
	if cost > writeBufferBytes {
		if err = t.ensureStage(); err != nil {
			return err
		}
		if err = t.stage.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("writes")).Put(k, value) }); err != nil {
			return err
		}
	} else {
		if t.buffered == nil {
			t.buffered = make(map[string][]byte)
		}
		t.buffered[string(k)] = value
		t.bufferBytes += cost - oldCost
	}
	t.stagedBytes += delta
	return nil
}
