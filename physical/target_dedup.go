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
//     therefore does not need to re-derive the candidate — no join replay, no ordinal
//     replay, no lookup keyed on a rescan-vulnerable ledger.
//
// Resource model: the operator owns no unbounded seen map over every candidate. The dedup
// ledger is the shared bounded/spillable stable-dedup kernel, exactly like Distinct, so the
// injected sorter decides how much is retained in memory and spills the rest. The winner
// payloads are staged one entry per *distinct target*, not one per candidate, so a target
// matched by a thousand join combinations occupies a single payload slot.
//
// Ownership: the staged payload is retained past the input's Yield callback, so whoever
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
	// byKey=false orders by ordinal alone.
	NewSort func(byKey bool) (Sorter[TargetDedupRow[T]], error)
}

// TargetDedupRow is the retained form of one deduped candidate: the identity ledger key, the
// input ordinal, and the candidate itself.
//
// It is what a caller's sorter receives. A sorter that must spill is responsible for
// encoding Row into primitives it can persist; a sorter that keeps everything in memory can
// simply retain the struct as it is.
type TargetDedupRow[T any] struct {
	Key     string
	Ordinal uint64
	Row     T
}

// ledgerOnlySorter exposes a TargetDedupRow sorter to the ledger-only kernel.
//
// The payload-preserving path does not route the payload through the sorter — the operator
// stages the winner itself — so the kernel only needs the key and ordinal.
type ledgerOnlySorter[T any] struct {
	Sorter[TargetDedupRow[T]]
}

func (s ledgerOnlySorter[T]) Add(row dedupRow[string]) error {
	return s.Sorter.Add(TargetDedupRow[T]{Key: row.Key, Ordinal: row.Ordinal})
}

func (s ledgerOnlySorter[T]) Finish(yield func(dedupRow[string]) error) error {
	return s.Sorter.Finish(func(row TargetDedupRow[T]) error {
		return yield(dedupRow[string]{Key: row.Key, Ordinal: row.Ordinal})
	})
}

// keyedCandidate is an input candidate paired with its resolved ledger key. Resolving the
// identity exactly once per input row is why the key travels with the candidate: neither the
// include filter nor the kernel has to re-consult Identity.
type keyedCandidate[T any] struct {
	key string
	row T
}

func (d TargetRowDedup[T]) Run(ctx context.Context, y Yield[T]) error {
	// Resolve identity once per input row. A candidate that is not a target, or whose ledger key
	// cannot be encoded, is dropped here (empty key) because neither may reach the write path.
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
	newSort := func(byKey bool) (Sorter[dedupRow[string]], error) {
		sorter, err := d.NewSort(byKey)
		if err != nil {
			return nil, err
		}
		return ledgerOnlySorter[T]{Sorter: sorter}, nil
	}
	return runPayloadDedup[keyedCandidate[T]](ctx, keyed, include, identity, newSort, func(survivor keyedCandidate[T]) error {
		return y(survivor.row)
	})
}
