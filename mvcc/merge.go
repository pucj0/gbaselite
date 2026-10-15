package mvcc

import (
	"context"
	"fmt"
)

func (t *Tx) mergeParent(ctx context.Context) error {
	p := t.parent
	if p.closed.Load() || p.generation != p.store.generation.Load() {
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
		// The parent's write set was empty, so the child's counters can be folded in
		// wholesale: its write set becomes the parent's, and the work it did was done
		// on the parent's behalf.
		p.stats().addWriteSet(t.stats())
		p.stats().addDependencies(t.stats())
		p.stats().addObservations(t.stats())
		if err := p.mergeGuardedKeys(t); err != nil {
			_ = p.Rollback()
			return err
		}
		return nil
	}
	if err := t.WalkWrites(ctx, p.write); err != nil {
		// A failed merge cannot expose a partially merged statement through the
		// lower-level Tx API. SQL already aborts the outer transaction on merge errors.
		_ = p.Rollback()
		return err
	}
	// The parent's write and dependency counters were updated by re-buffering each
	// operation above; only the child's read observations still need folding in.
	p.stats().addObservations(t.stats())
	if err := p.mergeGuardedKeys(t); err != nil {
		_ = p.Rollback()
		return err
	}
	return nil
}

func (p *Tx) mergeGuardedKeys(child *Tx) error {
	child.mu.Lock()
	defer child.mu.Unlock()
	if len(child.guardedKeys) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	newKeys := 0
	for k := range child.guardedKeys {
		if p.guardedKeys == nil || !p.guardedKeys[k] {
			newKeys++
		}
	}
	if len(p.guardedKeys)+newKeys > maxGuardedKeys {
		return fmt.Errorf("%w: merged guarded keys exceed limit %d", ErrWriteSetLimit, maxGuardedKeys)
	}
	if p.guardedKeys == nil {
		p.guardedKeys = make(map[string]bool, len(child.guardedKeys))
	}
	for k := range child.guardedKeys {
		p.guardedKeys[k] = true
	}
	return nil
}
