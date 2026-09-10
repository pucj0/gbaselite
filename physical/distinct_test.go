package physical

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

type distinctTestSorter struct {
	rows    []DistinctRow[int]
	byKey   bool
	closed  *int
	failure error
}

func (s *distinctTestSorter) Add(r DistinctRow[int]) error { s.rows = append(s.rows, r); return nil }
func (s *distinctTestSorter) Finish(y func(DistinctRow[int]) error) error {
	sort.Slice(s.rows, func(i, j int) bool {
		a, b := s.rows[i], s.rows[j]
		if s.byKey && a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.Ordinal < b.Ordinal
	})
	for _, r := range s.rows {
		if err := y(r); err != nil {
			return err
		}
	}
	return nil
}
func (s *distinctTestSorter) Close() error { *s.closed++; return s.failure }
func TestDistinctStableRepresentativeLifecycle(t *testing.T) {
	closed := 0
	op := Distinct[int]{Input: values(21, 10, 11, 20, 32), Key: func(v int) (string, error) { return strconv.Itoa(v % 10), nil }, NewSort: func(byKey bool) (Sorter[DistinctRow[int]], error) {
		return &distinctTestSorter{byKey: byKey, closed: &closed}, nil
	}}
	for i := 0; i < 2; i++ {
		if got := collect(t, op); !reflect.DeepEqual(got, []int{21, 10, 32}) {
			t.Fatal(got)
		}
	}
	if closed != 4 {
		t.Fatal(closed)
	}
	failure := errors.New("downstream failed")
	if err := op.Run(context.Background(), func(int) error { return failure }); !errors.Is(err, failure) || closed != 6 {
		t.Fatal(err, closed)
	}
	op.NewSort = func(byKey bool) (Sorter[DistinctRow[int]], error) {
		if !byKey {
			return nil, failure
		}
		return &distinctTestSorter{byKey: true, closed: &closed}, nil
	}
	if err := op.Run(context.Background(), func(int) error { return nil }); !errors.Is(err, failure) || closed != 7 {
		t.Fatal("factory cleanup", err, closed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := op.Run(ctx, func(int) error { return nil }); !errors.Is(err, context.Canceled) || closed != 7 {
		t.Fatal("opened on cancellation", err)
	}
}
