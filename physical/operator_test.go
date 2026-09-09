package physical

import (
	"context"
	"errors"
	"gbaselite/storageengine"
	"reflect"
	"sort"
	"testing"
)

func values(v ...int) Operator[int] {
	return Source[int](func(_ context.Context, y Yield[int]) error {
		for _, r := range v {
			if err := y(r); err != nil {
				return err
			}
		}
		return nil
	})
}
func collect[T any](t *testing.T, op Operator[T]) []T {
	t.Helper()
	var rows []T
	if err := op.Run(context.Background(), func(r T) error { rows = append(rows, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return rows
}

type testIterator struct {
	n        int
	buf      []byte
	closed   bool
	closeErr error
}

func (i *testIterator) Next() bool {
	if i.n == 3 {
		return false
	}
	i.n++
	i.buf[0] = byte(i.n)
	return true
}
func (i *testIterator) Key() []byte   { return i.buf }
func (i *testIterator) Value() []byte { return i.buf }
func (i *testIterator) Err() error    { return nil }
func (i *testIterator) Close() error  { i.closed = true; return i.closeErr }
func scan(i *testIterator) Operator[[]byte] {
	return Scan[[]byte]{Open: func(context.Context) (storageengine.Iterator, error) { return i, nil }, Decode: func(_, v []byte) ([]byte, error) { return v, nil }}
}
func TestScanLimitOwnershipAndErrors(t *testing.T) {
	it := &testIterator{buf: make([]byte, 1)}
	op := Materialize[[]byte]{Input: scan(it), Clone: func(v []byte) []byte { return append([]byte(nil), v...) }, Charge: func([]byte) error { return nil }}
	if got := collect(t, op); !reflect.DeepEqual(got, [][]byte{{1}, {2}, {3}}) || !it.closed {
		t.Fatal(got, it.closed)
	}
	cleanup := errors.New("close failed")
	it = &testIterator{buf: make([]byte, 1), closeErr: cleanup}
	err := (Limit[[]byte]{Input: scan(it), Count: 1}).Run(context.Background(), func([]byte) error { return nil })
	if !errors.Is(err, cleanup) || it.n != 1 || !it.closed {
		t.Fatal(err, it)
	}
	downstream := errors.New("consumer failed")
	it = &testIterator{buf: make([]byte, 1)}
	err = scan(it).Run(context.Background(), func([]byte) error { return downstream })
	if !errors.Is(err, downstream) || !it.closed {
		t.Fatal(err)
	}
	it = &testIterator{buf: make([]byte, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	err = scan(it).Run(ctx, func([]byte) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || !it.closed {
		t.Fatal(err)
	}
	opened := false
	zero := Limit[int]{Input: Source[int](func(context.Context, Yield[int]) error { opened = true; return nil }), Count: 0}
	collect(t, zero)
	if opened {
		t.Fatal("zero limit opened input")
	}
}

type sumState struct {
	total  int
	closed *bool
}

func (s *sumState) Add(v int) error           { s.total += v; return nil }
func (s *sumState) Finish(y Yield[int]) error { return y(s.total) }
func (s *sumState) Close() error              { *s.closed = true; return nil }

type intSorter struct {
	rows   []int
	closed *bool
}

func (s *intSorter) Add(v int) error { s.rows = append(s.rows, v); return nil }
func (s *intSorter) Finish(y func(int) error) error {
	sort.Ints(s.rows)
	for _, r := range s.rows {
		if err := y(r); err != nil {
			return err
		}
	}
	return nil
}
func (s *intSorter) Close() error { *s.closed = true; return nil }
func TestRelationalComposition(t *testing.T) {
	closed := false
	filter := Filter[int]{Input: values(3, 1, 2, 4), Predicate: func(v int) (bool, error) { return v%2 == 0, nil }}
	project := Projection[int, int]{Input: filter, Project: func(v int) (int, error) { return v * 10, nil }}
	agg := Aggregate[int, int]{Input: project, New: func() (Accumulator[int, int], error) { return &sumState{closed: &closed}, nil }}
	if got := collect(t, agg); !reflect.DeepEqual(got, []int{60}) || !closed {
		t.Fatal(got, closed)
	}
	if got := collect(t, agg); !reflect.DeepEqual(got, []int{60}) {
		t.Fatal("state reused", got)
	}
	sorter := Sort[int]{Input: values(4, 1, 3, 2), New: func() (Sorter[int], error) { return &intSorter{closed: &closed}, nil }}
	closed = false
	if got := collect(t, TopN[int]{Sort: sorter, Offset: 1, Count: 2}); !reflect.DeepEqual(got, []int{2, 3}) || !closed {
		t.Fatal(got, closed)
	}
	join := Join[int]{Left: values(1, 2), Right: func(l int) (Operator[int], error) {
		if l == 1 {
			return values(1, 1), nil
		}
		return values(), nil
	}, Combine: func(a, b int) int { return a*10 + b }, NullRight: func(a int) int { return a * 10 }}
	union := Union[int]{Inputs: []Operator[int]{join, values(99)}}
	if got := collect(t, union); !reflect.DeepEqual(got, []int{11, 11, 20, 99}) {
		t.Fatal(got)
	}
	material := Materialize[int]{Input: values(1, 2, 3), Clone: func(v int) int { return v }, Charge: func(int) error { return nil }}
	window := Window[int, int]{Input: material, Evaluate: func(rows []int, y Yield[int]) error {
		sum := 0
		for _, v := range rows {
			sum += v
			if err := y(sum); err != nil {
				return err
			}
		}
		return nil
	}}
	modified := []int{}
	modify := Modify[int, int]{Input: window, Apply: func(_ context.Context, v int) (int, error) { modified = append(modified, v); return v, nil }}
	if got := collect(t, modify); !reflect.DeepEqual(got, []int{1, 3, 6}) || !reflect.DeepEqual(got, modified) {
		t.Fatal(got, modified)
	}
	failure := errors.New("budget exceeded")
	material.Charge = func(int) error { return failure }
	emitted := false
	if err := material.Run(context.Background(), func(int) error { emitted = true; return nil }); !errors.Is(err, failure) || emitted {
		t.Fatal(err, emitted)
	}
}
