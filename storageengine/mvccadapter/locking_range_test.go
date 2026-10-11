package mvccadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking_range_test.go is T027 at the neutral boundary: a range one transaction protects over the
// storage's own encoding, and what it does to the insertions another transaction attempts inside
// and outside it.
//
// Everything here goes through the neutral capability in `package mvccadapter`, so the resources
// are the ones the engine really installs — the same Space and the same key bytes the write path
// uses — and the assertions are about the engine's own lock state (`e.locks`), not about a request
// having once been queued. A test that only checked "it waited" would pass on a manager that never
// woke anyone.

// t027Space and t027Key are the namespace and the protected lower bound every case uses: the range
// is [key, +inf), so a larger key is inside it and a smaller one is outside.
var t027Space = "row/t027"

var t027Key = []byte("m")

// t027Engine opens one engine with its lock manager attached.
func t027Engine(t *testing.T) *engine {
	t.Helper()
	e := t018OpenEngine(t)
	return e
}

// t027Begin starts one root transaction through the neutral engine API.
func t027Begin(t *testing.T, e *engine) storageengine.Txn {
	t.Helper()
	tx, err := e.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

// t027RangeBundle is the range claim: a Gap over [t027Key, +inf) in the row namespace.
func t027RangeBundle() []storageengine.LockComponent {
	return []storageengine.LockComponent{{
		Mode:           storageengine.LockModeGap,
		Kind:           storageengine.LockComponentInterval,
		Space:          t027Space,
		Lower:          t027Key,
		LowerInclusive: true,
	}}
}

// t027Acquire takes one bundle for one transaction and reports the outcome without failing: the
// outcome is what most of these cases assert.
func t027Acquire(t *testing.T, tx storageengine.Txn, scope uint64, components []storageengine.LockComponent) (storageengine.LockGrant, error) {
	t.Helper()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("no locking capability: %v", err)
	}
	return locking.Acquire(context.Background(), scope, storageengine.LockRequest{Components: components})
}

// TestT027RangeBlocksInsertionsInsideItOnly is the range half of FR-003/FR-005 through the adapter.
func TestT027RangeBlocksInsertionsInsideItOnly(t *testing.T) {
	e := t027Engine(t)

	guard := t027Begin(t, e)
	guardLocking, err := storageengine.LockingTxnOf(guard)
	if err != nil {
		t.Fatal(err)
	}
	guardScope, err := guardLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guardLocking.Acquire(context.Background(), guardScope, storageengine.LockRequest{Components: t027RangeBundle()}); err != nil {
		t.Fatalf("acquire the range: %v", err)
	}

	// The insertion claim: the intention at the exact key plus its record lock, which is what the
	// manager requires a caller to pair.
	insertion := func(key string) []storageengine.LockComponent {
		return []storageengine.LockComponent{
			{Mode: storageengine.LockModeInsertIntention, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte(key)},
			{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte(key)},
		}
	}

	// Inside the range: the caller is told to wait, and nothing is granted to it yet.
	inside := t027Begin(t, e)
	insideLocking, err := storageengine.LockingTxnOf(inside)
	if err != nil {
		t.Fatal(err)
	}
	insideScope, err := insideLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	waited := t027TrackWait(t, insideLocking, insideScope, insertion("n"))
	// The queued request is the engine's own state, and the key is not held by the waiter.
	t027AwaitPending(t, e, inside.ID(), 1)
	if _, held := e.locks.HeldMode(inside.ID(), lockmanager.ResourceRef{Identity: lockmanager.Identity{Kind: lockmanager.KindRecord, Mode: lockmanager.ModeX, Space: t027Space, Key: []byte("n")}}); held {
		t.Fatal("the waiting insertion already holds its record lock")
	}

	guardLocking, err = storageengine.LockingTxnOf(guard)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Rollback(); err != nil {
		t.Fatalf("release the range: %v", err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("the insertion inside the range failed: %v", err)
	}

	// Outside the range (below the lower bound): granted immediately, without ever waiting.
	outside := t027Begin(t, e)
	outsideLocking, err := storageengine.LockingTxnOf(outside)
	if err != nil {
		t.Fatal(err)
	}
	outsideScope, err := outsideLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if grant, err := t027Acquire(t, outside, outsideScope, insertion("a")); err != nil {
		t.Fatalf("an insertion outside the range failed: %v", err)
	} else if grant.ID == 0 {
		t.Fatal("an insertion outside the range was granted without a grant id")
	}
	if err := outside.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := inside.Rollback(); err != nil {
		t.Fatal(err)
	}
	if size := e.locks.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after every transaction ended, want 0", size)
	}
}

