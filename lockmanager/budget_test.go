package lockmanager

import (
	"testing"
	"time"
)

// budget_test.go pins the exact boundaries of every lock budget and proves that a
// rejected request has no side effect. The rule under test is "an exact fit is
// allowed, one unit more is refused, and nothing is left behind either way": a lock
// budget must never be satisfied by silently releasing a grant or by degrading to a
// global transaction gate.

// TestB02MemoryBudgetExactAndPlusOne pins the memory boundary at the byte.
func TestB02MemoryBudgetExactAndPlusOne(t *testing.T) {
	component := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k")}}
	perComponent := int64(component.OwnedBytes())
	// A reservation charges the owner overhead once, the component bytes once and the
	// waiter overhead once.
	exact := ownerFixedBytes + perComponent + waiterFixedBytes
	ledger, err := NewLedger(BudgetLimits{MemoryBytes: exact, RootRequests: 8, Waiters: 8, GraphWork: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve("root-1", []ResourceRef{component}, true); err != nil {
		t.Fatalf("an exact fit must be allowed: %v", err)
	}
	requireLedgerMemory(t, ledger, exact)
	// One byte less must refuse, and the refusal must leave the accounting untouched.
	tight, err := NewLedger(BudgetLimits{MemoryBytes: exact - 1, RootRequests: 8, Waiters: 8, GraphWork: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tight.Reserve("root-1", []ResourceRef{component}, true); err != ErrBudgetMemory {
		t.Fatalf("one byte over = %v, want ErrBudgetMemory", err)
	}
	requireBudgetZero(t, tight)
	if tight.Rejections() != 1 {
		t.Fatalf("rejections = %d, want 1", tight.Rejections())
	}
}

// TestB02RootRequestBudgetExactAndPlusOne pins the per-root component boundary and
// proves the count is components, not bundles.
func TestB02RootRequestBudgetExactAndPlusOne(t *testing.T) {
	ledger, err := NewLedger(BudgetLimits{MemoryBytes: 1 << 20, RootRequests: 2, Waiters: 8, GraphWork: 8})
	if err != nil {
		t.Fatal(err)
	}
	first := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("a")}}
	second := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("b")}}
	third := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("c")}}

	// One bundle with exactly two components is an exact fit.
	exact, err := ledger.Reserve("root-1", []ResourceRef{first, second}, false)
	if err != nil {
		t.Fatalf("exact fit: %v", err)
	}
	if exact.Components() != 2 {
		t.Fatalf("reserved components = %d, want 2", exact.Components())
	}
	// A second bundle of one component is one over.
	if _, err := ledger.Reserve("root-1", []ResourceRef{third}, false); err != ErrBudgetRootRequests {
		t.Fatalf("one component over = %v, want ErrBudgetRootRequests", err)
	}
	// The refusal left the first reservation intact.
	if usage, _ := ledger.Usage("root-1"); usage.Components != 2 {
		t.Fatalf("components after a refusal = %d, want 2", usage.Components)
	}
	// Releasing the first bundle frees the budget again.
	ledger.Release(exact)
	if _, err := ledger.Reserve("root-1", []ResourceRef{third}, false); err != nil {
		t.Fatalf("reserve after release: %v", err)
	}
	ledger.ReleaseOwner("root-1")
	requireBudgetZero(t, ledger)

	// The count is components, so one bundle with four components is refused by a
	// budget of three even though it is a single request.
	small, err := NewLedger(BudgetLimits{MemoryBytes: 1 << 20, RootRequests: 3, Waiters: 8, GraphWork: 8})
	if err != nil {
		t.Fatal(err)
	}
	duplicated := make([]ResourceRef, 0, 4)
	for i := 0; i < 4; i++ {
		duplicated = append(duplicated, ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: []byte{byte('a' + i)}}})
	}
	if _, err := small.Reserve("root-1", duplicated, false); err != ErrBudgetRootRequests {
		t.Fatalf("four components under a three-component budget = %v", err)
	}
	requireBudgetZero(t, small)
}

