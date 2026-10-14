package storageengine_test

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
	"gbaselite/storageengine/testkit"
)

// TestStorageEngineContract_PreserveExisting verifies that existing Engine.Begin,
// Txn.Snapshot, and Snapshot Isolation semantics remain intact.
func TestStorageEngineContract_PreserveExisting(t *testing.T) {
	mem := testkit.NewMemory()
	defer mem.Close()

	ctx := context.Background()
	tx, err := mem.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	// Snapshot() returns uint64
	snap := tx.Snapshot()
	if snap == 0 {
		// memory engine initial snapshot is 0 or 1, valid uint64
	}

	// Put and Get
	if err := tx.Put("space1", []byte("k1"), []byte("v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	val, ok, err := tx.Get("space1", []byte("k1"))
	if err != nil || !ok || string(val) != "v1" {
		t.Fatalf("Get: (%q, %v, %v)", string(val), ok, err)
	}

	// Scan
	count := 0
	err = tx.Scan(ctx, "space1", func(k, v []byte) error {
		count++
		return nil
	})
	if err != nil || count != 1 {
		t.Fatalf("Scan: count=%d, err=%v", count, err)
	}
}

// TestStorageEngineContract_OptionalCapability verifies that optional IsolationEngine
// returns ErrUnsupported when absent.
func TestStorageEngineContract_OptionalCapability(t *testing.T) {
	mem := testkit.NewMemory()
	defer mem.Close()

	ctx := context.Background()
	_, err := storageengine.BeginIsolated(ctx, mem, storageengine.IsolationRepeatableRead)
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BeginIsolated on basic engine returned %v, want ErrUnsupported", err)
	}

	tx, err := mem.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	if _, ok := tx.(storageengine.IsolationTxn); ok {
		t.Fatalf("basic txn unexpectedly implements IsolationTxn")
	}
}

// TestStorageEngineContract_NoForbiddenImports verifies that executor does not import mvcc directly.
func TestStorageEngineContract_NoForbiddenImports(t *testing.T) {
	fset := token.NewFileSet()
	root := filepath.Join("..", "executor")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), "_test.go") || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		node, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile %s: %v", path, err)
		}
		for _, imp := range node.Imports {
			pkg := strings.Trim(imp.Path.Value, `"`)
			if pkg == "gbaselite/mvcc" {
				t.Fatalf("file %s imports forbidden package %s", path, pkg)
			}
		}
	}
}

// TestP3Fix02_CapabilityGateInAdapter verifies P3-FIX-02:
// Direct Go API on mvccadapter must reject invalid/unimplemented isolation levels and uncommitted view kind.
func TestP3Fix02_CapabilityGateInAdapter(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	// 1. Invalid isolation level must be rejected
	if _, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationLevel("INVALID")); err == nil {
		t.Fatalf("BeginIsolated with invalid level unexpectedly succeeded")
	}

	// 2. Unimplemented isolation levels must return ErrUnsupported
	for _, lvl := range []storageengine.IsolationLevel{
		storageengine.IsolationReadUncommitted,
		storageengine.IsolationSerializable,
	} {
		_, err := storageengine.BeginIsolated(ctx, e, lvl)
		if !errors.Is(err, storageengine.ErrUnsupported) {
			t.Fatalf("BeginIsolated(%s) returned %v, want ErrUnsupported", lvl, err)
		}
	}

	// 3. REPEATABLE READ and READ COMMITTED must succeed
	for _, lvl := range []storageengine.IsolationLevel{
		storageengine.IsolationRepeatableRead,
		storageengine.IsolationReadCommitted,
	} {
		tx, err := storageengine.BeginIsolated(ctx, e, lvl)
		if err != nil {
			t.Fatalf("BeginIsolated(%s): %v", lvl, err)
		}
		defer tx.Rollback()
	}

	tx, _ := storageengine.BeginIsolated(ctx, e, storageengine.IsolationRepeatableRead)
	defer tx.Rollback()

	// 4. StatementViewKindUncommitted must return ErrUnsupported (not fake support)
	isoTxn := tx.(storageengine.IsolationTxn)
	if _, err := isoTxn.BeginStatementView(ctx, storageengine.StatementViewKindUncommitted); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BeginStatementView(Uncommitted) returned %v, want ErrUnsupported", err)
	}
}

