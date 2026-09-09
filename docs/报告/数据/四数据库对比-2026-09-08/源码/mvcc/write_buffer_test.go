package mvcc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBufferBoundsReadOwnWritesAndBudget(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin(context.Background(), nil)
	defer tx.Rollback()
	key, value := []byte("own"), []byte("original")
	if err = tx.Put("rows", key, value); err != nil {
		t.Fatal(err)
	}
	key[0] = 'X'
	value[0] = 'X'
	if got, ok, err := tx.Get("rows", []byte("own")); err != nil || !ok || string(got) != "original" {
		t.Fatal(got, ok, err)
	}
	if tx.stage != nil {
		t.Fatal("small write unnecessarily opened staging file")
	}
	originalBytes := tx.stagedBytes
	if err = tx.Guard("rows", []byte("own")); err != nil || tx.stagedBytes != originalBytes {
		t.Fatal("guard overwrote put", err)
	}
	for i := 0; i < 1200; i++ {
		if err = tx.Put("rows", []byte(fmt.Sprintf("%05d", i)), bytes.Repeat([]byte{1}, 512)); err != nil {
			t.Fatal(err)
		}
		if tx.bufferBytes > writeBufferBytes {
			t.Fatal("buffer budget", tx.bufferBytes)
		}
	}
	if tx.stage == nil {
		t.Fatal("large transaction did not spill")
	}
	if err = tx.Delete("rows", []byte("own")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := tx.Get("rows", []byte("own")); err != nil || ok {
		t.Fatal("buffered tombstone ignored", err)
	}
	s.writeSetLimit = tx.stagedBytes
	if err = tx.Put("rows", []byte("over-budget"), []byte("x")); !errors.Is(err, ErrWriteSetLimit) {
		t.Fatal(err)
	}
	if _, ok, err := tx.Get("rows", []byte("over-budget")); err != nil || ok {
		t.Fatal("rejected operation became visible")
	}
}
func TestLocalAtomicCommitConflictAndStreaming(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	seed, _ := s.Begin(ctx, nil)
	seed.Put("rows", []byte("x"), []byte("old"))
	before := s.localSeq
	if _, err = seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if s.localSeq != before+1 || s.db.NoSync {
		t.Fatal("small standalone commit path/durability")
	}
	old, _ := s.Begin(ctx, nil)
	defer old.Rollback()
	a, _ := s.Begin(ctx, nil)
	b, _ := s.Begin(ctx, nil)
	a.Put("rows", []byte("x"), []byte("a"))
	b.Put("rows", []byte("x"), []byte("b"))
	b.Put("rows", []byte("y"), []byte("must rollback"))
	if _, err = a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if got, ok, _ := old.Get("rows", []byte("x")); !ok || string(got) != "old" {
		t.Fatal("snapshot", string(got))
	}
	now, _ := s.Begin(ctx, nil)
	defer now.Rollback()
	if _, ok, _ := now.Get("rows", []byte("y")); ok {
		t.Fatal("partial local transaction committed")
	}
	if s.AvailabilityError() != nil {
		t.Fatal("conflict poisoned store")
	}
	large, _ := s.Begin(ctx, nil)
	for i := 0; i < 20; i++ {
		large.Put("rows", []byte(fmt.Sprint(i)), bytes.Repeat([]byte{byte(i)}, 4096))
	}
	before = s.localSeq
	if _, err = large.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if s.localSeq != before+1 || s.db.NoSync {
		t.Fatal("large standalone commit must use one logical version and synced batches")
	}
	p, err := s.PendingBytes()
	if err != nil || p != 0 {
		t.Fatal("pending writes retained", p, err)
	}
}
func TestLocalAtomicCommitReopensWithoutGracefulClose(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestLocalAtomicCrashHelper$")
	command.Env = append(os.Environ(), "GBASELITE_ATOMIC_HELPER="+dir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %s %v", output, err)
	}
	if !strings.Contains(string(output), "committed") {
		t.Fatal("no durable acknowledgement", string(output))
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin(context.Background(), nil)
	defer tx.Rollback()
	for i := 0; i < 100; i++ {
		v, ok, err := tx.Get("rows", []byte(fmt.Sprint(i)))
		if err != nil || !ok || string(v) != "durable" {
			t.Fatalf("row %d %s %v %v", i, v, ok, err)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "transactions")); !os.IsNotExist(err) {
		t.Fatalf("small transaction created temporary database: %v", err)
	}
}
func TestLocalAtomicCrashHelper(t *testing.T) {
	dir := os.Getenv("GBASELITE_ATOMIC_HELPER")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := s.Begin(context.Background(), nil)
	for i := 0; i < 100; i++ {
		if err = tx.Put("rows", []byte(fmt.Sprint(i)), []byte("durable")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	fmt.Println("committed")
	os.Exit(0) // Deliberately omit Store.Close: process exit is not a power-cut test.
}