// TestT027InsertionsAtDifferentKeysInsideOneRangeDoNotSerialise proves the interval rule is about
// positions, not about one lock per namespace: without a range claim, two insertions at different
// keys are granted at once, and neither waits for the other's record lock.
func TestT027InsertionsAtDifferentKeysInsideOneRangeDoNotSerialise(t *testing.T) {
	e := t027Engine(t)

	holder := t027Begin(t, e)
	holderLocking, err := storageengine.LockingTxnOf(holder)
	if err != nil {
		t.Fatal(err)
	}
	holderScope, err := holderLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := t027Acquire(t, holder, holderScope, []storageengine.LockComponent{
		{Mode: storageengine.LockModeInsertIntention, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("n")},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("n")},
	}); err != nil {
		t.Fatalf("the first insertion: %v", err)
	}

	// A second insertion at another key is not blocked by the first one's claim: the intention is
	// compatible with another intention, and the record locks are on different keys.
	second := t027Begin(t, e)
	secondLocking, err := storageengine.LockingTxnOf(second)
	if err != nil {
		t.Fatal(err)
	}
	secondScope, err := secondLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if grant, err := t027Acquire(t, second, secondScope, []storageengine.LockComponent{
		{Mode: storageengine.LockModeInsertIntention, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("t")},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("t")},
	}); err != nil {
		t.Fatalf("a second insertion at a different key was serialised: %v", err)
	} else if grant.ID == 0 {
		t.Fatal("the second insertion was granted without a grant id")
	}

	// The same key, however, is one claim: the third transaction has to wait for the first.
	same := t027Begin(t, e)
	sameLocking, err := storageengine.LockingTxnOf(same)
	if err != nil {
		t.Fatal(err)
	}
	sameScope, err := sameLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	waited := t027TrackWait(t, sameLocking, sameScope, []storageengine.LockComponent{
		{Mode: storageengine.LockModeInsertIntention, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("n")},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("n")},
	})
	t027AwaitPending(t, e, same.ID(), 1)
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("the same-key insertion failed after the holder ended: %v", err)
	}
	_ = second.Rollback()
	_ = same.Rollback()
	if size := e.locks.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after every transaction ended, want 0", size)
	}
}

// TestT027RangeBundleIsAtomic proves a Next-Key style bundle is granted as a whole: while one of
// its components conflicts, none of them is held, so a caller can never run with half the claim.
func TestT027RangeBundleIsAtomic(t *testing.T) {
	e := t027Engine(t)

	// One transaction holds the insertion intention at "n", which is inside the range below.
	blocker := t027Begin(t, e)
	blockerLocking, err := storageengine.LockingTxnOf(blocker)
	if err != nil {
		t.Fatal(err)
	}
	blockerScope, err := blockerLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := t027Acquire(t, blocker, blockerScope, []storageengine.LockComponent{
		{Mode: storageengine.LockModeInsertIntention, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("n")},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("n")},
	}); err != nil {
		t.Fatalf("the blocker: %v", err)
	}

	// The Next-Key bundle: the range [m, +inf) plus the record lock of an *unrelated* key "z". The
	// interval conflicts with the held intention at "n"; the record lock does not conflict with
	// anything. A partial grant would leave the caller holding "z" while its interval waits.
	nextKey := t027Begin(t, e)
	nextKeyLocking, err := storageengine.LockingTxnOf(nextKey)
	if err != nil {
		t.Fatal(err)
	}
	nextKeyScope, err := nextKeyLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	waited := t027TrackWait(t, nextKeyLocking, nextKeyScope, []storageengine.LockComponent{
		t027RangeBundle()[0],
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("z")},
	})
	t027AwaitPending(t, e, nextKey.ID(), 1)
	for _, key := range []string{"z"} {
		if _, held := e.locks.HeldMode(nextKey.ID(), lockmanager.ResourceRef{Identity: lockmanager.Identity{Kind: lockmanager.KindRecord, Mode: lockmanager.ModeX, Space: t027Space, Key: []byte(key)}}); held {
			t.Fatalf("the bundle was partially granted: the record lock on %q is held while the interval waits", key)
		}
	}
	// The interval is not held either.
	if _, held := e.locks.HeldMode(nextKey.ID(), lockmanager.ResourceRef{Identity: lockmanager.Identity{Kind: lockmanager.KindInterval, Mode: lockmanager.ModeGap, Space: t027Space, Lower: t027Key, LowerInclusive: true}}); held {
		t.Fatal("the bundle was partially granted: the interval is held while its own request waits")
	}

	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("the Next-Key bundle failed after the blocker ended: %v", err)
	}
	// Now the whole bundle is held, both halves at once.
	if _, held := e.locks.HeldMode(nextKey.ID(), lockmanager.ResourceRef{Identity: lockmanager.Identity{Kind: lockmanager.KindRecord, Mode: lockmanager.ModeX, Space: t027Space, Key: []byte("z")}}); !held {
		t.Fatal("the woken bundle does not hold its record lock")
	}
	_ = nextKey.Rollback()
	if size := e.locks.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after every transaction ended, want 0", size)
	}
}

