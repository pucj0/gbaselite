package mvccadapter

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/lockmanager"
	"gbaselite/mvcc"
	"gbaselite/storageengine"
	"iter"
)

var errStopScan = errors.New("iterator stopped")

type entry struct{ k, v []byte }
type iterator struct {
	next    func() (entry, bool)
	stop    func()
	current entry
	err     error
	closed  bool
}

func newIterator(ctx context.Context, r storageengine.ScanRequest, scan func(func([]byte, []byte) error) error) (storageengine.Iterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Limit < 0 || r.Unordered && (r.Range.Lower != nil || r.Range.Upper != nil || r.Range.Reverse) {
		return nil, fmt.Errorf("invalid scan request")
	}
	it := &iterator{}
	it.next, it.stop = iter.Pull(func(yield func(entry) bool) {
		count := 0
		err := scan(func(k, v []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !yield(entry{k, v}) {
				return errStopScan
			}
			count++
			if r.Limit > 0 && count >= r.Limit {
				return errStopScan
			}
			return nil
		})
		if !errors.Is(err, errStopScan) {
			it.err = err
		}
	})
	return it, nil
}
func (i *iterator) Next() bool {
	if i.closed {
		return false
	}
	v, ok := i.next()
	i.current = v
	if !ok {
		i.closed = true
		i.stop()
	}
	return ok
}
func (i *iterator) Key() []byte   { return i.current.k }
func (i *iterator) Value() []byte { return i.current.v }
func (i *iterator) Err() error    { return i.err }
func (i *iterator) Close() error {
	if !i.closed {
		i.closed = true
		i.stop()
	}
	i.current = entry{}
	return i.err
}

// currentScanPageSize bounds how many keys one page of a locked scan holds, and
// currentScanPageBytes bounds how many bytes. Both exist for the same reason the snapshot
// iterator pages: a locked scan may wait between two pages, and the storage view of one page must
// be gone before that wait starts.
const (
	currentScanPageSize  = 64
	currentScanPageBytes = 1 << 20
)

// errCurrentPageFull ends one page of a locked scan early, so the underlying scan stops after a
// bounded amount of work. It never escapes: the iterator turns it back into "not exhausted".
var errCurrentPageFull = errors.New("current scan page is full")

// currentIterator is the lock-aware, finite-page iterator of a locked scan.
//
// It is deliberately not built on the snapshot iterator. A locked read has two rules a snapshot
// read does not have:
//
//   - The access interval is protected *before* anything is examined. The caller's plan components
//     (the proven interval, an insertion claim, or the whole-domain fallback) are acquired first,
//     so a concurrent insert cannot slip into the range between the first page and the last.
//   - Every visited key is claimed and then re-read under that claim. The value a candidate had
//     when the page was listed is never returned: between listing a key and taking its lock the row
//     can be deleted, rewritten or moved, so the iterator re-reads the latest valid version and
//     drops a key that no longer exists. That is also why the write baseline recorded by the read
//     belongs to the version that was actually observed under the lock.
//
// No storage view is held across a wait: the page listing releases its bbolt read transaction
// before the consumer sees a key (mvcc's range scan pages that way), and the iterator takes its
// record locks after the listing returned, one key at a time. A caller that stops between two
// Next() calls therefore holds no view, no cursor and no gate 鈥?only the lock claims it asked for.
type currentIterator struct {
	ctx    context.Context
	tx     *mvcc.Tx
	scope  uint64
	space  string
	bounds storageengine.KeyRange
	mode   lockmanager.Mode
	limit  int

	// pending holds the keys of the current listing that have not been claimed yet, in access
	// order. Only the *listing* is paged: each key is claimed and yielded one at a time, so the
	// first row of a scan is never held back by a lock the scan has to wait for on a later row.
	pending [][]byte
	// listingDone reports that the underlying range scan has no further keys in the range.
	listingDone bool
	// lastKey is the last key the listing produced. The next listing resumes strictly after it, in
	// the scan's own direction, which is what keeps progress stable when rows are deleted or
	// rewritten while the iterator waits.
	lastKey  []byte
	produced int

	current entry
	err     error
	closed  bool
}

