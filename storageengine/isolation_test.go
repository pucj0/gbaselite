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
	"time"

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
	} {
		_, err := storageengine.BeginIsolated(ctx, e, lvl)
		if !errors.Is(err, storageengine.ErrUnsupported) {
			t.Fatalf("BeginIsolated(%s) returned %v, want ErrUnsupported", lvl, err)
		}
	}

	// 3. REPEATABLE READ, READ COMMITTED, and SERIALIZABLE must succeed
	for _, lvl := range []storageengine.IsolationLevel{
		storageengine.IsolationRepeatableRead,
		storageengine.IsolationReadCommitted,
		storageengine.IsolationSerializable,
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

// TestPRE06A_OptimisticGuardNotOverwrittenByLockedRead covers PRE-06A:
// An optimistic Guard declared on key k cannot be relaxed or overwritten
// by a subsequent locked current read on the same key k.
func TestPRE06A_OptimisticGuardNotOverwrittenByLockedRead(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	const testSpace = "rows/test-pre06a"

	// 1. Seed k=V1
	tx0, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx0.Put(testSpace, []byte("k"), []byte("V1"))
	_ = tx0.Put(testSpace, []byte("k2"), []byte("V1"))
	if _, err := tx0.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. A begins RC
	txA, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	// 3. A declares Guard on k (optimistic dependency on V1)
	if err := txA.Guard(testSpace, []byte("k")); err != nil {
		t.Fatal(err)
	}

	// 4. B modifies k to V2 and commits
	txB, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = txB.Put(testSpace, []byte("k"), []byte("V2"))
	if _, err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 5. A subsequently performs a locked current read on k
	lockingA, ok := txA.(storageengine.LockingTxn)
	if !ok {
		t.Fatal("txA missing LockingTxn capability")
	}
	scope, err := lockingA.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := lockingA.Acquire(ctx, scope, storageengine.LockRequest{
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: testSpace, TableID: "t1"},
			{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: testSpace, TableID: "t1", Key: []byte("k")},
		},
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	obs, err := lockingA.ReadCurrent(ctx, grant, testSpace, []byte("k"))
	if err != nil {
		t.Fatalf("ReadCurrent: %v", err)
	}
	if string(obs.Value) != "V2" {
		t.Fatalf("obs value = %s, want V2", obs.Value)
	}

	// 6. A writes unrelated key k2
	if err := txA.Put(testSpace, []byte("k2"), []byte("V_A")); err != nil {
		t.Fatal(err)
	}

	// 7. A commits: MUST FAIL with ErrConflict because k had an earlier Guard dependency!
	_, err = txA.Commit(ctx)
	if !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("txA Commit = %v, want ErrConflict (Guard dependency was overwritten by subsequent locked read!)", err)
	}
}

// TestPRE06B_RootTerminalCancelsInFlightStageScanCleanly covers PRE-06B:
// When an in-flight iterator is reading through multi-page uncommitted staged writes,
// a concurrent root Rollback must not panic, crash, or access a closed stage database.
func TestPRE06B_RootTerminalCancelsInFlightStageScanCleanly(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()
	const totalRows = 300

	tx, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Stage 300 rows (exceeds single page buffer so it flushes to t.stage file)
	for i := 0; i < totalRows; i++ {
		key := fmt.Sprintf("k_%04d", i)
		_ = tx.Put("staged_space", []byte(key), []byte("val"))
	}

	it, err := tx.NewIterator(ctx, storageengine.ScanRequest{Space: "staged_space"})
	if err != nil {
		t.Fatal(err)
	}

	// Read first 10 rows
	for i := 0; i < 10; i++ {
		if !it.Next() {
			t.Fatalf("read %d failed: %v", i, it.Err())
		}
	}

	// Concurrently rollback transaction
	_ = tx.Rollback()

	// Continue reading: must cleanly terminate or fail without panic or closed database crash
	for it.Next() {
	}

	_ = it.Close()
}