// TestT027CancelledRangeWaitLeavesNothingBehind proves a cancelled wait for a range is not a leak:
// the caller learns the context ended, the range is still held by its owner, and the waiter leaves
// no claim, request or charge behind.
func TestT027CancelledRangeWaitLeavesNothingBehind(t *testing.T) {
	e := t027Engine(t)

	guard := t027Begin(t, e)
	guardLocking, err := storageengine.LockingTxnOf(guard)
	if err != nil {
		t.Fatal(err)
	}
	guardScope, err := guardLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := t027Acquire(t, guard, guardScope, t027RangeBundle()); err != nil {
		t.Fatalf("acquire the range: %v", err)
	}

	waiter := t027Begin(t, e)
	waiterLocking, err := storageengine.LockingTxnOf(waiter)
	if err != nil {
		t.Fatal(err)
	}
	waiterScope, err := waiterLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = waiterLocking.Acquire(ctx, waiterScope, storageengine.LockRequest{Components: []storageengine.LockComponent{
		{Mode: storageengine.LockModeInsertIntention, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("n")},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t027Space, Key: []byte("n")},
	}})
	if err == nil {
		t.Fatal("an insertion inside a protected range was granted under a context that had already expired")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, storageengine.ErrLockWaitTimeout) && !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the cancelled wait returned %v, want a deadline or lock error", err)
	}
	// The waiter holds nothing and has nothing queued; the holder still owns its range.
	if pending := e.locks.PendingRequests(waiter.ID()); pending != 0 {
		t.Fatalf("the cancelled waiter left %d queued requests", pending)
	}
	if _, held := e.locks.HeldMode(waiter.ID(), lockmanager.ResourceRef{Identity: lockmanager.Identity{Kind: lockmanager.KindRecord, Mode: lockmanager.ModeX, Space: t027Space, Key: []byte("n")}}); held {
		t.Fatal("the cancelled waiter holds the record lock")
	}
	if _, held := e.locks.HeldMode(guard.ID(), lockmanager.ResourceRef{Identity: lockmanager.Identity{Kind: lockmanager.KindInterval, Mode: lockmanager.ModeGap, Space: t027Space, Lower: t027Key, LowerInclusive: true}}); !held {
		t.Fatal("the cancelled wait released the range its owner still holds")
	}
	// The waiter can still be ended normally, and the whole engine returns to idle.
	if err := waiter.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Rollback(); err != nil {
		t.Fatal(err)
	}
	if size := e.locks.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d after every transaction ended, want 0", size)
	}
	if stats := e.locks.Stats(); stats.PendingRequests != 0 || stats.WaitEdges != 0 || stats.ActiveOwners != 0 {
		t.Fatalf("lock statistics after cleanup = %+v, want an idle manager", stats)
	}
}

// t027TrackWait starts one acquisition in another goroutine and reports when it can legitimately be
// waited for: it hands the acquisition a `ShouldWait` decision that signals exactly when the
// manager found a conflict and is about to queue the request, and it returns a channel carrying the
// acquisition's final error.
//
// Signalling from the decision is what makes "it waited" an observed fact: the manager consults it
// under its own critical section, after the conflict was found and before the request joins the
// queue, so a signal proves the request was queued rather than merely slow.
func t027TrackWait(t *testing.T, locking storageengine.LockingTxn, scope uint64, components []storageengine.LockComponent) <-chan error {
	t.Helper()
	ready := make(chan struct{})
	once := make(chan struct{}, 1)
	outcome := make(chan error, 1)
	go func() {
		_, err := locking.Acquire(context.Background(), scope, storageengine.LockRequest{
			Components: components,
			ShouldWait: func(context.Context) bool {
				select {
				case once <- struct{}{}:
				default:
				}
				return true
			},
		})
		outcome <- err
	}()
	select {
	case <-once:
	case <-time.After(10 * time.Second):
		t.Fatal("the acquisition never reported a conflict; it did not wait")
	}
	close(ready)
	return outcome
}

// t027AwaitPending blocks until one root has exactly the expected number of queued requests, which
// is the engine's own view of the wait rather than an inference from timing.
func t027AwaitPending(t *testing.T, e *engine, rootID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if e.locks.PendingRequests(rootID) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has %d queued requests, want %d", rootID, e.locks.PendingRequests(rootID), want)
		}
		time.Sleep(time.Millisecond)
	}
}
