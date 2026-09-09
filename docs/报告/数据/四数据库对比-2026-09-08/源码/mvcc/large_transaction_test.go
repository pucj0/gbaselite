package mvcc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	bolt "go.etcd.io/bbolt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStagedBinaryEncodingAndOwnership(t *testing.T) {
	for _, op := range []Op{{Space: "row/表", Key: []byte{0, 255, 1}, Value: []byte("文\x00本")}, {Space: "rows", Key: []byte("k"), Delete: true}, {Space: "catalog", Key: []byte("k"), Check: true}} {
		k, _ := key(op.Space, op.Key)
		value := encodeStagedOp(op)
		got, err := decodeStagedOp(k, value)
		if err != nil || got.Space != op.Space || !bytes.Equal(got.Key, op.Key) || !bytes.Equal(got.Value, op.Value) || got.Delete != op.Delete || got.Check != op.Check {
			t.Fatal(got, err)
		}
		k[0] = 'x'
		value[0] = 0
		if got.Space != op.Space || !bytes.Equal(got.Value, op.Value) {
			t.Fatal("borrowed data")
		}
	}
	for _, value := range [][]byte{nil, {1}, {0, 0}, {1, 4}, append([]byte{1, 0}, make([]byte, MaxValueBytes+1)...)} {
		if _, err := decodeStagedOp([]byte("r\x00k"), value); err == nil {
			t.Fatal("invalid encoding accepted")
		}
	}
	for _, k := range [][]byte{nil, []byte("missing separator"), {0, 1}, append([]byte("r\x00"), make([]byte, 8193)...)} {
		if _, err := decodeStagedOp(k, []byte{1, 0}); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
func FuzzStagedBinaryDecode(f *testing.F) {
	f.Add([]byte("rows\x00key"), []byte{1, 0, 123})
	f.Add([]byte("rows\x00key"), []byte{1, 1})
	f.Fuzz(func(t *testing.T, k, v []byte) {
		op, err := decodeStagedOp(k, v)
		if err != nil {
			return
		}
		if err = validateCommand(Command{Ops: []Op{op}}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encodeStagedOp(op), v) {
			t.Fatal("roundtrip")
		}
	})
}
func TestWriteSetBudgetAndUnlimitedAccounting(t *testing.T) {
	s, err := OpenWithOptions(t.TempDir(), Options{WriteSetLimitBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin(context.Background(), nil)
	defer tx.Rollback()
	// key r\0k has 3 bytes and encoded value has 2 + 59 bytes: exact boundary.
	if err = tx.Put("r", []byte("k"), bytes.Repeat([]byte{1}, 59)); err != nil {
		t.Fatal(err)
	}
	if tx.stagedBytes != 64 {
		t.Fatal(tx.stagedBytes)
	}
	if err = tx.Put("r", []byte("k"), []byte("short")); err != nil {
		t.Fatal(err)
	}
	before := tx.stagedBytes
	if err = tx.Guard("r", []byte("k")); err != nil || tx.stagedBytes != before {
		t.Fatal(err)
	}
	if err = tx.Put("r", []byte("other"), make([]byte, 60)); !errors.Is(err, ErrWriteSetLimit) {
		t.Fatal(err)
	}
	if _, ok, _ := tx.Get("r", []byte("other")); ok {
		t.Fatal("rejected write leaked")
	}
	unlimited, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer unlimited.Close()
	u, _ := unlimited.Begin(context.Background(), nil)
	defer u.Rollback()
	// Accounting boundary unit test, separate from the real >256 MiB benchmark.
	u.stagedBytes = 256 << 20
	if err = u.Put("rows", []byte("beyond-old-limit"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if u.stagedBytes <= 256<<20 {
		t.Fatal(u.stagedBytes)
	}
	if _, err = OpenWithOptions(t.TempDir(), Options{WriteSetLimitBytes: -1}); err == nil {
		t.Fatal("negative budget")
	}
}
func TestChildTransfersLargeStageAndFailedMergeClosesParent(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir())
	defer s.Close()
	p, _ := s.Begin(ctx, nil)
	defer p.Rollback()
	c, _ := p.Child()
	for i := 0; i < 200; i++ {
		if err := c.Put("rows", []byte(fmt.Sprint(i)), make([]byte, 2048)); err != nil {
			t.Fatal(err)
		}
	}
	stage, path := c.stage, c.path
	if stage == nil {
		t.Fatal("not spilled")
	}
	if _, err := c.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if p.stage != stage || p.path != path || c.stage != nil {
		t.Fatal("ownership transfer failed")
	}
	if _, ok, err := p.Get("rows", []byte("199")); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := p.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stage not cleaned", err)
	}
	p, _ = s.Begin(ctx, nil)
	p.Put("rows", []byte("original"), []byte("x"))
	c, _ = p.Child()
	c.Put("rows", []byte("a"), []byte("x"))
	c.Put("rows", []byte("z"), make([]byte, 100))
	s.writeSetLimit = p.stagedBytes + 30
	if _, err := c.Commit(ctx); !errors.Is(err, ErrWriteSetLimit) {
		t.Fatal(err)
	}
	if !p.closed {
		t.Fatal("partly merged parent survived")
	}
	if _, err := p.Commit(ctx); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

// This context observes actual durable batch boundaries without adding fault
// injection hooks to production storage code.
type afterInstalledContext struct {
	context.Context
	store  *Store
	action func()
	fired  bool
}

func (c *afterInstalledContext) Err() error {
	if c.fired {
		return context.Canceled
	}
	installed := false
	_ = c.store.db.View(func(tx *bolt.Tx) error {
		m := tx.Bucket(metaBucket)
		a := number(m.Get([]byte("allocated")))
		installed = a > number(m.Get([]byte("applied")))
		return nil
	})
	if installed {
		c.fired = true
		if c.action != nil {
			c.action()
		}
		return context.Canceled
	}
	return nil
}
func seedLargeTest(t *testing.T, s *Store) {
	t.Helper()
	tx, _ := s.Begin(context.Background(), nil)
	for i := 0; i < 100; i++ {
		if err := tx.Put("rows", []byte(fmt.Sprintf("%03d", i)), []byte("old")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func largeUpdateTest(t *testing.T, s *Store) *Tx {
	t.Helper()
	tx, _ := s.Begin(context.Background(), nil)
	for i := 0; i < 100; i++ {
		if err := tx.Put("rows", []byte(fmt.Sprintf("%03d", i)), bytes.Repeat([]byte{'N'}, 4096)); err != nil {
			t.Fatal(err)
		}
	}
	return tx
}
func verifyLargeTest(t *testing.T, s *Store, updated bool) {
	t.Helper()
	tx, _ := s.Begin(context.Background(), nil)
	defer tx.Rollback()
	for i := 0; i < 100; i++ {
		v, ok, err := tx.Get("rows", []byte(fmt.Sprintf("%03d", i)))
		want := []byte("old")
		if updated {
			want = bytes.Repeat([]byte{'N'}, 4096)
		}
		if err != nil || !ok || !bytes.Equal(v, want) {
			t.Fatalf("partial state at %d: len=%d %v", i, len(v), err)
		}
	}
}
func TestLargeCommitCancellationPreservesAtomicity(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	seedLargeTest(t, s)
	old, _ := s.Begin(context.Background(), nil)
	defer old.Rollback()
	update := largeUpdateTest(t, s)
	ctx := &afterInstalledContext{Context: context.Background(), store: s}
	if _, err := update.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !ctx.fired || s.AvailabilityError() != nil {
		t.Fatal("not canceled after installation or store poisoned")
	}
	verifyLargeTest(t, s, false)
	if n, err := s.PendingBytes(); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	update = largeUpdateTest(t, s)
	if _, err := update.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	verifyLargeTest(t, s, true)
	for i := 0; i < 100; i++ {
		v, ok, _ := old.Get("rows", []byte(fmt.Sprintf("%03d", i)))
		if !ok || string(v) != "old" {
			t.Fatal("old snapshot changed")
		}
	}
}
func TestLargeCommitTailConflict(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	seedLargeTest(t, s)
	update := largeUpdateTest(t, s)
	other, _ := s.Begin(context.Background(), nil)
	other.Put("rows", []byte("099"), []byte("winner"))
	other.Commit(context.Background())
	if _, err := update.Commit(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	tx, _ := s.Begin(context.Background(), nil)
	defer tx.Rollback()
	for i := 0; i < 100; i++ {
		v, _, _ := tx.Get("rows", []byte(fmt.Sprintf("%03d", i)))
		want := "old"
		if i == 99 {
			want = "winner"
		}
		if string(v) != want {
			t.Fatal(i, string(v))
		}
	}
}
func TestLargeCommitCrashRecoveryAndSequenceReservation(t *testing.T) {
	for _, mode := range []string{"middle", "acknowledged"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			exe, _ := os.Executable()
			cmd := exec.Command(exe, "-test.run=^TestLargeCrashHelper$")
			cmd.Env = append(os.Environ(), "GBASELITE_LARGE_HELPER="+dir, "GBASELITE_LARGE_CRASH_MODE="+mode)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "crash-boundary") {
				t.Fatalf("helper %s %v", out, err)
			}
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			verifyLargeTest(t, s, mode == "acknowledged")
			// Publishing another transaction must not make the crashed versions visible.
			tx, _ := s.Begin(context.Background(), nil)
			tx.Put("other", []byte("key"), []byte("fresh"))
			if _, err = tx.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			verifyLargeTest(t, s, mode == "acknowledged")
			files, _ := filepath.Glob(filepath.Join(dir, "transactions", "*.tmp"))
			if len(files) != 0 {
				t.Fatal("stale temp files", files)
			}
		})
	}
}
func TestLargeCrashHelper(t *testing.T) {
	dir := os.Getenv("GBASELITE_LARGE_HELPER")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedLargeTest(t, s)
	update := largeUpdateTest(t, s)
	var ctx context.Context = context.Background()
	if os.Getenv("GBASELITE_LARGE_CRASH_MODE") == "middle" {
		ctx = &afterInstalledContext{Context: ctx, store: s, action: func() { fmt.Println("crash-boundary: installed, unpublished"); os.Exit(0) }}
	}
	if _, err = update.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fmt.Println("crash-boundary: acknowledged")
	os.Exit(0)
}
func TestReplicatedCommitCanReplayReservedSequence(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	ctx := context.Background()
	update, _ := s.Begin(ctx, nil)
	for i := 0; i < 100; i++ {
		update.Put("rows", []byte(fmt.Sprintf("%03d", i)), bytes.Repeat([]byte{'N'}, 4096))
	}
	// Stage using the normal proposer, then interrupt installation before its marker.
	err := update.WalkWrites(ctx, func(op Op) error {
		_, e := s.Propose(ctx, Command{Kind: "stage", ID: update.ID, Ops: []Op{op}})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	index := s.localSeq + 1
	command := Command{Kind: "commit", ID: update.ID, Snapshot: update.Snapshot}
	interrupted := &afterInstalledContext{Context: ctx, store: s}
	if _, err = s.commitContext(interrupted, index, command); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if r, err := s.Apply(index, command); err != nil || r.Err() != nil {
		t.Fatal(r, err)
	}
	update.Rollback()
	verifyLargeTest(t, s, true)
}
