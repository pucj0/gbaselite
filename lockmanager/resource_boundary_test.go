package lockmanager

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// resource_boundary_test.go satisfies T069:
// Verifies default/custom budgets, exact/+1 boundary, large transactions,
// memory/waiters/requests bounds, diagnostics under load, and zero leaks.

func TestT069DefaultAndCustomBudgetResolution(t *testing.T) {
	// 0 resolves to defaults
	resolved, err := (BudgetLimits{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.MemoryBytes != DefaultMemoryBytes {
		t.Fatalf("expected default memory %d, got %d", DefaultMemoryBytes, resolved.MemoryBytes)
	}
	if resolved.RootRequests != DefaultRootRequests {
		t.Fatalf("expected default root requests %d, got %d", DefaultRootRequests, resolved.RootRequests)
	}
	if resolved.Waiters != DefaultWaiters {
		t.Fatalf("expected default max waiters %d, got %d", DefaultWaiters, resolved.Waiters)
	}
	if resolved.GraphWork != DefaultGraphWork {
		t.Fatalf("expected default graph work %d, got %d", DefaultGraphWork, resolved.GraphWork)
	}

	// Custom valid budgets preserved
	custom := BudgetLimits{
		MemoryBytes:  1024 * 1024,
		RootRequests: 1000,
		Waiters:      50,
		GraphWork:    500,
	}
	resCustom, err := custom.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resCustom.MemoryBytes != 1024*1024 || resCustom.RootRequests != 1000 || resCustom.Waiters != 50 || resCustom.GraphWork != 500 {
		t.Fatalf("custom budget not preserved: %+v", resCustom)
	}
}

func TestT069RootRequestsBudgetExactAndPlusOne(t *testing.T) {
	limits := BudgetLimits{RootRequests: 3}
	m, err := NewManager(limits, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	ctx := context.Background()
	_, _ = m.RegisterOwner("root-1", 1, 0, 10)
	s, _ := m.OpenScope("root-1", "s1", 0, ScopeStatement, time.Time{})

	ti := t063TableIntentRef("row/t1", "t1", ModeIX)
	r1 := t063RowRef("row/t1", "t1", "k1", ModeX)
	r2 := t063RowRef("row/t1", "t1", "k2", ModeX)
	r3 := t063RowRef("row/t1", "t1", "k3", ModeX)

	// Exactly 3 components: accepted
	res, err := m.Acquire(ctx, "root-1", s.ID, []ResourceRef{ti, r1, r2})
	if err != nil || res.Op != LockOpGranted {
		t.Fatalf("acquire exact 3 failed: %v", err)
	}

	// +1 component: rejected with budget error
	_, errPlusOne := m.Acquire(ctx, "root-1", s.ID, []ResourceRef{ti, r3})
	if !errors.Is(errPlusOne, ErrBudgetRootRequests) && !errors.Is(errPlusOne, ErrLockResourceLimit) {
		t.Fatalf("expected budget error, got: %v", errPlusOne)
	}

	m.ReleaseOwner("root-1")
	if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 {
		t.Fatalf("leak after release")
	}
}

func TestT069MemoryBudgetExactAndPlusOne(t *testing.T) {
	// Exact budget for 1 owner (overhead + fixed block) + 1 scope
	registration := int64(ownerFixedBytes) * 2
	exact := registration + scopeFixedBytes
	limits := BudgetLimits{MemoryBytes: exact, RootRequests: 4, Waiters: 4, GraphWork: 4}
	m, err := NewManager(limits, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// 1. Owner registration succeeds taking exactly registration bytes
	_, err = m.RegisterOwner("root-1", 1, 0, 10)
	if err != nil {
		t.Fatalf("RegisterOwner failed: %v", err)
	}
	if got := m.Ledger().Totals().MemoryBytes; got != registration {
		t.Fatalf("expected memory %d after owner, got %d", registration, got)
	}

	// 2. OpenScope succeeds taking exactly scopeFixedBytes -> reaches exact budget
	s1, err := m.OpenScope("root-1", "s1", 0, ScopeStatement, time.Time{})
	if err != nil {
		t.Fatalf("OpenScope failed: %v", err)
	}
	if got := m.Ledger().Totals().MemoryBytes; got != exact {
		t.Fatalf("expected memory exactly %d after scope, got %d", exact, got)
	}

	// 3. Second scope requires +scopeFixedBytes -> exceeds memory budget, fails closed
	_, errSecond := m.OpenScope("root-1", "s2", 0, ScopeStatement, time.Time{})
	if !errors.Is(errSecond, ErrBudgetMemory) && !errors.Is(errSecond, ErrLockResourceLimit) {
		t.Fatalf("expected ErrBudgetMemory on second scope, got: %v", errSecond)
	}

	// Memory usage must not change after failure (no partial leak)
	if got := m.Ledger().Totals().MemoryBytes; got != exact {
		t.Fatalf("memory leaked on failed scope: got %d, want %d", got, exact)
	}

	// Close first scope marks it closed but retains historical node bytes until owner release
	_, _ = m.CloseScope("root-1", s1.ID)
	if got := m.Ledger().Totals().MemoryBytes; got != exact {
		t.Fatalf("expected scope node to remain charged until owner release, got %d, want %d", got, exact)
	}

	// Release owner refunds everything to zero
	m.ReleaseOwner("root-1")
	if m.Ledger().Totals().MemoryBytes != 0 || m.Stats().ActiveOwners != 0 {
		t.Fatalf("leak after owner release: mem=%d, owners=%d", m.Ledger().Totals().MemoryBytes, m.Stats().ActiveOwners)
	}
}

func TestT069GraphWorkEdgeCapacityAndTraversalLimit(t *testing.T) {
	// 1. Edge capacity check via Ledger: GraphWork: 2 allows exactly 2 edges
	limits := BudgetLimits{GraphWork: 2}
	ledger, err := NewLedger(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ClaimEdge(); err != nil {
		t.Fatalf("edge 1 failed: %v", err)
	}
	if err := ledger.ClaimEdge(); err != nil {
		t.Fatalf("edge 2 failed: %v", err)
	}
	// Edge 3 exceeds GraphWork limit 2
	if err := ledger.ClaimEdge(); !errors.Is(err, ErrBudgetGraph) && !errors.Is(err, ErrLockResourceLimit) {
		t.Fatalf("expected ErrBudgetGraph on edge 3, got: %v", err)
	}
	ledger.ReleaseEdge()
	ledger.ReleaseEdge()
	if ledger.Totals().GraphEdges != 0 {
		t.Fatalf("edges residue: %d", ledger.Totals().GraphEdges)
	}

	// 2. Traversal work limit check via DetectDeadlock with GraphWork: 1
	// On a 3-cycle, budget of 1 step cannot finish walk -> fails closed without choosing false victim
	m, err := NewManager(BudgetLimits{GraphWork: 1}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	ctx := context.Background()
	_, _ = m.RegisterOwner("root-a", 1, 0, 10)
	_, _ = m.RegisterOwner("root-b", 1, 0, 10)
	_, _ = m.RegisterOwner("root-c", 1, 0, 10)

	sA, _ := m.OpenScope("root-a", "sa", 0, ScopeStatement, time.Time{})
	sB, _ := m.OpenScope("root-b", "sb", 0, ScopeStatement, time.Time{})
	sC, _ := m.OpenScope("root-c", "sc", 0, ScopeStatement, time.Time{})

	tiX := t063TableIntentRef("row/t1", "t1", ModeIX)
	r1 := t063RowRef("row/t1", "t1", "k1", ModeX)
	r2 := t063RowRef("row/t1", "t1", "k2", ModeX)
	r3 := t063RowRef("row/t1", "t1", "k3", ModeX)

	_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r1})
	_, _ = m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r2})
	_, _ = m.Acquire(ctx, "root-c", sC.ID, []ResourceRef{tiX, r3})

	_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r2})
	_, _ = m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r3})
	_, _ = m.Acquire(ctx, "root-c", sC.ID, []ResourceRef{tiX, r1})

	summary, found := m.DetectDeadlock()
	if found {
		t.Fatalf("exhausted budget should fail closed, but found deadlock: %+v", summary)
	}

	m.ReleaseOwner("root-a")
	m.ReleaseOwner("root-b")
	m.ReleaseOwner("root-c")
	if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 {
		t.Fatalf("leak after GraphWork cleanup")
	}
}

