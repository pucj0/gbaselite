package mvccadapter_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_grant_test.go pins the neutral capability's identity across a real wait.
//
// The SQL layer only ever sees storageengine DTOs, so this is the boundary where a grant identity
// has to survive: the id the lock manager issued, the scope the request really came from, and
// exactly the components that request acquired. A grant rebuilt from the owner's registry instead
// would arrive here carrying every other lock the same transaction holds.
func TestB02NeutralWaitedGrantCarriesOnlyItsOwnComponents(t *testing.T) {
	ctx := context.Background()
	e := openAdapter(t, t.TempDir())

	seed := begin(t, e)
	for _, key := range []string{"k1", "k2"} {
		if err := seed.Put(rowSpace, []byte(key), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	commit(t, seed)

	// The blocker holds the row the waiter needs.
	blocker := begin(t, e)
	acquireRows(t, ctx, blocker, "k1")

	// The waiter already holds an unrelated row of its own, which must not appear in the grant it
	// receives for the row it has to wait for.
	waiter := begin(t, e)
	locking, err := storageengine.LockingTxnOf(waiter)
	if err != nil {
		t.Fatal(err)
	}
	acquireRows(t, ctx, waiter, "k2")
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeChild)
	if err != nil {
		t.Fatal(err)
	}

	components := []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: rowSpace, TableID: "t1"},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: rowSpace, TableID: "t1", Key: []byte("k1")},
	}
	queued := make(chan struct{}, 1)
	type outcome struct {
		grant storageengine.LockGrant
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{
			Components: components,
			// The neutral decision is consulted while the manager holds its own critical section,
			// so a signal from here proves the request is about to join the queue — before the
			// blocker's release can be processed.
			ShouldWait: func(context.Context) bool {
				select {
				case queued <- struct{}{}:
				default:
				}
				return true
			},
		})
		done <- outcome{grant: grant, err: err}
	}()

	select {
	case <-queued:
	case <-time.After(10 * time.Second):
		t.Fatal("the second acquisition never reached a conflict; the row lock did not block it")
	}
	// Ending the blocker's root releases its owner, which is what has to wake the waiter.
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}

	var grant storageengine.LockGrant
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("the woken acquisition failed: %v", out.err)
		}
		grant = out.grant
	case <-time.After(10 * time.Second):
		t.Fatal("the queued acquisition was never woken")
	}

	if grant.ID == 0 {
		t.Fatal("the neutral grant carries no id")
	}
	if grant.OwnerID != waiter.ID() {
		t.Fatalf("grant owner = %q, want the waiting root %q", grant.OwnerID, waiter.ID())
	}
	if grant.ScopeID != scope {
		t.Fatalf("grant scope = %d, want the requesting scope %d", grant.ScopeID, scope)
	}
	requireNeutralComponents(t, grant.Components,
		storageengine.LockComponent{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: rowSpace, TableID: "t1"},
		storageengine.LockComponent{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: rowSpace, TableID: "t1", Key: []byte("k1")},
	)
}

// neutralComponentKey renders one component as the identity a test compares against.
func neutralComponentKey(component storageengine.LockComponent) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s", component.Mode, component.Kind, component.Space, component.TableID, component.IndexID, string(component.Key))
}

// requireNeutralComponents asserts a neutral component list covers exactly the expected identities,
// ignoring order: the covered set is the contract, the order is not.
func requireNeutralComponents(t *testing.T, components []storageengine.LockComponent, want ...storageengine.LockComponent) {
	t.Helper()
	got := make([]string, 0, len(components))
	for _, component := range components {
		got = append(got, neutralComponentKey(component))
	}
	expected := make([]string, 0, len(want))
	for _, component := range want {
		expected = append(expected, neutralComponentKey(component))
	}
	sort.Strings(got)
	sort.Strings(expected)
	if len(got) != len(expected) {
		t.Fatalf("grant covers %v, want exactly %v", got, expected)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("grant covers %v, want exactly %v", got, expected)
		}
	}
}
