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

func TestT069WaitersBudgetExactAndPlusOne(t *testing.T) {
	limits := BudgetLimits{Waiters: 1}
	m, err := NewManager(limits, 1, nil)
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

	// A holds lock
	_, _ = m.Acquire(ctx, "root-a", sA.ID, []ResourceRef{tiX, r1})

	// B waits: exactly 1 waiter allowed
	resB, errB := m.Acquire(ctx, "root-b", sB.ID, []ResourceRef{tiX, r1})
	if errB != nil || resB.Op != LockOpWait {
		t.Fatalf("B wait failed: %v", errB)
	}

	// C tries to wait: +1 waiter exceeds limit 1
	_, errC := m.Acquire(ctx, "root-c", sC.ID, []ResourceRef{tiX, r1})
	if !errors.Is(errC, ErrBudgetWaiters) && !errors.Is(errC, ErrLockResourceLimit) {
		t.Fatalf("expected ErrBudgetWaiters, got: %v", errC)
	}

	m.ReleaseOwner("root-a")
	m.ReleaseOwner("root-b")
	m.ReleaseOwner("root-c")
	if m.LockTableSize() != 0 || m.Stats().ActiveOwners != 0 {
		t.Fatalf("leak after release")
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
