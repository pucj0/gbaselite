package mvccadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking_internal_test.go is T018's residue and identity half.
//
// The neutral, black-box tests in locking_engine_test.go can prove what a caller sees:
// which owner a grant names, which scope it came from, and that two engines do not block
// each other. They cannot prove what a finished transaction *leaves behind*, because the
// lock manager is deliberately not part of the neutral surface: no storageengine call
// reports an owner, a grant, a wait edge or a ledger charge after the transaction ended.
// These tests therefore reach the engine's own manager, exactly as
// diagnostics_internal_test.go reaches the engine's own transaction registry, and assert
// the state the adapter is responsible for keeping consistent with the MVCC lifecycle.
//
// The property under test is ownership and residue: one manager per engine, a child that
// takes locks for its root and releases nothing itself, and no owner, grant, edge, waiter
// or accounting charge that outlives the root that caused it.

// t018Space is the namespace every internal T018 test locks in. It is intentionally not
// the rowSpace of the black-box tests: these tests reason about one engine's own manager,
// so a distinct namespace keeps a failure readable on its own.
const t018Space = "rows/t018-locking-engine"

// t018OpenEngine opens one MVCC-backed adapter engine and hands back the concrete type,
// because every assertion here has to read the engine's own lock manager. Close is
// idempotent, so the cleanup also covers a test that closed the engine itself.
func t018OpenEngine(t *testing.T) *engine {
	t.Helper()
	opened, err := Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := opened.(*engine)
	t.Cleanup(func() { e.Close() })
	return e
}

// t018Begin starts one root transaction and ends it at cleanup. A committed or aborted
// transaction is ended a second time harmlessly, so cleanup never depends on which path
// the test took.
func t018Begin(t *testing.T, e *engine) *txn {
	t.Helper()
	tx, err := e.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx.(*txn)
}

// t018RowComponents is the bundle one row lock uses: the exclusive record plus the table
// intention the bundle-level prerequisite requires. Both components carry the same table
// label, so the bundle is a legal B02 request rather than a bare record.
func t018RowComponents(key string) []storageengine.LockComponent {
	return []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: t018Space, TableID: "t018"},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t018Space, TableID: "t018", Key: []byte(key)},
	}
}

// t018RowRecord is the same record identity as a lock-manager reference, so a test can ask
// the manager *directly* what one owner holds on it. It mirrors the component the adapter
// builds, which is what makes a direct HeldMode probe meaningful rather than a guess about
// the adapter's encoding.
func t018RowRecord(key string) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind: lockmanager.KindRecord, Mode: lockmanager.ModeX, Space: t018Space, TableID: "t018", Key: []byte(key),
	}}
}

// t018TableIntent is the table intention that belongs with t018RowRecord in one bundle.
func t018TableIntent() lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind: lockmanager.KindTableIntent, Mode: lockmanager.ModeIX, Space: t018Space, TableID: "t018",
	}}
}

// t018Acquire takes one row lock through the transaction's neutral capability, opening a
// statement scope for it, and returns the grant.
func t018Acquire(t *testing.T, ctx context.Context, tx storageengine.Txn, key string) storageengine.LockGrant {
	t.Helper()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("locking capability: %v", err)
	}
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatalf("begin scope: %v", err)
	}
	grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{Components: t018RowComponents(key)})
	if err != nil {
		t.Fatalf("acquire %q: %v", key, err)
	}
	return grant
}

// t018RequireNoLockOwnership asserts the lock manager holds no owner, grant, resource, wait
// edge or waiter. It is the ownership half of the residue contract, and the half that must
// hold the instant a root ends or a generation is replaced: a resource that still has a
// holder or a waiter would block the next writer even though nothing owns it any more.
func t018RequireNoLockOwnership(t *testing.T, e *engine) {
	t.Helper()
	if stats := e.locks.Stats(); stats.ActiveOwners != 0 || stats.GrantedComponents != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 {
		t.Fatalf("the lock manager still owns state after every owner ended: %+v", stats)
	}
	if size := e.locks.LockTableSize(); size != 0 {
		t.Fatalf("the lock table still holds %d resources", size)
	}
	if edges := e.locks.WaitEdges(); len(edges) != 0 {
		t.Fatalf("the wait graph still has %d edges: %v", len(edges), edges)
	}
	if resources := e.locks.Snapshot(); len(resources) != 0 {
		t.Fatalf("the lock table still snapshots %d resources: %+v", len(resources), resources)
	}
}

