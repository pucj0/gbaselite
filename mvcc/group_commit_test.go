package mvcc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGroupCommitConflictsCancellationAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	request := func(id, k string, ctx context.Context) *localCommitRequest {
		return &localCommitRequest{ctx: ctx, generation: s.generation.Load(), id: id, ops: []Op{{Space: "r", Key: []byte(k), Value: []byte(id)}}, done: make(chan struct{})}
	}
	a, b, c, d := request("first", "same", context.Background()), request("conflict", "same", context.Background()), request("canceled", "other", canceled), request("last", "last", context.Background())
	s.commitLocalGroup([]*localCommitRequest{a, b, c, d})
	if a.err != nil || d.err != nil || !errors.Is(b.err, ErrConflict) || !errors.Is(c.err, context.Canceled) {
		t.Fatal(a.err, b.err, c.err, d.err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for k, want := range map[string]string{"same": "first", "last": "last"} {
		v, _, ok, err := s.Get(^uint64(0), "r", []byte(k))
		if err != nil || !ok || string(v) != want {
			t.Fatal(k, string(v), err)
		}
	}
	if _, _, ok, err := s.Get(^uint64(0), "r", []byte("other")); err != nil || ok {
		t.Fatal("canceled write installed", err)
	}
}
func TestConcurrentLocalCommitQueue(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	fail := make(chan error, 80)
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint(i)
			_, err := s.commitLocal(context.Background(), s.generation.Load(), 0, id, []Op{{Space: "r", Key: []byte(id), Value: []byte(id)}})
			if err != nil {
				fail <- err
			}
		}(i)
	}
	wg.Wait()
	close(fail)
	for err := range fail {
		t.Error(err)
	}
	for i := 0; i < 80; i++ {
		id := fmt.Sprint(i)
		v, _, ok, err := s.Get(^uint64(0), "r", []byte(id))
		if err != nil || !ok || string(v) != id {
			t.Fatal(id, err)
		}
	}
}
func TestGroupCommitFailsClosedOnStorageFailure(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.db.Close()
	r := &localCommitRequest{ctx: context.Background(), id: "fail", ops: []Op{{Space: "r", Key: []byte("k"), Value: []byte("v")}}, done: make(chan struct{})}
	s.commitLocalGroup([]*localCommitRequest{r})
	if r.err == nil || r.index != 0 || s.AvailabilityError() == nil {
		t.Fatal("write failure was acknowledged")
	}
}

func BenchmarkLocalCommitGrouping(b *testing.B) {
	for _, grouped := range []bool{false, true} {
		name := "serial"
		if grouped {
			name = "grouped"
		}
		b.Run(name, func(b *testing.B) {
			s, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			var next atomic.Uint64
			b.SetParallelism(4)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					id := fmt.Sprint(next.Add(1))
					ops := []Op{{Space: "r", Key: []byte(id), Value: []byte("value")}}
					var err error
					if grouped {
						_, err = s.commitLocal(context.Background(), 0, 0, id, ops)
					} else {
						r := &localCommitRequest{ctx: context.Background(), id: id, ops: ops, done: make(chan struct{})}
						s.commitLocalGroup([]*localCommitRequest{r})
						err = r.err
					}
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
