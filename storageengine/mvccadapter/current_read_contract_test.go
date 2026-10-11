package mvccadapter_test

import (
	"context"
	"errors"
	"testing"

	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
)

// current_read_contract_test.go is T028: the adapter-boundary contract of the locked current read.
//
// Everything here is asserted through the neutral storageengine surface, exactly as the SQL layer
// uses it, so the properties pinned are the ones a caller can actually rely on:
//
//   - identity: scope, owner and the grant's own coverage (FR-026);
//   - visibility: a parent never observes an unmerged child, a child observes its own and its
//     ancestors' staged writes, and a sibling observes neither (FR-011);
//   - read modes: a plain Get/Scan keeps B01 fixed-snapshot semantics while a locked ReadCurrent
//     observes the latest published version (FR-011/FR-013);
//   - capability absence: a backend without a lock manager fails closed instead of degrading to an
//     unlocked read (FR-026);
//   - observation metadata and the write baseline an observation authorises (FR-011/FR-030);
//   - coverage and epoch refusal: no grant, however fabricated, may authorise a read it does not
//     really cover (FR-026).
//
// The MVCC-layer halves of the same rules live in mvcc/current_read_test.go and
// mvcc/current_read_authorization_test.go; the neutral-boundary happy path and the basic forgery
// matrix live in locking_current_read_test.go and locking_engine_test.go. This file adds the parts
// those cannot state: the child/sibling visibility rules through the adapter, the two-probe
// capability answer, the observation and write-baseline contract reached from the neutral API, and
// the epoch reset driven through the engine's own public maintenance entry point.
//
// A locked current read is authorised by the grant's *coverage*, and coverage is per key: a read of
// key X needs a grant that carries a record component for exactly (space, X). That is why the
// visibility tests below acquire one grant per key they read. It is the same rule requirement 7
// pins from the other side, and it is why reading an ancestor's key through a child's grant is
// refused rather than served.
//
// Every helper is t028-prefixed because other agents are editing this package concurrently.

// t028Space is the namespace these tests write to. It is its own namespace so a failure names the
// test rather than a neighbor's state.
const t028Space = "rows/t028-current-read-contract"

// t028OpenAdapter opens one MVCC-backed adapter engine. The neutral return type is deliberate: a
// black-box test must not reach the engine's own lock manager, so the only lock state it can assert
// is lock state reachable through the neutral surface or through observable behavior.
func t028OpenAdapter(t *testing.T) storageengine.FullEngine {
	t.Helper()
	opened, err := mvccadapter.Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatalf("open adapter: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened.(storageengine.FullEngine)
}

// t028Begin starts one root transaction and ends it at cleanup. Ending an already-ended transaction
// is harmless, so cleanup never depends on the path the test took.
func t028Begin(t *testing.T, e storageengine.Engine) storageengine.Txn {
	t.Helper()
	tx, err := e.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// t028Commit commits and fails the test when the commit is refused.
func t028Commit(t *testing.T, tx storageengine.Txn) {
	t.Helper()
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// t028Seed publishes one key/value pair in a transaction of its own, so the value is a committed
// version any later transaction can observe.
func t028Seed(t *testing.T, e storageengine.Engine, key, value string) {
	t.Helper()
	tx := t028Begin(t, e)
	if err := tx.Put(t028Space, []byte(key), []byte(value)); err != nil {
		t.Fatalf("seed %q: %v", key, err)
	}
	t028Commit(t, tx)
}

// t028Locking returns the neutral locking capability of one transaction.
func t028Locking(t *testing.T, tx storageengine.Txn) storageengine.LockingTxn {
	t.Helper()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("locking capability: %v", err)
	}
	return locking
}

// t028RowComponents is the bundle one row lock uses in this file: the exclusive record plus the
// table intention the bundle-level prerequisite requires.
func t028RowComponents(key string) []storageengine.LockComponent {
	return []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: t028Space, TableID: "t028"},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t028Space, TableID: "t028", Key: []byte(key)},
	}
}

// t028Acquire opens a statement scope on one transaction and acquires one row lock through it. The
// scope is returned with the grant, because a grant's ScopeID is part of what a caller has to be
// able to check against the scope it really requested. One call covers one key: the grant's
// components are the coverage a later read is authorise against.
func t028Acquire(t *testing.T, ctx context.Context, tx storageengine.Txn, key string) (uint64, storageengine.LockGrant) {
	t.Helper()
	locking := t028Locking(t, tx)
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatalf("begin scope: %v", err)
	}
	grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{Components: t028RowComponents(key)})
	if err != nil {
		t.Fatalf("acquire %q: %v", key, err)
	}
	return scope, grant
}

// t028ReadCurrent performs one locked current read in the file's own namespace and returns the
// observation and the error, so a test can assert the refused cases without a helper that fails the
// test.
func t028ReadCurrent(ctx context.Context, tx storageengine.Txn, grant storageengine.LockGrant, key string) (storageengine.CurrentObservation, error) {
	return t028ReadCurrentIn(ctx, tx, grant, t028Space, key)
}

// t028ReadCurrentIn is the same call with an explicit namespace, which is what the "a namespace the
// grant does not cover" case needs: the same key bytes in another namespace are another resource.
func t028ReadCurrentIn(ctx context.Context, tx storageengine.Txn, grant storageengine.LockGrant, space, key string) (storageengine.CurrentObservation, error) {
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		return storageengine.CurrentObservation{}, err
	}
	return locking.ReadCurrent(ctx, grant, space, []byte(key))
}

// t028NeutralLockError reports whether an error is one of the neutral errors the locking contract
// defines. A locked read that fails must fail with one of these rather than with a backend type, so
// no refused case may accept an arbitrary error.
func t028NeutralLockError(err error) bool {
	return errors.Is(err, storageengine.ErrConflict) ||
		errors.Is(err, storageengine.ErrLockInvalidated) ||
		errors.Is(err, storageengine.ErrLockWaitTimeout) ||
		errors.Is(err, storageengine.ErrDeadlock) ||
		errors.Is(err, storageengine.ErrLockResourceLimit) ||
		errors.Is(err, storageengine.ErrUnsupported) ||
		errors.Is(err, storageengine.ErrClosed)
}