// newCurrentIterator opens one locked scan.
//
// The interval claim is taken here, in the caller's goroutine, before any page is read; the
// per-record claims are taken page by page inside Next.
func newCurrentIterator(ctx context.Context, tx *mvcc.Tx, scope uint64, request storageengine.CurrentScanRequest) (storageengine.Iterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Space == "" || request.Limit < 0 {
		return nil, fmt.Errorf("invalid current scan request")
	}
	mode, err := currentScanRecordMode(request.Mode)
	if err != nil {
		return nil, err
	}
	if len(request.Plan.Components) > 0 {
		// The access interval first. This may wait, which is exactly why it happens before the
		// first page is read: nothing of the storage is held while the lock manager decides.
		components, err := lockComponents(request.Plan.Components)
		if err != nil {
			return nil, err
		}
		_, op, wait, err := tx.AcquireLock(ctx, scope, components, nil)
		if err != nil {
			return nil, err
		}
		if op == lockmanager.LockOpWait {
			// A conflicting interval acquisition is not a grant: the scan must wait for it exactly
			// like a record claim, and it must do so before reading anything.
			if wait == nil {
				return nil, storageengine.ErrConflict
			}
			if _, err := tx.WaitForLock(ctx, wait); err != nil {
				return nil, err
			}
		}
	}
	return &currentIterator{
		ctx:    ctx,
		tx:     tx,
		scope:  scope,
		space:  request.Space,
		bounds: request.Range,
		mode:   mode,
		limit:  request.Limit,
	}, nil
}

// currentScanRecordMode maps the neutral record mode of a locked scan onto the manager's.
//
// Only S and X are legal: a gap protects insert positions rather than a value, and an intention is
// not a read. Anything else is refused rather than silently treated as a shared or exclusive read.
func currentScanRecordMode(mode storageengine.LockMode) (lockmanager.Mode, error) {
	switch mode {
	case storageengine.LockModeS:
		return lockmanager.ModeS, nil
	case storageengine.LockModeX:
		return lockmanager.ModeX, nil
	default:
		return "", fmt.Errorf("a locked scan needs an explicit S or X record mode, got %q", mode)
	}
}

// Next advances to the next row of the locked scan.
//
// One row is claimed and re-read per call. The listing behind it is paged, so the storage view of
// one page is gone long before the row is claimed; a key that no longer exists after the claim is
// skipped and the loop continues, which is how a row deleted while the scan waited disappears from
// the result instead of being returned as it was listed.
func (i *currentIterator) Next() bool {
	if i.closed || i.err != nil {
		return false
	}
	for {
		if i.limit > 0 && i.produced >= i.limit {
			i.finish()
			return false
		}
		if len(i.pending) == 0 {
			if i.listingDone {
				i.finish()
				return false
			}
			if !i.listKeys() {
				return false
			}
			if len(i.pending) == 0 {
				i.finish()
				return false
			}
		}
		key := i.pending[0]
		i.pending = i.pending[1:]
		value, exists, err := i.claimAndRead(key)
		if err != nil {
			i.fail(err)
			return false
		}
		if !exists {
			// The row was deleted or moved while this scan waited for its claim. The value the
			// listing saw was never observed under the lock, so it is not this scan's to return.
			continue
		}
		i.current = entry{k: key, v: value}
		i.produced++
		return true
	}
}

// claimAndRead takes the record claim of one key and reads its latest valid version under it.
//
// The claim may wait. The listing that produced the key already finished and released its view, so
// nothing of the storage is open while the manager decides.
func (i *currentIterator) claimAndRead(key []byte) ([]byte, bool, error) {
	record := currentScanRecord(i.space, key, i.mode)
	grant, op, wait, err := i.tx.AcquireLock(i.ctx, i.scope, []lockmanager.ResourceRef{record}, nil)
	if err != nil {
		return nil, false, err
	}
	if op == lockmanager.LockOpWait {
		if wait == nil {
			return nil, false, storageengine.ErrConflict
		}
		if grant, err = i.tx.WaitForLock(i.ctx, wait); err != nil {
			return nil, false, err
		}
	}
	observation, err := i.tx.ReadCurrentLocked(i.ctx, i.space, key, grant)
	if err != nil {
		return nil, false, err
	}
	if !observation.Exists {
		return nil, false, nil
	}
	return append([]byte(nil), observation.Value...), true, nil
}

