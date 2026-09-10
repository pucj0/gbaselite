package physical

import (
	"context"
	"errors"
	"fmt"
)

// PartitionStore owns appended rows. At returns a borrowed row valid until the
// next store call. Implementations may use memory or temporary files. Close must
// release all owned buffers/files, on success, failure and cancellation.
type PartitionStore[T any] interface {
	Append(context.Context, T) error
	Len() int
	At(context.Context, int) (T, error)
	Close() error
}

// GroupStore is the state boundary for future spillable accumulators. Get yields
// a borrowed value; Put must retain its own copy. Visit may return in any order.
// SQL binds grouping equality, final ordering, codecs and resource policy.
type GroupStore[K comparable, V any] interface {
	Get(context.Context, K) (V, bool, error)
	Put(context.Context, K, V) error
	Visit(context.Context, func(K, V) error) error
	Close() error
}

// MemoryPartitionStore preserves the existing charge-before-clone policy.
// Charge owns accounting; it must account for row bytes and evaluator overhead.
type MemoryPartitionStore[T any] struct {
	Clone  func(T) T
	Charge func(T) error
	rows   []T
	closed bool
}

func (m *MemoryPartitionStore[T]) Append(ctx context.Context, r T) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closed {
		return errors.New("partition store closed")
	}
	if err := m.Charge(r); err != nil {
		return err
	}
	m.rows = append(m.rows, m.Clone(r))
	return nil
}
func (m *MemoryPartitionStore[T]) Len() int { return len(m.rows) }
func (m *MemoryPartitionStore[T]) At(ctx context.Context, i int) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if m.closed || i < 0 || i >= len(m.rows) {
		return zero, fmt.Errorf("partition row out of range")
	}
	return m.rows[i], nil
}
func (m *MemoryPartitionStore[T]) Close() error { m.rows = nil; m.closed = true; return nil }
