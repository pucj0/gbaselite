package mvccadapter_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
	"strings"
	"testing"
)

func openAdapter(t *testing.T, dir string) storageengine.FullEngine {
	t.Helper()
	e, err := mvccadapter.Open(dir, storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e.(storageengine.FullEngine)
}
func begin(t *testing.T, e storageengine.Engine) storageengine.Txn {
	t.Helper()
	tx, err := e.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}
func commit(t *testing.T, tx storageengine.Txn) {
	t.Helper()
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestAdapterContractDurabilitySnapshotsAndConflicts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e := openAdapter(t, dir)
	seed := begin(t, e)
	row, index := seed.Table("items"), seed.Table("items").Index("unique", storageengine.UniqueIndex)
	input := []byte("original")
	if err := row.Put([]byte("a"), input); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	if err := index.Put([]byte("lookup"), []byte("a")); err != nil {
		t.Fatal(err)
	}
	commit(t, seed)
	old := begin(t, e)
	writer := begin(t, e)
	if err := writer.Table("items").Put([]byte("a"), []byte("updated")); err != nil {
		t.Fatal(err)
	}
	commit(t, writer)
	value, _, err := old.Table("items").Get([]byte("a"))
	if err != nil || string(value) != "original" {
		t.Fatalf("snapshot %q %v", value, err)
	}
	value[0] = '!'
	again, _, _ := old.Table("items").Get([]byte("a"))
	if string(again) != "original" {
		t.Fatal("Get returned mutable backend memory")
	}
	if err := old.Table("items").Put([]byte("a"), []byte("conflict")); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("conflict %v", err)
	}
	first, err := e.ReserveCounter(ctx, "auto", 3)
	if err != nil || first != 1 {
		t.Fatalf("reserve %d %v", first, err)
	}
	if err = e.AdvanceCounter(ctx, "auto", 10); err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openAdapter(t, dir)
	read := begin(t, e)
	key, ok, err := read.Table("items").Index("unique", storageengine.UniqueIndex).Get([]byte("lookup"))
	if err != nil || !ok {
		t.Fatal(err)
	}
	value, _, err = read.Table("items").Get(key)
	if err != nil || string(value) != "updated" {
		t.Fatalf("reopen %q %v", value, err)
	}
	first, err = e.ReserveCounter(ctx, "auto", 1)
	if err != nil || first != 11 {
		t.Fatalf("durable counter %d %v", first, err)
	}
}
func TestAdapterIteratorRangeOverlayCancellationAndClose(t *testing.T) {
	e := openAdapter(t, t.TempDir())
	ctx := context.Background()
	seed := begin(t, e)
	for i := 0; i < 520; i++ {
		if err := seed.Table("t").Put([]byte(fmt.Sprintf("k%03d", i)), []byte(strings.Repeat("v", 300))); err != nil {
			t.Fatal(err)
		}
	}
	commit(t, seed)
	tx := begin(t, e)
	child, err := tx.Child()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Table("t").Delete([]byte("k102")); err != nil {
		t.Fatal(err)
	}
	if err = child.Table("t").Put([]byte("k104"), []byte("changed")); err != nil {
		t.Fatal(err)
	}
	commit(t, child)
	it, err := tx.Table("t").Scan(ctx, storageengine.ScanRequest{Range: storageengine.KeyRange{Lower: []byte("k100"), LowerInclusive: true, Upper: []byte("k105"), Reverse: true}, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err = storageengine.Consume(it, func(k, v []byte) error {
		keys = append(keys, string(k))
		if string(k) == "k104" && !bytes.Equal(v, []byte("changed")) {
			t.Fatal("child write invisible")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys) != "[k104 k103 k101]" {
		t.Fatal(keys)
	}
	// Spill to disk, stop in the middle, then commit: Close must release scan state.
	for i := 0; i < 520; i++ {
		if err = tx.Table("staged").Put([]byte(fmt.Sprintf("k%03d", i)), []byte(strings.Repeat("x", 300))); err != nil {
			t.Fatal(err)
		}
	}
	it, err = tx.Table("staged").Scan(ctx, storageengine.ScanRequest{Unordered: true})
	if err != nil {
		t.Fatal(err)
	}
	if !it.Next() {
		t.Fatal(it.Err())
	}
	if err = it.Close(); err != nil {
		t.Fatal(err)
	}
	if it.Next() {
		t.Fatal("iterator advanced after Close")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	it, err = tx.Table("t").Scan(cancelCtx, storageengine.ScanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !it.Next() {
		t.Fatal(it.Err())
	}
	cancel()
	if it.Next() || !errors.Is(it.Err(), context.Canceled) {
		t.Fatalf("cancel %v", it.Err())
	}
	it.Close()
	child, err = tx.Child()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Table("t").Put([]byte("discard"), []byte("no")); err != nil {
		t.Fatal(err)
	}
	child.Rollback()
	if _, ok, _ := tx.Table("t").Get([]byte("discard")); ok {
		t.Fatal("child rollback leaked")
	}
	commit(t, tx)
	read := begin(t, e)
	it, err = read.Table("t").Scan(ctx, storageengine.ScanRequest{Limit: -1})
	if err == nil {
		it.Close()
		t.Fatal("negative limit accepted")
	}
	stop := errors.New("consumer stopped")
	it, err = read.Table("t").Scan(ctx, storageengine.ScanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err = storageengine.Consume(it, func([]byte, []byte) error { return stop }); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if it.Next() {
		t.Fatal("consumer error did not close iterator")
	}
}
