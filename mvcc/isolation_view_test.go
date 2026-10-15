package mvcc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// TestViewToken_LifecycleAndPin tests basic view pin and release mechanics (T022).
func TestViewToken_LifecycleAndPin(t *testing.T) {
	m := NewTransactionManager()

	// Register a standalone view at revision 10
	reg := ViewRegistration{
		ID:         "v1",
		RootID:     "",
		Revision:   10,
		Generation: 1,
	}
	if err := m.RegisterView(reg); err != nil {
		t.Fatalf("RegisterView: %v", err)
	}

	if count := managerRefCount(t, m, 10); count != 1 {
		t.Fatalf("snapshotRefs[10] = %d, want 1", count)
	}

	oldest, ok := m.OldestReadTS()
	if !ok || oldest != 10 {
		t.Fatalf("OldestReadTS = (%d, %v), want (10, true)", oldest, ok)
	}

	// Release view
	m.ReleaseView("v1")
	if count := managerRefCount(t, m, 10); count != 0 {
		t.Fatalf("snapshotRefs[10] after release = %d, want 0", count)
	}
	if _, ok := m.OldestReadTS(); ok {
		t.Fatalf("OldestReadTS after release should be unset")
	}

	// Idempotent double release
	m.ReleaseView("v1")
	if count := managerRefCount(t, m, 10); count != 0 {
		t.Fatalf("snapshotRefs[10] after double release = %d, want 0", count)
	}
}

// TestViewToken_RootOwnershipAndAutoCleanup verifies that root terminal state cleans up orphan views (T023).
func TestViewToken_RootOwnershipAndAutoCleanup(t *testing.T) {
	m := NewTransactionManager()

	registerTestRoot(t, m, "root-1", 30)
	if err := m.RegisterView(ViewRegistration{ID: "v1", RootID: "root-1", Revision: 30, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterView(ViewRegistration{ID: "v2", RootID: "root-1", Revision: 40, Generation: 1}); err != nil {
		t.Fatal(err)
	}

	// OldestReadTS should be 30
	if oldest, ok := m.OldestReadTS(); !ok || oldest != 30 {
		t.Fatalf("OldestReadTS = (%d, %v), want (30, true)", oldest, ok)
	}

	// When root terminates, all views owned by root-1 must be released automatically
	if err := m.Transition("root-1", TransactionCommitted, TransitionOptions{}); err != nil {
		t.Fatalf("Transition root-1: %v", err)
	}

	if count := managerRefCount(t, m, 30); count != 0 {
		t.Fatalf("snapshotRefs[30] = %d, want 0 after root commit", count)
	}
	if count := managerRefCount(t, m, 40); count != 0 {
		t.Fatalf("snapshotRefs[40] = %d, want 0 after root commit", count)
	}
	if _, ok := m.OldestReadTS(); ok {
		t.Fatalf("OldestReadTS should be unset after all views and root are released")
	}
}

// TestViewToken_StoreBeginViewToken tests view creation against real Store (T024).
func TestViewToken_StoreBeginViewToken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()

	// Write key at revision 1
	tx1, _ := s.Begin(ctx, s)
	_ = tx1.Put("space", []byte("k1"), []byte("v1"))
	_, _ = tx1.Commit(ctx)

	// Create view token at revision 1
	v, err := s.BeginViewToken(ctx, nil)
	if err != nil {
		t.Fatalf("BeginViewToken: %v", err)
	}
	defer v.Close()

	if v.Revision() == 0 {
		t.Fatalf("view revision is 0")
	}

	// Write key at revision 2
	tx2, _ := s.Begin(ctx, s)
	_ = tx2.Put("space", []byte("k1"), []byte("v2"))
	_, _ = tx2.Commit(ctx)

	// View must read v1 (retained revision)
	val, ok, err := v.Get("space", []byte("k1"))
	if err != nil || !ok || string(val) != "v1" {
		t.Fatalf("view.Get = (%q, %v, %v), want v1", string(val), ok, err)
	}

	// Close view
	if err := v.Close(); err != nil {
		t.Fatalf("v.Close: %v", err)
	}
	if !v.Closed() {
		t.Fatalf("view should be closed")
	}

	// Reading after close must return ErrViewClosed
	_, _, err = v.Get("space", []byte("k1"))
	if !errors.Is(err, storageengine.ErrViewClosed) {
		t.Fatalf("Get on closed view returned %v, want ErrViewClosed", err)
	}
}

// TestViewToken_GCAndCompactHistory (M37) verifies views prevent premature GC reclamation.
func TestViewToken_GCAndCompactHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()

	// Rev 1
	t1, _ := s.Begin(ctx, s)
	_ = t1.Put("data", []byte("row1"), []byte("version1"))
	_, _ = t1.Commit(ctx)

	// Open view at Rev 1
	view, err := s.BeginViewToken(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()

	// Rev 2, Rev 3 updates
	t2, _ := s.Begin(ctx, s)
	_ = t2.Put("data", []byte("row1"), []byte("version2"))
	_, _ = t2.Commit(ctx)

	t3, _ := s.Begin(ctx, s)
	_ = t3.Put("data", []byte("row1"), []byte("version3"))
	_, _ = t3.Commit(ctx)

	// Run CompactHistory while view is active
	if err := s.CompactHistory(ctx); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}

	// View must still successfully read version1!
	val, ok, err := view.Get("data", []byte("row1"))
	if err != nil || !ok || string(val) != "version1" {
		t.Fatalf("view.Get after CompactHistory = (%q, %v, %v), want version1", string(val), ok, err)
	}

	// Now close the view
	_ = view.Close()

	// Run CompactHistory again; now horizon can advance
	if err := s.CompactHistory(ctx); err != nil {
		t.Fatalf("CompactHistory 2: %v", err)
	}
}

