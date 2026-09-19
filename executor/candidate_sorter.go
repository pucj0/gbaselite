package executor

import (
	"fmt"
	"strings"

	"gbaselite/physical"
)

// This file owns the executor's candidate sorter: the adapter that lets the Modify Pipeline's
// TargetRowDedup persist a *candidate*, not just a dedup ledger entry.
//
// The dedup kernel hands this sorter a (ledger key, ordinal, payload) triple and reads the same
// triple back. The payload is what the write path needs — the old row, the joined evaluation
// row, the target's physical key — so the adapter encodes it with the temporary payload codec
// and lets the external sorter spill it like any other column. A winner whose candidate spilled
// is therefore decoded back from the run file, and the operator never keeps a side table of
// winners keyed on the dedup key.
//
// Memory: every sorter built for one statement shares that statement's sorterBudget, so the
// amount retained across concurrently live sorters is bounded by the configured sort memory
// rather than multiplied by the number of sorters.

// sorterBudget is the sort memory one statement's sorters may retain *in total*.
//
// It exists because a pipeline may have more than one sorter alive at once: the dedup kernel
// hands its survivors from pass 1 to pass 2 while both are open. Sharing one pool keeps the
// statement's peak retained sort memory at the configured budget instead of a multiple of it,
// and it keeps every sorter on the statement's single temporary-file budget.
type sorterBudget struct {
	total int64
	used  int64
}

func newSorterBudget(total int64) *sorterBudget {
	if total <= 0 {
		total = defaultSortMemoryBytes
	}
	return &sorterBudget{total: total}
}

func (b *sorterBudget) rowLimit() int64 { return b.total / 16 }

// reserve takes at least one byte, so a caller that has something to retain always makes
// progress even when the budget is otherwise fully spoken for.
func (b *sorterBudget) reserve(requested int64) (int64, bool) {
	available := b.total - b.used
	if requested > available {
		requested = available
	}
	if requested < 1 {
		return 0, false
	}
	b.used += requested
	return requested, true
}

func (b *sorterBudget) release(size int64) {
	b.used -= size
	if b.used < 0 {
		b.used = 0
	}
}

const defaultSortMemoryBytes = 4 << 20

// temporaryCodec encodes and decodes one candidate payload. encode must be lossless and
// decode must own the bytes it returns.
type temporaryCodec[T any] interface {
	encode(value T) ([]byte, error)
	decode(data []byte) (T, error)
	// bytes is the exact encoded size, so the sorter can reject an over-wide candidate before
	// allocating for it.
	bytes(value T) int
}

// candidateSorter is the physical.Sorter the dedup kernel sees: it round-trips the payload
// through the external sorter, so nothing about a winner is kept outside it.
type candidateSorter[T any] struct {
	sorter *externalRowSorter
	codec  temporaryCodec[T]
	order  sorterOrder
}

// sorterOrder decides how the two ledger columns compare. byKey orders by ledger key and then
// ordinal; the other pass orders by ordinal alone to restore source order.
type sorterOrder bool

// addCandidate writes one candidate through the sorter.
//
// The order matters: the candidate's payload is encoded only after the sorter has accepted the
// row's width. The codec reports the payload's exact serialized length, so the full row width —
// ledger key, ordinal, payload, and the sorter's own tag and header overhead — is known before
// codec.encode allocates anything. An over-wide candidate therefore costs nothing: it is refused
// with ErrQueryResourceLimit instead of allocating a payload that would then be rejected.
//
// The sorter's own Add repeats the width check, so this preflight is an optimisation for the
// expensive caller rather than the only guard.
func (s candidateSorter[T]) Add(row physical.SortRow[T]) error {
	// The payload column is sized as the materialised cell the codec will hand back — tag, length
	// prefix and payload — from the payload length the codec reports. Sizing it as a nil cell would
	// under-count by the cell header and let an over-wide row slip past this check and be caught only
	// after its payload had been allocated.
	width := sortOrdinalAndCountBytes + sortCellBytes(row.Key) + sortCellBytes(ordinalLedgerKey(row.Ordinal)) +
		sortTagAndLengthBytes + int64(s.codec.bytes(row.Row))
	if err := s.sorter.preflightWidth(width); err != nil {
		return err
	}
	payload, err := s.codec.encode(row.Row)
	if err != nil {
		return err
	}
	return s.sorter.Add([]any{row.Key, ordinalLedgerKey(row.Ordinal), payload})
}

func (s candidateSorter[T]) Finish(yield func(physical.SortRow[T]) error) error {
	return s.sorter.Finish(func(values []any) error {
		row, err := s.decodeRow(values)
		if err != nil {
			return err
		}
		return yield(row)
	})
}

func (s candidateSorter[T]) decodeRow(values []any) (physical.SortRow[T], error) {
	key, ok := values[0].(string)
	if !ok {
		return physical.SortRow[T]{}, fmt.Errorf("candidate sorter ledger key has type %T", values[0])
	}
	ordinal, ok := values[1].(string)
	if !ok {
		return physical.SortRow[T]{}, fmt.Errorf("candidate sorter ordinal has type %T", values[1])
	}
	payload, ok := values[2].([]byte)
	if !ok {
		return physical.SortRow[T]{}, fmt.Errorf("candidate sorter payload has type %T", values[2])
	}
	candidate, err := s.codec.decode(payload)
	if err != nil {
		return physical.SortRow[T]{}, err
	}
	return physical.SortRow[T]{Key: key, Ordinal: ordinalFromLedgerKey(ordinal), Row: candidate}, nil
}

func (s candidateSorter[T]) Close() error { return s.sorter.Close() }

// newCandidateSorter builds the sorter for one dedup pass over one candidate type.
//
// The comparison is shared by every candidate type because part 1 of the row is always the
// ledger key and part 2 is always the big-endian ordinal; only part 3, the payload, differs,
// and it is never compared.
func newCandidateSorter[T any](control *queryControl, budget *sorterBudget, codec temporaryCodec[T], byKey bool) (physical.Sorter[physical.SortRow[T]], error) {
	compare := func(a, b []any) int {
		if byKey {
			if c := strings.Compare(a[0].(string), b[0].(string)); c != 0 {
				return c
			}
		}
		return strings.Compare(a[1].(string), b[1].(string))
	}
	sorter, err := newExternalRowSorter(control, budget, compare)
	if err != nil {
		return nil, err
	}
	return candidateSorter[T]{sorter: sorter, codec: codec, order: sorterOrder(byKey)}, nil
}

// ordinalLedgerKey renders an ordinal so a bytewise comparison orders it numerically, which the
// shared sorter needs because it compares sort values in order.
func ordinalLedgerKey(ordinal uint64) string {
	var scratch [8]byte
	for i := range scratch {
		scratch[7-i] = byte(ordinal >> (8 * i))
	}
	return string(scratch[:])
}

func ordinalFromLedgerKey(key string) uint64 {
	var ordinal uint64
	for i := 0; i < len(key) && i < 8; i++ {
		ordinal = ordinal<<8 | uint64(key[i])
	}
	return ordinal
}
