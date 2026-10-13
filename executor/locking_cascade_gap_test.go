package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/sqllayout"
	"gbaselite/storageengine"
)

// locking_cascade_gap_test.go verifies that foreign-key CASCADE modifications of a child table's
// primary key properly acquire the insert intention and are blocked by covering Gap locks.
func TestP1CascadePrimaryKeyUpdateBlockedByChildGap(t *testing.T) {
	f := newT035Fixture(t,
		"CREATE TABLE parent(id BIGINT PRIMARY KEY)",
		"CREATE TABLE child(id BIGINT PRIMARY KEY, FOREIGN KEY(id) REFERENCES parent(id) ON UPDATE CASCADE)",
		"INSERT INTO parent VALUES(10)",
		"INSERT INTO child VALUES(10)",
	)

	sA := t034Session(t, f, 20)
	sB := t034Session(t, f, 2)

	lm := engineLockManager(f.engine.Backend)
	if lm == nil {
		t.Fatal("no lock manager")
	}

	var capturedB []storageengine.LockRequest
	var mu sync.Mutex
	ready := make(chan struct{})
	var once sync.Once

	previous := dmlLockRequestHook
	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		mu.Lock()
		capturedB = append(capturedB, req)
		mu.Unlock()
		return storageengine.LockRequest{
			Components: req.Components,
			ShouldWait: func(context.Context) bool {
				once.Do(func() { close(ready) })
				return true
			},
		}
	}
	defer func() { dmlLockRequestHook = previous }()

	// Session A holds Gap covering missing range [15, 25] in child table, with NO record lock on 20!
	f.must(sA, "BEGIN")
	res := f.must(sA, "SELECT * FROM child WHERE id BETWEEN 15 AND 25 FOR UPDATE")
	if got := t035Rows(t, res); got != "" {
		t.Fatalf("expected no rows for missing range, got %q", got)
	}

	// Verify Session A holds the Gap lock on child and extract its RootID
	var foundChildGap bool
	rootA := sA.transaction.ID()
	for _, snap := range lm.Snapshot() {
		if snap.Resource.Identity.Mode == lockmanager.ModeGap &&
			snap.Resource.Identity.Kind == lockmanager.KindInterval &&
			snap.Resource.Identity.TableID != "" {
			foundChildGap = true
			break
		}
	}
	if !foundChildGap {
		t.Fatal("Session A did not hold expected Gap lock on child")
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(sB, "UPDATE parent SET id=20 WHERE id=10")
		done <- err
	}()

	select {
	case <-ready:
		// Reached barrier: Session B is waiting for the Gap lock
	case <-time.After(2 * time.Second):
		t.Fatal("Session B did not reach wait barrier: Gap did not block derived child insert")
	}

	// Verify wait edges show Session B waiting on Session A specifically on the Gap resource
	edges := lm.WaitEdges()
	if len(edges) == 0 {
		t.Fatal("expected wait edges during Gap block, got none")
	}
	var matchedEdge bool
	for _, edge := range edges {
		if edge.ToRootID == rootA && edge.Reason == lockmanager.EdgeHolder && edge.ResourceID != 0 {
			matchedEdge = true
			t.Logf("Verified exact wait edge: from=%s to=%s reason=%s resourceID=%d",
				edge.FromRootID, edge.ToRootID, edge.Reason, edge.ResourceID)
			break
		}
	}
	if !matchedEdge {
		t.Fatalf("expected wait edge to rootA (%s) on Gap resource, got: %+v", rootA, edges)
	}

	// Verify Session B's captured requests contain child new primary key with BOTH Record X and Insert Intention
	mu.Lock()
	var foundChildRecordX, foundChildII bool
	for _, req := range capturedB {
		for _, c := range req.Components {
			if c.TableID != "" && c.IndexID == "" && len(c.Key) > 0 {
				key20 := sqlIntegerKey(20)
				if string(c.Key) == string(key20) {
					childTable := t026LoadTable(t, f.engine, sB, "child")
					if c.Space == sqllayout.Rows(childTable.ID) {
						if c.Mode == storageengine.LockModeX && c.Kind == storageengine.LockComponentRecord {
							foundChildRecordX = true
						}
						if c.Mode == storageengine.LockModeInsertIntention && c.Kind == storageengine.LockComponentRecord {
							foundChildII = true
						}
					}
				}
			}
		}
	}
	mu.Unlock()

	if !foundChildRecordX {
		t.Fatal("Session B did not request Record X on child new primary key 20")
	}
	if !foundChildII {
		t.Fatal("Session B did not request Insert Intention on child new primary key 20: child Gap was bypassed!")
	}

	// Session A rolls back, releasing the Gap lock
	f.must(sA, "ROLLBACK")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Session B failed after Gap released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Session B did not wake up after Gap released")
	}

	// Final verification: parent and child are both id=20, id=10 is gone
	pRows := f.must(sB, "SELECT id FROM parent")
	cRows := f.must(sB, "SELECT id FROM child")
	if got := t035Rows(t, pRows); got != "20;" {
		t.Fatalf("parent rows = %q, want 20;", got)
	}
	if got := t035Rows(t, cRows); got != "20;" {
		t.Fatalf("child rows = %q, want 20;", got)
	}

	oldP := f.must(sB, "SELECT COUNT(*) FROM parent WHERE id=10")
	oldC := f.must(sB, "SELECT COUNT(*) FROM child WHERE id=10")
	if t035Rows(t, oldP) != "0;" || t035Rows(t, oldC) != "0;" {
		t.Fatal("old id=10 still exists in parent or child")
	}

	// Post-release cleanup verification: no residual wait edges or pending requests
	statsEnd := lm.Stats()
	if statsEnd.PendingRequests != 0 || statsEnd.WaitEdges != 0 || len(lm.WaitEdges()) != 0 {
		t.Fatalf("lock manager has residue after transactions settled: stats=%+v, edges=%+v", statsEnd, lm.WaitEdges())
	}
}

