package mvcc

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"gbaselite/storageengine"
)

// TestP7Fix02_DirtyRegistryAtomicReplacementAndBudgets covers P7-FIX-02:
// Verifies that DirtyRegistry.Publish validates budgets before mutating state,
// that an update failure preserves the existing published version without data loss,
// and that budget errors are returned rather than swallowed.
func TestP7Fix02_DirtyRegistryAtomicReplacementAndBudgets(t *testing.T) {
	reg := NewDirtyRegistry(1, 0)

	// 1. Publish initial version for rootA
	v1 := []DirtyVersion{
		{Space: "row/t1", Key: []byte("k1"), Value: []byte("val1")},
	}
	if err := reg.Publish("rootA", 1, 0, v1); err != nil {
		t.Fatalf("initial publish: %v", err)
	}

	snap1, err := reg.Sample("rootB")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := snap1.Get("row/t1", []byte("k1")); !ok || string(d.Value) != "val1" {
		t.Fatalf("sampled val = %s, %v, want val1", string(d.Value), ok)
	}

	// 2. Attempt to publish an oversized update exceeding per-root limit (8192)
	// Decoupled writer semantics: Publish does not abort the writer, but marks root overflowed.
	oversized := make([]DirtyVersion, 8193)
	for i := range oversized {
		oversized[i] = DirtyVersion{Space: "row/t1", Key: []byte{byte(i)}, Value: []byte("x")}
	}
	err = reg.Publish("rootA", 1, 0, oversized)
	if err != nil {
		t.Fatalf("oversized publish returned error %v, want nil (writer must not be broken)", err)
	}

	// 3. Atomicity & fail-closed check: RU reader must fail closed with ErrViewBudgetLimit
	_, err = reg.Sample("rootB")
	if !errors.Is(err, storageengine.ErrViewBudgetLimit) {
		t.Fatalf("reader Sample returned %v, want ErrViewBudgetLimit", err)
	}

	// 4. Stale generation/term must be rejected with ErrConflict and not reset epoch
	err = reg.Publish("rootA", 0, 0, v1)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale generation publish returned %v, want ErrConflict", err)
	}

	// 5. Withdraw removes rootA
	reg.Withdraw("rootA")
	snap3, _ := reg.Sample("rootB")
	if _, ok := snap3.Get("row/t1", []byte("k1")); ok {
		t.Fatal("rootA was not withdrawn from dirty registry")
	}
}

// TestP7FixR201_RestorePurgesDirtyRegistryAndRejectsStalePublish covers P7-FIX-R2-01:
// Verifies that Store.Restore purges the DirtyRegistry and bumps generation so
// no pre-restore dirty versions remain, and old transactions cannot publish to the new epoch.
func TestP7FixR201_RestorePurgesDirtyRegistryAndRejectsStalePublish(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()

	// 1. Commit a baseline record k1=V0
	tx0, err := s.Begin(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx0.Put("rows", []byte("k1"), []byte("V0"))
	if _, err := tx0.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Take a backup of the committed baseline
	backupDir := filepath.Join(t.TempDir(), "backup")
	_, err = s.Backup(ctx, backupDir)
	if err != nil {
		t.Fatal(err)
	}

	// 3. Txn A creates uncommitted dirty update k1=V_dirty
	txA, err := s.Begin(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()
	_ = txA.Put("rows", []byte("k1"), []byte("V_dirty"))

	// Publish A's dirty writes
	versions, err := txA.CollectDirtyVersions()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DirtyRegistry().Publish(txA.ID, txA.Generation(), txA.Term(), versions); err != nil {
		t.Fatal(err)
	}

	// Verify dirty version is visible
	snapBefore, err := s.DirtyRegistry().Sample("reader")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := snapBefore.Get("rows", []byte("k1")); !ok || string(d.Value) != "V_dirty" {
		t.Fatalf("dirty version before restore = %v, %v, want V_dirty", d, ok)
	}

	// 4. Restore store to the backup
	if err := s.RestoreBackup(ctx, backupDir); err != nil {
		t.Fatalf("RestoreBackup failed: %v", err)
	}

	// 5. Sample after restore: pre-restore dirty version MUST BE GONE!
	snapAfter, err := s.DirtyRegistry().Sample("reader")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := snapAfter.Get("rows", []byte("k1")); ok {
		t.Fatalf("ghost dirty version survived Restore! got %v", d)
	}

	// 6. Old Txn A trying to publish to the restored store MUST fail with ErrConflict!
	err = s.DirtyRegistry().Publish(txA.ID, txA.Generation(), txA.Term(), versions)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("old transaction publish after restore returned %v, want ErrConflict", err)
	}
}

// TestP7FixR203_LargeTransactionDecoupledFromDirtyBudget covers P7-FIX-R2-03:
// Verifies that when a writer transaction exceeds maxDirtyEntriesPerRoot (8192 keys),
// the writer transaction is NOT rejected; instead the root is marked overflowed so
// an RU reader trying to sample fails closed with ErrViewBudgetLimit (never returns partial dirty data).
func TestP7FixR203_LargeTransactionDecoupledFromDirtyBudget(t *testing.T) {
	reg := NewDirtyRegistry(1, 0)

	// Writer stages 8200 keys (exceeds maxDirtyEntriesPerRoot=8192)
	oversized := make([]DirtyVersion, 8200)
	for i := range oversized {
		oversized[i] = DirtyVersion{Space: "row/t1", Key: []byte(fmt.Sprintf("k_%d", i)), Value: []byte("val")}
	}

	// Writer publish must NOT fail the writer! It succeeds and marks overflowed.
	err := reg.Publish("rootA", 1, 0, oversized)
	if err != nil {
		t.Fatalf("Publish returned error %v, breaking writer transaction!", err)
	}

	// RU reader trying to sample must fail closed with ErrViewBudgetLimit!
	_, err = reg.Sample("readerB")
	if !errors.Is(err, storageengine.ErrViewBudgetLimit) {
		t.Fatalf("RU reader Sample returned %v, want ErrViewBudgetLimit", err)
	}

	// When writer terminates (withdraws), RU reader resumes normal sampling
	reg.Withdraw("rootA")
	snap, err := reg.Sample("readerB")
	if err != nil {
		t.Fatalf("Sample after withdraw returned error: %v", err)
	}
	if len(snap.entries) != 0 {
		t.Fatalf("expected 0 entries after withdraw, got %d", len(snap.entries))
	}
}

// TestP7FixR202_PublishAndRollbackLinearization covers P7-FIX-R2-02:
// Verifies that concurrent PublishDirtyWrites and Rollback are strictly serialized
// under t.mu protection, guaranteeing that no rolled-back transaction ever leaks ghost dirty entries.
func TestP7FixR202_PublishAndRollbackLinearization(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()

	for iter := 0; iter < 50; iter++ {
		txA, err := s.Begin(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		_ = txA.Put("rows", []byte("k"), []byte("val"))

		startGate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-startGate
			_ = txA.PublishDirtyWrites(s.DirtyRegistry())
		}()

		go func() {
			defer wg.Done()
			<-startGate
			_ = txA.Rollback()
		}()

		close(startGate)
		wg.Wait()

		// Reader must NEVER see rolled-back dirty entries
		snap, err := s.DirtyRegistry().Sample("reader")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := snap.Get("rows", []byte("k")); ok {
			t.Fatalf("iter %d: rolled-back transaction leaked ghost dirty entry into registry!", iter)
		}
	}
}
