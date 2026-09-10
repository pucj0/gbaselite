package physical

import (
	"context"
	"errors"
	"testing"
)

type failingPartition struct {
	MemoryPartitionStore[int]
	addErr, readErr, closeErr error
	closes                    int
}

func (p *failingPartition) Append(c context.Context, v int) error {
	if p.addErr != nil {
		return p.addErr
	}
	return p.MemoryPartitionStore.Append(c, v)
}
func (p *failingPartition) At(c context.Context, i int) (int, error) {
	if p.readErr != nil {
		return 0, p.readErr
	}
	return p.MemoryPartitionStore.At(c, i)
}
func (p *failingPartition) Close() error {
	p.closes++
	p.MemoryPartitionStore.Close()
	return p.closeErr
}
func TestWindowStoreLifecycle(t *testing.T) {
	boom := errors.New("store failure")
	cleanup := errors.New("cleanup failure")
	for _, stage := range []string{"append", "read", "yield", "cancel", "success"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &failingPartition{MemoryPartitionStore: MemoryPartitionStore[int]{Clone: func(v int) int { return v }, Charge: func(int) error { return nil }}, closeErr: cleanup}
			if stage == "append" {
				p.addErr = boom
			}
			if stage == "read" {
				p.readErr = boom
			}
			op := Window[int, int]{Input: Source[int](func(c context.Context, y Yield[int]) error { return y(7) }), NewStore: func() (PartitionStore[int], error) { return p, nil }, EvaluateStore: func(c context.Context, s PartitionStore[int], y Yield[int]) error {
				if stage == "cancel" {
					cancel()
				}
				v, e := s.At(c, 0)
				if e != nil {
					return e
				}
				return y(v)
			}}
			err := op.Run(ctx, func(int) error {
				if stage == "yield" {
					return boom
				}
				return nil
			})
			if !errors.Is(err, cleanup) || p.closes != 1 || p.Len() != 0 {
				t.Fatal(err, p.closes, p.Len())
			}
			if (stage == "append" || stage == "read" || stage == "yield") && !errors.Is(err, boom) {
				t.Fatal(err)
			}
			if stage == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
func TestMemoryPartitionChargesBeforeRetaining(t *testing.T) {
	boom := errors.New("budget")
	cloned := false
	p := MemoryPartitionStore[int]{Clone: func(v int) int { cloned = true; return v }, Charge: func(int) error { return boom }}
	if err := p.Append(context.Background(), 1); !errors.Is(err, boom) || cloned || p.Len() != 0 {
		t.Fatal(err, cloned)
	}
}
