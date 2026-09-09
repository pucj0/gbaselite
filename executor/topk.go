package executor

import "sort"

type rankedValue[T any] struct {
	value   T
	ordinal uint64
}

// boundedRows holds the worst retained row at its root. Ordinals make ties
// match the original stable full sort, including OFFSET at a tie boundary.
type boundedRows[T any] struct {
	rows    []rankedValue[T]
	limit   int
	next    uint64
	compare func(T, T) int
}

func (b *boundedRows[T]) less(a, c rankedValue[T]) bool {
	cmp := b.compare(a.value, c.value)
	return cmp < 0 || cmp == 0 && a.ordinal < c.ordinal
}
func (b *boundedRows[T]) offer(value T, own func(T) T) {
	candidate := rankedValue[T]{value: value, ordinal: b.next}
	b.next++
	if b.limit <= 0 {
		return
	}
	if len(b.rows) < b.limit {
		candidate.value = own(value)
		b.rows = append(b.rows, candidate)
		for child := len(b.rows) - 1; child > 0; {
			parent := (child - 1) / 2
			if !b.less(b.rows[parent], b.rows[child]) {
				break
			}
			b.rows[parent], b.rows[child] = b.rows[child], b.rows[parent]
			child = parent
		}
		return
	}
	if !b.less(candidate, b.rows[0]) {
		return
	}
	candidate.value = own(value)
	b.rows[0] = candidate
	for parent := 0; ; {
		child := 2*parent + 1
		if child >= len(b.rows) {
			break
		}
		if child+1 < len(b.rows) && b.less(b.rows[child], b.rows[child+1]) {
			child++
		}
		if !b.less(b.rows[parent], b.rows[child]) {
			break
		}
		b.rows[parent], b.rows[child] = b.rows[child], b.rows[parent]
		parent = child
	}
}
func (b *boundedRows[T]) sorted() []T {
	sort.Slice(b.rows, func(i, j int) bool { return b.less(b.rows[i], b.rows[j]) })
	result := make([]T, len(b.rows))
	for i, row := range b.rows {
		result[i] = row.value
	}
	return result
}

// Large pages use the existing full sort. Never preallocate from an unchecked
// client-supplied LIMIT/OFFSET, and guard addition against integer overflow.
func boundedSortLimit(offset, limit, rows int) (int, bool) {
	if offset < 0 || limit < 0 || offset > int(^uint(0)>>1)-limit {
		return 0, false
	}
	k := offset + limit
	return k, k <= 65536 && k < rows/2
}
