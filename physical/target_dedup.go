package physical

import "context"

// TargetRowDedup collapses mutation candidates that address the same physical row and keeps
// the first occurrence, in source order, together with that occurrence's full payload.
//
// It is the unified Modify Pipeline's answer to "a target may be matched more than once": an
// UPDATE JOIN whose target matches several source rows and a multi-table DELETE whose target
// appears in several JOIN combinations both fan one physical row out into many candidates,
// and exactly one of them may reach the write path.
//
// Semantics:
//
//   - Identity yields the candidate's physical target identity. The operator never infers it
//     from row values, so two value-identical heap rows stay distinct and a table identifier
//     is always part of the key.
//   - A candidate whose identity is not a target (ok=false, invalid, or keyless) is dropped
//     instead of deduplicated. Outer-join NULL extension is the reason this matters: those
//     rows have no physical provenance at all, so they must not produce a mutation, and they
//     must not consume a first-occurrence slot either.
//   - First occurrence wins and output order is input order.
//   - The winner's T is preserved exactly: Input and output are both Operator[T]. A caller
//     therefore does not need to re-derive the candidate — no join replay, no ordinal replay,
//     no lookup keyed on a rescan-vulnerable ledger.
//
// Resource model: the operator owns no unbounded seen map over every candidate, and it keeps
// no payload side table either. The winner's payload travels through the sorter itself, so
// the injected sorter either retains it in memory or writes it to a temporary run — a winner
// whose payload spilled is decoded back from disk rather than recovered from a map. The dedup
// ledger is the shared bounded/spillable stable-dedup kernel, exactly like Distinct.
//
// Ownership: the sorter retains the candidate past the input's Yield callback, so whoever
// builds the candidate must hand over owned bytes. A candidate that borrows a scan buffer
// must be cloned before it reaches this operator, exactly as for Sort and Distinct.
//
// The caller supplies NewSort with its memory budget and temporary directory. The kernel
// closes both sorters on every exit path, including upstream error, downstream error and
// context cancellation.
type TargetRowDedup[T any] struct {
	Input Operator[T]

	// Identity reports the physical target identity of one candidate. The bool is false when
	// the candidate is not a target and must be dropped.
	Identity func(T) (RowIdentity, bool)

	// NewSort supplies the two sorters. byKey=true orders by identity key and then ordinal,
	// byKey=false orders by ordinal alone. The sorter must round-trip Row[T]: it is the only
	// place the winner's payload is kept, so a sorter that drops it drops the candidate.
	NewSort func(byKey bool) (Sorter[SortRow[T]], error)
}

// keyedCandidate is an input candidate paired with its resolved ledger key. Resolving the
// identity exactly once per input row is why the key travels with the candidate: neither the
// include filter nor the kernel has to re-consult Identity.
type keyedCandidate[T any] struct {
	key string
	row T
}

func (d TargetRowDedup[T]) Run(ctx context.Context, y Yield[T]) error {
	// Resolve identity once per input row. A candidate that is not a target, or whose ledger
	// key cannot be encoded, is dropped here (empty key) because neither may reach the write
	// path.
	keyed := Projection[T, keyedCandidate[T]]{Input: d.Input, Project: func(row T) (keyedCandidate[T], error) {
		resolved, ok := d.Identity(row)
		if !ok {
			return keyedCandidate[T]{}, nil
		}
		key, ok := resolved.StorageKey()
		if !ok {
			return keyedCandidate[T]{}, nil
		}
		return keyedCandidate[T]{key: string(key), row: row}, nil
	}}
	include := func(candidate keyedCandidate[T]) (bool, error) {
		return candidate.key != "", nil
	}
	identity := func(candidate keyedCandidate[T]) (string, error) {
		return candidate.key, nil
	}
	// The kernel's SortRow is the sorter's row type, so the payload travels through the sorter
	// unchanged. There is no adapter to drop it and no side table to keep in step with the ledger:
	// a spilled winner is decoded back from the run file.
	newSort := func(byKey bool) (Sorter[SortRow[keyedCandidate[T]]], error) {
		sorter, err := d.NewSort(byKey)
		if err != nil {
			return nil, err
		}
		return candidateSorter[T]{Sorter: sorter}, nil
	}
	return dedup[keyedCandidate[T]](ctx, keyed, include, identity, newSort, func(survivor SortRow[keyedCandidate[T]]) error {
		return y(survivor.Row.row)
	})
}

// candidateSorter bridges the kernel's keyed row onto the caller's payload sorter.
//
// It moves the candidate in both directions and nothing else: the resolved ledger key is carried
// through so the kernel can compare groups, and the payload is passed on untouched. It is a pure
// transformation with no retained state, which is why it cannot lose a winner.
type candidateSorter[T any] struct {
	Sorter[SortRow[T]]
}

func (s candidateSorter[T]) Add(row SortRow[keyedCandidate[T]]) error {
	return s.Sorter.Add(SortRow[T]{Key: row.Key, Ordinal: row.Ordinal, Row: row.Row.row})
}

func (s candidateSorter[T]) Finish(yield func(SortRow[keyedCandidate[T]]) error) error {
	return s.Sorter.Finish(func(row SortRow[T]) error {
		return yield(SortRow[keyedCandidate[T]]{
			Key:     row.Key,
			Ordinal: row.Ordinal,
			Row:     keyedCandidate[T]{key: row.Key, row: row.Row},
		})
	})
}
