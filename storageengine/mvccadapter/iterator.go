package mvccadapter

import (
	"context"
	"errors"
	"fmt"
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
