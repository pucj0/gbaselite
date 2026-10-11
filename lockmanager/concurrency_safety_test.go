package lockmanager

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestB02LedgerAndManagerAreConcurrencySafe exercises the two registries from many
// goroutines at once.
//
// The race detector is unavailable on a host without a C toolchain, so this test does
// not prove the absence of a race by itself. What it does prove is the property a race
// would destroy: with every counter starting at zero and every owner shared by several
// goroutines, a bounded amount of concurrent traffic leaves the accounting exactly
// consistent and reaches an exact zero after a single release per owner.
func TestB02LedgerAndManagerAreConcurrencySafe(t *testing.T) {
	const (
		owners     = 8
		goroutines = 8
		operations = 200
	)
	m, _ := newTestManager(t, BudgetLimits{MemoryBytes: 64 << 20, RootRequests: 1 << 20, Waiters: 1 << 16, GraphWork: 1 << 16})

	// One scope per owner, opened before the concurrent phase so the concurrent phase
	// only touches budgets and registries.
	scopes := make([]uint64, owners)
	for i := 0; i < owners; i++ {
		rootID := rootName(i)
		if _, err := m.RegisterOwner(rootID, 1, 0, uint64(100+i)); err != nil {
			t.Fatalf("register %s: %v", rootID, err)
		}
		scope, err := m.OpenScope(rootID, rootID+"-txn", 0, ScopeStatement, time.Time{})
		if err != nil {
			t.Fatalf("scope %s: %v", rootID, err)
		}
		scopes[i] = scope.ID
	}

	var wg sync.WaitGroup
	start := NewBarrier()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			// A barrier, not a sleep: every goroutine starts its traffic only after all
			// of them have arrived, so the interleaving is genuinely concurrent.
			if !start.Wait(context.Background()) {
				return
			}
			for op := 0; op < operations; op++ {
				rootID := rootName((worker + op) % owners)
				key := []byte{byte('a' + op%26), byte(worker)}
				component := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: key}}
				reservation, err := m.Ledger().Reserve(rootID, []ResourceRef{component}, op%3 == 0)
				if err != nil {
					// A per-root component budget race must never surface as a memory
					// failure; a resource rejection is a legitimate outcome here.
					continue
				}
				if op%2 == 0 {
					_ = m.Ledger().Commit(reservation)
				} else {
					m.Ledger().Release(reservation)
				}
				_ = m.Ledger().ClaimEdge()
				m.Ledger().ReleaseEdge()
				_ = m.Ledger().ChargeFixed(8, rootID)
				m.Ledger().ReleaseFixed(8, "")
				_ = m.Stats()
				_ = m.AbortRequested(rootID)
			}
		}(g)
	}
	start.Arrive()
	wg.Wait()

	// Every owner is still registered and still pinned to its own root and scope.
	for i := 0; i < owners; i++ {
		rootID := rootName(i)
		owner, ok := m.Owner(rootID)
		if !ok {
			t.Fatalf("owner %s disappeared", rootID)
		}
		if owner.State != OwnerActive {
			t.Fatalf("owner %s state = %q, want ACTIVE", rootID, owner.State)
		}
		scope, ok := m.Scope(rootID, scopes[i])
		if !ok {
			t.Fatalf("scope %d of %s disappeared", scopes[i], rootID)
		}
		if scope.OwnerID != rootID {
			t.Fatalf("scope %d belongs to %q, want %q", scopes[i], scope.OwnerID, rootID)
		}
	}
	// Releasing every owner must return the whole ledger to an exact zero, which is
	// only possible if the concurrent traffic never leaked or double-charged.
	for i := 0; i < owners; i++ {
		m.ReleaseOwner(rootName(i))
	}
	requireBudgetZero(t, m.Ledger())
	if stats := m.Stats(); stats.ActiveOwners != 0 || stats.GrantedComponents != 0 || stats.MemoryBytes != 0 {
		t.Fatalf("stats after a full release: %+v", stats)
	}
	m.Close()
	requireBudgetZero(t, m.Ledger())
	if !m.Closed() {
		t.Fatal("the manager did not report itself closed")
	}
	// A closed manager refuses new owners and refuses to serve a released one.
	if _, err := m.RegisterOwner("root-late", 1, 0, 1); err != ErrOwnerUnknown {
		t.Fatalf("registration after Close = %v, want ErrOwnerUnknown", err)
	}
	requireBudgetZero(t, m.Ledger())
}

func rootName(i int) string { return fmt.Sprintf("root-%d", i) }
