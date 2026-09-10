// Package physical implements synchronous, composable relational operators.
// Rows are borrowed for the duration of Yield. Retaining operators must clone
// them. Run owns all resources it opens and releases them on every exit path.
package physical

import (
	"context"
	"errors"
	"gbaselite/storageengine"
)

type Yield[T any] func(T) error
type Operator[T any] interface {
	Run(context.Context, Yield[T]) error
}
type Source[T any] func(context.Context, Yield[T]) error

func (s Source[T]) Run(ctx context.Context, yield Yield[T]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s(ctx, func(row T) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return yield(row)
	})
}

type Scan[T any] struct {
	Open   func(context.Context) (storageengine.Iterator, error)
	Decode func([]byte, []byte) (T, error)
}

func (s Scan[T]) Run(ctx context.Context, yield Yield[T]) (err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	it, err := s.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	for it.Next() {
		if err = ctx.Err(); err != nil {
			return
		}
		row, e := s.Decode(it.Key(), it.Value())
		if e != nil {
			return e
		}
		if err = yield(row); err != nil {
			return
		}
	}
	return it.Err()
}

type Filter[T any] struct {
	Input     Operator[T]
	Predicate func(T) (bool, error)
}

func (f Filter[T]) Run(ctx context.Context, y Yield[T]) error {
	return f.Input.Run(ctx, func(r T) error {
		ok, e := f.Predicate(r)
		if e != nil {
			return e
		}
		if ok {
			return y(r)
		}
		return nil
	})
}

type Projection[A, B any] struct {
	Input   Operator[A]
	Project func(A) (B, error)
}

func (p Projection[A, B]) Run(ctx context.Context, y Yield[B]) error {
	return p.Input.Run(ctx, func(a A) error {
		b, e := p.Project(a)
		if e != nil {
			return e
		}
		return y(b)
	})
}

// Join opens a right input per left row. The planner may choose an index probe
// or a scan without changing the join algorithm. NullRight implements LEFT JOIN.
type Join3[L, R, O any] struct {
	Left      Operator[L]
	Right     func(L) (Operator[R], error)
	Combine   func(L, R) O
	Predicate func(O) (bool, error)
	NullRight func(L) O
}

func (j Join3[L, R, O]) Run(ctx context.Context, y Yield[O]) error {
	return j.Left.Run(ctx, func(l L) error {
		right, e := j.Right(l)
		if e != nil {
			return e
		}
		matched := false
		e = right.Run(ctx, func(r R) error {
			row := j.Combine(l, r)
			ok := true
			var e error
			if j.Predicate != nil {
				ok, e = j.Predicate(row)
			}
			if e != nil {
				return e
			}
			if !ok {
				return nil
			}
			matched = true
			return y(row)
		})
		if e != nil {
			return e
		}
		if !matched && j.NullRight != nil {
			return y(j.NullRight(l))
		}
		return nil
	})
}

// Join preserves the homogeneous API while sharing the sole Join3 algorithm.
type Join[T any] Join3[T, T, T]

func (j Join[T]) Run(ctx context.Context, y Yield[T]) error { return Join3[T, T, T](j).Run(ctx, y) }

// Aggregate creates fresh query-local state on each run, including empty input.
type Accumulator[A, B any] interface {
	Add(A) error
	Finish(Yield[B]) error
	Close() error
}
type Aggregate[A, B any] struct {
	Input Operator[A]
	New   func() (Accumulator[A, B], error)
}

func (a Aggregate[A, B]) Run(ctx context.Context, y Yield[B]) (err error) {
	state, err := a.New()
	if err != nil {
		return
	}
	defer func() { err = errors.Join(err, state.Close()) }()
	if err = a.Input.Run(ctx, state.Add); err != nil {
		return
	}
	return state.Finish(y)
}

type Sorter[T any] interface {
	Add(T) error
	Finish(func(T) error) error
	Close() error
}
type Sort[T any] struct {
	Input Operator[T]
	New   func() (Sorter[T], error)
}

func (s Sort[T]) Run(ctx context.Context, y Yield[T]) (err error) {
	sorter, err := s.New()
	if err != nil {
		return
	}
	defer func() { err = errors.Join(err, sorter.Close()) }()
	if err = s.Input.Run(ctx, sorter.Add); err != nil {
		return
	}
	return sorter.Finish(func(r T) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return y(r)
	})
}

type Limit[T any] struct {
	Input         Operator[T]
	Offset, Count int
} // Count < 0 means unlimited.
func (l Limit[T]) Run(ctx context.Context, y Yield[T]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.Count == 0 {
		return nil
	}
	stop := errors.New("physical limit complete")
	seen, n := 0, 0
	err := l.Input.Run(ctx, func(r T) error {
		if seen < l.Offset {
			seen++
			return nil
		}
		if err := y(r); err != nil {
			return err
		}
		n++
		if l.Count >= 0 && n >= l.Count {
			return stop
		}
		return nil
	})
	return removeStop(err, stop)
}

// TopN shares the bounded/spilling sorter rather than allocating an unbounded heap.
type TopN[T any] struct {
	Sort          Sort[T]
	Offset, Count int
}

func (t TopN[T]) Run(ctx context.Context, y Yield[T]) error {
	return (Limit[T]{Input: t.Sort, Offset: t.Offset, Count: t.Count}).Run(ctx, y)
}

type Union[T any] struct{ Inputs []Operator[T] } // UNION ALL; DISTINCT is a separate relational step.
func (u Union[T]) Run(ctx context.Context, y Yield[T]) error {
	for _, input := range u.Inputs {
		if err := input.Run(ctx, y); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Materialize requires ownership and budget policies from the binding layer.
// Charge must reject rows before the budget is exceeded; no cross-run cache exists.
type Materialize[T any] struct {
	Input  Operator[T]
	Clone  func(T) T
	Charge func(T) error
}

func (m Materialize[T]) Run(ctx context.Context, y Yield[T]) error {
	var rows []T
	err := m.Input.Run(ctx, func(r T) error {
		if err := m.Charge(r); err != nil {
			return err
		}
		rows = append(rows, m.Clone(r))
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := y(r); err != nil {
			return err
		}
	}
	return nil
}

// Window receives a bounded materialized partition; expression and frame
// semantics belong to the SQL binding layer, not the storage implementation.
type Window[A, B any] struct {
	Input    Materialize[A]
	Evaluate func([]A, Yield[B]) error
}

func (w Window[A, B]) Run(ctx context.Context, y Yield[B]) error {
	var rows []A
	if err := w.Input.Run(ctx, func(r A) error { rows = append(rows, r); return nil }); err != nil {
		return err
	}
	return w.Evaluate(rows, func(r B) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return y(r)
	})
}

// Modify does not commit. Its caller owns the statement child transaction and
// rolls it back if Apply or any downstream operator fails.
type Modify[A, B any] struct {
	Input Operator[A]
	Apply func(context.Context, A) (B, error)
}

func (m Modify[A, B]) Run(ctx context.Context, y Yield[B]) error {
	return m.Input.Run(ctx, func(r A) error {
		v, e := m.Apply(ctx, r)
		if e != nil {
			return e
		}
		return y(v)
	})
}

// Preserve cleanup failures even when LIMIT intentionally stops the upstream.
func removeStop(err, stop error) error {
	if err == stop {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining []error
		for _, child := range joined.Unwrap() {
			if e := removeStop(child, stop); e != nil {
				remaining = append(remaining, e)
			}
		}
		return errors.Join(remaining...)
	}
	return err
}