// t018RequireNoResidue asserts the whole lock manager returned to its empty state.
//
// Two independent views are checked -- the ownership view above and the accounting ledger --
// because each answers a different question: ownership can be gone while a ledger charge
// survives, and a charge that outlives its owner is memory that no later transaction can
// ever release through the owner registry.
func t018RequireNoResidue(t *testing.T, e *engine) {
	t.Helper()
	t018RequireNoLockOwnership(t, e)
	if stats := e.locks.Stats(); stats.MemoryBytes != 0 {
		t.Fatalf("the lock manager kept %d bytes of metadata after every owner ended", stats.MemoryBytes)
	}
	ledger := e.locks.Ledger()
	if totals := ledger.Totals(); totals != (lockmanager.BudgetUsage{}) {
		t.Fatalf("the ledger did not return to zero: %+v", totals)
	}
	if grants := ledger.Grants(); grants != 0 {
		t.Fatalf("the ledger still counts %d grants", grants)
	}
	if waiters := ledger.Waiters(); waiters != 0 {
		t.Fatalf("the ledger still counts %d waiters", waiters)
	}
	if edges := ledger.Edges(); edges != 0 {
		t.Fatalf("the ledger still counts %d wait graph edges", edges)
	}
}

// TestT018LockCapabilityIsAbsentOnAnEngineWithoutALockManager pins the optional-capability
// contract at the adapter boundary.
//
// Open always attaches one manager, so the "no lock manager" configuration is only
// reachable by building the engine directly -- which is exactly what a future option that
// disables locking, or a partially initialised engine, would look like. The property that
// matters is fail-closed: a transaction of such an engine must never hand out a capability
// that silently takes no lock.
func TestT018LockCapabilityIsAbsentOnAnEngineWithoutALockManager(t *testing.T) {
	ctx := context.Background()
	raw := openRaw(t)
	e := &engine{store: raw, proposer: raw}

	tx, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	adapterTxn := tx.(*txn)
	// The adapter's own accessor is the boolean every locking path consults, so absence is
	// reported here rather than inferred from an empty result.
	if capability, ok := adapterTxn.LockingTxn(); ok || capability != nil {
		t.Fatalf("an engine without a lock manager advertised the capability: %v", capability)
	}
	// The neutral probe cannot see the absent manager: the transaction *type* implements the
	// interface unconditionally, so the type assertion succeeds even though no manager was
	// ever attached. What the contract actually promises -- and what every caller depends
	// on -- is asserted next: using the capability refuses instead of running unprotected.
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("LockingTxnOf on an adapter transaction = %v; the adapter's type implements the interface even with no manager", err)
	}
	if _, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BeginLockScope without a lock manager = %v, want ErrUnsupported", err)
	}
	if _, err := locking.Acquire(ctx, 1, storageengine.LockRequest{Components: t018RowComponents("k1")}); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("Acquire without a lock manager = %v, want ErrUnsupported", err)
	}
	if _, err := locking.ReadCurrent(ctx, storageengine.LockGrant{}, t018Space, []byte("k1")); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("ReadCurrent without a lock manager = %v, want ErrUnsupported", err)
	}
	if _, err := locking.NewCurrentIterator(ctx, 1, storageengine.CurrentScanRequest{Space: t018Space}); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("NewCurrentIterator without a lock manager = %v, want ErrUnsupported", err)
	}
	if err := locking.CloseLockScope(ctx, 1); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("CloseLockScope without a lock manager = %v, want ErrUnsupported", err)
	}
}

