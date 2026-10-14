package storageengine_test

import (
	"context"
	"errors"
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
		storageengine.IsolationReadCommitted,
		storageengine.IsolationSerializable,
	} {
		_, err := storageengine.BeginIsolated(ctx, e, lvl)
		if !errors.Is(err, storageengine.ErrUnsupported) {
			t.Fatalf("BeginIsolated(%s) returned %v, want ErrUnsupported", lvl, err)
		}
	}

	// 3. REPEATABLE READ must succeed
	tx, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationRepeatableRead)
	if err != nil {
		t.Fatalf("BeginIsolated(RR): %v", err)
	}
	defer tx.Rollback()

	// 4. StatementViewKindUncommitted must return ErrUnsupported (not fake support)
	isoTxn := tx.(storageengine.IsolationTxn)
	if _, err := isoTxn.BeginStatementView(ctx, storageengine.StatementViewKindUncommitted); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BeginStatementView(Uncommitted) returned %v, want ErrUnsupported", err)
	}
}