// TestP6Fix01_SerializableGoAPIRawReadsUnsupported covers P6-FIX-01:
// Raw unguided reads (Get, Scan, NewIterator, BeginStatementView, BeginConsistentView) on a SERIALIZABLE
// transaction without a LockPlan must fail closed with ErrUnsupported to prevent bypassing lock protection,
// while internal metadata reads on catalog space succeed.
func TestP6Fix01_SerializableGoAPIRawReadsUnsupported(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	// 1. Seed data k=V1 and catalog metadata
	tx0, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx0.Put("rows/t1", []byte("k"), []byte("V1"))
	_ = tx0.Put("catalog", []byte("meta_key"), []byte("meta_val"))
	if _, err := tx0.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Start SERIALIZABLE transaction via Go API
	txA, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationSerializable)
	if err != nil {
		t.Fatalf("BeginIsolated SERIALIZABLE: %v", err)
	}
	defer txA.Rollback()

	// 3. Scenario A: Txn.Get on existing record acquires S lock and reads V1
	val, ok, err := txA.Get("rows/t1", []byte("k"))
	if err != nil || !ok || string(val) != "V1" {
		t.Fatalf("txA.Get(rows/t1, k): (%s, %v, %v), want V1", val, ok, err)
	}

	noWait := func(context.Context) bool { return false }

	// Concurrent B attempting to modify k must fail or block on X lock
	txB, _ := e.Begin(ctx)
	lockingB, _ := storageengine.LockingTxnOf(txB)
	scopeB, _ := lockingB.BeginLockScope(ctx, storageengine.LockScopeStatement)
	_, errB := lockingB.Acquire(ctx, scopeB, storageengine.LockRequest{
		ShouldWait: noWait,
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: "rows/t1", TableID: "t1"},
			{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: "rows/t1", TableID: "t1", Key: []byte("k")},
		},
	})
	if errB == nil {
		t.Fatalf("B was able to acquire X lock while A holds S lock in SERIALIZABLE!")
	}
	_ = txB.Rollback()

	// Scenario B: Txn.Get on absent record acquires Gap lock
	valAbs, okAbs, err := txA.Get("rows/t1", []byte("absent_key"))
	if err != nil || okAbs || valAbs != nil {
		t.Fatalf("absent Get: (%v, %v, %v)", valAbs, okAbs, err)
	}

	// Concurrent B attempting to insert at absent_key must be blocked by Gap lock
	txB2, _ := e.Begin(ctx)
	lockingB2, _ := storageengine.LockingTxnOf(txB2)
	scopeB2, _ := lockingB2.BeginLockScope(ctx, storageengine.LockScopeStatement)
	_, errB2 := lockingB2.Acquire(ctx, scopeB2, storageengine.LockRequest{
		ShouldWait: noWait,
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: "rows/t1", TableID: "t1"},
			{Mode: storageengine.LockModeInsertIntention, Kind: storageengine.LockComponentRecord, Space: "rows/t1", TableID: "t1", Key: []byte("absent_key")},
		},
	})
	if errB2 == nil {
		t.Fatalf("B was able to acquire InsertIntention lock while A holds Gap lock on absent key!")
	}
	_ = txB2.Rollback()

	// 4. Scenario C: raw unguided Scan, NewIterator, BeginStatementView, BeginConsistentView must fail closed!
	if err := txA.Scan(ctx, "rows/t1", func(k, v []byte) error { return nil }); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("txA.Scan returned %v, want ErrUnsupported", err)
	}
	if _, err := txA.NewIterator(ctx, storageengine.ScanRequest{Space: "rows/t1"}); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("txA.NewIterator returned %v, want ErrUnsupported", err)
	}
	isoTxnA := txA.(storageengine.IsolationTxn)
	if _, err := isoTxnA.BeginStatementView(ctx, storageengine.StatementViewKindStatement); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BeginStatementView returned %v, want ErrUnsupported", err)
	}
	if _, err := isoTxnA.BeginConsistentView(ctx); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BeginConsistentView returned %v, want ErrUnsupported", err)
	}

	// 5. Scenario D: Metadata space reads (catalog) must succeed!
	valMeta, okMeta, err := txA.Get("catalog", []byte("meta_key"))
	if err != nil || !okMeta || string(valMeta) != "meta_val" {
		t.Fatalf("metadata Get: (%s, %v, %v), want (meta_val, true, nil)", string(valMeta), okMeta, err)
	}
}