// TestViewToken_EpochResetOnRestore (M38) verifies Restore invalidates stale view tokens.
func TestViewToken_EpochResetOnRestore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	tx, _ := s.Begin(ctx, s)
	_ = tx.Put("data", []byte("k"), []byte("val"))
	_, _ = tx.Commit(ctx)

	// Backup
	backupPath := filepath.Join(t.TempDir(), "backup.json")
	if _, err := s.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}

	// Open view before restore
	view, err := s.BeginViewToken(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()

	// Restore backup (bumps generation and resets registry)
	if err := s.RestoreBackup(ctx, backupPath); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}

	// View should now be invalidated
	_, _, err = view.Get("data", []byte("k"))
	if !errors.Is(err, storageengine.ErrViewInvalidated) {
		t.Fatalf("Get after restore returned %v, want ErrViewInvalidated", err)
	}
}

// TestP3Fix04_ViewStateInvalidatedAndRootOwnership covers P3-FIX-04.
func TestP3Fix04_ViewStateInvalidatedAndRootOwnership(t *testing.T) {
	m := NewTransactionManager()

	// 1. Root ownership validation: cannot register view for non-existent root
	err := m.RegisterView(ViewRegistration{
		ID:         "v-orphan",
		RootID:     "non-existent-root",
		Revision:   10,
		Generation: 1,
	})
	if err == nil {
		t.Fatalf("RegisterView with non-existent root unexpectedly succeeded")
	}

	// Cannot register view for ended root
	registerTestRoot(t, m, "root-ended", 10)
	_ = m.Transition("root-ended", TransactionCommitted, TransitionOptions{})
	err = m.RegisterView(ViewRegistration{
		ID:         "v-ended",
		RootID:     "root-ended",
		Revision:   10,
		Generation: 1,
	})
	if err == nil {
		t.Fatalf("RegisterView with ended root unexpectedly succeeded")
	}

	// 2. ViewToken.State() reflects ViewStateInvalidated after Restore
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	tx, _ := s.Begin(ctx, s)
	_ = tx.Put("data", []byte("k"), []byte("val"))
	_, _ = tx.Commit(ctx)

	backupPath := filepath.Join(t.TempDir(), "backup.json")
	if _, err := s.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}

	view, err := s.BeginViewToken(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()

	if view.State() != storageengine.ViewStateActive {
		t.Fatalf("view state before restore = %v, want ACTIVE", view.State())
	}

	if err := s.RestoreBackup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}

	if view.State() != storageengine.ViewStateInvalidated {
		t.Fatalf("view state after restore = %v, want INVALIDATED", view.State())
	}
}