// listKeys lists the next bounded page of keys into pending, in the scan's direction.
func (i *currentIterator) listKeys() bool {
	keys, exhausted, err := i.pageKeys()
	if err != nil {
		i.fail(err)
		return false
	}
	i.pending = keys
	i.listingDone = exhausted
	return true
}

// pageKeys lists the keys of the next page, in the scan's direction, resuming strictly after the
// last consumed key.
//
// The listing is a snapshot+overlay range scan of this transaction, so a key this transaction
// inserted itself is listed as well. The underlying scan releases its bbolt read transaction before
// every consumer call, which is what makes "no view is held across a wait" structural rather than a
// promise this loop has to keep.
//
// Known limitation, stated rather than hidden: because the listing is taken at the transaction's
// snapshot, a key that another transaction committed *after* this snapshot is not enumerated by the
// scan. Its insertion is still excluded from the protected range by the interval claim, and any key
// that *is* enumerated is re-read under its claim (so no stale value is ever returned), but a locked
// scan that must publish rows committed after the snapshot needs a latest-published key enumeration
// that this release does not have. Locking reads are also not enabled yet on the SQL side, so no
// user-visible statement depends on it.
func (i *currentIterator) pageKeys() ([][]byte, bool, error) {
	bounds := i.bounds
	if i.lastKey != nil {
		if bounds.Reverse {
			bounds.Upper = i.lastKey
			bounds.UpperInclusive = false
		} else {
			bounds.Lower = i.lastKey
			bounds.LowerInclusive = false
		}
	}
	var keys [][]byte
	bytes := 0
	err := i.tx.ScanRange(i.ctx, i.space, bounds, func(k, v []byte) error {
		keys = append(keys, append([]byte(nil), k...))
		// The resume point is the last key the *listing* produced, not the last row the caller
		// consumed: every listed key is claimed before the next page is read, so resuming after the
		// listing's last key cannot skip a row.
		i.lastKey = append(i.lastKey[:0], k...)
		bytes += len(k) + len(v)
		if len(keys) >= currentScanPageSize || bytes >= currentScanPageBytes {
			return errCurrentPageFull
		}
		return nil
	})
	switch {
	case err == nil:
		// The range scan reached the end of its bounds: this was the last page.
		return keys, true, nil
	case errors.Is(err, errCurrentPageFull):
		return keys, false, nil
	default:
		return nil, false, err
	}
}

// Key, Value and Err follow the neutral iterator contract: Key/Value describe the row the last
// Next returned and stay valid until the iterator is closed, and Err reports the terminal error.
func (i *currentIterator) Key() []byte   { return i.current.k }
func (i *currentIterator) Value() []byte { return i.current.v }
func (i *currentIterator) Err() error    { return i.err }

// Close releases the iterator. The lock claims it took stay with the transaction's root, exactly
// like any other grant; what Close drops is the page buffer and the current row.
func (i *currentIterator) Close() error {
	if i.closed {
		return i.err
	}
	i.closed = true
	i.pending = nil
	i.current = entry{}
	return i.err
}

// finish marks the iterator exhausted without an error (the LIMIT was reached).
func (i *currentIterator) finish() {
	i.closed = true
	i.pending = nil
}

// fail records the terminal error and stops the iterator.
func (i *currentIterator) fail(err error) {
	if err != nil && i.err == nil {
		i.err = err
	}
	i.closed = true
	i.pending = nil
}

// currentScanRecord builds the per-record claim one visited key needs.
func currentScanRecord(space string, key []byte, mode lockmanager.Mode) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:  lockmanager.KindRecord,
		Mode:  mode,
		Space: space,
		Key:   append([]byte(nil), key...),
	}}
}
