package mvcc

import "context"

func (t *Tx) mergeParent(ctx context.Context) error {
	p := t.parent
	if p.closed || p.generation != p.store.generation.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Autocommit has an empty parent. Transfer ownership of the bounded buffer
	// and disk stage instead of decoding and copying a million rows a second time.
	if p.stage == nil && len(p.buffered) == 0 && p.stagedBytes == 0 {
		p.stage, p.path, p.buffered = t.stage, t.path, t.buffered
		p.stagedBytes, p.bufferBytes = t.stagedBytes, t.bufferBytes
		t.stage, t.buffered = nil, nil
		t.stagedBytes, t.bufferBytes = 0, 0
		return nil
	}
	if err := t.WalkWrites(ctx, p.write); err != nil {
		// A failed merge cannot expose a partially merged statement through the
		// lower-level Tx API. SQL already aborts the outer transaction on merge errors.
		_ = p.Rollback()
		return err
	}
	return nil
}
