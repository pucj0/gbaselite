package mvcc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifiedBackupAndRestore(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Put("r", []byte("k"), []byte("before")); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backup")
	manifest, err := s.Backup(context.Background(), backup)
	if err != nil || manifest.Bytes == 0 {
		t.Fatal(manifest, err)
	}
	target, err := OpenWithOptions(filepath.Join(root, "target"), Options{LocalWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err = target.RestoreBackup(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	v, _, ok, err := target.Get(^uint64(0), "r", []byte("k"))
	if err != nil || !ok || !bytes.Equal(v, []byte("before")) {
		t.Fatal(string(v), err)
	}
	tx, err = target.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Put("r", []byte("k"), []byte("after")); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Commit(context.Background()); err != nil {
		t.Fatal("WAL identity after restore", err)
	}
	active, err := target.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Rollback()
	f, err := os.OpenFile(filepath.Join(backup, "mvcc.db"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte{255}, 100); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err = target.RestoreBackup(context.Background(), backup); err == nil {
		t.Fatal("accepted corrupt backup")
	}
	v, ok, err = active.Get("r", []byte("k"))
	if err != nil || !ok || string(v) != "after" {
		t.Fatal("invalid restore disturbed active transaction", string(v), err)
	}
	if _, err = s.Backup(context.Background(), backup); err == nil {
		t.Fatal("overwrote backup")
	}
}

func TestOnlineBackupConcurrentAtomicWrites(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	write := func(n int) error {
		tx, err := s.Begin(context.Background(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, key := range []string{"a", "b"} {
			if err = tx.Put("pair", []byte(key), []byte(fmt.Sprint(n))); err != nil {
				return err
			}
		}
		_, err = tx.Commit(context.Background())
		return err
	}
	if err = write(0); err != nil {
		t.Fatal(err)
	}
	ready, stop, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		close(ready)
		for n := 1; ; n++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := write(n); err != nil {
				done <- err
				return
			}
		}
	}()
	<-ready
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 3; i++ {
		backup := filepath.Join(root, fmt.Sprintf("backup%d", i))
		manifest, err := s.Backup(context.Background(), backup)
		if err != nil {
			t.Fatal(err)
		}
		target, err := Open(filepath.Join(root, fmt.Sprintf("restore%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		err = target.RestoreBackup(context.Background(), backup)
		if err != nil {
			target.Close()
			t.Fatal(err)
		}
		a, _, okA, errA := target.Get(manifest.Head, "pair", []byte("a"))
		b, _, okB, errB := target.Get(manifest.Head, "pair", []byte("b"))
		target.Close()
		if errA != nil || errB != nil || !okA || !okB || !bytes.Equal(a, b) {
			t.Fatal("torn online snapshot", string(a), string(b), errA, errB)
		}
	}
}