// TestT018EachEngineOwnsItsOwnLockManager pins the per-engine binding.
//
// Two engines serve two databases, so they must not share one lock domain: a shared manager
// would block one engine's committed write behind another engine's uncommitted lock, and
// mutual exclusion between unrelated databases is a liveness failure, not a safety gain.
// The MVCC-level refusal to attach a second manager to one store is covered by
// mvcc/locking_lifecycle_test.go, so it is not repeated here; what is asserted is the
// adapter-level consequence -- Open builds a distinct manager per engine, and both managers
// can hold the same identity at the same time without either one waiting.
func TestT018EachEngineOwnsItsOwnLockManager(t *testing.T) {
	ctx := context.Background()
	a := t018OpenEngine(t)
	b := t018OpenEngine(t)
	if a.locks == nil || b.locks == nil {
		t.Fatal("an opened engine has no lock manager")
	}
	if a.locks == b.locks {
		t.Fatal("two engines share one lock manager")
	}

	rootA := t018Begin(t, a)
	t018Acquire(t, ctx, rootA, "shared")
	rootB := t018Begin(t, b)
	// B's acquisition uses a bounded context. If the two engines shared one manager, this
	// call would queue behind A's exclusive record and the deadline would expire, so the
	// failure is reported as a failed acquisition instead of as a hung test.
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	grantB := t018Acquire(t, bounded, rootB, "shared")
	if grantB.OwnerID != rootB.ID() {
		t.Fatalf("grant owner = %q, want engine B's root %q", grantB.OwnerID, rootB.ID())
	}
	if mode, ok := a.locks.HeldMode(rootA.ID(), t018RowRecord("shared")); !ok || mode != lockmanager.ModeX {
		t.Fatalf("engine A lost its own claim: mode %q, held %v", mode, ok)
	}
	if mode, ok := b.locks.HeldMode(rootB.ID(), t018RowRecord("shared")); !ok || mode != lockmanager.ModeX {
		t.Fatalf("engine B does not hold the same identity in its own manager: mode %q, held %v", mode, ok)
	}
	if mode, ok := b.locks.HeldMode(rootA.ID(), t018RowRecord("shared")); ok {
		t.Fatalf("engine B knows engine A's owner: it holds mode %q", mode)
	}
}

