package mvcc

import (
	"context"
	"errors"
	"testing"

	"gbaselite/lockmanager"
)

// current_read_authorization_test.go is the deterministic authorization matrix of the locked
// current read.
//
// A locked current read is the one place where an observation becomes a *write baseline*: the
// version seen under the lock is what a later write to the same key is validated against. That
// makes the grant check load-bearing rather than a formality, so every way of presenting a
// capability that does not really cover the key is pinned here: a wrong key, a wrong space, a
// borrowed owner, a fabricated component list, a stale generation, an owner that already ended and
// a request that is still waiting. In every case the read must fail closed *and* leave no baseline
// behind, so the following write is still decided by the ordinary readTS rule.

// requireNoBaseline asserts that no observation baseline survives a refused read.
func requireNoBaseline(t *testing.T, tx *Tx, what string) {
	t.Helper()
	if tx.HasWriteBaseline() || tx.WriteBaselineSize() != 0 {
		t.Fatalf("%s recorded a write baseline (held=%d)", what, tx.WriteBaselineSize())
	}
}

// TestB02CurrentReadRejectsGrantForAnotherKey proves a capability is bound to the key it was
// issued for.
func TestB02CurrentReadRejectsGrantForAnotherKey(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	for _, key := range []string{"k1", "k2"} {
		if err := seed.Put("index/PRIMARY", []byte(key), []byte("v1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	reader := mustBegin(t, f.store)
	// k2 changes after the reader began, so an unobserved write to it must conflict.
	concurrent := mustBegin(t, f.store)
	if err := concurrent.Put("index/PRIMARY", []byte("k2"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := concurrent.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	grant := f.rowLock(ctx, reader, "k1", lockmanager.ModeX)
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant); err != nil {
		t.Fatalf("the covering grant was refused: %v", err)
	}
	// A different key under the same grant must be refused, even though the owner really holds a
	// lock and really is this transaction's root.
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k2"), grant); !errors.Is(err, ErrConflict) {
		t.Fatalf("a grant for k1 authorised a read of k2: err = %v, want ErrConflict", err)
	}
	// The refused read must not have authorised a write to k2 either.
	if err := reader.Put("index/PRIMARY", []byte("k2"), []byte("v3")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("a write after a refused read committed: err = %v, want ErrConflict", err)
	}
}

// TestB02CurrentReadRejectsGrantForAnotherSpace proves the namespace is part of the coverage: the
// same key bytes in another space are another resource.
func TestB02CurrentReadRejectsGrantForAnotherSpace(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	reader := mustBegin(t, f.store)
	grant := f.rowLock(ctx, reader, "k1", lockmanager.ModeX)
	if _, err := reader.ReadCurrentLocked(ctx, "index/SECONDARY", []byte("k1"), grant); !errors.Is(err, ErrConflict) {
		t.Fatalf("a grant in index/PRIMARY authorised a read in index/SECONDARY: err = %v, want ErrConflict", err)
	}
	requireNoBaseline(t, reader, "a read in a foreign space")
}

// TestB02CurrentReadRejectsReducedAndFabricatedGrants proves the capability cannot be reduced to an
// owner identity or invented from components the owner does not hold.
func TestB02CurrentReadRejectsReducedAndFabricatedGrants(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	reader := mustBegin(t, f.store)
	grant := f.rowLock(ctx, reader, "k1", lockmanager.ModeX)

	// An owner ID alone is not a capability, even when it names this very transaction.
	ownerOnly := lockmanager.LockGrant{OwnerID: reader.LockOwnerID()}
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), ownerOnly); !errors.Is(err, ErrConflict) {
		t.Fatalf("an owner-only grant authorised a read: err = %v, want ErrConflict", err)
	}
	requireNoBaseline(t, reader, "an owner-only grant")

	// A fabricated component list for a row this owner never locked is refused, even with the
	// owner, generation and term copied from a real grant.
	fabricated := lockmanager.LockGrant{
		OwnerID:    grant.OwnerID,
		Generation: grant.Generation,
		Term:       grant.Term,
		Components: []lockmanager.ResourceRef{recordRef("k9", lockmanager.ModeX)},
	}
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k9"), fabricated); !errors.Is(err, ErrConflict) {
		t.Fatalf("a fabricated grant authorised a read: err = %v, want ErrConflict", err)
	}
	requireNoBaseline(t, reader, "a fabricated grant")

	// A capability whose declared component is not the one the manager holds is refused too: the
	// grant below claims a shared lock where only the exclusive claim exists.
	downgraded := lockmanager.LockGrant{
		OwnerID:    grant.OwnerID,
		Generation: grant.Generation,
		Term:       grant.Term,
		Components: []lockmanager.ResourceRef{recordRef("k9", lockmanager.ModeS)},
	}
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k9"), downgraded); !errors.Is(err, ErrConflict) {
		t.Fatalf("a grant for an unheld resource authorised a read: err = %v, want ErrConflict", err)
	}

	// The genuine capability still works, so the refusals above are about the forgery and not
	// about the read being impossible.
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant); err != nil {
		t.Fatalf("the genuine grant was refused: %v", err)
	}
	if reader.WriteBaselineSize() != 1 {
		t.Fatalf("baselines = %d, want the single genuinely observed key", reader.WriteBaselineSize())
	}
}