// TestB02WaiterBudgetExactAndPlusOne pins the engine-wide pending-request boundary.
func TestB02WaiterBudgetExactAndPlusOne(t *testing.T) {
	ledger, err := NewLedger(BudgetLimits{MemoryBytes: 1 << 20, RootRequests: 64, Waiters: 2, GraphWork: 8})
	if err != nil {
		t.Fatal(err)
	}
	component := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k")}}
	first, err := ledger.Reserve("root-1", []ResourceRef{component}, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Reserve("root-2", []ResourceRef{component}, true)
	if err != nil {
		t.Fatalf("exact fit for the second waiter: %v", err)
	}
	if _, err := ledger.Reserve("root-3", []ResourceRef{component}, true); err != ErrBudgetWaiters {
		t.Fatalf("one waiter over = %v, want ErrBudgetWaiters", err)
	}
	if ledger.Waiters() != 2 {
		t.Fatalf("waiters = %d, want 2", ledger.Waiters())
	}
	// A non-waiting reservation is not charged against the waiter budget, which is
	// what keeps a grant from being refused because other owners are waiting.
	if _, err := ledger.Reserve("root-4", []ResourceRef{component}, false); err != nil {
		t.Fatalf("a non-waiter reservation must not consume the waiter budget: %v", err)
	}
	// Committing a waiter releases its waiter slot without releasing its lock.
	if err := ledger.Commit(first); err != nil {
		t.Fatal(err)
	}
	if ledger.Waiters() != 1 {
		t.Fatalf("waiters after commit = %d, want 1", ledger.Waiters())
	}
	if _, err := ledger.Reserve("root-5", []ResourceRef{component}, true); err != nil {
		t.Fatalf("the released waiter slot must be reusable: %v", err)
	}
	_ = second
	ledger.Release(second)
	ledger.ReleaseOwner("root-1")
	ledger.ReleaseOwner("root-4")
	ledger.ReleaseOwner("root-5")
	requireBudgetZero(t, ledger)
}

// TestB02GraphBudgetExactAndPlusOne pins the wait-edge boundary and the traversal
// work limit, which share one number.
func TestB02GraphBudgetExactAndPlusOne(t *testing.T) {
	ledger, err := NewLedger(BudgetLimits{MemoryBytes: 1 << 20, RootRequests: 8, Waiters: 8, GraphWork: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.ClaimEdge(); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ClaimEdge(); err != nil {
		t.Fatalf("exact fit for the second edge: %v", err)
	}
	if err := ledger.ClaimEdge(); err != ErrBudgetGraph {
		t.Fatalf("one edge over = %v, want ErrBudgetGraph", err)
	}
	if ledger.Edges() != 2 {
		t.Fatalf("edges = %d, want 2", ledger.Edges())
	}
	if ledger.Rejections() != 1 {
		t.Fatalf("rejections = %d, want 1", ledger.Rejections())
	}
	ledger.ReleaseEdge()
	ledger.ReleaseEdge()
	requireBudgetZero(t, ledger)
	// A repeated release must be a no-op, never a negative count.
	ledger.ReleaseEdge()
	ledger.ReleaseEdge()
	requireBudgetZero(t, ledger)
	if ledger.Edges() != 0 {
		t.Fatalf("edges went negative: %d", ledger.Edges())
	}
}

// TestB02RepeatedReleaseAndFailedReserveHaveNoSideEffect proves idempotence and the
// "failure leaves nothing behind" rule for every rejection path.
func TestB02RepeatedReleaseAndFailedReserveHaveNoSideEffect(t *testing.T) {
	component := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k")}}
	ledger, err := NewLedger(BudgetLimits{MemoryBytes: ownerFixedBytes + int64(component.OwnedBytes()) + waiterFixedBytes, RootRequests: 1, Waiters: 1, GraphWork: 1})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve("root-1", []ResourceRef{component}, true)
	if err != nil {
		t.Fatal(err)
	}
	before := ledger.Totals()

	// A second reservation fails on every budget at once and must change nothing.
	if _, err := ledger.Reserve("root-1", []ResourceRef{component}, true); err == nil {
		t.Fatal("a reservation over every budget must be refused")
	}
	if after := ledger.Totals(); after != before {
		t.Fatalf("a failed reservation changed the accounting: %+v -> %+v", before, after)
	}

	// Releasing twice refunds once, and the release also removes the last identity the
	// owner pinned, so the owner entry itself is pruned and every counter reaches zero.
	ledger.Release(reservation)
	ledger.Release(reservation)
	requireBudgetZero(t, ledger)

	// Committing after a release must not resurrect a charge.
	if err := ledger.Commit(reservation); err != nil {
		t.Fatalf("commit after release = %v, want nil", err)
	}
	requireBudgetZero(t, ledger)

	// Releasing an owner twice, and an owner that never had a charge, are no-ops too.
	ledger.ReleaseOwner("root-1")
	ledger.ReleaseOwner("root-1")
	ledger.ReleaseOwner("never-registered")
	requireBudgetZero(t, ledger)
}

// TestB02ReservationToGrantIsNotDoubleCharged proves the transition from a pending
// request to a grant adds only the one-time grant overhead: a duplicate charge would
// make a large transaction hit the memory budget at half its real size.
func TestB02ReservationToGrantIsNotDoubleCharged(t *testing.T) {
	component := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k")}}
	ledger, err := NewLedger(BudgetLimits{MemoryBytes: 1 << 20, RootRequests: 8, Waiters: 8, GraphWork: 8})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve("root-1", []ResourceRef{component}, true)
	if err != nil {
		t.Fatal(err)
	}
	pending := ledger.Totals().MemoryBytes
	wantPending := ownerFixedBytes + int64(component.OwnedBytes()) + waiterFixedBytes
	if pending != wantPending {
		t.Fatalf("pending memory = %d, want %d", pending, wantPending)
	}
	if err := ledger.Commit(reservation); err != nil {
		t.Fatal(err)
	}
	granted := ledger.Totals().MemoryBytes
	wantGranted := ownerFixedBytes + int64(component.OwnedBytes()) + grantFixedBytes
	if granted != wantGranted {
		t.Fatalf("granted memory = %d, want %d", granted, wantGranted)
	}
	// The transition must be a pure reclassification plus one grant overhead: the
	// resource bytes are charged once.
	if granted-pending != grantFixedBytes-waiterFixedBytes {
		t.Fatalf("transition delta = %d, want %d", granted-pending, grantFixedBytes-waiterFixedBytes)
	}
	// Releasing a committed reservation must not refund its resource bytes: the owner
	// still holds the grant.
	ledger.Release(reservation)
	if after := ledger.Totals().MemoryBytes; after != wantGranted {
		t.Fatalf("releasing a committed reservation changed memory to %d, want %d", after, wantGranted)
	}
	// The owner still owns the resource identity, which is what a later conflict check
	// reads.
	if resources := ledger.Resources("root-1"); len(resources) != 1 {
		t.Fatalf("owner resources = %d, want 1", len(resources))
	}
	ledger.ReleaseOwner("root-1")
	requireBudgetZero(t, ledger)
}

// TestB02LedgerTotalsAreDerivedFromTrackedState proves the ledger has exactly one
// source of truth: every mutation rebuilds the total from the tracked collections, so
// the budget check and the reported usage can never disagree. It also proves that a
// deliberately messy release sequence still reaches an exact zero.
func TestB02LedgerTotalsAreDerivedFromTrackedState(t *testing.T) {
	ledger, err := NewLedger(BudgetLimits{MemoryBytes: 1 << 20, RootRequests: 64, Waiters: 64, GraphWork: 64})
	if err != nil {
		t.Fatal(err)
	}
	var reservations []*Reservation
	for i := 0; i < 5; i++ {
		component := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte{byte('a' + i)}}}
		reservation, err := ledger.Reserve("root-1", []ResourceRef{component}, i%2 == 0)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := ledger.Commit(reservation); err != nil {
				t.Fatal(err)
			}
		}
		reservations = append(reservations, reservation)
	}
	for i := 0; i < 3; i++ {
		if err := ledger.ClaimEdge(); err != nil {
			t.Fatal(err)
		}
	}
	// A fixed owner charge must be counted exactly once, including the owner's own
	// entry overhead: double counting it would make a real budget smaller than the
	// configured one.
	beforeFixed := ledger.Totals().MemoryBytes
	if err := ledger.ChargeFixed(scopeFixedBytes, "root-1"); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Totals().MemoryBytes - beforeFixed; got != scopeFixedBytes {
		t.Fatalf("a fixed charge added %d bytes, want %d", got, scopeFixedBytes)
	}
	if _, owner := ledger.Usage("root-1"); owner.MemoryBytes != ledger.Totals().MemoryBytes {
		t.Fatalf("the owner view %d disagrees with the total %d", owner.MemoryBytes, ledger.Totals().MemoryBytes)
	}

	// A repeated identity is satisfied by the protection the owner already holds: it
	// holds no new state, so it is charged nothing and never waits.
	repeated := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: []byte("a")}}
	beforeRepeat := ledger.Totals().MemoryBytes
	repeatReservation, err := ledger.Reserve("root-1", []ResourceRef{repeated}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledger.Totals().MemoryBytes; got != beforeRepeat {
		t.Fatalf("a repeated identity changed memory from %d to %d", beforeRepeat, got)
	}
	if err := ledger.Commit(repeatReservation); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Totals().MemoryBytes; got != beforeRepeat {
		t.Fatalf("committing a satisfied request changed memory from %d to %d", beforeRepeat, got)
	}
	// The component budget still counted it, because it bounds requests rather than
	// distinct identities: the request is charged one component before the manager
	// decides it is already satisfied.
	if _, owner := ledger.Usage("root-1"); owner.Components != 5 {
		t.Fatalf("components after a repeated request = %d, want 5 distinct identities", owner.Components)
	}
	if resources := ledger.Resources("root-1"); len(resources) != 5 {
		t.Fatalf("distinct identities = %d, want 5", len(resources))
	}

	// A deliberately messy release: repeats, a committed reservation, an owner-wide
	// release and an explicit fixed refund must still reach an exact zero.
	for i := len(reservations) - 1; i >= 0; i-- {
		ledger.Release(reservations[i])
		ledger.Release(reservations[i])
	}
	ledger.Release(repeatReservation)
	ledger.ReleaseFixed(scopeFixedBytes, "root-1")
	for i := 0; i < 4; i++ {
		ledger.ReleaseEdge()
	}
	ledger.ReleaseOwner("root-1")
	ledger.ReleaseOwner("root-1")
	requireBudgetZero(t, ledger)
}