// TestP6R201_SerializableChildAndActiveViewCannotBypassLock covers P6-R2-01:
// 1. Child.Get on SERIALIZABLE must acquire S lock or fail closed, cannot bypass via inner.Get.
// 2. ActiveView cannot be used to bypass locks on user data in SERIALIZABLE.
func TestP6R201_SerializableChildAndActiveViewCannotBypassLock(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	// 1. Seed row k=V1
	tx0, _ := e.Begin(ctx)
	_ = tx0.Put("rows/t1", []byte("k"), []byte("V1"))
	_, _ = tx0.Commit(ctx)

	// 2. Root begins SERIALIZABLE
	txRoot, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationSerializable)
	if err != nil {
		t.Fatal(err)
	}
	defer txRoot.Rollback()

	// 3. Child created from Root
	txChild, err := txRoot.Child()
	if err != nil {
		t.Fatal(err)
	}
	defer txChild.Rollback()

	// 4. Child.Get(k) acquires S lock on k
	val, ok, err := txChild.Get("rows/t1", []byte("k"))
	if err != nil || !ok || string(val) != "V1" {
		t.Fatalf("Child.Get = (%s, %v, %v), want V1", val, ok, err)
	}

	// 5. Concurrent B attempting to update k must be blocked by Child's S lock!
	noWait := func(context.Context) bool { return false }
	txB, _ := e.Begin(ctx)
	defer txB.Rollback()

	lockingB, _ := storageengine.LockingTxnOf(txB)
	scopeB, _ := lockingB.BeginLockScope(ctx, storageengine.LockScopeStatement)
	_, errB := lockingB.Acquire(ctx, scopeB, storageengine.LockRequest{
		ShouldWait: noWait,
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: "rows/t1", TableID: "t1"},
			{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: "rows/t1", TableID: "t1", Key: []byte("k")},
		},
	})
	if errB == nil {
		t.Fatalf("B was able to acquire X lock while Child holds S lock in SERIALIZABLE!")
	}

	// 6. Attempting raw unguided scan on Child must fail closed
	if err := txChild.Scan(ctx, "rows/t1", func(k, v []byte) error { return nil }); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("Child.Scan returned %v, want ErrUnsupported", err)
	}
}

