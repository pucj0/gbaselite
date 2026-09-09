package mvcc

import (
	"bytes"
	"context"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exit without Close at durable boundaries. This checks process-crash recovery,
// not device-cache or torn-sector behavior during a physical power failure.
func TestLocalWALProcessCrash(t *testing.T) {
	if dir := os.Getenv("GBASE_WAL_CRASH_DIR"); dir != "" {
		s, err := OpenWithOptions(dir, Options{LocalWAL: true})
		if err != nil {
			t.Fatal(err)
		}
		head, err := s.Head()
		if err != nil {
			t.Fatal(err)
		}
		if err = s.writeLocalWAL(context.Background(), func(w *localWALWriter) error {
			for group := 0; group < 2; group++ {
				if err := w.begin(head+uint64(group)+1, fmt.Sprintf("crash-%d", group)); err != nil {
					return err
				}
				for i := 0; i < 180; i++ {
					k, _ := key("r", []byte(fmt.Sprintf("%d-%03d", group, i)))
					op := Op{Value: bytes.Repeat([]byte{byte(group + 1)}, 2048)}
					if i == 179 {
						op = Op{Delete: true}
					}
					if err := w.op(k, encodeStagedOp(op)); err != nil {
						return err
					}
				}
				if err := w.frame([]byte{walEnd}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		phase := os.Getenv("GBASE_WAL_CRASH_PHASE")
		if phase != "sealed" {
			// Simulate flushed install pages before/after the first commit marker.
			err = s.db.Update(func(tx *bolt.Tx) error {
				n := 40
				if phase == "first-committed" {
					n = 180
				}
				for i := 0; i < n; i++ {
					k, _ := key("r", []byte(fmt.Sprintf("0-%03d", i)))
					v := append([]byte{1}, bytes.Repeat([]byte{1}, 2048)...)
					if i == 179 {
						v = []byte{0}
					}
					if err := putVersion(tx, k, sequence(head+1), v); err != nil {
						return err
					}
				}
				if err := tx.Bucket(metaBucket).Put([]byte("allocated"), sequence(head+1)); err != nil {
					return err
				}
				if phase == "first-committed" {
					return publishCommit(tx, "crash-0", head+1, false)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		os.Exit(23)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, layout := range []string{"nested", "flat"} {
		for _, phase := range []string{"sealed", "partial", "first-committed"} {
			t.Run(layout+"/"+phase, func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, "source")
				s, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := s.Begin(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				for group := 0; group < 2; group++ {
					for i := 0; i < 180; i++ {
						if err = tx.Put("r", []byte(fmt.Sprintf("%d-%03d", group, i)), []byte("old")); err != nil {
							t.Fatal(err)
						}
					}
				}
				snapshot, err := tx.Commit(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if layout == "flat" {
					target := filepath.Join(root, "flat")
					if err = s.ExportLayout(context.Background(), target, layout); err != nil {
						t.Fatal(err)
					}
					dir = target
				}
				s.Close()
				cmd := exec.Command(exe, "-test.run=^TestLocalWALProcessCrash$")
				cmd.Env = append(os.Environ(), "GBASE_WAL_CRASH_DIR="+dir, "GBASE_WAL_CRASH_PHASE="+phase)
				output, err := cmd.CombinedOutput()
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 23 {
					t.Fatalf("crash helper: %v %s", err, output)
				}
				recovered, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer recovered.Close()
				for group := 0; group < 2; group++ {
					for i := 0; i < 180; i++ {
						k := []byte(fmt.Sprintf("%d-%03d", group, i))
						old, _, ok, err := recovered.Get(snapshot, "r", k)
						if err != nil || !ok || string(old) != "old" {
							t.Fatal("old snapshot", group, i, err)
						}
						v, _, ok, err := recovered.Get(^uint64(0), "r", k)
						if err != nil || ok != (i != 179) || i != 179 && !bytes.Equal(v, bytes.Repeat([]byte{byte(group + 1)}, 2048)) {
							t.Fatal("recovery", group, i, err)
						}
					}
				}
				info, err := os.Stat(recovered.walPath())
				if err != nil || info.Size() != 0 {
					t.Fatal("WAL not cleared", err)
				}
			})
		}
	}
}
