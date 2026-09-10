package physical

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
)

func TestJoinHeterogeneousInputsAndCleanup(t *testing.T) {
	type output struct {
		ID   int
		Text string
	}
	visits := 0
	join := Join3[int, string, output]{Left: values(1, 2), Right: func(n int) (Operator[string], error) {
		return Source[string](func(_ context.Context, y Yield[string]) error { visits++; return y(strconv.Itoa(n)) }), nil
	}, Combine: func(n int, s string) output { return output{n, s} }, Predicate: func(r output) (bool, error) { return r.ID == 1, nil }, NullRight: func(n int) output { return output{ID: n} }}
	if got := collect(t, join); !reflect.DeepEqual(got, []output{{1, "1"}, {2, ""}}) {
		t.Fatal(got)
	}
	if visits != 2 {
		t.Fatal(visits)
	}
	failure := errors.New("downstream")
	if err := join.Run(context.Background(), func(output) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	join.Right = func(int) (Operator[string], error) { return nil, failure }
	if err := join.Run(context.Background(), func(output) error { return nil }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := join.Run(ctx, func(output) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