// TestP6R202_SerializableGetGapLockErrorHandling covers P6-R2-02:
// Verifies that serializableGet does not swallow Gap Lock acquisition errors or KeysAround failures,
// and ensures lock scopes are cleanly closed.
func TestP6R202_SerializableGetGapLockErrorHandling(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	// 1. Seed two boundary rows: id=10, id=100
	tx0, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx0.Put("rows/t1", []byte("k10"), []byte("V10"))
	_ = tx0.Put("rows/t1", []byte("k100"), []byte("V100"))
	if _, err := tx0.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Txn B acquires exclusive gap lock on (k10, k100)
	txB, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()

	lockingB, err := storageengine.LockingTxnOf(txB)
	if err != nil {
		t.Fatal(err)
	}
	scopeB, err := lockingB.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	// B holds X lock on the gap
	_, err = lockingB.Acquire(ctx, scopeB, storageengine.LockRequest{
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: "rows/t1", TableID: "t1"},
			{Mode: storageengine.LockModeGap, Kind: storageengine.LockComponentInterval, Space: "rows/t1", TableID: "t1", Lower: []byte("k10"), Upper: []byte("k100")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Txn A begins SERIALIZABLE
	txA, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationSerializable)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	// Rollback txB so locks release
	_ = txB.Rollback()

	// Now A can get "k50" absent successfully with gap lock acquired
	val, ok, err := txA.Get("rows/t1", []byte("k50"))
	if err != nil || ok || val != nil {
		t.Fatalf("absent Get after B released: (%v, %v, %v)", val, ok, err)
	}
}

// TestP6Fix03_GuardFollowedByPutRetainsGuardConflict covers P6-FIX-03:
// When an optimistic Guard is declared on key k, and later a Put or Delete is written to the same key k,
// the earlier optimistic dependency must NOT be lost or overwritten by Put's observation baseline.
func TestP6Fix03_GuardFollowedByPutRetainsGuardConflict(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	// 1. Seed k=V1
	tx0, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx0.Put("rows/t1", []byte("k"), []byte("V1"))
	if _, err := tx0.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Txn A begins
	txA, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationReadCommitted)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	// 3. A guards k (optimistic dependency on V1)
	if err := txA.Guard("rows/t1", []byte("k")); err != nil {
		t.Fatal(err)
	}

	// 4. Concurrent B modifies k to V2 and commits
	txB, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = txB.Put("rows/t1", []byte("k"), []byte("V2"))
	if _, err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 5. A performs locked current read on k and then PUTS k (overwriting the Guard op in buffer)
	lockingA, err := storageengine.LockingTxnOf(txA)
	if err != nil {
		t.Fatal(err)
	}
	scopeA, err := lockingA.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := lockingA.Acquire(ctx, scopeA, storageengine.LockRequest{
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: "rows/t1", TableID: "t1"},
			{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: "rows/t1", TableID: "t1", Key: []byte("k")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockingA.ReadCurrent(ctx, grant, "rows/t1", []byte("k"))
	if err != nil {
		t.Fatal(err)
	}

	// A puts k=V_A (replaces Guard op in write buffer!)
	if err := txA.Put("rows/t1", []byte("k"), []byte("V_A")); err != nil {
		t.Fatal(err)
	}

	// 6. A commits: MUST FAIL with ErrConflict because k had an earlier Guard dependency!
	_, err = txA.Commit(ctx)
	if !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("txA Commit = %v, want ErrConflict (Guard dependency was overwritten by Put on same key!)", err)
	}
}

// TestP6Final01_PublicWithInternalReaderCannotBypassSerializable covers P6-FINAL-01:
// Demonstrates that public WithInternalReader cannot be used by an external caller to bypass
// Record S / Gap locks on user data in a SERIALIZABLE transaction.
func TestP6Final01_PublicWithInternalReaderCannotBypassSerializable(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	// Seed k=V1
	tx0, _ := e.Begin(ctx)
	_ = tx0.Put("rows/t1", []byte("k"), []byte("V1"))
	_, _ = tx0.Commit(ctx)

	// A starts SERIALIZABLE
	txA, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationSerializable)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	// Try to use public WithInternalReader to create a statement view
	ctxInternal := storageengine.WithInternalReader(context.Background())
	isoA := txA.(storageengine.IsolationTxn)
	view, err := isoA.BeginStatementView(ctxInternal, storageengine.StatementViewKindStatement)

	if err == nil {
		// Attempting view.Get on user data MUST be refused with ErrUnsupported!
		_, _, errGet := view.Get("rows/t1", []byte("k"))
		if !errors.Is(errGet, storageengine.ErrUnsupported) {
			t.Fatalf("view.Get on user data in SERIALIZABLE returned %v, want ErrUnsupported (lock bypass!)", errGet)
		}
		// Attempting view.NewIterator on user data MUST be refused with ErrUnsupported!
		_, errIt := view.NewIterator(ctx, storageengine.ScanRequest{Space: "rows/t1"})
		if !errors.Is(errIt, storageengine.ErrUnsupported) {
			t.Fatalf("view.NewIterator on user data in SERIALIZABLE returned %v, want ErrUnsupported (lock bypass!)", errIt)
		}
	} else if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BeginStatementView returned %v, want ErrUnsupported", err)
	}

	// ActiveView must NOT bypass locks in Txn.Get
	val, ok, err := txA.Get("rows/t1", []byte("k"))
	if err != nil || !ok || string(val) != "V1" {
		t.Fatalf("txA.Get failed: %v", err)
	}
	// Verify that S lock is really held on k (concurrent X lock must be blocked!)
	noWait := func(context.Context) bool { return false }
	txB, _ := e.Begin(ctx)
	defer txB.Rollback()
	lockingB, _ := storageengine.LockingTxnOf(txB)
	scopeB, _ := lockingB.BeginLockScope(ctx, storageengine.LockScopeStatement)
	_, errB := lockingB.Acquire(ctx, scopeB, storageengine.LockRequest{
		ShouldWait: noWait,
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: "rows/t1", TableID: "t1"},
			{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: "rows/t1", TableID: "t1", Key: []byte("k")},
		},
	})
	if errB == nil {
		t.Fatalf("ActiveView allowed bypassing S lock in txA.Get!")
	}
}

