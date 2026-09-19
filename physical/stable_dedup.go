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
// ordinal. The surviving representative is the whole SortRow, payload included, so a caller
// that needs the winner's payload back reads it off the survivor. There is no side staging
// and no map keyed on the dedup key: the injected sorter either retains the payload in
// memory or writes it to a temporary run, so a winner's payload survives a spill exactly as
// it survives being held in memory.
//
// SortRow is exported because a caller's sorter has to name it: the sorter is generic over
// the payload type, and that payload really does travel through the sorter.

// SortRow is one retained row paired with its dedup key and the input ordinal that decided
// first occurrence.
//
// It is the row type the dedup kernel sorts. A sorter that must spill is responsible for
// encoding Row into something it can persist; a sorter that keeps everything in memory can
// retain the struct as it is.
type SortRow[T any] struct {
	Key     string
	Ordinal uint64
	Row     T
}

// The two sort orders the kernel uses. Pass 1 orders by (key, ordinal) so the first
// occurrence of a key leads its group; pass 2 orders by ordinal alone and so restores source
// order. Naming them keeps the call sites readable.
const (
	byKeyOrder     = true
	byOrdinalOrder = false
)

// dedup is the shared stable deduplication kernel: two bounded/spillable sorts, never an
// unbounded seen map.
//
//   - Pass 1 (byKeyOrder) orders by key and then ordinal, so the first occurrence of each
//     key is the first entry of its key group.
//   - Pass 2 (byOrdinalOrder) restores source order, so output order is the input order of
//     the surviving representatives.
//
// The key type is string: it is strictly comparable, so the kernel can drop a key group with
// ==, and it orders bytewise for a spilled sorter on disk. Callers encode their own dedup
// key into it.
//
// include decides whether an input row participates at all; a nil include keeps every row.
// Rows rejected by include are dropped before ordinal assignment, so they neither produce
// output nor consume a first-occurrence slot.
//
// identity extracts the dedup key of one included row. newSort supplies the two sorters; the
// caller owns spilling, memory accounting and temporary-file cleanup, and dedup guarantees
// that every sorter it opened is closed exactly once on every exit path, including an
// upstream error, a downstream error and context cancellation. A sorter must own every row
// it retains, exactly as for Sort.
//
// Resource model: pass 1 releases its sorter as soon as it has finished yielding, which is
// before pass 2 runs its own Finish. Pass 2 therefore never overlaps pass 1's emit-and-merge
// phase, and the first sorter's batch and run files are gone before the second one drains.
// The two passes do overlap while pass 1 feeds pass 2 — that handoff is unavoidable, because
// pass 2 must receive the survivors while pass 1 is producing them — so a caller that needs
// a hard bound on the *sum* of live sorter memory shares one budget across both sorters
// through its sorter factory.
//
// The output is the surviving SortRow itself, payload included, which is what a caller such
// as TargetRowDedup needs in order to hand the winner on unchanged.
func dedup[T any](ctx context.Context, input Operator[T], include func(T) (bool, error), identity func(T) (string, error), newSort func(byKey bool) (Sorter[SortRow[T]], error), yield Yield[SortRow[T]]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	first, err := newSort(byKeyOrder)
	if err != nil {
		return err
	}
	second, err := newSort(byOrdinalOrder)
	if err != nil {
		return errors.Join(err, first.Close())
	}
	// Close each sorter exactly once. A sorter that has already been closed reports nil, so
	// closing eagerly below and again on the way out is safe and keeps the error
	// attribution exact: the exit path only joins an error for a sorter still open here.
	firstOpen, secondOpen := true, true
	closeFirst := func() error {
		if !firstOpen {
			return nil
		}
		firstOpen = false
		return first.Close()
	}
	closeSecond := func() error {
		if !secondOpen {
			return nil
		}
		secondOpen = false
		return second.Close()
	}
	defer func() {
		if firstOpen {
			_ = first.Close()
		}
		if secondOpen {
			_ = second.Close()
		}
	}()

	// Pass 1: fill the key-ordered sorter, resolving identity once per included input row.
	ordinal := uint64(0)
	if err := input.Run(ctx, func(row T) error {
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
		entry := SortRow[T]{Key: key, Ordinal: ordinal, Row: row}
		ordinal++
		return first.Add(entry)
	}); err != nil {
		return errors.Join(err, closeFirst(), closeSecond())
	}

	// Stream the group leaders into the ordinal-ordered sorter. Sorted by (key, ordinal), the
	// first entry of each key group is that key's first occurrence, so dropping every later
	// duplicate of the previous key keeps exactly the survivors.
	var previous string
	seen := false
	if err := first.Finish(func(row SortRow[T]) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen && previous == row.Key {
			return nil
		}
		previous, seen = row.Key, true
		return second.Add(row)
	}); err != nil {
		return errors.Join(err, closeFirst(), closeSecond())
	}
	// Pass 1 is done: release it before pass 2 drains, so its batch and run files do not stay
	// live across the second sort.
	if err := closeFirst(); err != nil {
		return errors.Join(err, closeSecond())
	}

	finishErr := second.Finish(func(row SortRow[T]) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return yield(row)
	})
	if finishErr != nil {
		return errors.Join(finishErr, closeSecond())
	}
	return closeSecond()
}