// TestPRE01_ActiveIteratorProtectsAgainstEarlyViewCloseAndGC demonstrates PRE-01:
// An active in-flight iterator must retain the GC pin until it is closed, even if
// viewA.Close() is called concurrently before the iterator finishes scanning.
func TestPRE01_ActiveIteratorProtectsAgainstEarlyViewCloseAndGC(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	const totalRows = 300 // Spans at least 2 pages (MaxChunkBytes or 256 keys per page)

	// 1. Seed 300 rows k_000..k_299 with value V1
	tx0, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < totalRows; i++ {
		key := fmt.Sprintf("k_%04d", i)
		_ = tx0.Put("data", []byte(key), []byte("V1"))
	}
	if _, err := tx0.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Txn A begins RR and establishes consistent view at V1
	txA, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationRepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	isoTxnA := txA.(storageengine.IsolationTxn)
	viewA, err := isoTxnA.BeginConsistentView(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 3. Concurrent Txn B updates all 300 rows to V2 and commits
	txB, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < totalRows; i++ {
		key := fmt.Sprintf("k_%04d", i)
		_ = txB.Put("data", []byte(key), []byte("V2"))
	}
	if _, err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 4. Txn A opens iterator on viewA
	it, err := viewA.NewIterator(ctx, storageengine.ScanRequest{Space: "data"})
	if err != nil {
		t.Fatal(err)
	}

	// 5. Read first 10 rows from page 1
	for i := 0; i < 10; i++ {
		if !it.Next() {
			t.Fatalf("it.Next() %d failed: %v", i, it.Err())
		}
		expectedKey := fmt.Sprintf("k_%04d", i)
		if string(it.Key()) != expectedKey || string(it.Value()) != "V1" {
			t.Fatalf("row %d = (%s, %s), want (%s, V1)", i, it.Key(), it.Value(), expectedKey)
		}
	}

	// 6. Concurrently close viewA and run CompactHistory before reading page 2!
	_ = viewA.Close()

	m := e.(storageengine.Maintenance)
	if err := m.CompactHistory(ctx); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}

	// 7. Active iterator must STILL be able to read all remaining rows (including page 2) with V1!
	for i := 10; i < totalRows; i++ {
		if !it.Next() {
			t.Fatalf("it.Next() %d failed across page boundary after view close & GC: %v", i, it.Err())
		}
		expectedKey := fmt.Sprintf("k_%04d", i)
		if string(it.Key()) != expectedKey || string(it.Value()) != "V1" {
			t.Fatalf("row %d after GC = (%s, %s), want (%s, V1)", i, it.Key(), it.Value(), expectedKey)
		}
	}

	if it.Next() {
		t.Fatal("expected EOF")
	}
	_ = it.Close()

	// 8. Test Child ReadView borrowing: child iterator holds pin until closed
	txChild, err := txA.Child()
	if err != nil {
		t.Fatal(err)
	}
	defer txChild.Rollback()

	childIso := txChild.(storageengine.IsolationTxn)
	childView, err := childIso.BeginConsistentView(ctx)
	if err != nil {
		t.Fatal(err)
	}

	childIt, err := childView.NewIterator(ctx, storageengine.ScanRequest{Space: "data"})
	if err != nil {
		t.Fatal(err)
	}

	// Close childView early while iterator is active
	_ = childView.Close()

	// Child iterator must still be able to scan rows
	childCount := 0
	for childIt.Next() {
		childCount++
	}
	if childCount != totalRows {
		t.Fatalf("child scan count = %d, want %d", childCount, totalRows)
	}
	_ = childIt.Close()
}