// t028Diag returns one transaction's neutral diagnostics snapshot, or fails when the transaction is
// not registered. It exists so a test can assert the *absence* of lock participation: a plain read
// records observations and no dependencies, while a locked read is the only read that may.
func t028Diag(t *testing.T, e storageengine.Engine, id string) storageengine.TransactionInfo {
	t.Helper()
	diagnostics, ok := e.(storageengine.TransactionDiagnostics)
	if !ok {
		t.Fatal("the adapter engine does not report transaction diagnostics")
	}
	for _, info := range diagnostics.ActiveTransactions() {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("transaction %q is not registered", id)
	return storageengine.TransactionInfo{}
}

// TestT028ScopeOwnerAndGrantIdentity covers requirement 1's positive half: a legal root scope, a
// child's own scope, and the identity the grant must carry.
//
// The grant is the only thing a later locked read is authorised by, so its identity has to be
// checkable rather than decorative: a non-zero id, an owner that is the *root* (a child is a request
// origin, never an owner), the scope the request really came from, and the generation/term pair the
// root lives in. The pair is compared against a second acquisition by the same root, because that is
// what a caller can observe: two answers for one owner must agree, or a grant would stop matching the
// registry it is checked against.
func TestT028ScopeOwnerAndGrantIdentity(t *testing.T) {
	ctx := context.Background()
	e := t028OpenAdapter(t)
	t028Seed(t, e, "k1", "v1")

	root := t028Begin(t, e)
	rootScope, rootGrant := t028Acquire(t, ctx, root, "k1")

	if rootGrant.ID == 0 {
		t.Fatal("the grant carries no id, so it cannot name the request that produced it")
	}
	if rootGrant.OwnerID != root.ID() {
		t.Fatalf("grant owner = %q, want the root transaction %q", rootGrant.OwnerID, root.ID())
	}
	if rootGrant.ScopeID != rootScope {
		t.Fatalf("grant scope = %d, want the requesting scope %d", rootGrant.ScopeID, rootScope)
	}
	requireNeutralComponents(t, rootGrant.Components, t028RowComponents("k1")...)

	// A child acquires through its own scope. Ownership does not move: the grant names the ROOT, and
	// the scope names the child's request. If a child owned its grant, a child rollback would have to
	// release a lock the root still needs and a child merge would have to transfer one.
	//
	// The child asks for a key of its own, because a re-entrant acquisition of a component the root
	// already holds legitimately reports the owner's existing protection and *no* fresh grant id (it
	// took no new lock — lockmanager.LockGrant.GrantID is declared as the id of a granted bundle).
	// A distinct key is what makes "the grant really carries the manager's identity" observable here.
	child, err := root.Child()
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	childLocking := t028Locking(t, child)
	childScope, err := childLocking.BeginLockScope(ctx, storageengine.LockScopeChild)
	if err != nil {
		t.Fatalf("child scope: %v", err)
	}
	childGrant, err := childLocking.Acquire(ctx, childScope, storageengine.LockRequest{Components: t028RowComponents("k1b")})
	if err != nil {
		t.Fatalf("the child could not acquire through its own scope: %v", err)
	}
	if childGrant.ID == 0 {
		t.Fatal("the child's grant carries no id")
	}
	if childGrant.OwnerID != root.ID() {
		t.Fatalf("child grant owner = %q, want the ROOT %q (the child is %q)", childGrant.OwnerID, root.ID(), child.ID())
	}
	if childGrant.ScopeID != childScope {
		t.Fatalf("child grant scope = %d, want the child's requesting scope %d", childGrant.ScopeID, childScope)
	}
	requireNeutralComponents(t, childGrant.Components, t028RowComponents("k1b")...)

	// A re-entrant acquisition of the key the root already holds is the third shape: it reports the
	// owner's protection, its own requesting scope, and no new grant identity, because it took no new
	// lock. It is still a usable capability, which the read below proves.
	childScopeAgain, childReentrant := t028Acquire(t, ctx, child, "k1b")
	if childReentrant.OwnerID != root.ID() {
		t.Fatalf("the re-entrant grant names owner %q, want the root %q", childReentrant.OwnerID, root.ID())
	}
	if childReentrant.ScopeID != childScopeAgain {
		t.Fatalf("the re-entrant grant names scope %d, want the requesting scope %d", childReentrant.ScopeID, childScopeAgain)
	}
	if childReentrant.ID != 0 {
		t.Fatalf("a re-entrant acquisition reported grant id %d; it took no new lock", childReentrant.ID)
	}
	requireNeutralComponents(t, childReentrant.Components, t028RowComponents("k1b")...)

	// The generation/term pair is part of the capability, not a timestamp: the child's grant and a
	// second acquisition by the root must report the pair the root's owner is registered under.
	_, second := t028Acquire(t, ctx, root, "k1")
	if rootGrant.Generation != second.Generation || rootGrant.Term != second.Term {
		t.Fatalf("a second acquisition reports generation/term %d/%d, the first reports %d/%d",
			second.Generation, second.Term, rootGrant.Generation, rootGrant.Term)
	}
	if childGrant.Generation != rootGrant.Generation || childGrant.Term != rootGrant.Term {
		t.Fatalf("the child's grant reports generation/term %d/%d, the root's reports %d/%d",
			childGrant.Generation, childGrant.Term, rootGrant.Generation, rootGrant.Term)
	}

	// Both grants authorise a real current read, so the identity above is a usable capability and not
	// merely a well-shaped value. The child reads through the grant it acquired in its own scope, and
	// the re-entrant grant is exercised too, so "no fresh grant id" is not the same as "unusable".
	if err := root.Put(t028Space, []byte("k1b"), []byte("v1b")); err != nil {
		t.Fatalf("root write: %v", err)
	}
	if err := child.Put(t028Space, []byte("k1b"), []byte("child-owned")); err != nil {
		t.Fatalf("child write: %v", err)
	}
	observation, err := childLocking.ReadCurrent(ctx, childGrant, t028Space, []byte("k1b"))
	if err != nil {
		t.Fatalf("the child's own covering grant was refused: %v", err)
	}
	if !observation.Exists || string(observation.Value) != "child-owned" {
		t.Fatalf("child observation = %+v, want the child's own staged value", observation)
	}
	if reentrantObservation, err := childLocking.ReadCurrent(ctx, childReentrant, t028Space, []byte("k1b")); err != nil {
		t.Fatalf("the re-entrant grant did not authorise the read it covers: %v", err)
	} else if string(reentrantObservation.Value) != "child-owned" {
		t.Fatalf("re-entrant observation = %+v, want the child's own staged value", reentrantObservation)
	}
	if _, err := t028Locking(t, root).ReadCurrent(ctx, rootGrant, t028Space, []byte("k1")); err != nil {
		t.Fatalf("the root's covering grant was refused: %v", err)
	}
}

// TestT028FabricatedGrantsCannotAuthoriseARead covers requirement 1's and requirement 7's negative
// half: a capability is only accepted when it is the one the manager issued, for this root, for the
// generation the root lives in, and covering exactly the key being read.
//
// The reason this matters more than a shape check: a successful locked read records the observation
// as that transaction's write baseline, which is what a later write to the same key is validated
// against. Accepting a fabricated or stretched capability would therefore authorise a write to a row
// nothing protected — the commit-conflict check would be bypassed by forging a grant. That is why
// every refusal below also asserts that no value came back with the error.
func TestT028FabricatedGrantsCannotAuthoriseARead(t *testing.T) {
	ctx := context.Background()
	e := t028OpenAdapter(t)
	t028Seed(t, e, "k1", "v1")
	t028Seed(t, e, "k2", "v2")

	reader := t028Begin(t, e)
	_, grant := t028Acquire(t, ctx, reader, "k1")

	// A second live root holds its own grant on k2. It is used to build the "borrowed owner" and
	// "borrowed coverage" cases below.
	other := t028Begin(t, e)
	_, otherGrant := t028Acquire(t, ctx, other, "k2")

	ownerOnly := grant
	ownerOnly.Components = nil
	wrongOwner := grant
	wrongOwner.OwnerID = otherGrant.OwnerID
	staleGeneration := grant
	staleGeneration.Generation++
	staleTerm := grant
	staleTerm.Term++
	borrowedCoverage := grant
	borrowedCoverage.Components = otherGrant.Components
	// A component list the request never took: a record lock at the right space but the wrong key,
	// plus the table intention the grant really carries. The coverage answer comes from the manager's
	// own held-lock table, so this must be refused even though the shape looks plausible.
	neverTaken := grant
	neverTaken.Components = []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: t028Space, TableID: "t028"},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t028Space, TableID: "t028", Key: []byte("k2")},
	}
	// An interval that does not cover the key is not coverage either: an interval protects insertion
	// points, never an existing value, so a gap component must not authorise a point read.
	wrongKind := grant
	wrongKind.Components = []storageengine.LockComponent{
		{Mode: storageengine.LockModeGap, Kind: storageengine.LockComponentInterval, Space: t028Space, Lower: []byte("a"), Upper: []byte("b")},
	}

	cases := []struct {
		name  string
		grant storageengine.LockGrant
		space string
		key   string
	}{
		{"reduced to an owner id", ownerOnly, t028Space, "k1"},
		{"another live root's owner", wrongOwner, t028Space, "k1"},
		{"another generation", staleGeneration, t028Space, "k1"},
		{"another leader term", staleTerm, t028Space, "k1"},
		{"coverage borrowed from another root", borrowedCoverage, t028Space, "k1"},
		{"a component the request never took", neverTaken, t028Space, "k1"},
		{"a gap that does not contain the key", wrongKind, t028Space, "k1"},
		{"a key the grant does not cover", grant, t028Space, "k2"},
		{"a namespace the grant does not cover", grant, t028Space + "-other", "k1"},
	}
	for _, test := range cases {
		observation, err := t028ReadCurrentIn(ctx, reader, test.grant, test.space, test.key)
		if err == nil {
			t.Fatalf("%s: the read was authorised with observation %+v; a capability that does not cover the key must fail closed", test.name, observation)
		}
		if !t028NeutralLockError(err) {
			t.Fatalf("%s: err = %v, want one of the neutral locking errors", test.name, err)
		}
		if observation.Exists || observation.Tombstone || observation.HasChange || len(observation.Value) != 0 {
			t.Fatalf("%s: a refused read returned a value: %+v", test.name, observation)
		}
	}

	// The genuine capability still works, so the refusals above are about forgery rather than about a
	// transaction that had become unusable for an unrelated reason.
	observation, err := t028ReadCurrent(ctx, reader, grant, "k1")
	if err != nil {
		t.Fatalf("the genuine grant was refused after the forged ones: %v", err)
	}
	if !observation.Exists || string(observation.Value) != "v1" {
		t.Fatalf("genuine observation = %+v, want the published v1", observation)
	}
}

