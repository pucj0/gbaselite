package mvccadapter_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_engine_test.go is the black-box half of T018: everything asserted here is
// asserted through the neutral storageengine surface only, exactly as the SQL layer uses it.
//
// The property under test is the binding between one engine and one lock domain, and the
// identity that binding exposes: every engine gets its own manager, a grant is owned by the
// root transaction that caused it, a component travels to the manager and back without being
// rewritten, and a waited grant is a complete, stable capability rather than a token that
// happens to have an owner.

// t018RowComponents is the bundle one row lock uses in rowSpace. It is deliberately
// identical to what acquireRows builds, so a competing transaction in these tests asks for
// the same resource and a conflict assertion means what it says.
func t018RowComponents(key string) []storageengine.LockComponent {
	return []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: rowSpace, TableID: "t1"},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: rowSpace, TableID: "t1", Key: []byte(key)},
	}
}

// t018AcquireNoWait asks for one row lock and refuses to queue, so a conflict is reported
// immediately. It is how a test proves a lock is still held without blocking on it.
func t018AcquireNoWait(t *testing.T, ctx context.Context, tx storageengine.Txn, key string) (storageengine.LockGrant, error) {
	t.Helper()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("locking capability: %v", err)
	}
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatalf("begin scope: %v", err)
	}
	return locking.Acquire(ctx, scope, storageengine.LockRequest{
		Components: t018RowComponents(key),
		ShouldWait: func(context.Context) bool { return false },
	})
}

// t018ComponentShape renders every field of one neutral component, byte slices included, so
// two component lists can be compared as exact sets rather than by position. The manager is
// free to reorder a bundle (it sorts by canonical resource order), and a comparison that
// depended on order would fail for a reason that has nothing to do with pass-through.
func t018ComponentShape(component storageengine.LockComponent) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%x|%x|%x|%t|%t",
		component.Mode, component.Kind, component.Space, component.TableID, component.IndexID,
		component.Key, component.Lower, component.Upper, component.LowerInclusive, component.UpperInclusive)
}

// t018RequireSameComponents asserts the granted list is exactly the requested set: same
// count, and every requested shape present the same number of times.
func t018RequireSameComponents(t *testing.T, got, want []storageengine.LockComponent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("the grant covers %d components, the request named %d: got %v want %v", len(got), len(want), got, want)
	}
	counts := make(map[string]int, len(got))
	for _, component := range got {
		counts[t018ComponentShape(component)]++
	}
	for _, component := range want {
		shape := t018ComponentShape(component)
		if counts[shape] == 0 {
			t.Fatalf("the grant does not carry the requested component %s; granted %v", shape, got)
		}
		counts[shape]--
	}
}

// TestB02LockingCapabilityIsPresentOnAnAdapterTxn pins the positive half of the optional
// capability: an MVCC-backed transaction must advertise the lock surface, because the SQL
// layer's fail-closed path is "the capability is absent", and a backend that locks must not
// send every locking statement down that path.
func TestB02LockingCapabilityIsPresentOnAnAdapterTxn(t *testing.T) {
	ctx := context.Background()
	e := openAdapter(t, t.TempDir())
	tx := begin(t, e)

	if _, ok := tx.(storageengine.LockingTxn); !ok {
		t.Fatal("an MVCC-backed transaction does not implement the neutral locking surface")
	}
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("LockingTxnOf on an MVCC-backed transaction = %v, want the capability", err)
	}
	if locking == nil {
		t.Fatal("LockingTxnOf returned a nil capability without an error")
	}
	grant := acquireRows(t, ctx, tx, "k1")
	// The capability is not merely present: it names the transaction that caused the grant as
	// the owner, which is what a later current read re-validates.
	if grant.ID == 0 {
		t.Fatal("the neutral grant carries no id")
	}
	if grant.OwnerID != tx.ID() {
		t.Fatalf("grant owner = %q, want the root transaction %q", grant.OwnerID, tx.ID())
	}
}