// TestB02BudgetLimitsResolution pins "0 means the documented default, never unlimited"
// and the legal ranges a YAML, environment or embedded option must obey.
func TestB02BudgetLimitsResolution(t *testing.T) {
	resolved, err := (BudgetLimits{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := BudgetLimits{
		MemoryBytes:  DefaultMemoryBytes,
		RootRequests: DefaultRootRequests,
		Waiters:      DefaultWaiters,
		GraphWork:    DefaultGraphWork,
	}
	if resolved != want {
		t.Fatalf("zero limits resolved to %+v, want %+v", resolved, want)
	}
	if want.RootRequests != 262144 {
		t.Fatalf("the calibrated per-root component default is %d, want 262144", want.RootRequests)
	}
	if want.MemoryBytes != 64<<20 {
		t.Fatalf("the memory default is %d, want 64 MiB", want.MemoryBytes)
	}

	for _, limits := range []BudgetLimits{
		{MemoryBytes: -1},
		{RootRequests: -1},
		{Waiters: -1},
		{GraphWork: -1},
		{MemoryBytes: MaxMemoryBytes + 1},
		{RootRequests: MaxRootRequests + 1},
	} {
		if _, err := limits.Resolve(); err != ErrInvalidOptions {
			t.Fatalf("%+v resolved to %v, want ErrInvalidOptions", limits, err)
		}
	}
	// A very small but legal budget is accepted: refusing it would make the limit
	// unimplementable rather than bounded.
	small, err := (BudgetLimits{MemoryBytes: 1, RootRequests: 1, Waiters: 1, GraphWork: 1}).Resolve()
	if err != nil {
		t.Fatalf("a small positive budget must be legal: %v", err)
	}
	if small.MemoryBytes != 1 {
		t.Fatalf("small budget = %+v", small)
	}
	ledger, err := NewLedger(BudgetLimits{MemoryBytes: 1, RootRequests: 1, Waiters: 1, GraphWork: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve("root-1", []ResourceRef{{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: []byte("k")}}}, false); err != ErrBudgetMemory {
		t.Fatalf("a one-byte budget must fail closed: %v", err)
	}
	requireBudgetZero(t, ledger)
}

// TestB02DiagnosticPageLimitClamping pins the 256-row page bound.
func TestB02DiagnosticPageLimitClamping(t *testing.T) {
	if limit, truncated := ResolvePageLimit(0); limit != MaxDiagnosticRows || truncated {
		t.Fatalf("limit 0 = (%d, %v), want the contract default", limit, truncated)
	}
	if limit, truncated := ResolvePageLimit(-5); limit != MaxDiagnosticRows || truncated {
		t.Fatalf("negative limit = (%d, %v)", limit, truncated)
	}
	if limit, truncated := ResolvePageLimit(10); limit != 10 || truncated {
		t.Fatalf("limit 10 = (%d, %v)", limit, truncated)
	}
	if limit, truncated := ResolvePageLimit(MaxDiagnosticRows); limit != MaxDiagnosticRows || truncated {
		t.Fatalf("exact limit = (%d, %v), want no truncation", limit, truncated)
	}
	if limit, truncated := ResolvePageLimit(MaxDiagnosticRows + 1); limit != MaxDiagnosticRows || !truncated {
		t.Fatalf("limit 257 = (%d, %v), want truncation reported", limit, truncated)
	}
}

// TestB02OwnerAndScopeChargesAreBounded proves registration and scope creation fail
// closed under a tiny budget instead of registering an unaccounted owner.
//
// The budget is derived from the exact charges: a registration costs one owner
// overhead and one fixed charge for the owner entry's own accounting block, and a scope
// costs one scope's fixed bytes.
func TestB02OwnerAndScopeChargesAreBounded(t *testing.T) {
	registration := int64(ownerFixedBytes) * 2
	limits := BudgetLimits{MemoryBytes: registration + scopeFixedBytes, RootRequests: 4, Waiters: 4, GraphWork: 4}
	m, _ := newTestManager(t, limits)
	owner, err := m.RegisterOwner("root-1", 1, 0, 7)
	if err != nil {
		t.Fatalf("registration must fit its own overhead: %v", err)
	}
	if got := m.Ledger().Totals().MemoryBytes; got != registration {
		t.Fatalf("registration charged %d bytes, want %d", got, registration)
	}
	if owner.Ordinal != 1 {
		t.Fatalf("first ordinal = %d, want 1", owner.Ordinal)
	}
	if _, err := m.OpenScope("root-1", "txn", 0, ScopeStatement, time.Time{}); err != nil {
		t.Fatalf("first scope must fit: %v", err)
	}
	if got := m.Ledger().Totals().MemoryBytes; got != limits.MemoryBytes {
		t.Fatalf("after one scope: %d bytes, want the exact budget %d", got, limits.MemoryBytes)
	}
	// The budget is now exactly consumed, so a second scope must fail and must not
	// leave a half-registered scope behind.
	if _, err := m.OpenScope("root-1", "txn", 0, ScopeStatement, time.Time{}); err != ErrBudgetMemory {
		t.Fatalf("second scope = %v, want ErrBudgetMemory", err)
	}
	if tree := m.scopes["root-1"]; tree == nil || tree.Len() != 1 || tree.OpenScopes() != 1 {
		t.Fatalf("a rejected scope left state behind: %+v", tree)
	}
	if got := m.Ledger().Totals().MemoryBytes; got != limits.MemoryBytes {
		t.Fatalf("a rejected scope changed memory to %d, want %d", got, limits.MemoryBytes)
	}
	// One byte less must refuse the very first scope, leaving only the registration.
	tight, _ := newTestManager(t, BudgetLimits{MemoryBytes: registration + scopeFixedBytes - 1, RootRequests: 4, Waiters: 4, GraphWork: 4})
	if _, err := tight.RegisterOwner("root-1", 1, 0, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := tight.OpenScope("root-1", "txn", 0, ScopeStatement, time.Time{}); err != ErrBudgetMemory {
		t.Fatalf("one byte under = %v, want ErrBudgetMemory", err)
	}
	// Releasing the owner returns everything to zero.
	m.ReleaseOwner("root-1")
	requireBudgetZero(t, m.ledger)
}

// requireLedgerMemory asserts the exact ledger memory.
func requireLedgerMemory(t *testing.T, ledger *Ledger, want int64) {
	t.Helper()
	if got := ledger.Totals().MemoryBytes; got != want {
		t.Fatalf("ledger memory = %d, want %d", got, want)
	}
}
