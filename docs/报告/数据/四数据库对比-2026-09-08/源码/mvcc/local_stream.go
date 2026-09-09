package mvcc

import (
	"bytes"
	"context"
	"errors"
	bolt "go.etcd.io/bbolt"
)

// commitLocalStream installs a standalone transaction from its private write
// set. Each bounded version batch is synced, but none is visible until the
// synced publication marker. A crash before publication abandons the private
// write set: the durable allocated high-water mark prevents version reuse.
// Raft must not use this path; its pending records are needed for replay.
const localInstallBytes = 256 << 10

func (t *Tx) commitLocalStream(ctx context.Context) (index uint64, err error) {
	s := t.store
	s.apply.Lock()
	defer s.apply.Unlock()
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	if t.generation != s.generation.Load() {
		return 0, ErrConflict
	}
	if err = s.AvailabilityError(); err != nil {
		return 0, err
	}
	// Freeze the buffered writes before either read pass. Consumers never mutate
	// this temporary database while its read transaction is open.
	if err = t.flushWrites(); err != nil {
		return 0, err
	}
	defer func() {
		if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.failure.Lock()
			s.fatal = err
			s.failure.Unlock()
		}
	}()
	// Validate all guards using encoded keys. The frozen stage is read-only;
	// no payload copy or namespace reconstruction is needed for conflict checks.
	err = s.db.View(func(dbtx *bolt.Tx) error {
		reader := newVisibilityReader(dbtx, ^uint64(0))
		return t.stage.View(func(stageTx *bolt.Tx) error {
			return stageTx.Bucket([]byte("writes")).ForEach(func(k, encoded []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if _, err := stagedOpHeader(k, encoded); err != nil {
					return err
				}
				_, version, _ := reader.visible(k)
				if version > t.Snapshot {
					return ErrConflict
				}
				return nil
			})
		})
	})
	if err != nil {
		return 0, err
	}
	s.localSeq++
	index = s.localSeq
	if s.localWAL {
		err = s.writeLocalWAL(ctx, func(w *localWALWriter) error {
			if err := w.begin(index, t.ID); err != nil {
				return err
			}
			if err := t.stage.View(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte("writes")).ForEach(func(k, v []byte) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					return w.op(k, v)
				})
			}); err != nil {
				return err
			}
			return w.frame([]byte{walEnd})
		})
		if err != nil {
			return 0, err
		}
		err = s.recoverLocalWAL()
		if err != nil {
			s.failLocalWAL(err)
			return 0, err
		}
		return index, nil
	}
	version := sequence(index)
	// References in batch borrow the frozen temporary read transaction. Every
	// flush, including the tail, must finish before that transaction closes.
	type encodedWrite struct{ key, encoded []byte }
	var batch []encodedWrite
	size := 0
	catalogChanged := false
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.db.Update(func(dbtx *bolt.Tx) error {
			meta := dbtx.Bucket(metaBucket)
			if number(meta.Get([]byte("allocated"))) < index {
				if err := meta.Put([]byte("allocated"), version); err != nil {
					return err
				}
			}
			for _, op := range batch {
				flags := op.encoded[1]
				if flags&2 != 0 {
					continue
				}
				value := []byte{0}
				if flags&1 == 0 {
					value = make([]byte, len(op.encoded)-1)
					value[0] = 1
					copy(value[1:], op.encoded[2:])
				}
				if err := putVersion(dbtx, op.key, version, value); err != nil {
					return err
				}
				catalogChanged = catalogChanged || bytes.HasPrefix(op.key, []byte("catalog\x00"))
			}
			return nil
		})
		// Drop payload references at every boundary, independently of total writes.
		clear(batch)
		batch = batch[:0]
		size = 0
		return err
	}
	err = t.stage.View(func(stageTx *bolt.Tx) error {
		err := stageTx.Bucket([]byte("writes")).ForEach(func(k, encoded []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			cost := len(k) - 1 + len(encoded) - 2 + 64
			if size+cost > localInstallBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			batch = append(batch, encodedWrite{key: k, encoded: encoded})
			size += cost
			return nil
		})
		if err != nil {
			return err
		}
		return flush() // Must consume the tail while borrowed bytes are still valid.
	})
	if err != nil {
		return 0, err
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	err = s.db.Update(func(dbtx *bolt.Tx) error { return publishCommit(dbtx, t.ID, index, catalogChanged) })
	if err != nil {
		return 0, err
	}
	return index, nil
}
