package physical

import (
	"context"
	"errors"
	"testing"
)

func TestDrainLimitPreservesLateErrors(t *testing.T) {
	for _, count := range []int{0, 1} {
		visited, emitted := 0, 0
		boom := errors.New("late projection failure")
		input := Source[int](func(c context.Context, y Yield[int]) error {
			for i := 0; i < 3; i++ {
				visited++
				if i == 2 {
					return boom
				}
				if err := y(i); err != nil {
					return err
				}
			}
			return nil
		})
		err := (Limit[int]{Drain: true, Input: input, Count: count}).Run(context.Background(), func(int) error { emitted++; return nil })
		if !errors.Is(err, boom) || visited != 3 || emitted != count {
			t.Fatal(count, err, visited, emitted)
		}
	}
}
