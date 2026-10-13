package lockmanager

import (
	"context"
	"errors"
	"testing"
	"time"
)

// epoch_owner_test.go tests Section 1 of Phase 7 review:
// RegisterOwner validates the caller's epoch identity (generation and term) in the same atomic zone.
func TestT054EpochOwnerRegistrationRejectsStaleTerm(t *testing.T) {
	const G uint64 = 1
	const N uint64 = 10
	m, err := NewManager(BudgetLimits{}, G, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// 1. Manager at Epoch (G, N)
	m.ResetEpoch(G, N)
	if m.Generation() != G || m.Term() != N {
		t.Fatalf("expected Epoch (%d, %d), got (%d, %d)", G, N, m.Generation(), m.Term())
	}

	// 2. Switch to Epoch (G, N+1)
	m.ResetEpoch(G, N+1)
	if m.Term() != N+1 {
		t.Fatalf("expected Term %d, got %d", N+1, m.Term())
	}

	// 3. Stale owner calls RegisterOwner with old Term N -> must fail with ErrLockInvalidated!
	staleOwner, err := m.RegisterOwner("stale-root", G, N, 100)
	if !errors.Is(err, ErrLockInvalidated) {
		t.Fatalf("expected ErrLockInvalidated for stale term owner registration, got: %v (owner: %+v)", err, staleOwner)
	}
	if staleOwner != nil {
		t.Fatal("expected nil owner on rejected registration")
	}

	// 4. Valid owner calls RegisterOwner with matching Term N+1 -> succeeds!
	validOwner, err := m.RegisterOwner("valid-root", G, N+1, 100)
	if err != nil {
		t.Fatalf("valid owner registration failed: %v", err)
	}
	if validOwner == nil {
		t.Fatal("expected valid owner, got nil")
	}

	// 5. Valid owner acquires Record X -> succeeds!
	ctx := context.Background()
	scope, err := m.OpenScope("valid-root", "valid-root-scope", 0, ScopeStatement, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	record := ResourceRef{
		Identity: Identity{
			Space:   "row/t1",
			TableID: "t1",
			Key:     []byte("k1"),
			Kind:    KindRecord,
			Mode:    ModeX,
		},
	}
	res, err := m.Acquire(ctx, "valid-root", scope.ID, rowBundle(record))
	if err != nil {
		t.Fatalf("valid owner Acquire failed: %v", err)
	}
	if res.Op != LockOpGranted {
		t.Fatalf("expected LockOpGranted, got %v", res.Op)
	}
	if res.Grant.Term != N+1 {
		t.Fatalf("expected grant.Term %d, got %d", N+1, res.Grant.Term)
	}
}

func TestT054EpochOwnerRegistrationStandaloneModeAcceptsZeroTerm(t *testing.T) {
	const G uint64 = 1
	m, err := NewManager(BudgetLimits{}, G, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if m.Term() != 0 {
		t.Fatalf("expected initial Term 0 in standalone mode, got %d", m.Term())
	}

	// In standalone mode, Term=0 is accepted
	owner, err := m.RegisterOwner("standalone-root", G, 0, 100)
	if err != nil {
		t.Fatalf("standalone registration failed: %v", err)
	}
	if owner == nil {
		t.Fatal("expected owner, got nil")
	}
}

func TestT054EpochOwnerRegistrationRejectsStaleGeneration(t *testing.T) {
	const G uint64 = 2
	const N uint64 = 5
	m, err := NewManager(BudgetLimits{}, G, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.ResetEpoch(G, N)

	// Caller with stale generation 1 must be rejected
	staleOwner, err := m.RegisterOwner("stale-gen", 1, N, 100)
	if !errors.Is(err, ErrLockInvalidated) {
		t.Fatalf("expected ErrLockInvalidated for stale generation registration, got: %v (owner: %+v)", err, staleOwner)
	}
	if staleOwner != nil {
		t.Fatal("expected nil owner on rejected registration")
	}
}
