package mvcc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
	"testing"
)

func TestLocalWALStreamingAndRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWithOptions(dir, Options{LocalWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err = tx.Put("r", []byte(fmt.Sprint(i)), bytes.Repeat([]byte("x"), 4000)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.db.NoSync {
		t.Fatal("durability disabled")
	}
	head, _ := s.Head()
	k, _ := key("r", []byte("recovered"))
	if err = s.writeLocalWAL(context.Background(), func(w *localWALWriter) error {
		if err := w.begin(head+1, "recovery"); err != nil {
			return err
		}
		if err := w.op(k, encodeStagedOp(Op{Value: []byte("durable")})); err != nil {
			return err
		}
		return w.frame([]byte{walEnd})
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, _, ok, err := s.Get(^uint64(0), "r", []byte("recovered"))
	if err != nil || !ok || string(v) != "durable" {
		t.Fatal(string(v), err)
	}
	for i := 0; i < 100; i++ {
		v, _, ok, err := s.Get(^uint64(0), "r", []byte(fmt.Sprint(i)))
		if err != nil || !ok || len(v) != 4000 {
			t.Fatal(i, err)
		}
	}
	st, err := os.Stat(s.walPath())
	if err != nil || st.Size() != 0 {
		t.Fatal("WAL not checkpointed", err)
	}
}
func TestLocalWALRejectsDamagedSealBeforeInstall(t *testing.T) {
	s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	k, _ := key("r", []byte("k"))
	err = s.writeLocalWAL(context.Background(), func(w *localWALWriter) error {
		if err := w.begin(1, "bad"); err != nil {
			return err
		}
		if err := w.op(k, encodeStagedOp(Op{Value: []byte("v")})); err != nil {
			return err
		}
		return w.frame([]byte{walEnd})
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.walPath())
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err = os.WriteFile(s.walPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.recoverLocalWAL(); err == nil {
		t.Fatal("corruption accepted")
	}
	if err = s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(dataBucket).Bucket(k) != nil {
			t.Fatal("prefix installed before seal validated")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestLocalWALGroupConflictAndCancel(t *testing.T) {
	s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	request := func(id string) *localCommitRequest {
		return &localCommitRequest{ctx: context.Background(), id: id, ops: []Op{{Space: "r", Key: []byte("k"), Value: []byte(id)}}, done: make(chan struct{})}
	}
	a, b := request("a"), request("b")
	s.commitLocalGroup([]*localCommitRequest{a, b})
	if a.err != nil || !errors.Is(b.err, ErrConflict) {
		t.Fatal(a.err, b.err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.writeLocalWAL(ctx, func(w *localWALWriter) error {
		if err := w.begin(2, "cancel"); err != nil {
			return err
		}
		return w.frame([]byte{walEnd})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = s.recoverLocalWAL(); err != nil {
		t.Fatal(err)
	}
	head, _ := s.Head()
	if head != a.index {
		t.Fatal(head)
	}
}