// TestT028RootNeverObservesAnUnmergedChild covers requirement 2 (FR-011).
//
// The root stages its own value, a child stages a different value for the same key, and the ROOT's
// locked current read must return the root's own staged value. A parent that could observe an
// unmerged child would be reading a value that can still be rolled back, and would record it as a
// write baseline. After the child merges, the same read must see the child's value, which is what
// separates "not visible yet" from "dropped".
func TestT028RootNeverObservesAnUnmergedChild(t *testing.T) {
	ctx := context.Background()
	e := t028OpenAdapter(t)
	t028Seed(t, e, "k1", "published")

	root := t028Begin(t, e)
	if err := root.Put(t028Space, []byte("k1"), []byte("root")); err != nil {
		t.Fatalf("root write: %v", err)
	}
	// The root's grant covers k1, the key every read below is about. The root's own write already
	// holds the same record, so this acquisition is re-entrant for the root's owner.
	_, rootGrant := t028Acquire(t, ctx, root, "k1")
	rootLocking := t028Locking(t, root)

	child, err := root.Child()
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	childLocking := t028Locking(t, child)
	if err := child.Put(t028Space, []byte("k1"), []byte("child")); err != nil {
		t.Fatalf("child write: %v", err)
	}
	// The child's own grant is acquired through the child's own scope, exactly as a savepoint layer
	// does; it names the root as owner and this child's scope as origin.
	_, childGrant := t028Acquire(t, ctx, child, "k1")

	// The root's own observation: its own unmerged write wins, and the child's value is nowhere.
	rootObservation, err := rootLocking.ReadCurrent(ctx, rootGrant, t028Space, []byte("k1"))
	if err != nil {
		t.Fatalf("root current read: %v", err)
	}
	if string(rootObservation.Value) != "root" {
		t.Fatalf("root observed %q, want its own staged value %q; a parent must never observe an unmerged child",
			rootObservation.Value, "root")
	}
	if !rootObservation.Exists || rootObservation.Tombstone {
		t.Fatalf("root observation = %+v, want a live staged value", rootObservation)
	}

	// The child's own observation: the child sees its own write, which is the other half of the rule.
	childObservation, err := childLocking.ReadCurrent(ctx, childGrant, t028Space, []byte("k1"))
	if err != nil {
		t.Fatalf("child current read: %v", err)
	}
	if string(childObservation.Value) != "child" {
		t.Fatalf("child observed %q, want its own staged value %q", childObservation.Value, "child")
	}

	// Merging the child makes its value part of the root's own write set, so the root now observes it.
	// Without this the test above could pass on an implementation that simply never merges anything.
	t028Commit(t, child)
	merged, err := rootLocking.ReadCurrent(ctx, rootGrant, t028Space, []byte("k1"))
	if err != nil {
		t.Fatalf("root current read after the child merged: %v", err)
	}
	if string(merged.Value) != "child" {
		t.Fatalf("after the child merged the root observed %q, want the merged %q", merged.Value, "child")
	}
	// The root read an overlay value, never a published version: what it saw is unpublished by
	// definition, so it must not be reported as a committed change.
	if merged.HasChange {
		t.Fatalf("a staged overlay observation reported a committed change: %+v", merged)
	}

	// The child's merge transferred the write set and nothing else: the row stays locked by the root,
	// which is why the root's own end is the only release point.
	probe := t028Begin(t, e)
	probeLocking := t028Locking(t, probe)
	probeScope, err := probeLocking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatalf("probe scope: %v", err)
	}
	if _, err := probeLocking.Acquire(ctx, probeScope, storageengine.LockRequest{
		Components: t028RowComponents("k1"),
		ShouldWait: func(context.Context) bool { return false },
	}); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("after the child merged another root acquired the row: err = %v, want storageengine.ErrConflict", err)
	}
	if err := root.Rollback(); err != nil {
		t.Fatalf("root rollback: %v", err)
	}
}