// TestB02LockManagersAreNotSharedBetweenEngines pins the one-manager-per-engine binding.
//
// Two engines serve two databases; a shared lock domain would make one database's committed
// write wait for another database's uncommitted transaction. The test keeps two roots holding
// the SAME identity at the same time: with a shared manager the second acquisition would have
// to queue behind the first exclusive record, and the bounded context turns that into a
// reported failure instead of a hung test.
func TestB02LockManagersAreNotSharedBetweenEngines(t *testing.T) {
	ctx := context.Background()
	a := openAdapter(t, t.TempDir())
	b := openAdapter(t, t.TempDir())

	rootA := begin(t, a)
	if grantA := acquireRows(t, ctx, rootA, "shared"); grantA.ID == 0 {
		t.Fatal("the first engine's acquisition returned no grant")
	}

	rootB := begin(t, b)
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	grantB := acquireRows(t, bounded, rootB, "shared")
	if grantB.ID == 0 {
		t.Fatal("the second engine's acquisition returned no grant")
	}
	// The grant belongs to engine B's own root, so the two engines did not merely both succeed
	// by accident: each names the transaction of its own engine.
	if grantB.OwnerID != rootB.ID() {
		t.Fatalf("grant owner = %q, want engine B's root %q", grantB.OwnerID, rootB.ID())
	}
	if grantB.OwnerID == rootA.ID() {
		t.Fatal("engine B's grant names engine A's root")
	}
}

// TestB02ChildGrantBelongsToTheRootAndSurvivesTheChild pins the ownership rule across a
// child's whole life: a child acquires for its root, the child's merge releases nothing, and
// only the root's own end frees the row.
//
// Ownership is asserted through the neutral grant, and the release point through a competing
// root that refuses to wait: if the child commit had released the grant, that competing root
// would be granted the row immediately, which is the lost update this rule prevents.
func TestB02ChildGrantBelongsToTheRootAndSurvivesTheChild(t *testing.T) {
	ctx := context.Background()
	e := openAdapter(t, t.TempDir())
	root := begin(t, e)
	child, err := root.Child()
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("the child could not acquire through its own scope: %v", err)
	}
	// The grant names the ROOT, not the child: a child is a request origin, never an owner.
	if grant.OwnerID != root.ID() {
		t.Fatalf("grant owner = %q, want the ROOT transaction %q (the child is %q)", grant.OwnerID, root.ID(), child.ID())
	}
	if grant.ScopeID != scope {
		t.Fatalf("grant scope = %d, want the child's requesting scope %d", grant.ScopeID, scope)
	}

	commit(t, child)

	// The child merge is not a commit: a competing root still cannot take the row.
	probe := begin(t, e)
	blocked, err := t018AcquireNoWait(t, ctx, probe, "k1")
	if !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("after the child committed, acquiring the row = %v (grant %+v), want storageengine.ErrConflict", err, blocked)
	}

	// The root's own end is the only release point, and it releases at once.
	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := t018AcquireNoWait(t, ctx, probe, "k1"); err != nil {
		t.Fatalf("after the root ended the row was still locked: %v", err)
	}
}