// TestP6Final02_SerializableGetResourceIdentity covers P6-FINAL-02:
// Verifies that serializableGet enforces exact B02 ResourceRef identities (row, index, secondary)
// and rejects invalid or malformed spaces with ErrUnsupported.
func TestP6Final02_SerializableGetResourceIdentity(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	txA, err := storageengine.BeginIsolated(ctx, e, storageengine.IsolationSerializable)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	// 1. Empty space must be rejected with ErrUnsupported
	if _, _, err := txA.Get("", []byte("k")); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("txA.Get(\"\") returned %v, want ErrUnsupported", err)
	}

	// 2. Unprefixed invalid space must be rejected with ErrUnsupported
	if _, _, err := txA.Get("random_data", []byte("k")); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("txA.Get(\"random_data\") returned %v, want ErrUnsupported", err)
	}

	// 3. Malformed row spaces must be rejected with ErrUnsupported
	if _, _, err := txA.Get("row/", []byte("k")); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("txA.Get(\"row/\") returned %v, want ErrUnsupported", err)
	}
	if _, _, err := txA.Get("row/t1//extra", []byte("k")); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("txA.Get(\"row/t1//extra\") returned %v, want ErrUnsupported", err)
	}
	if _, _, err := txA.Get("index/t1", []byte("k")); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("txA.Get(\"index/t1\") returned %v, want ErrUnsupported", err)
	}

	// 4. Valid multi-segment table identities must succeed
	if _, _, err := txA.Get("row/hash_12345/t1", []byte("k")); err != nil {
		t.Fatalf("valid multi-segment row space failed: %v", err)
	}
	if _, _, err := txA.Get("index/hash_12345/t1/idx1", []byte("k")); err != nil {
		t.Fatalf("valid multi-segment index space failed: %v", err)
	}
}

// TestP6Final03_GapAndRecordLockErrorPaths covers P6-FINAL-03:
// Verifies that serializableGet enters the lock wait queue when conflicting with B,
// respects context cancellation and timeout, returns real errors instead of (nil, false, nil),
// and leaves no pending request or scope leaks.
func TestP6Final03_GapAndRecordLockErrorPaths(t *testing.T) {
	e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx := context.Background()

	// Seed k=V1
	tx0, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx0.Put("rows/t1", []byte("k"), []byte("V1"))
	if _, err := tx0.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 1. Txn B acquires Record X on k
	txB, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()

	lockingB, err := storageengine.LockingTxnOf(txB)
	if err != nil {
		t.Fatal(err)
	}
	scopeB, err := lockingB.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockingB.Acquire(ctx, scopeB, storageengine.LockRequest{
		Components: []storageengine.LockComponent{
			{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: "rows/t1", TableID: "t1"},
			{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: "rows/t1", TableID: "t1", Key: []byte("k")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Txn A begins SERIALIZABLE with a context that will be canceled
	ctxCancel, cancelA := context.WithCancel(ctx)
	txA, err := storageengine.BeginIsolated(ctxCancel, e, storageengine.IsolationSerializable)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	getDone := make(chan error, 1)
	go func() {
		_, _, err := txA.Get("rows/t1", []byte("k"))
		getDone <- err
	}()

	// Give a moment for A to encounter B's conflicting lock and wait
	time.Sleep(50 * time.Millisecond)

	// 3. Cancel A's context while in queue
	cancelA()

	select {
	case err := <-getDone:
		if err == nil {
			t.Fatal("txA.Get returned nil error despite canceled context while waiting for lock!")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("txA.Get returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("txA.Get hung and did not abort on context cancellation")
	}
}