// TestT028ChildSeesItsAncestorsAndItselfButNotItsSibling covers requirement 3 (FR-011).
//
// Two children of one root write different keys while the root has staged a key of its own. Each
// child must observe its own key and the root's staged key, and must NOT observe the other child's
// key: a sibling is another branch of the same tree, not an ancestor, so its unmerged write is not
// visible. Every read goes through the child's own request scope and a grant that scope acquired for
// the key being read, which is what the SQL layer does for a savepoint.
//
// A note on the shape: the sibling's key is a key the sibling's own write holds the record lock on.
// The sibling's request is re-entrant for the shared root owner, so the first child can still take a
// grant on that key and ask the read layer — which is exactly the call that would expose a
// tree-walking read layer. Through the flat published state the answer must be absence.
func TestT028ChildSeesItsAncestorsAndItselfButNotItsSibling(t *testing.T) {
	ctx := context.Background()
	e := t028OpenAdapter(t)
	t028Seed(t, e, "k_published", "published")

	root := t028Begin(t, e)
	if err := root.Put(t028Space, []byte("k_root"), []byte("root")); err != nil {
		t.Fatalf("root write: %v", err)
	}

	first, err := root.Child()
	if err != nil {
		t.Fatalf("first child: %v", err)
	}
	second, err := root.Child()
	if err != nil {
		t.Fatalf("second child: %v", err)
	}
	if err := first.Put(t028Space, []byte("k_first"), []byte("first")); err != nil {
		t.Fatalf("first child write: %v", err)
	}
	if err := second.Put(t028Space, []byte("k_second"), []byte("second")); err != nil {
		t.Fatalf("second child write: %v", err)
	}

	firstLocking := t028Locking(t, first)
	firstScope, err := firstLocking.BeginLockScope(ctx, storageengine.LockScopeChild)
	if err != nil {
		t.Fatalf("first child scope: %v", err)
	}
	grantFor := func(key string) storageengine.LockGrant {
		t.Helper()
		grant, acquireErr := firstLocking.Acquire(ctx, firstScope, storageengine.LockRequest{Components: t028RowComponents(key)})
		if acquireErr != nil {
			t.Fatalf("the first child could not acquire %q: %v", key, acquireErr)
		}
		return grant
	}

	// Its own staged key: the observation comes from the child's own overlay.
	own, err := firstLocking.ReadCurrent(ctx, grantFor("k_first"), t028Space, []byte("k_first"))
	if err != nil {
		t.Fatalf("first child reading its own key: %v", err)
	}
	if string(own.Value) != "first" {
		t.Fatalf("the child observed %q for its own key, want %q", own.Value, "first")
	}
	if own.HasChange {
		t.Fatalf("an own-overlay observation reported a committed change: %+v", own)
	}

	// The ancestor's staged key: an unmerged write of the root is visible to every layer below it.
	ancestor, err := firstLocking.ReadCurrent(ctx, grantFor("k_root"), t028Space, []byte("k_root"))
	if err != nil {
		t.Fatalf("the child could not observe its ancestor's staged key: %v", err)
	}
	if string(ancestor.Value) != "root" {
		t.Fatalf("the child observed %q for the ancestor's key, want %q", ancestor.Value, "root")
	}
	if ancestor.HasChange {
		t.Fatalf("an ancestor-overlay observation reported a committed change: %+v", ancestor)
	}

	// The sibling's staged key: it is on no ancestor chain of this child, so the only correct answer
	// for a key the database never published is absence. This is the assertion that fails if the read
	// layer walked the whole tree instead of walking upward.
	sibling, err := firstLocking.ReadCurrent(ctx, grantFor("k_second"), t028Space, []byte("k_second"))
	if err != nil {
		t.Fatalf("the child's locked read of the sibling's key was refused (err = %v); the grant covers that key, so the read layer had a real answer available", err)
	}
	if sibling.Exists {
		t.Fatalf("a child observed its unmerged sibling's write: %+v", sibling)
	}
	if sibling.Tombstone {
		t.Fatalf("a sibling's unpublished write was reported as a tombstone of the published state: %+v", sibling)
	}
	if sibling.HasChange {
		t.Fatalf("absence reported a committed change: %+v", sibling)
	}

	// The published key is visible to the second child as an ordinary committed version, which shows
	// the refusals and absences above are about visibility rather than about a broken read path.
	secondLocking := t028Locking(t, second)
	_, secondGrant := t028Acquire(t, ctx, second, "k_published")
	published, err := secondLocking.ReadCurrent(ctx, secondGrant, t028Space, []byte("k_published"))
	if err != nil {
		t.Fatalf("the second child reading the published key: %v", err)
	}
	if !published.Exists || string(published.Value) != "published" {
		t.Fatalf("the second child observed %+v, want the published value", published)
	}
	if !published.HasChange || published.ChangeTS == 0 {
		t.Fatalf("a published observation carries no change metadata: %+v", published)
	}

	// Each child ends without disturbing the other, and the root still holds the whole tree.
	if err := first.Rollback(); err != nil {
		t.Fatalf("first child rollback: %v", err)
	}
	if err := second.Rollback(); err != nil {
		t.Fatalf("second child rollback: %v", err)
	}
	if err := root.Rollback(); err != nil {
		t.Fatalf("root rollback: %v", err)
	}
}

