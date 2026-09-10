package mvcc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestBoundedGuardSameCommitGroup(t *testing.T) {
	for _, wal := range []bool{false, true} {
		for _, key := range []string{"inside", "outside"} {
			s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: wal})
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(rangeDependency{Space: "s", Bounds: KeyRange{Lower: []byte("inside"), Upper: []byte("inside"), LowerInclusive: true, UpperInclusive: true}})
			requests := []*localCommitRequest{{ctx: context.Background(), generation: s.generation.Load(), id: "writer", ops: []Op{{Space: "s", Key: []byte(key), Value: []byte("v")}}, done: make(chan struct{})}, {ctx: context.Background(), generation: s.generation.Load(), id: "guard", ops: []Op{{Space: rangeGuardSpace, Key: append([]byte{0, 1}, payload...), Check: true}}, done: make(chan struct{})}}
			s.commitLocalGroup(requests)
			if requests[0].err != nil {
				t.Fatal(requests[0].err)
			}
			if key == "inside" && !errors.Is(requests[1].err, ErrConflict) || key == "outside" && requests[1].err != nil {
				t.Fatal(wal, key, requests[1].err)
			}
			s.Close()
		}
	}
}
func TestBoundedGuardFlatLayoutAndLongEndpoints(t *testing.T) {
	s := flatScanFixture(t, 0, 0)
	ctx := context.Background()
	guarded, err := s.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.Repeat([]byte("a"), 8192)
	upper := bytes.Repeat([]byte("z"), 8192)
	if err := guarded.GuardRange("s", KeyRange{Lower: lower, Upper: upper}); err != nil {
		t.Fatal(err)
	}
	writer, err := s.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Put("s", []byte("m"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = guarded.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal("flat phantom", err)
	}
}