// TestP1CascadePrimaryKeyUpdateOutsideGapProceeds verifies that when the new child primary key
// falls outside the held Gap, the cascade update proceeds without blocking, asserting deterministic
// coordination and bounded completion without wall-clock sleep.
func TestP1CascadePrimaryKeyUpdateOutsideGapProceeds(t *testing.T) {
	f := newT035Fixture(t,
		"CREATE TABLE parent(id BIGINT PRIMARY KEY)",
		"CREATE TABLE child(id BIGINT PRIMARY KEY, FOREIGN KEY(id) REFERENCES parent(id) ON UPDATE CASCADE)",
		"INSERT INTO parent VALUES(10)",
		"INSERT INTO child VALUES(10)",
	)

	sA := t034Session(t, f, 20)
	sB := t034Session(t, f, 2)

	lm := engineLockManager(f.engine.Backend)
	if lm == nil {
		t.Fatal("no lock manager")
	}

	// Session A holds Gap covering range [30, 40] in child table (key 20 is outside!)
	f.must(sA, "BEGIN")
	f.must(sA, "SELECT * FROM child WHERE id BETWEEN 30 AND 40 FOR UPDATE")

	// Deterministic wait-detector: If Session B were to block on the Gap, ShouldWait would be called.
	var waitedOnGap bool
	var mu sync.Mutex
	previous := dmlLockRequestHook
	dmlLockRequestHook = func(req storageengine.LockRequest) storageengine.LockRequest {
		return storageengine.LockRequest{
			Components: req.Components,
			ShouldWait: func(context.Context) bool {
				mu.Lock()
				waitedOnGap = true
				mu.Unlock()
				return true
			},
		}
	}
	defer func() { dmlLockRequestHook = previous }()

	// Session B updates parent id=10 to 20 with bounded completion coordination
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(sB, "UPDATE parent SET id=20 WHERE id=10")
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Session B failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Session B was boundedly blocked on unrelated Gap")
	}

	mu.Lock()
	didWait := waitedOnGap
	mu.Unlock()
	if didWait {
		t.Fatal("Session B called ShouldWait on unrelated Gap: false block occurred!")
	}

	// Verify no wait edges were formed
	if len(lm.WaitEdges()) != 0 {
		t.Fatalf("unexpected wait edges formed for unrelated Gap: %+v", lm.WaitEdges())
	}

	pRows := f.must(sB, "SELECT id FROM parent")
	cRows := f.must(sB, "SELECT id FROM child")
	if got := t035Rows(t, pRows); got != "20;" || got != t035Rows(t, cRows) {
		t.Fatalf("unexpected parent/child rows: p=%q c=%q", t035Rows(t, pRows), t035Rows(t, cRows))
	}
	f.must(sA, "ROLLBACK")
}