// TestT028PlainReadsKeepSnapshotSemanticsAndTakeNoLocks covers requirement 4 (FR-013/FR-030).
//
// The snapshot half: another transaction has staged an uncommitted change to a key, and a plain Get
// and a plain Scan must both keep returning the published value, while a locked ReadCurrent under a
// covering grant observes the published version too — a current read observes the latest *published*
// version, and another root's staged write is not published.
//
// The lock half is asserted two ways, both of them observable facts rather than an inference from an
// elapsed time. First, the reader takes exactly the grant this test hands it: a competing root that
// refuses to wait is still refused the row afterwards, so the reader really is a lock holder and not
// a reader that merely believes it is protected. Second, and this is the part about the plain API,
// the reader's own lock is on k4 while its plain Get and Scan run over k4-other: the writer's Put to
// k4-other completes immediately, which it could not do if Get or Scan had taken a lock on that key.
// The engine's own diagnostics corroborate it from the dependency side, because a plain read records
// an observation and no dependency while a locked read is the only read that may take one.
func TestT028PlainReadsKeepSnapshotSemanticsAndTakeNoLocks(t *testing.T) {
	ctx := context.Background()
	e := t028OpenAdapter(t)
	t028Seed(t, e, "k4", "published")
	t028Seed(t, e, "k4-other", "other-published")

	reader := t028Begin(t, e)
	// The reader takes the row lock on k4 explicitly, before the writer exists, so the writer's own
	// acquisition of k4 is what would have to wait if the reader held anything beyond its grant.
	_, grant := t028Acquire(t, ctx, reader, "k4")

	// A competing root that refuses to wait cannot take the row. This is the observation that the
	// reader's grant is real and still held, which the plain-API assertions below depend on.
	probe := t028Begin(t, e)
	probeLocking := t028Locking(t, probe)
	probeScope, err := probeLocking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatalf("probe scope: %v", err)
	}
	if _, err := probeLocking.Acquire(ctx, probeScope, storageengine.LockRequest{
		Components: t028RowComponents("k4"),
		ShouldWait: func(context.Context) bool { return false },
	}); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("a competing root acquired the reader's row: err = %v, want storageengine.ErrConflict", err)
	}

	before := t028Diag(t, e, reader.ID())

	// The plain reads run while the reader holds the row lock. They must return the snapshot value,
	// and they must not extend the reader's protection to the keys they touch.
	value, ok, err := reader.Get(t028Space, []byte("k4"))
	if err != nil {
		t.Fatalf("plain Get: %v", err)
	}
	if !ok || string(value) != "published" {
		t.Fatalf("plain Get = %q found=%v, want the published value", value, ok)
	}
	other, ok, err := reader.Get(t028Space, []byte("k4-other"))
	if err != nil {
		t.Fatalf("plain Get of the other key: %v", err)
	}
	if !ok || string(other) != "other-published" {
		t.Fatalf("plain Get of k4-other = %q found=%v, want the published value", other, ok)
	}

	seen := map[string]string{}
	if err := reader.Scan(ctx, t028Space, func(k, v []byte) error {
		seen[string(k)] = string(v)
		return nil
	}); err != nil {
		t.Fatalf("plain Scan: %v", err)
	}
	if got := seen["k4"]; got != "published" {
		t.Fatalf("plain Scan saw k4 = %q, want the published value", got)
	}
	if got := seen["k4-other"]; got != "other-published" {
		t.Fatalf("plain Scan saw k4-other = %q, want the published value", got)
	}

	// The writer stages a change to the key the plain reads just touched. It must not block: the
	// reader's only lock is the one on k4, which the writer is not asking for.
	writer := t028Begin(t, e)
	if err := writer.Put(t028Space, []byte("k4-other"), []byte("staged-other")); err != nil {
		t.Fatalf("the writer could not stage a key a plain reader had just read: %v", err)
	}

	// The plain reads of that key still return the published value: the other transaction's staged
	// write is not visible to a snapshot read.
	afterStage, ok, err := reader.Get(t028Space, []byte("k4-other"))
	if err != nil {
		t.Fatalf("plain Get after the staging: %v", err)
	}
	if !ok || string(afterStage) != "other-published" {
		t.Fatalf("plain Get = %q found=%v, want the published value while the other transaction is uncommitted", afterStage, ok)
	}

	// The locked read observes the latest *published* version, which is still the old one: the writer's
	// staged write is unpublished, so it is not current.
	current, err := t028ReadCurrent(ctx, reader, grant, "k4")
	if err != nil {
		t.Fatalf("locked current read: %v", err)
	}
	if !current.Exists || string(current.Value) != "published" {
		t.Fatalf("locked current read = %+v, want the published value", current)
	}

	// A plain read is an observation, never a lock: the reader's diagnostic counters must show no
	// dependency at all, while the reads it did perform are visible.
	after := t028Diag(t, e, reader.ID())
	if after.PointDependencies != 0 || after.RangeDependencies != 0 {
		t.Fatalf("plain reads recorded dependencies: %+v", after)
	}
	if after.RowsObserved <= before.RowsObserved {
		t.Fatalf("the plain reads recorded no observation at all (%d -> %d), so the counter check would be vacuous",
			before.RowsObserved, after.RowsObserved)
	}

	// The reader ending is what releases the row, and the writer then publishes its own key. The two
	// keys were independent throughout: the reader's lock never covered the writer's key, and the
	// writer's staged write never became visible to the reader.
	if err := reader.Rollback(); err != nil {
		t.Fatalf("reader rollback: %v", err)
	}
	t028Commit(t, writer)

	fresh := t028Begin(t, e)
	if value, ok, err := fresh.Get(t028Space, []byte("k4-other")); err != nil || !ok || string(value) != "staged-other" {
		t.Fatalf("the writer's committed value = %q found=%v err=%v, want staged-other", value, ok, err)
	}
	if value, ok, err := fresh.Get(t028Space, []byte("k4")); err != nil || !ok || string(value) != "published" {
		t.Fatalf("the reader never wrote, but k4 = %q found=%v err=%v", value, ok, err)
	}
}

