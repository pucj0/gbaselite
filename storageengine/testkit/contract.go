package testkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"gbaselite/storageengine"
	"reflect"
	"testing"
)

// Factory must return a fresh isolated engine; Run owns its lifetime. New
// backends run this suite unchanged, in addition to their durability tests.
type Factory func(*testing.T) storageengine.Engine

func Run(t *testing.T, factory Factory) {
	ctx := context.Background()
	fresh := func(t *testing.T) storageengine.Engine {
		e := factory(t)
		t.Cleanup(func() {
			if err := e.Close(); err != nil {
				t.Error(err)
			}
		})
		return e
	}
	begin := func(t *testing.T, e storageengine.Engine) storageengine.Txn {
		tx, err := e.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tx.Rollback() })
		return tx
	}
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, tx storageengine.Txn) uint64 {
		t.Helper()
		v, err := tx.Commit(ctx)
		must(t, err)
		return v
	}
	put := func(t *testing.T, tx storageengine.Txn, k, v string) {
		t.Helper()
		must(t, tx.Put("s", []byte(k), []byte(v)))
	}
	t.Run("SnapshotsOwnershipAndChildren", func(t *testing.T) {
		e := fresh(t)
		tx := begin(t, e)
		k, v := []byte("a"), []byte("one")
		must(t, tx.Put("s", k, v))
		k[0] = 'z'
		v[0] = 'x'
		got, ok, err := tx.Get("s", []byte("a"))
		must(t, err)
		if !ok || string(got) != "one" {
			t.Fatal(got, ok)
		}
		got[0] = 'x'
		got, _, err = tx.Get("s", []byte("a"))
		must(t, err)
		if string(got) != "one" {
			t.Fatal("Get aliases stored bytes")
		}
		revision := commit(t, tx)
		old := begin(t, e)
		parent := begin(t, e)
		put(t, parent, "p", "parent")
		child, err := parent.Child()
		must(t, err)
		must(t, child.Delete("s", []byte("a")))
		put(t, child, "b", "child")
		must(t, child.Rollback())
		got, ok, err = parent.Get("s", []byte("a"))
		must(t, err)
		if !ok || string(got) != "one" {
			t.Fatal("rollback changed parent")
		}
		child, err = parent.Child()
		must(t, err)
		put(t, child, "a", "two")
		commit(t, child)
		got, _, err = parent.Get("s", []byte("a"))
		must(t, err)
		if string(got) != "two" {
			t.Fatal("child not merged")
		}
		commit(t, parent)
		got, _, err = old.Get("s", []byte("a"))
		must(t, err)
		if string(got) != "one" {
			t.Fatal("snapshot changed")
		}
		if revisions, ok := e.(storageengine.RevisionReader); ok {
			it, err := revisions.NewIterator(ctx, revision, storageengine.ScanRequest{Space: "s"})
			must(t, err)
			var rows []string
			must(t, storageengine.Consume(it, func(k, v []byte) error { rows = append(rows, string(k)+"="+string(v)); return nil }))
			if !reflect.DeepEqual(rows, []string{"a=one"}) {
				t.Fatal(rows)
			}
		}

		must(t, old.Rollback())
		if _, _, err := old.Get("s", []byte("a")); !errors.Is(err, storageengine.ErrClosed) {
			t.Fatal("closed read", err)
		}
	})
	t.Run("IteratorsTablesAndIndexes", func(t *testing.T) {
		e := fresh(t)
		tx := begin(t, e)
		for _, k := range []string{"a", "b", "c", "d"} {
			must(t, tx.Table("t").Put([]byte(k), []byte(k+"!")))
		}
		must(t, tx.Table("t").Index("i", storageengine.SecondaryIndex).Put([]byte("b"), []byte("index")))
		child, err := tx.Child()
		must(t, err)
		must(t, child.Table("t").Delete([]byte("b")))
		must(t, child.Table("t").Put([]byte("bb"), []byte("new")))
		it, err := child.Table("t").Scan(ctx, storageengine.ScanRequest{Range: storageengine.KeyRange{Lower: []byte("a"), Upper: []byte("d"), UpperInclusive: true, Reverse: true}, Limit: 2})
		must(t, err)
		var rows []string
		must(t, storageengine.Consume(it, func(k, v []byte) error { rows = append(rows, string(k)); return nil }))
		if !reflect.DeepEqual(rows, []string{"d", "c"}) {
			t.Fatal(rows)
		}
		got, ok, err := child.Table("t").Index("i", storageengine.SecondaryIndex).Get([]byte("b"))
		must(t, err)
		if !ok || string(got) != "index" {
			t.Fatal("index space", got)
		}
		must(t, child.Rollback())
		it, err = tx.Table("t").Scan(ctx, storageengine.ScanRequest{})
		must(t, err)
		if !it.Next() {
			t.Fatal(it.Err())
		}
		must(t, it.Close())
		must(t, it.Close())
		if it.Next() {
			t.Fatal("closed iterator advanced")
		}
		for _, r := range []storageengine.ScanRequest{{Limit: -1}, {Unordered: true, Range: storageengine.KeyRange{Lower: []byte{}}}} {
			it, err := tx.NewIterator(ctx, r)
			if err == nil {
				it.Close()
				t.Fatal("invalid request accepted")
			}
		}
		cancelCtx, cancel := context.WithCancel(ctx)
		it, err = tx.Table("t").Scan(cancelCtx, storageengine.ScanRequest{})
		must(t, err)
		cancel()
		if it.Next() || !errors.Is(it.Err(), context.Canceled) {
			t.Fatal("cancel", it.Err())
		}
		it.Close()
	})
	t.Run("WriteAndPointGuards", func(t *testing.T) {
		e := fresh(t)
		a, b := begin(t, e), begin(t, e)
		put(t, a, "a", "first")
		put(t, b, "a", "second")
		commit(t, a)
		if _, err := b.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
			t.Fatal(err)
		}
		a, b = begin(t, e), begin(t, e)
		must(t, a.Guard("s", []byte("missing")))
		put(t, b, "missing", "v")
		commit(t, b)
		if _, err := a.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
			t.Fatal("point phantom", err)
		}
		a, b = begin(t, e), begin(t, e)
		put(t, a, "x", "x")
		put(t, b, "y", "y")
		commit(t, a)
		commit(t, b)
	})
	for _, bounds := range []storageengine.KeyRange{{}, {Lower: []byte("b"), Upper: []byte("d")}, {Lower: []byte("b"), Upper: []byte("d"), LowerInclusive: true, UpperInclusive: true}, {Lower: []byte("b"), Upper: []byte("b"), LowerInclusive: true, UpperInclusive: true}, {Lower: []byte("d"), Upper: []byte("b")}, {Upper: []byte{}}, {Upper: []byte{}, UpperInclusive: true}, {Lower: []byte{}, LowerInclusive: true}} {
		for _, key := range []string{"", "a", "b", "c", "d", "e"} {
			for _, deletion := range []bool{false, true} {
				t.Run(fmt.Sprintf("Range/%x-%x-%t-%t/%s/delete=%t", bounds.Lower, bounds.Upper, bounds.LowerInclusive, bounds.UpperInclusive, key, deletion), func(t *testing.T) {
					e := fresh(t)
					seed := begin(t, e)
					put(t, seed, key, "old")
					commit(t, seed)
					guard, writer := begin(t, e), begin(t, e)
					child, err := guard.Child()
					must(t, err)
					owned := bounds.CloneBounds()
					must(t, child.GuardRange("s", owned))
					if len(owned.Lower) > 0 {
						owned.Lower[0] = 'z'
					}
					commit(t, child)
					if deletion {
						must(t, writer.Delete("s", []byte(key)))
					} else {
						put(t, writer, key, "new")
					}
					commit(t, writer)
					_, err = guard.Commit(ctx)
					if bounds.Contains([]byte(key)) {
						if !errors.Is(err, storageengine.ErrConflict) {
							t.Fatal("range conflict missing", err)
						}
					} else {
						must(t, err)
					}
				})
			}
		}
	}
	t.Run("RangeInsertAndRollback", func(t *testing.T) {
		e := fresh(t)
		guard, writer := begin(t, e), begin(t, e)
		bounds := storageengine.KeyRange{Lower: []byte("b"), Upper: []byte("d")}
		must(t, guard.GuardRange("s", bounds))
		put(t, writer, "c", "new")
		commit(t, writer)
		if _, err := guard.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
			t.Fatal(err)
		}
		parent := begin(t, e)
		child, err := parent.Child()
		must(t, err)
		must(t, child.GuardRange("s", bounds))
		must(t, child.Rollback())
		writer = begin(t, e)
		put(t, writer, "c", "again")
		commit(t, writer)
		put(t, parent, "z", "ok")
		commit(t, parent)
	})
	t.Run("CountersAndCancellation", func(t *testing.T) {
		e := fresh(t)
		if counters, ok := e.(storageengine.CounterAllocator); ok {
			must(t, counters.AdvanceCounter(ctx, "counter", 10))
			first, err := counters.ReserveCounter(ctx, "counter", 2)
			must(t, err)
			if first != 11 {
				t.Fatal(first)
			}
			must(t, counters.AdvanceCounter(ctx, "counter", 1))
			first, err = counters.ReserveCounter(ctx, "counter", 1)
			must(t, err)
			if first != 13 {
				t.Fatal(first)
			}
		}

		c, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := e.Begin(c); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		tx := begin(t, e)
		put(t, tx, "cancel", "value")
		if _, err := tx.Commit(c); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		tx.Rollback()
		read := begin(t, e)
		v, ok, err := read.Get("s", []byte("cancel"))
		must(t, err)
		if ok || !bytes.Equal(v, nil) {
			t.Fatal("canceled commit visible")
		}
	})
}
