package physical

import (
	"context"
	"errors"
)

// This file owns the shared stable-deduplication kernel. It was extracted from Distinct so
// that the SQL DISTINCT operator and the unified Modify Pipeline's TargetRowDedup run the
// exact same bounded, spillable algorithm instead of two copies of it.
//
// The kernel decides dedup membership: which ledger key survives, and at which input
// ordinal. It never inspects the payload. A caller that needs the winner's payload back is
// responsible for staging it under the ledger key the kernel reports; see runPayloadDedup.

// dedupRow is one retained row paired with its dedup key and the input ordinal that
// decided first occurrence.
type dedupRow[T any] struct {
	Key     string
	Ordinal uint64
	Row     T
}

// dedup is the shared stable deduplication kernel: two bounded/spillable sorts, never an
// unbounded seen map.
//
//   - Pass 1 (byKey) orders by key and then ordinal, so the first occurrence of each key is
//     the first entry of its key group.
//   - Pass 2 restores source order, so output order is the input order of the surviving
//     representatives.
//
// The key type is string: it is strictly comparable, so the kernel can drop a key group
// with ==, and it orders bytewise for a spilled sorter on disk. Callers encode their own
// dedup key into it.
//
// include decides whether an input row participates at all; a nil include keeps every row.
// Rows rejected by include are dropped before ordinal assignment, so they neither produce
// output nor consume a first-occurrence slot.
//
// identity extracts the dedup key of one included row. newSort supplies the two sorters;
// the caller owns spilling, memory accounting and temporary-file cleanup, and dedup
// guarantees that every sorter it opened is closed on every exit path, including an
// upstream error, a downstream error and context cancellation. A sorter must own every row
// it retains, exactly as for Sort.
//
// The output is the surviving representative itself, which is what callers such as
// runPayloadDedup need in order to recover the winner's payload.
func dedup[T any](ctx context.Context, input Operator[T], include func(T) (bool, error), identity func(T) (string, error), newSort func(byKey bool) (Sorter[dedupRow[T]], error), yield Yield[dedupRow[T]]) (err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	first, err := newSort(true)
	if err != nil {
		return
	}
	defer func() { err = errors.Join(err, first.Close()) }()
	second, err := newSort(false)
	if err != nil {
		return
	}
	defer func() { err = errors.Join(err, second.Close()) }()
	ordinal := uint64(0)
	if err = input.Run(ctx, func(row T) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if include != nil {
			keep, err := include(row)
			if err != nil {
				return err
			}
			if !keep {
				return nil
			}
		}
		key, err := identity(row)
		if err != nil {
			return err
		}
		entry := dedupRow[T]{Key: key, Ordinal: ordinal, Row: row}
		ordinal++
		return first.Add(entry)
	}); err != nil {
		return
	}
	var previous string
	seen := false
	if err = first.Finish(func(row dedupRow[T]) error {
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
	return second.Finish(func(row dedupRow[T]) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return yield(row)
	})
}

// runPayloadDedup deduplicates rows by ledger key while preserving each winner's payload.
//
// The kernel above only reports which ledger keys survive and in which order; it does not
// carry the payload back, because a spilled sorter can only persist primitives. This helper
// therefore keeps the payload observed for each distinct key and re-emits the winner in the
// kernel's output order.
//
// Staging is keyed by the ledger key, so it holds one payload per *distinct target* rather
// than one per input candidate: a target matched by a thousand join combinations still
// occupies a single slot. The memory the sorter would otherwise spend on those thousand
// candidates is what the spillable ledger saves.
//
// The staged payload must own its bytes. A caller whose candidate borrows a scan buffer
// must clone it before calling, exactly as for Sort and Distinct.
func runPayloadDedup[T any](ctx context.Context, input Operator[T], include func(T) (bool, error), identity func(T) (string, error), newSort func(byKey bool) (Sorter[dedupRow[string]], error), yield Yield[T]) error {
	payloads := make(map[string]T)
	keys := Projection[T, string]{Input: input, Project: func(row T) (string, error) {
		keep := true
		if include != nil {
			var err error
			if keep, err = include(row); err != nil {
				return "", err
			}
		}
		if !keep {
			return "", nil
		}
		key, err := identity(row)
		if err != nil {
			return "", err
		}
		// The kernel keeps the first occurrence of a key, so recording the first payload seen
		// for that key lines the staged payload up with the survivor.
		if _, exists := payloads[key]; !exists {
			payloads[key] = row
		}
		return key, nil
	}}
	return dedup[string](ctx, keys, func(key string) (bool, error) { return key != "", nil }, func(key string) (string, error) { return key, nil }, newSort, func(survivor dedupRow[string]) error {
		row, ok := payloads[survivor.Key]
		if !ok {
			return nil
		}
		return yield(row)
	})
}