// TestT028CurrentObservationMetadata covers requirement 6(a)(b)(c): the observation has to
// distinguish "never existed" from "deleted" from "exists", and carry the change metadata a caller
// needs to revalidate what it saw.
//
// The distinction is load-bearing rather than cosmetic: the observation becomes the transaction's
// write baseline for that key, and a tombstone's baseline is its deleting sequence while an absent
// key's baseline is "no version at all".
func TestT028CurrentObservationMetadata(t *testing.T) {
	ctx := context.Background()

	t.Run("a never existing key is absent with no change", func(t *testing.T) {
		e := t028OpenAdapter(t)
		reader := t028Begin(t, e)
		_, grant := t028Acquire(t, ctx, reader, "k-missing")

		observation, err := t028ReadCurrent(ctx, reader, grant, "k-missing")
		if err != nil {
			t.Fatalf("locked read of an absent key: %v", err)
		}
		if observation.Exists {
			t.Fatalf("a key that was never written reported Exists: %+v", observation)
		}
		if observation.Tombstone {
			t.Fatalf("a key that was never written was reported as a tombstone: %+v", observation)
		}
		if observation.HasChange || observation.ChangeTS != 0 {
			t.Fatalf("absence reported a change: %+v", observation)
		}
	})

	t.Run("a deleted key reports its tombstone and deleting sequence", func(t *testing.T) {
		e := t028OpenAdapter(t)
		t028Seed(t, e, "k-gone", "v1")

		deleter := t028Begin(t, e)
		if err := deleter.Delete(t028Space, []byte("k-gone")); err != nil {
			t.Fatalf("delete: %v", err)
		}
		deleteSequence, err := deleter.Commit(ctx)
		if err != nil {
			t.Fatalf("delete commit: %v", err)
		}
		if deleteSequence == 0 {
			t.Fatal("the delete committed without a sequence, so the tombstone has no change to report")
		}

		reader := t028Begin(t, e)
		_, grant := t028Acquire(t, ctx, reader, "k-gone")
		observation, err := t028ReadCurrent(ctx, reader, grant, "k-gone")
		if err != nil {
			t.Fatalf("locked read of a deleted key: %v", err)
		}
		if observation.Exists {
			t.Fatalf("a deleted key reported Exists: %+v", observation)
		}
		if !observation.Tombstone {
			t.Fatalf("a deleted key must be distinguishable from one that never existed: %+v", observation)
		}
		if !observation.HasChange || observation.ChangeTS == 0 {
			t.Fatalf("the tombstone carries no change metadata: %+v", observation)
		}
		if observation.ChangeTS != deleteSequence {
			t.Fatalf("the tombstone reports change %d, want the deleting sequence %d", observation.ChangeTS, deleteSequence)
		}
	})

	t.Run("an existing key reports its published value and change", func(t *testing.T) {
		e := t028OpenAdapter(t)
		seeder := t028Begin(t, e)
		if err := seeder.Put(t028Space, []byte("k-live"), []byte("v-live")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		sequence, err := seeder.Commit(ctx)
		if err != nil {
			t.Fatalf("seed commit: %v", err)
		}

		reader := t028Begin(t, e)
		_, grant := t028Acquire(t, ctx, reader, "k-live")
		observation, err := t028ReadCurrent(ctx, reader, grant, "k-live")
		if err != nil {
			t.Fatalf("locked read of a live key: %v", err)
		}
		if !observation.Exists {
			t.Fatalf("a live key reported Exists=false: %+v", observation)
		}
		if observation.Tombstone {
			t.Fatalf("a live key was reported as a tombstone: %+v", observation)
		}
		if string(observation.Value) != "v-live" {
			t.Fatalf("observation value = %q, want %q", observation.Value, "v-live")
		}
		if !observation.HasChange || observation.ChangeTS == 0 {
			t.Fatalf("a published value carries no change metadata: %+v", observation)
		}
		if observation.ChangeTS != sequence {
			t.Fatalf("observation change = %d, want the publishing sequence %d", observation.ChangeTS, sequence)
		}
	})
}

// TestT028LockedObservationAuthorisesTheWriteItSaw covers requirement 6(d) through the neutral API.
//
// A locked current read records the version it observed as that key's write baseline, so a later
// write to the key is validated against what the transaction actually saw under the lock rather than
// against the timestamp it began at. The two cases are deliberately symmetric:
//
//   - a transaction that locks and observes the newer version may update the row, even though the row
//     changed after its readTS;
//   - the same update without the locked read is still rejected as a stale-commit conflict.
//
// The second case is the guard against the first becoming a general weakening of the B01 commit rule.
func TestT028LockedObservationAuthorisesTheWriteItSaw(t *testing.T) {
	ctx := context.Background()
	e := t028OpenAdapter(t)
	t028Seed(t, e, "k-locked", "v1")

	reader := t028Begin(t, e)
	// A concurrent root publishes a newer version after the reader began, so the reader's readTS is
	// stale for this key.
	concurrent := t028Begin(t, e)
	if err := concurrent.Put(t028Space, []byte("k-locked"), []byte("v2")); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	newSequence, err := concurrent.Commit(ctx)
	if err != nil {
		t.Fatalf("concurrent commit: %v", err)
	}

	_, grant := t028Acquire(t, ctx, reader, "k-locked")
	observation, err := t028ReadCurrent(ctx, reader, grant, "k-locked")
	if err != nil {
		t.Fatalf("locked current read: %v", err)
	}
	if string(observation.Value) != "v2" {
		t.Fatalf("the locked read observed %q, want the latest published %q", observation.Value, "v2")
	}
	if observation.ChangeTS != newSequence {
		t.Fatalf("the locked read observed change %d, want the concurrent commit sequence %d", observation.ChangeTS, newSequence)
	}

	// The update follows the observation. It must not be rejected: the transaction writes the version
	// it observed under the lock, which is exactly what the covering lock licenses.
	if err := reader.Put(t028Space, []byte("k-locked"), []byte("v3")); err != nil {
		t.Fatalf("update after a locked read: %v", err)
	}
	if _, err := reader.Commit(ctx); err != nil {
		t.Fatalf("a write authorised by a locked observation was rejected: %v", err)
	}
	verify := t028Begin(t, e)
	if value, ok, err := verify.Get(t028Space, []byte("k-locked")); err != nil || !ok || string(value) != "v3" {
		t.Fatalf("published value = %q found=%v err=%v, want v3", value, ok, err)
	}

	// The control: the same shape without the locked read. The transaction stages the update but never
	// observes the key under its lock, so the ordinary readTS rule applies and the commit conflicts.
	t028Seed(t, e, "k-control", "v1")
	control := t028Begin(t, e)
	writer := t028Begin(t, e)
	if err := writer.Put(t028Space, []byte("k-control"), []byte("v2")); err != nil {
		t.Fatalf("control writer: %v", err)
	}
	t028Commit(t, writer)
	if err := control.Put(t028Space, []byte("k-control"), []byte("v3")); err != nil {
		t.Fatalf("control update: %v", err)
	}
	if _, err := control.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("an unobserved update of a changed key committed: err = %v, want storageengine.ErrConflict", err)
	}
	after := t028Begin(t, e)
	if value, _, _ := after.Get(t028Space, []byte("k-control")); string(value) != "v2" {
		t.Fatalf("the rejected update published %q, want the surviving %q", value, "v2")
	}
}