// TestB02ComponentIdentityIsPassedThroughUnchanged pins the adapter's translation rule in
// both directions.
//
// The write path addresses bytes with Space, Key and the interval bounds; TableID and IndexID
// are labels that travel with them. The adapter must not parse, normalise or re-encode any of
// it: if it did, a lock taken by a SQL statement and a raw write to the same row would land on
// two resources, and the mutual exclusion between them would silently disappear. The up
// direction matters just as much: ReadCurrent re-encodes the grant's own components to look
// the claim up in the live registry, so an identity rewritten on the way out would make the
// very grant that covers the read fail to authorise it.
func TestB02ComponentIdentityIsPassedThroughUnchanged(t *testing.T) {
	ctx := context.Background()
	e := openAdapter(t, t.TempDir())
	writer := begin(t, e)
	if err := writer.Put(rowSpace, []byte("row-1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	commit(t, writer)

	reader := begin(t, e)
	locking, err := storageengine.LockingTxnOf(reader)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	// One bundle carrying every shape the identity has: a table intention, a record with both
	// labels, and an interval with bounds and inclusive flags.
	requested := []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: rowSpace, TableID: "t1", IndexID: "PRIMARY"},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: rowSpace, TableID: "t1", IndexID: "PRIMARY", Key: []byte("row-1")},
		{Mode: storageengine.LockModeGap, Kind: storageengine.LockComponentInterval, Space: rowSpace, TableID: "t1", IndexID: "idx-2", Lower: []byte("a"), Upper: []byte("z"), LowerInclusive: true, UpperInclusive: false},
	}
	grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{Components: requested})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t018RequireSameComponents(t, grant.Components, requested)

	// The returned capability is handed straight back: the adapter re-encodes the grant's own
	// components when it validates the read, so a component that changed on the way out would
	// fail here instead of authorising the observation.
	observation, err := locking.ReadCurrent(ctx, grant, rowSpace, []byte("row-1"))
	if err != nil {
		t.Fatalf("a grant built from the adapter's own translation was refused: %v", err)
	}
	if !observation.Exists || string(observation.Value) != "v1" {
		t.Fatalf("observation = %+v, want v1", observation)
	}
}

// TestB02NeutralWaitedGrantIsCompleteAndStable is the sibling of
// TestB02NeutralWaitedGrantCarriesOnlyItsOwnComponents.
//
// That test pins that a waited grant carries exactly its own components. This one pins the
// rest of the capability: the id the manager issued, the scope the request really came from,
// the root that owns it, and the generation/term pair that makes the capability valid only
// for the world it was issued in. The pair is compared against a second acquisition by the
// same root, because that is what a caller can actually observe: two answers for the same
// owner must agree, or a grant would stop matching the registry it is checked against.
func TestB02NeutralWaitedGrantIsCompleteAndStable(t *testing.T) {
	ctx := context.Background()
	e := openAdapter(t, t.TempDir())

	// The blocker holds the row the waiter needs.
	blocker := begin(t, e)
	acquireRows(t, ctx, blocker, "k1")

	waiter := begin(t, e)
	locking, err := storageengine.LockingTxnOf(waiter)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeChild)
	if err != nil {
		t.Fatal(err)
	}
	requested := t018RowComponents("k1")

	queued := make(chan struct{}, 1)
	type outcome struct {
		grant storageengine.LockGrant
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		grant, acquireErr := locking.Acquire(ctx, scope, storageengine.LockRequest{
			Components: requested,
			// The neutral decision is consulted while the manager holds its critical section,
			// so a signal from here proves the request is about to join the queue.
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
		t.Fatal("the acquisition never reached a conflict, so no wait was exercised")
	}
	// Ending the blocker's root releases its owner, which is what has to wake the waiter.
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}

	var waited storageengine.LockGrant
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("the woken acquisition failed: %v", out.err)
		}
		waited = out.grant
	case <-time.After(10 * time.Second):
		t.Fatal("the queued acquisition was never woken")
	}

	if waited.ID == 0 {
		t.Fatal("the waited grant carries no id")
	}
	if waited.OwnerID != waiter.ID() {
		t.Fatalf("grant owner = %q, want the waiting root %q", waited.OwnerID, waiter.ID())
	}
	if waited.ScopeID != scope {
		t.Fatalf("grant scope = %d, want the requesting scope %d", waited.ScopeID, scope)
	}
	requireNeutralComponents(t, waited.Components, requested...)

	// A second acquisition of the same bundle by the same root must report the same
	// generation/term pair: the pair is part of the capability, not a timestamp, and a caller
	// that re-validated the grant against a different pair would refuse its own valid grant.
	second := acquireRows(t, ctx, waiter, "k1")
	if second.Generation != waited.Generation || second.Term != waited.Term {
		t.Fatalf("a second acquisition reports generation/term %d/%d, the waited grant reports %d/%d",
			second.Generation, second.Term, waited.Generation, waited.Term)
	}
}