// TestB02CurrentReadRejectsStaleGenerationAndTerm proves a capability from another generation or
// leader term is refused.
func TestB02CurrentReadRejectsStaleGenerationAndTerm(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A fresh store starts at generation 0, which would make the generation check vacuous. Moving
	// the store to a real generation first proves the check compares real values.
	f.store.generation.Store(7)

	reader := mustBegin(t, f.store)
	grant := f.rowLock(ctx, reader, "k1", lockmanager.ModeX)
	if grant.Generation != f.store.generation.Load() {
		t.Fatalf("the grant carries generation %d, want the owner's generation %d", grant.Generation, f.store.generation.Load())
	}

	staleGeneration := grant
	staleGeneration.Generation = grant.Generation + 1
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), staleGeneration); !errors.Is(err, ErrConflict) {
		t.Fatalf("a grant from another generation authorised a read: err = %v, want ErrConflict", err)
	}
	staleTerm := grant
	staleTerm.Term = grant.Term + 1
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), staleTerm); !errors.Is(err, ErrConflict) {
		t.Fatalf("a grant from another leader term authorised a read: err = %v, want ErrConflict", err)
	}
	requireNoBaseline(t, reader, "a stale grant")

	// Releasing the row lock the capability was issued for invalidates it, even though the root
	// is still registered and the grant's generation is still current.
	if _, err := f.manager.Release(reader.LockOwnerID(), recordRef("k1", lockmanager.ModeX)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant); !errors.Is(err, ErrConflict) {
		t.Fatalf("a released grant authorised a read: err = %v, want ErrConflict", err)
	}
	requireNoBaseline(t, reader, "a released grant")
}

// TestB02CurrentReadRejectsWrongOwner proves a grant belonging to another live root is refused,
// including when that root really holds the row.
func TestB02CurrentReadRejectsWrongOwner(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	holder := mustBegin(t, f.store)
	other := mustBegin(t, f.store)
	grant := f.rowLock(ctx, holder, "k1", lockmanager.ModeX)

	if _, err := other.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant); !errors.Is(err, ErrConflict) {
		t.Fatalf("another root's grant authorised a read: err = %v, want ErrConflict", err)
	}
	requireNoBaseline(t, other, "a foreign owner's grant")

	// The holder itself may read: the refusal above is about ownership, not about the resource.
	if _, err := holder.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant); err != nil {
		t.Fatalf("the holding root's own grant was refused: %v", err)
	}
	_ = holder.Rollback()
	_ = other.Rollback()
}

// TestB02CurrentReadRejectsAPendingRequest proves a request that is still waiting is not a lock: its
// claim exists but is not applied, so it may not authorise an observation baseline.
func TestB02CurrentReadRejectsAPendingRequest(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	for _, key := range []string{"k1", "k2"} {
		if err := seed.Put("index/PRIMARY", []byte(key), []byte("v1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	waiter := mustBegin(t, f.store)
	blocker := mustBegin(t, f.store)
	grant := f.rowLock(ctx, waiter, "k1", lockmanager.ModeX)
	_ = f.rowLock(ctx, blocker, "k2", lockmanager.ModeX)

	// The waiter asks for k1 (already held) and k2 (held by the blocker), so the bundle becomes one
	// pending request and holds nothing new.
	result, err := f.manager.Acquire(ctx, waiter.LockOwnerID(), waiter.lockScopeID, []lockmanager.ResourceRef{
		recordRef("k1", lockmanager.ModeX), recordRef("k2", lockmanager.ModeX), intentRef(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Op != lockmanager.LockOpWait || result.Wait == nil {
		t.Fatalf("the contended bundle op = %s, want WAIT", result.Op)
	}
	pending := lockmanager.LockGrant{
		OwnerID:    grant.OwnerID,
		Generation: grant.Generation,
		Term:       grant.Term,
		Components: []lockmanager.ResourceRef{recordRef("k2", lockmanager.ModeX)},
	}
	if _, err := waiter.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k2"), pending); !errors.Is(err, ErrConflict) {
		t.Fatalf("a pending request authorised a read: err = %v, want ErrConflict", err)
	}
	requireNoBaseline(t, waiter, "a pending request")

	// Once the blocker ends, the same capability covers k2 and the read succeeds.
	f.manager.ReleaseOwner(blocker.LockOwnerID())
	if _, waitErr := f.manager.WaitForGrant(ctx, waiter.LockOwnerID(), result.Wait); waitErr != nil {
		t.Fatalf("the woken request failed: %v", waitErr)
	}
	if _, err := waiter.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k2"), pending); err != nil {
		t.Fatalf("the applied claim was refused: %v", err)
	}
	if waiter.WriteBaselineSize() != 1 {
		t.Fatalf("baselines = %d, want the observed key to be recorded once", waiter.WriteBaselineSize())
	}
}

// TestB02UnlockedCurrentReadRecordsNoBaseline proves the unlocked observation path can never become
// a commit basis.
func TestB02UnlockedCurrentReadRecordsNoBaseline(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	reader := mustBegin(t, f.store)
	concurrent := mustBegin(t, f.store)
	if err := concurrent.Put("index/PRIMARY", []byte("k1"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := concurrent.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The unlocked current read really does observe the latest published version...
	observation, err := reader.ReadCurrentUnlocked(ctx, "index/PRIMARY", []byte("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(observation.Value) != "v2" {
		t.Fatalf("unlocked current read = %q, want v2", observation.Value)
	}
	// ...but it records nothing, so the write it might inform is still decided by readTS.
	requireNoBaseline(t, reader, "an unlocked current read")
	if err := reader.Put("index/PRIMARY", []byte("k1"), []byte("v3")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("a write informed by an unlocked read committed: err = %v, want ErrConflict", err)
	}
	if reader.State() != TransactionAborted {
		t.Fatalf("state = %s, want ABORTED", reader.State())
	}
}