// TestT028EpochResetInvalidatesEveryStaleGrant covers requirement 7's "a grant whose generation no
// longer matches must fail closed after an epoch reset", driven through the engine's own public
// maintenance path rather than through a test-only shortcut.
//
// A backup of the empty store is taken first; restoring it performs the same generation change a
// restore or a leader-term change performs, and it is the only public entry point a black-box test
// has to it. After the reset the grant issued before it names a generation that is gone, so the read
// must be refused and the lock the old root held must be gone with it — otherwise a stale capability
// would keep a row blocked for a database that no longer exists.
func TestT028EpochResetInvalidatesEveryStaleGrant(t *testing.T) {
	ctx := context.Background()
	e := t028OpenAdapter(t)

	maintenance, ok := storageengine.Engine(e).(storageengine.Maintenance)
	if !ok {
		t.Fatal("the adapter engine does not expose the maintenance capability that drives a generation change")
	}
	// The backup target must not exist yet; the backup pins the store's state at this instant.
	backupDir := t.TempDir() + "/t028-backup"
	if _, err := maintenance.Backup(ctx, backupDir); err != nil {
		t.Fatalf("backup: %v", err)
	}

	root := t028Begin(t, e)
	if err := root.Put(t028Space, []byte("k-epoch"), []byte("v1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, grant := t028Acquire(t, ctx, root, "k-epoch")
	// A local store starts at generation 0, so the grant's generation is not required to be non-zero;
	// what matters is that the reset moves it, which the fresh grant below asserts.
	if observation, err := t028ReadCurrent(ctx, root, grant, "k-epoch"); err != nil {
		t.Fatalf("the locked read before the reset: %v", err)
	} else if string(observation.Value) != "v1" {
		t.Fatalf("the locked read before the reset observed %q, want v1", observation.Value)
	}

	// The generation change: every owner of the replaced generation is invalidated and every grant it
	// held is refused from here on.
	if err := maintenance.RestoreBackup(ctx, backupDir); err != nil {
		t.Fatalf("restore (the epoch change): %v", err)
	}

	// The stale capability must fail closed, and the value it would have returned must not be
	// returned: a value observed from an ended generation is not current.
	observation, err := t028ReadCurrent(ctx, root, grant, "k-epoch")
	if err == nil {
		t.Fatalf("a grant issued before the epoch reset still authorised a read: %+v", observation)
	}
	if !t028NeutralLockError(err) {
		t.Fatalf("the stale grant failed with %v, want one of the neutral locking errors", err)
	}
	if observation.Exists || observation.Tombstone || observation.HasChange || len(observation.Value) != 0 {
		t.Fatalf("the refused read returned a value: %+v", observation)
	}

	// The registry was reset with the generation, so the replaced transactions are no longer live.
	diagnostics, ok := storageengine.Engine(e).(storageengine.TransactionDiagnostics)
	if !ok {
		t.Fatal("the adapter engine does not report transaction diagnostics")
	}
	if stats := diagnostics.TransactionStats(); stats.ActiveRoot != 0 || stats.ActiveChildren != 0 {
		t.Fatalf("the generation change left live transactions behind: %+v", stats)
	}

	// The lock the old root held is gone, not merely refused: a fresh root acquires the same record
	// immediately and observes the restored database, in which the key was never published.
	fresh := t028Begin(t, e)
	_, freshGrant := t028Acquire(t, ctx, fresh, "k-epoch")
	if freshGrant.Generation == grant.Generation {
		t.Fatalf("the fresh grant reports the replaced generation %d, so the reset did not move the epoch", freshGrant.Generation)
	}
	freshObservation, err := t028ReadCurrent(ctx, fresh, freshGrant, "k-epoch")
	if err != nil {
		t.Fatalf("a fresh grant after the reset was refused: %v", err)
	}
	if freshObservation.Exists || freshObservation.Tombstone {
		t.Fatalf("the restored store still reports the replaced generation's write: %+v", freshObservation)
	}
	if err := fresh.Rollback(); err != nil {
		t.Fatalf("fresh rollback: %v", err)
	}
}

// t028NoLockTxn is a minimal transaction-shaped value that implements the neutral Txn contract and
// deliberately nothing else. It is the "backend without a lock manager" case as a caller sees it: the
// type has no locking methods at all, so there is no unlocked fallback that could be mistaken for a
// locked read.
type t028NoLockTxn struct{ id string }

var _ storageengine.Txn = t028NoLockTxn{}

func (t t028NoLockTxn) ID() string       { return t.id }
func (t t028NoLockTxn) Snapshot() uint64 { return 0 }
func (t t028NoLockTxn) Get(string, []byte) ([]byte, bool, error) {
	return nil, false, nil
}
func (t t028NoLockTxn) Put(string, []byte, []byte) error { return nil }
func (t t028NoLockTxn) Delete(string, []byte) error      { return nil }
func (t t028NoLockTxn) Guard(string, []byte) error       { return nil }
func (t t028NoLockTxn) GuardRange(string, storageengine.KeyRange) error {
	return nil
}
func (t t028NoLockTxn) NewIterator(context.Context, storageengine.ScanRequest) (storageengine.Iterator, error) {
	return nil, storageengine.ErrUnsupported
}
func (t t028NoLockTxn) Scan(context.Context, string, func([]byte, []byte) error) error {
	return storageengine.ErrUnsupported
}
func (t t028NoLockTxn) ScanRange(context.Context, string, storageengine.KeyRange, func([]byte, []byte) error) error {
	return storageengine.ErrUnsupported
}
func (t t028NoLockTxn) Table(id string) storageengine.Table { return storageengine.BindTable(t, id) }
func (t t028NoLockTxn) Child() (storageengine.Txn, error)   { return t, nil }
func (t t028NoLockTxn) Commit(context.Context) (uint64, error) {
	return 0, nil
}
func (t t028NoLockTxn) Rollback() error { return nil }

// t028TxnCapabilityProbe is the adapter's own transaction-level capability probe: an engine whose
// lock manager is absent answers "no capability" there. It is declared locally because the adapter's
// transaction type is unexported, so a black-box test can only reach the method through an interface
// it declares itself.
type t028TxnCapabilityProbe interface {
	LockingTxn() (storageengine.LockingTxn, bool)
}

// TestT028AnEngineWithoutALockManagerIsRefused covers requirement 5 (FR-026) at the neutral boundary.
//
// The property is fail-closed: a backend with no lock manager must report the capability as absent,
// so a caller never mistakes "no capability" for "no locks currently held" and never receives an
// unlocked read presented as a current read. The test also pins which of the two probes is
// authoritative for this backend, because they deliberately disagree and a caller has to know which
// one to trust.
//
// It complements storageengine/mvccadapter/locking_internal_test.go's
// TestT018LockCapabilityIsAbsentOnAnEngineWithoutALockManager, which builds an engine without a
// manager and asserts every locked path returns ErrUnsupported. That construction is not available
// from a black-box test — the engine's lock manager field is unexported and Open always attaches one —
// so this test pins the neutral probe's answer for a transaction that has no locking methods at all.
func TestT028AnEngineWithoutALockManagerIsRefused(t *testing.T) {
	// The neutral probe: a Txn that does not implement LockingTxn is refused explicitly, so absence is
	// reported rather than inferred from an empty result.
	tx := t028NoLockTxn{id: "t028-no-lock-txn"}
	if _, ok := storageengine.Txn(tx).(storageengine.LockingTxn); ok {
		t.Fatal("the no-lock transaction reports a locking capability, so the absence case would be untestable")
	}
	locking, err := storageengine.LockingTxnOf(tx)
	if !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("LockingTxnOf on a transaction without a lock manager = %v, want storageengine.ErrUnsupported", err)
	}
	if locking != nil {
		t.Fatalf("a refused capability probe returned a capability: %v", locking)
	}
	// The absence is what a caller must branch on: there is no locked path to call, so a locked
	// statement cannot silently be served by an unlocked read.
	if _, isLocking := storageengine.Txn(tx).(storageengine.LockingTxn); isLocking {
		t.Fatal("the no-lock transaction exposes a locked path")
	}

	// The adapter's own answer for the same question. Its type implements the neutral interface
	// unconditionally, so the assertion above is not authoritative for it: a caller that must
	// distinguish "this engine can lock" from "this engine has no lock manager" asks the adapter's own
	// probe (or the engine's Locking()), which is why it is declared above.
	adapterTxn := t028Begin(t, t028OpenAdapter(t))
	if _, ok := adapterTxn.(storageengine.LockingTxn); !ok {
		t.Fatal("an MVCC-backed transaction lost the neutral locking surface")
	}
	probe, ok := adapterTxn.(t028TxnCapabilityProbe)
	if !ok {
		t.Fatal("the adapter's transaction lost its own LockingTxn() capability probe, leaving no way to ask the authoritative question")
	}
	capability, present := probe.LockingTxn()
	if !present || capability == nil {
		t.Fatal("an engine opened with a lock manager reported the capability as absent")
	}
	// The capability is real, not a stub: it serves a locked read.
	if _, grantErr := capability.ReadCurrent(context.Background(), storageengine.LockGrant{}, t028Space, []byte("uncovered")); grantErr == nil {
		t.Fatal("the advertised capability authorised a read with no grant at all")
	}

	// The engine-level probe is absent on this backend. That is recorded rather than assumed: a caller
	// holding only the neutrally-typed engine handle cannot ask whether its backend can lock, so the
	// transaction-level probe above remains the authoritative answer for this adapter.
	if engineCapability, ok := storageengine.Engine(t028OpenAdapter(t)).(storageengine.LockingEngine); ok {
		if !engineCapability.Locking() {
			t.Fatal("an engine that advertises the locking capability reports Locking() == false")
		}
	}
}

// TestT028GenerationMismatchIsRefusedWhileTheCapabilityIsStillLive isolates requirement 7's
// generation rule from the epoch-reset case above.
//
// The distinction matters: the epoch-reset test proves a replaced generation drops its owners, while
// this one proves the *check itself* is real. A grant whose generation or term does not match the
// live registration is refused even though the owner is still registered and the row is still held,
// which is the only way to tell "the check ran and passed" from "the check never ran".
func TestT028GenerationMismatchIsRefusedWhileTheCapabilityIsStillLive(t *testing.T) {
	ctx := context.Background()
	e := t028OpenAdapter(t)
	t028Seed(t, e, "k-gen", "v1")

	root := t028Begin(t, e)
	_, grant := t028Acquire(t, ctx, root, "k-gen")

	// The genuine capability is accepted first, so the refusals below cannot be explained by an
	// unrelated failure of the transaction.
	if _, err := t028ReadCurrent(ctx, root, grant, "k-gen"); err != nil {
		t.Fatalf("the genuine grant was refused: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(storageengine.LockGrant) storageengine.LockGrant
	}{
		{"another generation", func(g storageengine.LockGrant) storageengine.LockGrant {
			g.Generation++
			return g
		}},
		{"another leader term", func(g storageengine.LockGrant) storageengine.LockGrant {
			g.Term++
			return g
		}},
	} {
		stale := test.mutate(grant)
		if stale.Generation == grant.Generation && stale.Term == grant.Term {
			t.Fatalf("%s: the mutation did not change the capability", test.name)
		}
		observation, err := t028ReadCurrent(ctx, root, stale, "k-gen")
		if err == nil {
			t.Fatalf("%s: a capability from another generation authorised a read: %+v", test.name, observation)
		}
		if !t028NeutralLockError(err) {
			t.Fatalf("%s: err = %v, want one of the neutral locking errors", test.name, err)
		}
		if observation.Exists || observation.HasChange || len(observation.Value) != 0 {
			t.Fatalf("%s: a refused read returned a value: %+v", test.name, observation)
		}
	}

	// After every refusal the live capability still works: a refused stale grant must not have
	// invalidated the owner or left the transaction unusable.
	if _, err := t028ReadCurrent(ctx, root, grant, "k-gen"); err != nil {
		t.Fatalf("the genuine grant stopped working after a stale one was refused: %v", err)
	}
}
