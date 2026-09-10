package physical

import (
	"context"
	"errors"
)

// DistinctRow preserves source order while deduplicating by a SQL-bound key.
// A sorter must own any row it retains, just as for Sort.
type DistinctRow[T any] struct {
	Key     string
	Ordinal uint64
	Row     T
}

// Distinct uses two bounded/spillable sorts: key+ordinal selects the first
// representative, then ordinal restores source order. No unbounded seen map.
// NewSort(true) orders by Key then Ordinal; false orders by Ordinal.
type Distinct[T any] struct {
	Input   Operator[T]
	Key     func(T) (string, error)
	NewSort func(byKey bool) (Sorter[DistinctRow[T]], error)
}

func (d Distinct[T]) Run(ctx context.Context, y Yield[T]) (err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	first, err := d.NewSort(true)
	if err != nil {
		return
	}
	defer func() { err = errors.Join(err, first.Close()) }()
	second, err := d.NewSort(false)
	if err != nil {
		return
	}
	defer func() { err = errors.Join(err, second.Close()) }()
	ordinal := uint64(0)
	if err = d.Input.Run(ctx, func(row T) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := d.Key(row)
		if err != nil {
			return err
		}
		entry := DistinctRow[T]{key, ordinal, row}
		ordinal++
		return first.Add(entry)
	}); err != nil {
		return
	}
	var previous string
	seen := false
	if err = first.Finish(func(row DistinctRow[T]) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen && previous == row.Key {
			return nil
		}
		previous, seen = row.Key, true
		return second.Add(row)
	}); err != nil {
		return
	}
	return second.Finish(func(row DistinctRow[T]) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return y(row.Row)
	})
}
