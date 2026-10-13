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

// TestT054EpochOwnerRegistrationMatrix verifies Section A:
// Manager Epoch = (G=2, Term=11):
// - G=1, Term=11: rejected (stale generation)
// - G=2, Term=11: allowed (exact match)
// - G=3, Term=11: rejected (future generation)
// - G=0, Term=11: rejected (zero generation when active generation is 2)
// - G=2, Term=10: rejected (stale term)
// - G=2, Term=12: rejected (future term)
func TestT054EpochOwnerRegistrationMatrix(t *testing.T) {
	const G uint64 = 2
	const Term uint64 = 11

	m, err := NewManager(BudgetLimits{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	m.ResetEpoch(G, Term)
	if m.Generation() != G || m.Term() != Term {
		t.Fatalf("expected Epoch (%d, %d), got (%d, %d)", G, Term, m.Generation(), m.Term())
	}

	cases := []struct {
		name       string
		generation uint64
		term       uint64
		wantErr    bool
	}{
		{name: "stale-generation-1", generation: 1, term: 11, wantErr: true},
		{name: "exact-match-2-11", generation: 2, term: 11, wantErr: false},
		{name: "future-generation-3", generation: 3, term: 11, wantErr: true},
		{name: "zero-generation-0", generation: 0, term: 11, wantErr: true},
		{name: "stale-term-10", generation: 2, term: 10, wantErr: true},
		{name: "future-term-12", generation: 2, term: 12, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, err := m.RegisterOwner(c.name, c.generation, c.term, 100)
			if c.wantErr {
				if !errors.Is(err, ErrLockInvalidated) {
					t.Fatalf("case %s: expected ErrLockInvalidated, got: %v (owner=%+v)", c.name, err, owner)
				}
				if owner != nil {
					t.Fatalf("case %s: expected nil owner, got %+v", c.name, owner)
				}
			} else {
				if err != nil {
					t.Fatalf("case %s: unexpected error: %v", c.name, err)
				}
				if owner == nil {
					t.Fatalf("case %s: expected valid owner, got nil", c.name)
				}
			}
		})
	}
}
