package physical

import "context"

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
//
// The algorithm itself is the shared stable-dedup kernel; Distinct only supplies the
// SQL dedup key and the sorter pair.
type Distinct[T any] struct {
	Input   Operator[T]
	Key     func(T) (string, error)
	NewSort func(byKey bool) (Sorter[DistinctRow[T]], error)
}

func (d Distinct[T]) Run(ctx context.Context, y Yield[T]) error {
	newSort := func(byKey bool) (Sorter[dedupRow[T]], error) {
		sorter, err := d.NewSort(byKey)
		if err != nil {
			return nil, err
		}
		return distinctSorter[T]{sorter}, nil
	}
	// Distinct only needs the surviving row; the kernel's ledger key and ordinal are its own
	// bookkeeping.
	return dedup[T](ctx, d.Input, nil, d.Key, newSort, func(survivor dedupRow[T]) error {
		return y(survivor.Row)
	})
}

// distinctSorter adapts the exported DistinctRow sorter contract onto the internal
// kernel row type, so an existing Distinct caller keeps its own sorter unchanged.
type distinctSorter[T any] struct {
	Sorter[DistinctRow[T]]
}

func (s distinctSorter[T]) Add(row dedupRow[T]) error {
	return s.Sorter.Add(DistinctRow[T]{Key: row.Key, Ordinal: row.Ordinal, Row: row.Row})
}

func (s distinctSorter[T]) Finish(yield func(dedupRow[T]) error) error {
	return s.Sorter.Finish(func(row DistinctRow[T]) error {
		return yield(dedupRow[T]{Key: row.Key, Ordinal: row.Ordinal, Row: row.Row})
	})
}
