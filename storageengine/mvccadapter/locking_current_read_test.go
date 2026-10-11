package mvccadapter_test

import (
	"context"
	"errors"
	"gbaselite/storageengine"
	"testing"
)

// locking_current_read_test.go is the neutral-boundary half of the locked current read's
// authorization contract.
//
// The SQL layer only ever sees storageengine DTOs, so this is the boundary where a capability can
// be forged: a caller can hand `ReadCurrent` any LockGrant value it likes, including one carrying
// another transaction's owner ID, a reduced component list or a component list for a key the owner
// never locked. Every one of those must be refused with a neutral error, because accepting one
// would record an observation baseline for a row nothing protected and would let the following
// write commit over a row the transaction never locked.

// rowSpace is the namespace these tests write to.
const rowSpace = "rows/locking-current-read"

// acquireRows opens a statement scope and acquires one exclusive record lock plus its table
// intention, returning the neutral capability.
func acquireRows(t *testing.T, ctx context.Context, tx storageengine.Txn, key string) storageengine.LockGrant {
	t.Helper()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("locking capability: %v", err)
	}
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatalf("begin scope: %v", err)
	}
	grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{Components: []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: rowSpace, TableID: "t1"},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: rowSpace, TableID: "t1", Key: []byte(key)},
	}})
	if err != nil {
		t.Fatalf("acquire %s: %v", key, err)
	}
	return grant
}

// TestB02NeutralReadCurrentHonoursCoveringGrant proves the happy path through the neutral DTOs.
func TestB02NeutralReadCurrentHonoursCoveringGrant(t *testing.T) {
	ctx := context.Background()
	e := openAdapter(t, t.TempDir())

	writer := begin(t, e)
	if err := writer.Put(rowSpace, []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	commit(t, writer)

	reader := begin(t, e)
	locking, err := storageengine.LockingTxnOf(reader)
	if err != nil {
		t.Fatal(err)
	}
	grant := acquireRows(t, ctx, reader, "k1")
	if len(grant.Components) == 0 {
		t.Fatal("the neutral grant carries no components; a locked read could not be verified")
	}
	observation, err := locking.ReadCurrent(ctx, grant, rowSpace, []byte("k1"))
	if err != nil {
		t.Fatalf("a covering grant was refused: %v", err)
	}
	if !observation.Exists || string(observation.Value) != "v1" {
		t.Fatalf("observation = %+v, want v1", observation)
	}

	// A second, re-entrant acquisition must return an equally usable capability: the manager
	// reports the protection the owner already holds, components included.
	reentrant := acquireRows(t, ctx, reader, "k1")
	if len(reentrant.Components) == 0 {
		t.Fatal("the re-entrant neutral grant carries no components")
	}
	if _, err := locking.ReadCurrent(ctx, reentrant, rowSpace, []byte("k1")); err != nil {
		t.Fatalf("a re-entrant covering grant was refused: %v", err)
	}
}

// TestB02NeutralReadCurrentRefusesForgedCapabilities proves every reduced or fabricated capability
// fails closed with a neutral error rather than a lock-manager type.
func TestB02NeutralReadCurrentRefusesForgedCapabilities(t *testing.T) {
	ctx := context.Background()
	e := openAdapter(t, t.TempDir())

	writer := begin(t, e)
	for _, key := range []string{"k1", "k2"} {
		if err := writer.Put(rowSpace, []byte(key), []byte("v1")); err != nil {
			t.Fatal(err)
		}
	}
	commit(t, writer)

	reader := begin(t, e)
	other := begin(t, e)
	locking, err := storageengine.LockingTxnOf(reader)
	if err != nil {
		t.Fatal(err)
	}
	grant := acquireRows(t, ctx, reader, "k1")
	otherGrant := acquireRows(t, ctx, other, "k2")

	ownerOnly := grant
	ownerOnly.Components = nil
	wrongKey := grant
	wrongKey.Components = otherGrant.Components
	wrongOwner := grant
	wrongOwner.OwnerID = otherGrant.OwnerID
	staleGeneration := grant
	staleGeneration.Generation++
	staleTerm := grant
	staleTerm.Term++

	cases := []struct {
		name  string
		grant storageengine.LockGrant
		key   string
	}{
		{"reduced to an owner id", ownerOnly, "k1"},
		{"components for another key", wrongKey, "k1"},
		{"another live root's grant", wrongOwner, "k1"},
		{"another generation", staleGeneration, "k1"},
		{"another leader term", staleTerm, "k1"},
		{"a key the grant does not cover", grant, "k2"},
		{"a namespace the grant does not cover", grant, "k1"},
	}
	for _, test := range cases {
		key := []byte(test.key)
		space := rowSpace
		if test.name == "a namespace the grant does not cover" {
			space = rowSpace + "-other"
		}
		observation, err := locking.ReadCurrent(ctx, test.grant, space, key)
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("%s: err = %v (observation %+v), want storageengine.ErrConflict", test.name, err, observation)
		}
	}

	// The genuine capability still works, so the refusals above are about forgery.
	if _, err := locking.ReadCurrent(ctx, grant, rowSpace, []byte("k1")); err != nil {
		t.Fatalf("the genuine grant was refused: %v", err)
	}
}
