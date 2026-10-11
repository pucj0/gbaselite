package mvcc

import (
	"context"
	"errors"
	bolt "go.etcd.io/bbolt"
)

var errLocalBatchFull = errors.New("local atomic batch full")

// tryLocalCommit bypasses persistent staging only for a complete, bounded
// standalone transaction. Larger local writes stream unpublished versions;
// Raft retains the persistent staged path for replay.
func (t *Tx) tryLocalCommit(ctx context.Context) (uint64, bool, error) {
	if t.proposer != t.store {
		// The replicated apply path validates from the Command alone, which carries no lock
		// observation baseline. A transaction that took a locked current read therefore cannot be
		// published through it without either changing the replicated command format or skipping
		// the observation check, and both are forbidden. Failing closed is the only honest answer:
		// the alternative is to accept a write whose authorization the durable path cannot verify.
		if t.HasWriteBaseline() {
			return 0, true, errBaselineNeedsLocalValidation
		}
		return 0, false, nil
	}
	var ops []Op
	size := 0
	err := t.WalkWrites(ctx, func(op Op) error {
		n := len(op.Space) + len(op.Key) + len(op.Value) + 64
		if size+n > MaxChunkBytes {
			return errLocalBatchFull
		}
		size += n
		ops = append(ops, op)
		return nil
	})
	if errors.Is(err, errLocalBatchFull) {
		ops = nil
		n, err := t.commitLocalStream(ctx)
		return n, true, err
	}
	if err != nil {
		return 0, true, err
	}
	if len(ops) == 0 {
		return t.Snapshot, true, nil
	}
	n, err := t.store.commitLocalFromTx(t, ctx, ops)
	return n, true, err
}
func publishCommit(tx *bolt.Tx, id string, index uint64, catalogChanged bool) error {
	b := tx.Bucket(commitsBucket)
	if err := b.Put(sequence(index), []byte{1}); err != nil {
		return err
	}
	if err := b.Put([]byte(id), sequence(index)); err != nil {
		return err
	}
	m := tx.Bucket(metaBucket)
	if catalogChanged {
		if err := m.Put([]byte("catalog_head"), sequence(index)); err != nil {
			return err
		}
	}
	if err := m.Put([]byte("head"), sequence(index)); err != nil {
		return err
	}
	return m.Put([]byte("applied"), sequence(index))
}