// TestT018ChildGrantUsesTheRootOwnerAndTheChildsOwnScope pins the identity mapping the SQL
// layer relies on when it locks a row through a child or savepoint layer.
//
// A child is a request origin, never a lock owner. If the adapter bound a child's grant to
// the child, the child's merge would have to transfer the grant, and a child rollback would
// have to release a lock the root still needs -- both are exactly the lost-update shapes the
// ownership rule exists to prevent. The scope chain is asserted through the manager's own
// scope tree, because that is the state diagnostics and the read layer actually read.
func TestT018ChildGrantUsesTheRootOwnerAndTheChildsOwnScope(t *testing.T) {
	ctx := context.Background()
	e := t018OpenEngine(t)
	root := t018Begin(t, e)
	child, err := root.Child()
	if err != nil {
		t.Fatal(err)
	}

	// The child opens its own request scope and acquires through it.
	childLocking, err := storageengine.LockingTxnOf(child)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := childLocking.BeginLockScope(ctx, storageengine.LockScopeChild)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := childLocking.Acquire(ctx, scope, storageengine.LockRequest{Components: t018RowComponents("k1")})
	if err != nil {
		t.Fatal(err)
	}
	if grant.OwnerID != root.ID() {
		t.Fatalf("grant owner = %q, want the ROOT transaction %q (the child is %q)", grant.OwnerID, root.ID(), child.ID())
	}
	if grant.ScopeID != scope {
		t.Fatalf("grant scope = %d, want the requesting scope %d", grant.ScopeID, scope)
	}

	// The child has no owner of its own, and the root's owner is the one that carries the grant.
	if _, ok := e.locks.Owner(child.ID()); ok {
		t.Fatal("a child transaction registered its own lock owner")
	}
	owner, ok := e.locks.Owner(root.ID())
	if !ok {
		t.Fatal("the root's lock owner is missing")
	}
	// The generation/term pair is not decoration: a locked current read compares it against the
	// owner's registration, so a grant that reported another pair would authorise nothing.
	if grant.Generation != owner.Generation || grant.Term != owner.Term {
		t.Fatalf("grant generation/term = %d/%d, want the owner's %d/%d", grant.Generation, grant.Term, owner.Generation, owner.Term)
	}

	// The scope the child acquired through is a child scope of the ROOT's owner, nested inside
	// the child's own transaction scope. The returned scope id is the one the grant names.
	openedScope, ok := e.locks.Scope(root.ID(), scope)
	if !ok {
		t.Fatal("the scope the child acquired through is not registered on the root owner")
	}
	if openedScope.OwnerID != root.ID() || openedScope.OriginTxnID != child.ID() {
		t.Fatalf("opened scope = %+v, want owner %q and origin %q", openedScope, root.ID(), child.ID())
	}
	if openedScope.Kind != lockmanager.ScopeChild {
		t.Fatalf("opened scope kind = %q, want %q", openedScope.Kind, lockmanager.ScopeChild)
	}
	if openedScope.ParentScopeID == 0 {
		t.Fatal("a scope opened on a live child has no parent scope")
	}
	// A child's own transaction scope is nested inside the scope its parent transaction opened. The
	// scope tree mirrors the savepoint nesting, so discarding an enclosing layer closes the child's
	// scope with it and cancels the requests that scope issued; ownership — not parentage — is what
	// ties a child's grant to the root, but parentage is what makes an enclosing rollback reach the
	// child's requests at all.
	childOwnScope, ok := e.locks.Scope(root.ID(), openedScope.ParentScopeID)
	if !ok {
		t.Fatal("the child's own transaction scope is not registered on the root owner")
	}
	if childOwnScope.OwnerID != root.ID() || childOwnScope.OriginTxnID != child.ID() || childOwnScope.Kind != lockmanager.ScopeChild {
		t.Fatalf("child transaction scope = %+v, want a child scope of owner %q with origin %q", childOwnScope, root.ID(), child.ID())
	}
	if childOwnScope.ParentScopeID == 0 {
		t.Fatal("a child's own transaction scope has no parent; an enclosing savepoint rollback could not reach it")
	}
	// The root's own scope is a sibling in that same tree, not the child's parent: opening one
	// more scope on the root shows the identical shape (origin = the root, parent = the root's
	// own transaction scope), which is why parentage alone could never bind a child to its root.
	rootLocking, err := storageengine.LockingTxnOf(root)
	if err != nil {
		t.Fatal(err)
	}
	rootExtra, err := rootLocking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	rootExtraScope, ok := e.locks.Scope(root.ID(), rootExtra)
	if !ok {
		t.Fatal("the scope opened on the root is not registered on the root owner")
	}
	rootOwnScope, ok := e.locks.Scope(root.ID(), rootExtraScope.ParentScopeID)
	if !ok {
		t.Fatal("the root's own transaction scope is not registered on the root owner")
	}
	if rootOwnScope.OriginTxnID != root.ID() || rootOwnScope.Kind != lockmanager.ScopeStatement || rootOwnScope.ParentScopeID != 0 {
		t.Fatalf("root transaction scope = %+v, want a parentless statement scope of %q", rootOwnScope, root.ID())
	}
	// The child's own scope hangs directly off the root's own scope: one tree, mirroring the
	// transaction nesting, so closing the root scope closes every scope below it.
	if childOwnScope.ParentScopeID != rootOwnScope.ID {
		t.Fatalf("the child's scope is parented to %d, want the root's own scope %d", childOwnScope.ParentScopeID, rootOwnScope.ID)
	}
	if rootOwnScope.ID == childOwnScope.ID {
		t.Fatalf("the root and the child share transaction scope %d", rootOwnScope.ID)
	}

	// The child's merge ends its scope and releases nothing: the root still holds the row.
	if _, err := child.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if mode, held := e.locks.HeldMode(root.ID(), t018RowRecord("k1")); !held || mode != lockmanager.ModeX {
		t.Fatalf("the child commit released the root's record: mode %q, held %v", mode, held)
	}
	if _, ok := e.locks.Owner(root.ID()); !ok {
		t.Fatal("the child commit unregistered the root's lock owner")
	}
	closed, ok := e.locks.Scope(root.ID(), scope)
	if !ok {
		t.Fatal("the closed scope lost its identity; a historical origin scope must stay nameable")
	}
	if closed.State != lockmanager.ScopeClosed {
		t.Fatalf("scope state after the child commit = %q, want %q", closed.State, lockmanager.ScopeClosed)
	}

	// Only the root's own end releases the grant.
	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// TestT018NoLockResidueAfterRootRollback pins the abort path.
//
// A rolled-back root must leave no owner, no grant, no wait edge and no ledger charge: the
// row it locked has to become lockable again immediately, or a long-lived store would
// accumulate locked rows for transactions that no longer exist.
func TestT018NoLockResidueAfterRootRollback(t *testing.T) {
	ctx := context.Background()
	e := t018OpenEngine(t)
	root := t018Begin(t, e)
	t018Acquire(t, ctx, root, "k1")
	// Without this the test could pass on an engine that never locked anything at all.
	if stats := e.locks.Stats(); stats.ActiveOwners == 0 || stats.GrantedComponents == 0 {
		t.Fatalf("the acquisition left no trace, so the residue assertion would be vacuous: %+v", stats)
	}
	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// TestT018NoLockResidueAfterRootCommit pins the durable path.
//
// A commit is the other way a root ends, and it must release exactly like an abort: the lock
// protects the write while the transaction is undecided, not after it is published, so a
// grant kept past publication would block the next writer forever.
func TestT018NoLockResidueAfterRootCommit(t *testing.T) {
	ctx := context.Background()
	e := t018OpenEngine(t)
	root := t018Begin(t, e)
	t018Acquire(t, ctx, root, "k1")
	if stats := e.locks.Stats(); stats.ActiveOwners == 0 || stats.GrantedComponents == 0 {
		t.Fatalf("the acquisition left no trace, so the residue assertion would be vacuous: %+v", stats)
	}
	if _, err := root.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// TestT018EpochChangeWakesTheWaiterAndLeavesNothingBehind pins the generation-change path
// that a restore or a leader-term change drives.
//
// The danger is a waiter left parked on a handle whose owner belongs to a generation that no
// longer exists: the goroutine never returns, and the resource stays blocked for the new
// database. The test therefore establishes the wait as a fact (the request is queued before
// the reset), performs the reset through the same store entry point Store.Restore uses, and
// then asserts both halves: the waiter is woken with a definite neutral error, and nothing of
// the replaced generation survives.
func TestT018EpochChangeWakesTheWaiterAndLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	e := t018OpenEngine(t)
	blocker := t018Begin(t, e)
	blockerGrant := t018Acquire(t, ctx, blocker, "k1")
	if blockerGrant.ID == 0 {
		t.Fatal("the blocker holds no grant, so no conflict could be created")
	}

	waiter := t018Begin(t, e)
	waiterLocking, err := storageengine.LockingTxnOf(waiter)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := waiterLocking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	queued := make(chan struct{}, 1)
	type outcome struct {
		grant storageengine.LockGrant
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		grant, acquireErr := waiterLocking.Acquire(ctx, scope, storageengine.LockRequest{
			Components: t018RowComponents("k1"),
			// The decision runs while the manager holds its critical section and before the
			// request is queued, so a signal from here proves the conflict was reached. The
			// queue entry itself is written before the manager releases its lock, which is why
			// the main goroutine cannot observe the signal and then reset the epoch first.
			ShouldWait: func(context.Context) bool {
				select {
				case queued <- struct{}{}:
				default:
				}
				return true
			},
		})
		done <- outcome{grant: grant, err: acquireErr}
	}()

	select {
	case <-queued:
	case <-time.After(10 * time.Second):
		t.Fatal("the second acquisition never conflicted, so no waiter was exercised")
	}
	if pending := e.locks.PendingRequests(waiter.ID()); pending != 1 {
		t.Fatalf("pending requests for the waiter = %d, want exactly 1", pending)
	}
	if edges := e.locks.WaitEdges(); len(edges) == 0 {
		t.Fatal("the waiter produced no wait edge, so the wait graph does not know it is blocked")
	}

	// ResetLockEpoch is the entry point Store.Restore calls after bumping the generation, so
	// this drives the real generation-change path rather than a test-only shortcut. The
	// generation counter itself is unexported in mvcc and not reachable from this package;
	// the reset does not depend on its value, because it ends every live owner, so the effect
	// under test -- invalidate, wake, drop -- is identical.
	if invalidated := e.store.ResetLockEpoch(7); invalidated != 2 {
		t.Fatalf("invalidated owners = %d, want both live roots", invalidated)
	}

	select {
	case out := <-done:
		if !errors.Is(out.err, storageengine.ErrLockInvalidated) {
			t.Fatalf("the woken waiter reported %v, want storageengine.ErrLockInvalidated", out.err)
		}
		if out.grant.ID != 0 || out.grant.OwnerID != "" {
			t.Fatalf("an invalidated wait returned a usable grant: %+v", out.grant)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter was never woken by the epoch change")
	}
	if stats := e.locks.Stats(); stats.EpochInvalidations != 1 {
		t.Fatalf("epoch invalidations = %d, want 1 for one reset event", stats.EpochInvalidations)
	}
	// The owner that was invalidated must not authorise the grant it handed out before, and no
	// owner, grant, resource, edge or waiter of the replaced generation may survive it.
	if mode, held := e.locks.HeldMode(blocker.ID(), t018RowRecord("k1")); held {
		t.Fatalf("a replaced generation still holds mode %q on the row", mode)
	}
	t018RequireNoLockOwnership(t, e)
	// The accounting is a separate question from ownership, so it is asserted at the point the
	// contract fixes it: once the stale handles end. A commit or a rollback of a transaction
	// whose generation was replaced still reaches the cleanup path, and that path must refund
	// the owner's ledger debit. Splitting the helper keeps a failure readable -- "the row is
	// still blocked" and "a charge leaked" are different defects with different owners.
	if err := waiter.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// TestT018ClosedEngineReleasesLocksAndRefusesReuse pins engine shutdown.
//
// Close ends both the store and the lock manager. If the manager stayed usable, a transaction
// begun afterwards (or a stale handle) could register an owner on a manager whose store is
// gone and take a lock nothing would ever release. The observable half of "the manager is
// closed" is that it refuses a late registration, reports no state and says so itself.
func TestT018ClosedEngineReleasesLocksAndRefusesReuse(t *testing.T) {
	ctx := context.Background()
	e := t018OpenEngine(t)
	root := t018Begin(t, e)
	t018Acquire(t, ctx, root, "k1")

	if e.locks.Closed() {
		t.Fatal("the manager reports itself closed before the engine was closed")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if !e.locks.Closed() {
		t.Fatal("the engine closed but its lock manager does not report itself closed")
	}
	if _, err := e.locks.RegisterOwner("t018-late-owner", 0, 0, 0); !errors.Is(err, lockmanager.ErrOwnerUnknown) {
		t.Fatalf("registration on a closed manager = %v, want lockmanager.ErrOwnerUnknown", err)
	}
	if stats := e.locks.Stats(); stats.ActiveOwners != 0 || stats.GrantedComponents != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 {
		t.Fatalf("a closed manager still reports lock state: %+v", stats)
	}
	if size := e.locks.LockTableSize(); size != 0 {
		t.Fatalf("a closed manager still holds %d resources", size)
	}
	// Nothing can reuse the closed manager: a new transaction on the closed engine fails, so
	// no caller can ever reach the manager through this engine again.
	if _, err := e.Begin(ctx); err == nil {
		t.Fatal("a closed engine began a new transaction, so its closed lock manager stayed reachable")
	}
}

// TestT018ChildReentryAndChildEndDoNotDisturbTheRootsGrant pins the no-deadlock lifecycle.
//
// The two failure modes are symmetric: a child re-acquiring a component its root already
// holds must be answered as re-entrant rather than made to wait for its own owner (which
// would deadlock the single goroutine that owns the transaction), and a child ending must
// release nothing, because the root's write set still relies on the lock. Both are asserted
// against the manager's own accounting, so a "no new claim" answer cannot be faked by an
// implementation that silently took a second claim and dropped it later.
func TestT018ChildReentryAndChildEndDoNotDisturbTheRootsGrant(t *testing.T) {
	ctx := context.Background()
	e := t018OpenEngine(t)
	root := t018Begin(t, e)
	t018Acquire(t, ctx, root, "k1")
	grantsBefore := e.locks.Ledger().Grants()
	if grantsBefore == 0 {
		t.Fatal("the root's acquisition recorded no grant")
	}
	if mode, held := e.locks.HeldMode(root.ID(), t018RowRecord("k1")); !held || mode != lockmanager.ModeX {
		t.Fatalf("the root does not hold its record: mode %q, held %v", mode, held)
	}

	child, err := root.Child()
	if err != nil {
		t.Fatal(err)
	}
	childLocking, err := storageengine.LockingTxnOf(child)
	if err != nil {
		t.Fatal(err)
	}
	childScope, err := childLocking.BeginLockScope(ctx, storageengine.LockScopeChild)
	if err != nil {
		t.Fatal(err)
	}
	// A bounded context makes a self-conflict a reported failure rather than a 50-second wait:
	// the request would have to queue behind its own owner, which never releases while the
	// calling goroutine is the one waiting.
	reentrant, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	childGrant, err := childLocking.Acquire(reentrant, childScope, storageengine.LockRequest{Components: t018RowComponents("k1")})
	if err != nil {
		t.Fatalf("a child re-acquiring the root's own component did not re-enter: %v", err)
	}
	if childGrant.OwnerID != root.ID() {
		t.Fatalf("the re-entrant grant names %q, want the root %q", childGrant.OwnerID, root.ID())
	}
	if after := e.locks.Ledger().Grants(); after != grantsBefore {
		t.Fatalf("a re-entrant acquisition took %d new grants", after-grantsBefore)
	}
	// The manager's own answer is the operation, which the neutral DTO deliberately hides: a
	// re-entrant request reports the owner's existing protection instead of a fresh grant.
	result, err := e.locks.AcquireWith(ctx, root.ID(), childScope, []lockmanager.ResourceRef{t018RowRecord("k1"), t018TableIntent()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Op != lockmanager.LockOpReentrant {
		t.Fatalf("re-acquiring a held component reported op %q, want %q", result.Op, lockmanager.LockOpReentrant)
	}

	// A child merge releases nothing: the root still holds the row and the accounting is
	// unchanged.
	if _, err := child.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if mode, held := e.locks.HeldMode(root.ID(), t018RowRecord("k1")); !held || mode != lockmanager.ModeX {
		t.Fatalf("the child commit disturbed the root's record: mode %q, held %v", mode, held)
	}
	if after := e.locks.Ledger().Grants(); after != grantsBefore {
		t.Fatalf("the child commit changed the grant count from %d to %d", grantsBefore, after)
	}

	// A child rollback releases nothing either.
	second, err := root.Child()
	if err != nil {
		t.Fatal(err)
	}
	secondLocking, err := storageengine.LockingTxnOf(second)
	if err != nil {
		t.Fatal(err)
	}
	secondScope, err := secondLocking.BeginLockScope(ctx, storageengine.LockScopeChild)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondLocking.Acquire(ctx, secondScope, storageengine.LockRequest{Components: t018RowComponents("k1")}); err != nil {
		t.Fatal(err)
	}
	if err := second.Rollback(); err != nil {
		t.Fatal(err)
	}
	if mode, held := e.locks.HeldMode(root.ID(), t018RowRecord("k1")); !held || mode != lockmanager.ModeX {
		t.Fatalf("the child rollback released the root's record: mode %q, held %v", mode, held)
	}
	if after := e.locks.Ledger().Grants(); after != grantsBefore {
		t.Fatalf("the child rollback changed the grant count from %d to %d", grantsBefore, after)
	}

	// The root's own end is the only release point.
	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}