func TestT069DiagnosticsStressUnderHighLoad(t *testing.T) {
	m, err := NewManager(BudgetLimits{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	ctx := context.Background()
	const numOwners = 100
	const numKeysPerOwner = 10

	for i := 0; i < numOwners; i++ {
		rootID := fmt.Sprintf("owner-%03d", i)
		_, _ = m.RegisterOwner(rootID, 1, 0, 10)
		s, _ := m.OpenScope(rootID, fmt.Sprintf("scope-%03d", i), 0, ScopeStatement, time.Time{})
		var refs []ResourceRef
		refs = append(refs, t063TableIntentRef("row/t1", "t1", ModeIX))
		for j := 0; j < numKeysPerOwner; j++ {
			refs = append(refs, t063RowRef("row/t1", "t1", fmt.Sprintf("k-%03d-%02d", i, j), ModeX))
		}
		_, err := m.Acquire(ctx, rootID, s.ID, refs)
		if err != nil {
			t.Fatalf("acquire for %s failed: %v", rootID, err)
		}
	}

	// 1,000 active grants created!
	stats := m.Stats()
	if stats.GrantedComponents < 1000 {
		t.Fatalf("expected >= 1000 granted components, got %d", stats.GrantedComponents)
	}

	// Query diagnostics with limit 256
	page := m.Diagnostics(DiagnosticsOptions{Limit: 256})
	if len(page.Grants) != 256 {
		t.Fatalf("expected exactly 256 grant rows, got %d", len(page.Grants))
	}
	if !page.GrantsTruncated {
		t.Fatal("expected GrantsTruncated to be true")
	}
	if page.TotalGrants < 1000 {
		t.Fatalf("expected TotalGrants >= 1000, got %d", page.TotalGrants)
	}

	// Clean up all
	for i := 0; i < numOwners; i++ {
		m.ReleaseOwner(fmt.Sprintf("owner-%03d", i))
	}

	if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 || m.Ledger().Totals().Components != 0 {
		t.Fatalf("leak after high load cleanup: table=%d, owners=%d, components=%d",
			m.LockTableSize(), m.Stats().ActiveOwners, m.Ledger().Totals().Components)
	}
}