// TestP3Fix05_HistoryHorizonAndBudgetLimit covers P3-FIX-05:
// cannot register unpinned view below GC horizon, and view budget limits are enforced.
func TestP3Fix05_HistoryHorizonAndBudgetLimit(t *testing.T) {
	m := NewTransactionManager()

	// 1. Cannot register view below GC horizon if not already pinned
	registerTestRoot(t, m, "root-1", 50) // horizon is 50
	err := m.RegisterView(ViewRegistration{
		ID:         "v-stale",
		Revision:   20, // 20 is below horizon 50, and 20 has 0 refs!
		Generation: 1,
	})
	if err == nil {
		t.Fatalf("RegisterView below GC horizon unexpectedly succeeded")
	}

	// 2. Budget limit enforcement
	for i := 0; i < DefaultMaxActiveViews; i++ {
		err := m.RegisterView(ViewRegistration{
			ID:         "v-budget-" + strconv.Itoa(i),
			Revision:   50, // 50 is pinned by root-1
			Generation: 1,
		})
		if err != nil {
			t.Fatalf("RegisterView %d: %v", i, err)
		}
	}

	// Registering one more must fail with ErrViewBudgetLimit
	err = m.RegisterView(ViewRegistration{
		ID:         "v-budget-overflow",
		Revision:   50,
		Generation: 1,
	})
	if !errors.Is(err, storageengine.ErrViewBudgetLimit) {
		t.Fatalf("RegisterView overflow returned %v, want ErrViewBudgetLimit", err)
	}
}

// TestP4Prereq_CompactHistoryPurgesUnpinnedHistory demonstrates the historical snapshot availability gap:
// after CompactHistory purges unpinned revisions 1 and 2, attempting to register a view at revision 1
// must be rejected, rather than handing out a truncated snapshot where row1 is missing.
func TestP4Prereq_CompactHistoryPurgesUnpinnedHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()

	// 1. Commit V1
	tx1, _ := s.Begin(ctx, s)
	_ = tx1.Put("data", []byte("k1"), []byte("v1"))
	_, _ = tx1.Commit(ctx)

	// 2. Commit V2
	tx2, _ := s.Begin(ctx, s)
	_ = tx2.Put("data", []byte("k1"), []byte("v2"))
	_, _ = tx2.Commit(ctx)

	// 3. Commit V3
	tx3, _ := s.Begin(ctx, s)
	_ = tx3.Put("data", []byte("k1"), []byte("v3"))
	_, _ = tx3.Commit(ctx)

	// Ensure no active transactions
	if stats := s.txns.Stats(); stats.ActiveRoot != 0 || stats.HasOldestReadTS {
		t.Fatalf("expected no active transactions, got %+v", stats)
	}

	// 4. Run CompactHistory. Because there are no active pins, horizon advances to Head (3)
	// and version 1 is physically deleted from the bbolt database!
	if err := s.CompactHistory(ctx); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}

	// 5. Attempt to request view at revision 1: must be rejected because revision 1 was purged!
	_, err = s.BeginViewTokenAt(ctx, nil, 1)
	if err == nil {
		t.Fatalf("BeginViewTokenAt for purged revision 1 unexpectedly succeeded! Must be rejected.")
	}
}

