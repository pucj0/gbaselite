package mvcc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gbaselite/storageengine"
)

// TestViewToken_LifecycleAndPin tests basic view pin and release mechanics (T022).
func TestViewToken_LifecycleAndPin(t *testing.T) {
	m := NewTransactionManager()

	// Register a view at revision 10
	reg := ViewRegistration{
		ID:         "v1",
		RootID:     "root-1",
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

	registerTestRoot(t, m, "root-1", 50)
	if err := m.RegisterView(ViewRegistration{ID: "v1", RootID: "root-1", Revision: 30, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterView(ViewRegistration{ID: "v2", RootID: "root-1", Revision: 40, Generation: 1}); err != nil {
		t.Fatal(err)
	}

	// OldestReadTS should be 30 (the oldest view pin)
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
	if count := managerRefCount(t, m, 50); count != 0 {
		t.Fatalf("snapshotRefs[50] = %d, want 0 after root commit", count)
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
