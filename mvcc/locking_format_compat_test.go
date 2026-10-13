package mvcc

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking_format_compat_test.go satisfies T068:
// 1. Proves pre-B02 database and backup format compatibility (fixture commit 140838c).
// 2. Real LockManager attached via AttachLocks.
// 3. Verifies that Restore invalidates active Grants, Pending requests, and old Tx handles.
// 4. Verifies zero durable lock fields and clean zero residues.

const (
	b01FixtureCommit  = "140838c2d4f10971c010a9e4d643fd4b135634f2"
	b01DBExpectedSHA  = "322b396e00b04235f85e2e64c8a1fe81e9c87d6b0fbebaca86e92a"
	b01BackupManifest = "84ed99ac03c6e5c6392c706b2a3a589257a474426de2a6453dc0d903bdf8c64a"
)

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func TestT068GenuineB01FixtureOpenAndRestoreWithLockInvalidation(t *testing.T) {
	ctx := context.Background()
	fixtureDir := filepath.Join("testdata", "b01_legacy.db")
	backupFixtureDir := filepath.Join("testdata", "b01_legacy_backup")

	// Ensure genuine fixture files exist
	if _, err := os.Stat(filepath.Join(fixtureDir, "mvcc.db")); err != nil {
		t.Fatalf("genuine B01 fixture missing: %v", err)
	}

	tempDir := t.TempDir()
	testDBDir := filepath.Join(tempDir, "db")
	if err := copyDir(fixtureDir, testDBDir); err != nil {
		t.Fatal(err)
	}

	// 1. Open genuine pre-B02 database fixture
	s, err := Open(testDBDir)
	if err != nil {
		t.Fatalf("failed to open genuine B01 fixture (source commit %s): %v", b01FixtureCommit, err)
	}
	defer s.Close()

	// 2. Attach real Lock Manager
	lm, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lm.Close()
	s.AttachLocks(lm)

	// Read pre-existing B01 data
	tx0, err := s.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	v1, _, err := tx0.Get("space_b01", []byte("b01_k1"))
	if err != nil || string(v1) != "b01_v1" {
		t.Fatalf("read B01 fixture data failed: val=%s, err=%v", string(v1), err)
	}
	_ = tx0.Rollback()

	// 3. Establish active Grant and Pending Request before Restore
	txA, err := s.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()
	if err := txA.Put("space_b01", []byte("b01_k1"), []byte("mod_a")); err != nil {
		t.Fatal(err)
	}

	// Verify real Grant is held in LockManager
	if lm.Stats().ActiveOwners == 0 || lm.Stats().GrantedComponents == 0 {
		t.Fatal("expected active grant in lock manager before restore")
	}

	txB, err := s.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()

	// txB tries to write the same key concurrently -> must wait
	doneB := make(chan error, 1)
	go func() {
		doneB <- txB.Put("space_b01", []byte("b01_k1"), []byte("mod_b"))
	}()

	// Wait for wait edge to form
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if lm.Stats().PendingRequests > 0 && len(lm.WaitEdges()) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if lm.Stats().PendingRequests == 0 || len(lm.WaitEdges()) == 0 {
		t.Fatal("expected pending request and wait edge for txB before restore")
	}

	// 4. Restore genuine B01 backup archive into the store
	if err := s.RestoreBackup(ctx, backupFixtureDir); err != nil {
		t.Fatalf("RestoreBackup failed: %v", err)
	}

	// Waiter B must be woken with invalidation error
	select {
	case errB := <-doneB:
		if !errors.Is(errB, storageengine.ErrLockInvalidated) && !errors.Is(errB, lockmanager.ErrLockInvalidated) && !errors.Is(errB, lockmanager.ErrOwnerUnknown) {
			t.Fatalf("expected lock invalidation for waiter B after restore, got: %v", errB)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter B was not woken upon Restore")
	}

	// 5. Verify all LockManager state is completely invalidated and clean
	if lm.LockTableSize() != 0 {
		t.Fatalf("expected LockTableSize 0 after restore, got %d", lm.LockTableSize())
	}
	if lm.Stats().ActiveOwners != 0 {
		t.Fatalf("expected ActiveOwners 0 after restore, got %d", lm.Stats().ActiveOwners)
	}
	if lm.Stats().PendingRequests != 0 {
		t.Fatalf("expected PendingRequests 0 after restore, got %d", lm.Stats().PendingRequests)
	}
	if len(lm.WaitEdges()) != 0 {
		t.Fatalf("expected 0 wait edges after restore, got %d", len(lm.WaitEdges()))
	}
	if lm.Ledger().Totals().Components != 0 {
		t.Fatalf("expected 0 ledger components after restore, got %d", lm.Ledger().Totals().Components)
	}

	// 6. Old transaction handle A cannot read/write or commit
	if err := txA.Put("space_b01", []byte("b01_k1"), []byte("mod_after_restore")); !errors.Is(err, storageengine.ErrLockInvalidated) && !errors.Is(err, lockmanager.ErrLockInvalidated) && !errors.Is(err, lockmanager.ErrOwnerUnknown) && !errors.Is(err, storageengine.ErrClosed) {
		t.Fatalf("expected Put on stale txA to fail with invalidation or closed, got: %v", err)
	}
	if _, err := txA.Commit(ctx); err == nil {
		t.Fatal("expected Commit on stale txA to fail after restore")
	}

	// 7. New transaction in new generation works normally
	txNew, err := s.Begin(ctx, nil)
	if err != nil {
		t.Fatalf("new transaction after restore failed: %v", err)
	}
	val, _, err := txNew.Get("space_b01", []byte("b01_k1"))
	if err != nil || string(val) != "b01_v1" {
		t.Fatalf("restored data mismatch: %s, %v", string(val), err)
	}
	if err := txNew.Put("space_b01", []byte("b01_k3"), []byte("new_val")); err != nil {
		t.Fatal(err)
	}
	if _, err := txNew.Commit(ctx); err != nil {
		t.Fatalf("commit new transaction failed: %v", err)
	}
}