// TestP4Fix03_StartTSImmutable covers P4-FIX-03:
// When an active root transaction establishes a lazy consistent read at a newer revision,
// UpdateRootReadTS must update ReadTS to reflect the view revision, but must NEVER overwrite startTS!
func TestP4Fix03_StartTSImmutable(t *testing.T) {
	m := NewTransactionManager()

	// Register lazy root at startTS = 10
	_, err := m.RegisterRoot(RootRegistration{
		ID:         "root-lazy",
		ReadTS:     10,
		Generation: 1,
		StartedAt:  testStartedAt,
		Lazy:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	infoBefore, ok := m.Info("root-lazy")
	if !ok || infoBefore.StartTS != 10 || infoBefore.ReadTS != 10 {
		t.Fatalf("before: StartTS=%d, ReadTS=%d", infoBefore.StartTS, infoBefore.ReadTS)
	}

	// Update root readTS to 25 (first consistent read)
	m.UpdateRootReadTS("root-lazy", 25)

	infoAfter, ok := m.Info("root-lazy")
	if !ok {
		t.Fatal("root-lazy not found")
	}

	if infoAfter.StartTS != 10 {
		t.Fatalf("StartTS was modified from 10 to %d! startTS must be immutable.", infoAfter.StartTS)
	}
	if infoAfter.ReadTS != 25 {
		t.Fatalf("ReadTS = %d, want 25", infoAfter.ReadTS)
	}
}

// TestP6R204_GuardCapacityOverflowAndMerge covers P6-R2-04:
// Tests >16384 distinct guarded keys, duplicate guards, overflow error ErrWriteSetLimit,
// and Child -> Parent merge capacity checking.
func TestP6R204_GuardCapacityOverflowAndMerge(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	tx, err := s.Begin(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// 1. Guard exactly 16384 distinct keys
	for i := 0; i < maxGuardedKeys; i++ {
		key := []byte(fmt.Sprintf("k_%05d", i))
		if err := tx.Guard("data", key); err != nil {
			t.Fatalf("Guard %d: %v", i, err)
		}
	}

	// 2. Duplicate guard on already guarded key must succeed
	if err := tx.Guard("data", []byte("k_00010")); err != nil {
		t.Fatalf("duplicate guard failed: %v", err)
	}

	// 3. Guarding 16385th distinct key must fail with ErrWriteSetLimit
	err = tx.Guard("data", []byte("k_overflow"))
	if !errors.Is(err, ErrWriteSetLimit) {
		t.Fatalf("overflow guard returned %v, want ErrWriteSetLimit", err)
	}

	_ = tx.Rollback()

	// 4. Test Child -> Parent merge overflow
	parent, _ := s.Begin(ctx, s)
	defer parent.Rollback()

	for i := 0; i < 10000; i++ {
		_ = parent.Guard("data", []byte(fmt.Sprintf("parent_k_%05d", i)))
	}

	child, _ := parent.Child()
	for i := 0; i < 7000; i++ {
		_ = child.Guard("data", []byte(fmt.Sprintf("child_k_%05d", i)))
	}

	// 10000 + 7000 = 17000 > 16384 -> merge must fail with ErrWriteSetLimit
	_, err = child.Commit(ctx)
	if !errors.Is(err, ErrWriteSetLimit) {
		t.Fatalf("child commit overflow returned %v, want ErrWriteSetLimit", err)
	}

	// 5. Savepoint rollback correctly discards child guards
	parent2, _ := s.Begin(ctx, s)
	defer parent2.Rollback()

	child2, _ := parent2.Child()
	_ = child2.Guard("data", []byte("sp_key"))
	_ = child2.Rollback()

	parent2.mu.Lock()
	guardedCount := len(parent2.guardedKeys)
	parent2.mu.Unlock()
	if guardedCount != 0 {
		t.Fatalf("parent inherited discarded child guards: %d", guardedCount)
	}
}

// TestP6R205_StageLeaseSynchronizesWithConcurrentCleanup covers P6-R2-05:
// Tests that stage handle leases guarantee that in-flight stage readers safely complete
// before cleanup() closes the underlying bolt.DB, and subsequent page fetches return ErrClosed.
func TestP6R205_StageLeaseSynchronizesWithConcurrentCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	tx, err := s.Begin(ctx, s)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Stage 300 rows so writes flush to t.stage bolt DB
	const totalRows = 300
	for i := 0; i < totalRows; i++ {
		key := []byte(fmt.Sprintf("k_%04d", i))
		_ = tx.Put("data", key, []byte("val"))
	}

	// 2. Start scanning staged rows
	scanDone := make(chan error, 1)
	read10 := make(chan struct{})
	go func() {
		count := 0
		err := tx.ScanRangeAt(ctx, "data", KeyRange{}, tx.Snapshot, func(k, v []byte) error {
			count++
			if count == 10 {
				close(read10)
				time.Sleep(50 * time.Millisecond) // hold in-flight scan
			}
			return nil
		})
		scanDone <- err
	}()

	<-read10

	// 3. Concurrently rollback the transaction while scan is in-flight
	rollbackDone := make(chan error, 1)
	go func() {
		rollbackDone <- tx.Rollback()
	}()

	select {
	case err := <-rollbackDone:
		if err != nil {
			t.Fatalf("rollback failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rollback hung waiting for stage lease")
	}

	// 4. Scan must either safely complete or return ErrClosed, never panic
	err = <-scanDone
	if err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("unexpected error from scan: %v", err)
	}
}

// TestP6Final04_StageLeaseDeterministicInterleaving covers P6-FINAL-04:
// Uses stageLeaseTestHook to pause execution inside stage.View and stagedValue,
// verifying that concurrent cleanup() blocks waiting for in-flight leases and never
// closes the bolt DB under active readers.
func TestP6Final04_StageLeaseDeterministicInterleaving(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	tx, err := s.Begin(ctx, s)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Stage 300 rows so writes flush to t.stage
	const totalRows = 300
	for i := 0; i < totalRows; i++ {
		key := []byte(fmt.Sprintf("k_%04d", i))
		_ = tx.Put("data", key, []byte("val"))
	}

	// 2. Set up hook to pause inside stage.View
	inView := make(chan struct{})
	releaseView := make(chan struct{})
	var hookOnce sync.Once

	stageLeaseTestHook = func(event string) {
		if event == "in_view" {
			hookOnce.Do(func() {
				close(inView)
				<-releaseView
			})
		}
	}
	defer func() { stageLeaseTestHook = nil }()

	scanDone := make(chan error, 1)
	go func() {
		err := tx.ScanRangeAt(ctx, "data", KeyRange{}, tx.Snapshot, func(k, v []byte) error {
			return nil
		})
		scanDone <- err
	}()

	// Wait until reader is inside stage.View
	<-inView

	// 3. Concurrently invoke Rollback: cleanup() must wait on stage lease!
	cleanupEntered := make(chan struct{})
	stageCleanupWaitHook = func() {
		close(cleanupEntered)
	}
	defer func() { stageCleanupWaitHook = nil }()

	rollbackDone := make(chan error, 1)
	go func() {
		rollbackDone <- tx.Rollback()
	}()

	// Barrier: deterministically verify rollback entered cleanup and is waiting on stage lease
	select {
	case <-cleanupEntered:
		// Cleanup is confirmed waiting on stage lease!
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup never entered wait on stage lease")
	}

	// Verify readers count > 0 while waiting
	tx.mu.Lock()
	readersWhileWaiting := tx.stageReaders
	closedWhileWaiting := tx.closed.Load()
	tx.mu.Unlock()
	if readersWhileWaiting <= 0 || !closedWhileWaiting {
		t.Fatalf("expected readers > 0 and closed true while cleanup waits: readers=%d, closed=%v", readersWhileWaiting, closedWhileWaiting)
	}

	// 4. Now allow reader to finish stage.View
	close(releaseView)

	// Reader should complete
	select {
	case err := <-scanDone:
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("scan error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader never completed")
	}

	// Rollback should now complete
	select {
	case err := <-rollbackDone:
		if err != nil {
			t.Fatalf("rollback error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rollback never completed")
	}

	// 5. Verify lease count is zero, stage is nil, and subsequent attempts return ErrClosed
	tx.mu.Lock()
	finalReaders := tx.stageReaders
	finalStage := tx.stage
	path := tx.path
	tx.mu.Unlock()

	if finalReaders != 0 {
		t.Fatalf("stageReaders = %d after cleanup, want 0", finalReaders)
	}
	if finalStage != nil {
		t.Fatalf("stage = %v after cleanup, want nil", finalStage)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stage file still exists after cleanup: %v", err)
	}

	_, _, err = tx.acquireStageLease()
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("acquireStageLease after rollback = %v, want ErrClosed", err)
	}
}
