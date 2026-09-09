package mvcc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestRangeMergeSnapshotAndBounds(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	seed, _ := s.Begin(ctx, nil)
	for i := 0; i < 600; i++ {
		if err = seed.Put("rows", []byte(fmt.Sprintf("%04d", i)), []byte("base")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	old, _ := s.Begin(ctx, nil)
	defer old.Rollback()
	writer, _ := s.Begin(ctx, nil)
	writer.Put("rows", []byte("0102"), []byte("committed"))
	writer.Delete("rows", []byte("0103"))
	writer.Put("rowss", []byte("0101"), []byte("other namespace"))
	if _, err = writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	parent, _ := s.Begin(ctx, nil)
	defer parent.Rollback()
	parent.Put("rows", []byte("0101"), []byte("parent"))
	parent.Delete("rows", []byte("0102"))
	parent.Put("rows", []byte("01035"), []byte("insert"))
	child, _ := parent.Child()
	defer child.Rollback()
	child.Guard("rows", []byte("0101"))
	child.Put("rows", []byte("0102"), []byte("child"))
	child.Delete("rows", []byte("0104"))
	child.Delete("rows", []byte("01035"))
	collect := func(tx *Tx, r KeyRange) []string {
		t.Helper()
		var got []string
		if err := tx.ScanRange(ctx, "rows", r, func(k, v []byte) error { got = append(got, string(k)+":"+string(v)); return nil }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	r := KeyRange{Lower: []byte("0100"), Upper: []byte("0105"), LowerInclusive: true, UpperInclusive: true}
	got := collect(child, r)
	want := []string{"0100:base", "0101:parent", "0102:child", "0105:base"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay %v", got)
	}
	r.Reverse = true
	got = collect(child, r)
	if !reflect.DeepEqual(got, []string{"0105:base", "0102:child", "0101:parent", "0100:base"}) {
		t.Fatalf("reverse %v", got)
	}
	r.Reverse = false
	if got = collect(old, r); !reflect.DeepEqual(got, []string{"0100:base", "0101:base", "0102:base", "0103:base", "0104:base", "0105:base"}) {
		t.Fatalf("old view %v", got)
	}
	for _, reverse := range []bool{false, true} {
		for _, bounds := range []KeyRange{
			{Lower: []byte("0100"), Upper: []byte("0100")},
			{Lower: []byte("0105"), Upper: []byte("0100"), LowerInclusive: true, UpperInclusive: true},
			{Lower: []byte("9999"), LowerInclusive: true},
			{Upper: []byte("0000")},
		} {
			bounds.Reverse = reverse
			if got = collect(old, bounds); len(got) != 0 {
				t.Fatalf("empty %+v: %v", bounds, got)
			}
		}
		single := KeyRange{Lower: []byte("0100"), Upper: []byte("0100"), LowerInclusive: true, UpperInclusive: true, Reverse: reverse}
		if got = collect(old, single); !reflect.DeepEqual(got, []string{"0100:base"}) {
			t.Fatal(got)
		}
	}
}

func TestRangePagingWorkAndEarlyStop(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	tx, _ := s.Begin(ctx, nil)
	for i := 0; i < 1500; i++ {
		if err = tx.Put("rows", []byte(fmt.Sprintf("%05d", i)), []byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	read, _ := s.Begin(ctx, nil)
	defer read.Rollback()
	for _, reverse := range []bool{false, true} {
		stats := ScanStats{}
		count := 0
		r := KeyRange{Lower: []byte("01000"), Upper: []byte("01100"), LowerInclusive: true, Reverse: reverse, Stats: &stats}
		err = read.ScanRange(ctx, "rows", r, func(k, v []byte) error {
			expected := 1000 + count
			if reverse {
				expected = 1099 - count
			}
			if string(k) != fmt.Sprintf("%05d", expected) {
				t.Fatalf("row %s expected %d", k, expected)
			}
			count++
			return nil
		})
		if err != nil || count != 100 || stats.StoredKeys != 100 {
			t.Fatalf("bounded work count=%d stats=%+v err=%v", count, stats, err)
		}
		stats = ScanStats{}
		count = 0
		stop := errors.New("stop")
		err = read.ScanRange(ctx, "rows", KeyRange{Reverse: reverse, Stats: &stats}, func(k, v []byte) error { count++; return stop })
		if !errors.Is(err, stop) || count != 1 || stats.StoredKeys > 256 {
			t.Fatalf("prefetch %+v %v", stats, err)
		}
		count = 0
		err = read.ScanRange(ctx, "rows", KeyRange{Reverse: reverse}, func(k, v []byte) error {
			expected := count
			if reverse {
				expected = 1499 - count
			}
			if string(k) != fmt.Sprintf("%05d", expected) {
				t.Fatalf("page row %s expected %d", k, expected)
			}
			count++
			return nil
		})
		if err != nil || count != 1500 {
			t.Fatalf("page count %d %v", count, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = read.ScanRange(canceled, "rows", KeyRange{}, func([]byte, []byte) error { t.Fatal("called after cancel"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	read.Rollback()
	if err = read.ScanRange(ctx, "rows", KeyRange{}, func([]byte, []byte) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestRangeStagePagesAndOwnInserts(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin(context.Background(), nil)
	defer tx.Rollback()
	for i := 0; i < 700; i++ {
		if err = tx.Put("rows", []byte(fmt.Sprintf("%05d", i)), []byte("new")); err != nil {
			t.Fatal(err)
		}
	}
	child, _ := tx.Child()
	defer child.Rollback()
	for i := 100; i < 600; i += 2 {
		if err = child.Delete("rows", []byte(fmt.Sprintf("%05d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for _, reverse := range []bool{false, true} {
		var got []string
		err = child.ScanRange(context.Background(), "rows", KeyRange{Reverse: reverse}, func(k, v []byte) error { got = append(got, string(k)); return nil })
		var want []string
		for i := 0; i < 700; i++ {
			if i >= 100 && i < 600 && i%2 == 0 {
				continue
			}
			want = append(want, fmt.Sprintf("%05d", i))
		}
		if reverse {
			for a, b := 0, len(want)-1; a < b; a, b = a+1, b-1 {
				want[a], want[b] = want[b], want[a]
			}
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("staged merge count %d want %d err %v", len(got), len(want), err)
		}
	}
}
