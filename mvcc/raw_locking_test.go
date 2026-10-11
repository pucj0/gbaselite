package mvcc

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/lockmanager"
)

// raw_locking_test.go is T019: the raw write entry points take the record lock their key needs, on
// the same resource the SQL write path uses, before anything is staged.
//
// What it pins, in the order the acceptance criteria state it:
//
//   - a raw writer cannot bypass another root's record X on the same key;
//   - the SQL-shaped component (with the table and index labels the executor attaches) and the raw
//     component (with no labels) are one resource, so either side can block the other;
//   - `Get` is untouched: it stays a snapshot read and never waits;
//   - the wait is bounded and cancellable through the manager's own lifecycle (epoch reset), and it
//     never holds a persistence latch: another root can still write an unrelated key;
//   - the lock is an exact-key record X only. Gap, missing-key and insert-intention protection for
//     the interval around a raw write is Phase 4 (T024) and is deliberately not claimed here.
//
// Blocking is observed through the manager's queue state and through a clock that reports the
// moment a caller arms its wait deadline, so no assertion depends on how long something took.

// armedClock is the fixture clock plus a signal for every wait deadline the manager arms.
//
// The manager arms the deadline immediately before a caller blocks on its request, so a signal from
// here proves that the caller is about to wait. That is the barrier these tests need, and it needs
// no hook inside production code: the clock is an injected dependency of the manager.
type armedClock struct {
	*lockmanager.FakeClock
	armed chan struct{}
}

func newArmedClock() *armedClock {
	return &armedClock{
		FakeClock: lockmanager.NewFakeClock(time.Unix(1750000000, 0)),
		armed:     make(chan struct{}, 16),
	}
}

// After reports the arming of one wait deadline and then behaves like the fake clock.
func (c *armedClock) After(deadline time.Time) <-chan time.Time {
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return c.FakeClock.After(deadline)
}

// rawLockFixture is one store with a lock manager and a clock whose waits are observable.
type rawLockFixture struct {
	t       *testing.T
	store   *Store
	manager *lockmanager.Manager
	clock   *armedClock
}

func newRawLockFixture(t *testing.T) *rawLockFixture {
	t.Helper()
	store, err := OpenWithOptions(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clock := newArmedClock()
	manager, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, clock)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(manager.Close)
	if _, err := store.AttachLocks(manager); err != nil {
		t.Fatalf("attach: %v", err)
	}
	return &rawLockFixture{t: t, store: store, manager: manager, clock: clock}
}

// begin starts one root transaction.
func (f *rawLockFixture) begin() *Tx {
	f.t.Helper()
	tx, err := f.store.Begin(context.Background(), nil)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	return tx
}

// awaitWait blocks until a caller armed its wait deadline, which is the moment before it blocks.
func (f *rawLockFixture) awaitWait(what string) {
	f.t.Helper()
	select {
	case <-f.clock.armed:
	case <-time.After(5 * time.Second):
		f.t.Fatalf("%s never started to wait", what)
	}
}

// awaitPending blocks until one root has exactly the expected number of queued requests.
//
// It is the barrier for the callers that use the two-step API: the queue entry is the conflict, so
// waiting for the manager to report it is an observation, and the polling interval is not what
// establishes the order. A request that never queues fails the test instead of hanging it.
func (f *rawLockFixture) awaitPending(rootID string, want int) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if f.manager.PendingRequests(rootID) == want {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%s has %d queued requests, want %d", rootID, f.manager.PendingRequests(rootID), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// requireIdle asserts that no owner, request, grant, edge or charge survived.
func (f *rawLockFixture) requireIdle() {
	f.t.Helper()
	if size := f.manager.LockTableSize(); size != 0 {
		f.t.Fatalf("lock table holds %d resources after cleanup, want 0", size)
	}
	stats := f.manager.Stats()
	if stats.ActiveOwners != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 {
		f.t.Fatalf("lock statistics after cleanup = %+v, want an idle manager", stats)
	}
	totals := f.manager.Ledger().Totals()
	if totals.Components != 0 || totals.Waiters != 0 || totals.MemoryBytes != 0 {
		f.t.Fatalf("budget after cleanup = %+v, want nothing charged", totals)
	}
}

// rawSpace and rawKey are the namespace and key every case in this file writes.
const rawSpace = "row/t1"

var rawKey = []byte("k1")

// gapRef is the interval component another root holds to protect an insertion range.
func gapRef(space, lower, upper string) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:  lockmanager.KindInterval,
		Mode:  lockmanager.ModeGap,
		Space: space,
		Lower: []byte(lower),
		Upper: []byte(upper),
	}}
}

// iiRef is the insert intention one insertion claim carries for its own key.
func iiRef(space string, key []byte) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:  lockmanager.KindRecord,
		Mode:  lockmanager.ModeInsertIntention,
		Space: space,
		Key:   key,
	}}
}

// xRef is the exclusive record lock of one key.
func xRef(space string, key []byte) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:  lockmanager.KindRecord,
		Mode:  lockmanager.ModeX,
		Space: space,
		Key:   key,
	}}
}

// TestB02RawInsertWaitsForAGapCoveringItsInsertionPoint is the T024 acceptance for a missing key.
//
// A raw Put of a key that does not exist is an insertion, so it must be blocked by another
// transaction's Gap over that insertion point. The record lock alone would not do it: the interval
// rule is independent of the record rule, and a Gap deliberately does not conflict with a record,
// so the intention is what carries the containment question. The test observes the wait through the
// injected clock (the manager arms the wait deadline immediately before the caller blocks) and then
// the wake-up through the gap holder ending, not through a timeout.
func TestB02RawInsertWaitsForAGapCoveringItsInsertionPoint(t *testing.T) {
	f := newRawLockFixture(t)

	// Another root protects the range the insertion would land in.
	gapHolder := f.begin()
	if _, op, _, err := gapHolder.AcquireLock(context.Background(), gapHolder.lockScopeID, []lockmanager.ResourceRef{gapRef(rawSpace, "a", "z")}, nil); err != nil {
		t.Fatalf("acquire the gap: %v", err)
	} else if op != lockmanager.LockOpGranted {
		t.Fatalf("the gap acquisition op = %s, want GRANTED", op)
	}

	// The key "m" has never existed, so this write is an insertion inside the gap.
	inserter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- inserter.Put(rawSpace, []byte("m"), []byte("new")) }()
	f.awaitWait("the raw insert inside the gap")
	select {
	case err := <-outcome:
		t.Fatalf("the raw insert returned (%v) instead of waiting for the covering gap", err)
	default:
	}
	if pending := f.manager.PendingRequests(inserter.ID); pending != 1 {
		t.Fatalf("the inserting root has %d queued requests, want 1", pending)
	}

	if err := gapHolder.Rollback(); err != nil {
		t.Fatalf("gap holder rollback: %v", err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken insert failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the insert inside the gap was never woken")
	}
	if _, err := inserter.Commit(context.Background()); err != nil {
		t.Fatalf("the woken insert could not commit: %v", err)
	}
	f.requireIdle()
}

// TestB02RawInsertOutsideAGapDoesNotWait is the other half of FR-005: only the protected positions
// wait, so two transactions inserting into different parts of one table never serialise.
func TestB02RawInsertOutsideAGapDoesNotWait(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	gapHolder := f.begin()
	if _, op, _, err := gapHolder.AcquireLock(ctx, gapHolder.lockScopeID, []lockmanager.ResourceRef{gapRef(rawSpace, "a", "z")}, nil); err != nil {
		t.Fatalf("acquire the gap: %v", err)
	} else if op != lockmanager.LockOpGranted {
		t.Fatalf("the gap acquisition op = %s, want GRANTED", op)
	}

	// "zz" sorts after the open upper bound, so the insertion is outside the protected range and
	// must be granted while the gap is still held. A wait would show up as the arming of a deadline.
	outside := f.begin()
	if err := outside.Put(rawSpace, []byte("zz"), []byte("new")); err != nil {
		t.Fatalf("an insert outside the gap waited or failed: %v", err)
	}
	select {
	case <-f.clock.armed:
		t.Fatal("an insert outside the gap armed a wait deadline")
	default:
	}
	if _, err := outside.Commit(ctx); err != nil {
		t.Fatalf("the outside insert could not commit: %v", err)
	}

	// Two insertions in the free part of the namespace proceed in parallel as well: one holding its
	// record lock must not stop the other at a different key. Both keys sort after the gap's open
	// upper bound on purpose - a key inside the range would legitimately wait.
	first := f.begin()
	if err := first.Put(rawSpace, []byte("zz1"), []byte("a")); err != nil {
		t.Fatal(err)
	}
	second := f.begin()
	if err := second.Put(rawSpace, []byte("zz2"), []byte("b")); err != nil {
		t.Fatalf("a second insert at a different key waited: %v", err)
	}
	_ = first.Rollback()
	_ = second.Rollback()
	_ = gapHolder.Rollback()
	f.requireIdle()
}

// TestB02RawInsertOfAnExistingKeyTakesOnlyTheRecordLock proves the classification the other way
// round: a key that already exists is not an insertion, so a Gap over its position must NOT block
// the write: the gap rule and the record rule are independent.
func TestB02RawInsertOfAnExistingKeyTakesOnlyTheRecordLock(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	seed := f.begin()
	if err := seed.Put(rawSpace, []byte("m"), []byte("published")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	gapHolder := f.begin()
	if _, op, _, err := gapHolder.AcquireLock(ctx, gapHolder.lockScopeID, []lockmanager.ResourceRef{gapRef(rawSpace, "a", "z")}, nil); err != nil {
		t.Fatalf("acquire the gap: %v", err)
	} else if op != lockmanager.LockOpGranted {
		t.Fatalf("the gap acquisition op = %s, want GRANTED", op)
	}

	// The key exists, so the write is an update of that row: it takes the record lock and must not
	// be held up by the interval another root protects.
	updater := f.begin()
	if err := updater.Put(rawSpace, []byte("m"), []byte("updated")); err != nil {
		t.Fatalf("an update of an existing key waited behind a gap: %v", err)
	}
	select {
	case <-f.clock.armed:
		t.Fatal("an update of an existing key armed a wait deadline")
	default:
	}

	// The same-key claim is still exclusive: another root inserting that exact key must wait.
	other := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- other.Put(rawSpace, []byte("m"), []byte("other")) }()
	f.awaitWait("the competing write of the same key")
	if err := updater.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the competing write failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the competing write of the same key was never woken")
	}
	_ = other.Rollback()
	_ = gapHolder.Rollback()
	f.requireIdle()
}

// TestB02RawDeleteThenReinsertIsAnInsertion proves the classification follows this transaction's own
// overlay rather than the published state: a key this transaction deleted and is re-inserting is an
// insertion, so a covering Gap blocks it.
func TestB02RawDeleteThenReinsertIsAnInsertion(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	seed := f.begin()
	if err := seed.Put(rawSpace, []byte("m"), []byte("published")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The re-inserting transaction deletes the row first, while no gap protects it.
	rewriter := f.begin()
	if err := rewriter.Delete(rawSpace, []byte("m")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if pending := f.manager.PendingRequests(rewriter.ID); pending != 0 {
		t.Fatalf("deleting an existing key queued %d requests, want 0", pending)
	}

	// Now another root protects the range. Re-inserting the key this transaction deleted is an
	// insertion for this transaction - its own overlay says the key is gone - so a covering Gap must
	// block it even though the published state still has a version for it.
	gapHolder := f.begin()
	if _, op, _, err := gapHolder.AcquireLock(ctx, gapHolder.lockScopeID, []lockmanager.ResourceRef{gapRef(rawSpace, "a", "z")}, nil); err != nil {
		t.Fatalf("acquire the gap: %v", err)
	} else if op != lockmanager.LockOpGranted {
		t.Fatalf("the gap acquisition op = %s, want GRANTED", op)
	}

	outcome := make(chan error, 1)
	go func() { outcome <- rewriter.Put(rawSpace, []byte("m"), []byte("reinserted")) }()
	f.awaitWait("the re-insert of a key this transaction deleted")
	if err := gapHolder.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken re-insert failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the re-insert of a deleted key was never woken")
	}
	if _, err := rewriter.Commit(ctx); err != nil {
		t.Fatalf("the woken re-insert could not commit: %v", err)
	}
	f.requireIdle()
}

// TestB02RawDeleteOfAMissingKeyRespectsTheGap proves a Delete of a key that does not exist is the
// same claim as an insert at that position.
//
// Refusing to take the intention here would make a delete of a missing row the one write that walks
// through another transaction's range protection; taking it means the delete waits for the range to
// be released, exactly as an insert would.
func TestB02RawDeleteOfAMissingKeyRespectsTheGap(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	gapHolder := f.begin()
	if _, op, _, err := gapHolder.AcquireLock(ctx, gapHolder.lockScopeID, []lockmanager.ResourceRef{gapRef(rawSpace, "a", "z")}, nil); err != nil {
		t.Fatalf("acquire the gap: %v", err)
	} else if op != lockmanager.LockOpGranted {
		t.Fatalf("the gap acquisition op = %s, want GRANTED", op)
	}

	deleter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- deleter.Delete(rawSpace, []byte("m")) }()
	f.awaitWait("the raw delete of a missing key inside the gap")
	if err := gapHolder.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken delete failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delete of a missing key was never woken")
	}
	_ = deleter.Rollback()
	f.requireIdle()
}

// TestB02RawWaitRefusesToStageAfterTheGenerationMoved proves a queued write cannot slip into a
// transaction whose world has moved on while it waited.
//
// The waiter is granted (the blocker really ended), so the wait itself succeeds; what must refuse
// the write is the transaction's own liveness, re-checked after the wait. Without that check the
// statement would stage a write into a world that no longer exists and only fail at commit, which is
// exactly the "an invalidated transaction must not keep writing" rule.
func TestB02RawWaitRefusesToStageAfterTheGenerationMoved(t *testing.T) {
	f := newRawLockFixture(t)

	holder := f.begin()
	if err := holder.Put(rawSpace, rawKey, []byte("holder")); err != nil {
		t.Fatalf("the holder write: %v", err)
	}

	waiter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- waiter.Put(rawSpace, rawKey, []byte("waiter")) }()
	f.awaitWait("the raw write that will outlive its generation")

	// The generation moves on while the caller is queued. The manager is deliberately not told, so
	// the request stays queued and is granted normally; the refusal has to come from the transaction
	// layer's own re-check.
	f.store.generation.Add(1)
	f.manager.ReleaseOwner(holder.ID)

	select {
	case err := <-outcome:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("the queued write returned %v, want ErrClosed for a transaction whose generation moved on", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued write was never decided")
	}
	// Nothing was staged: no buffered op and no staging database. A base error alone would not prove
	// it — the write could have been buffered first and refused later.
	if len(waiter.buffered) != 0 || waiter.stage != nil {
		t.Fatalf("an invalidated write left %d buffered ops (stage open: %v)", len(waiter.buffered), waiter.stage != nil)
	}
	// The granted request is still held: this test bypassed the reconcile path on purpose (a real
	// generation change goes through Store.Restore, which calls ResetLockEpoch and ends every owner
	// of the old generation), so the owner is ended here the way that path would.
	f.manager.ReleaseOwner(waiter.ID)
	f.requireIdle()
}

// TestB02RawInsertionComponentsPairTheIntentionWithItsRecord proves the request shape the raw path
// builds, without going through the manager: a missing key needs both halves of the claim, and an
// existing key needs only the record lock.
func TestB02RawInsertionComponentsPairTheIntentionWithItsRecord(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	missing := f.begin()
	components, err := missing.rawWriteLockComponents(Op{Space: rawSpace, Key: []byte("fresh")})
	if err != nil {
		t.Fatalf("missing key components: %v", err)
	}
	if len(components) != 2 {
		t.Fatalf("a missing key produced %d components, want the intention and its record lock: %+v", len(components), components)
	}
	if components[0].Mode != lockmanager.ModeInsertIntention || components[1].Mode != lockmanager.ModeX {
		t.Fatalf("a missing key produced modes %q/%q, want INSERT_INTENTION/X", components[0].Mode, components[1].Mode)
	}
	if !bytes.Equal(components[0].Key, []byte("fresh")) || !bytes.Equal(components[1].Key, []byte("fresh")) {
		t.Fatal("the intention and the record lock must name the same key")
	}
	// The manager's own bundle validation must accept the pair: the intention is legal exactly
	// because it travels with its row claim.
	if missing := lockmanager.MissingRowClaims(components); len(missing) != 0 {
		t.Fatalf("the raw insertion bundle is missing a row claim for %q", missing)
	}

	seed := f.begin()
	if err := seed.Put(rawSpace, []byte("present"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	existing := f.begin()
	components, err = existing.rawWriteLockComponents(Op{Space: rawSpace, Key: []byte("present")})
	if err != nil {
		t.Fatalf("existing key components: %v", err)
	}
	if len(components) != 1 || components[0].Mode != lockmanager.ModeX {
		t.Fatalf("an existing key produced %+v, want only the record X", components)
	}

	// A key this transaction deleted and is re-inserting is classified as missing again.
	deleter := f.begin()
	if err := deleter.Delete(rawSpace, []byte("present")); err != nil {
		t.Fatal(err)
	}
	components, err = deleter.rawWriteLockComponents(Op{Space: rawSpace, Key: []byte("present")})
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 || components[0].Mode != lockmanager.ModeInsertIntention {
		t.Fatalf("a deleted-and-reinserted key produced %+v, want an insertion claim", components)
	}
	_ = missing.Rollback()
	_ = existing.Rollback()
	_ = deleter.Rollback()
	f.requireIdle()
}

// sqlShapedRecord is the component the SQL executor takes for one row of that namespace: the same
// space and key, plus the table and index labels it knows and a raw caller does not.
func sqlShapedRecord() lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:    lockmanager.KindRecord,
		Mode:    lockmanager.ModeX,
		Space:   rawSpace,
		TableID: "t1",
		IndexID: "",
		Key:     rawKey,
	}}
}

// sqlShapedBundle is what the executor actually asks for: the record plus the table intention that
// a labelled record requires. The intention is required *because* the component carries a table
// label 閳?a raw write has no label and therefore needs none, which is the asymmetry this file
// exists to prove does not weaken either side.
func sqlShapedBundle() []lockmanager.ResourceRef {
	return []lockmanager.ResourceRef{
		sqlShapedRecord(),
		{Identity: lockmanager.Identity{
			Kind:    lockmanager.KindTableIntent,
			Mode:    lockmanager.ModeIX,
			Space:   rawSpace,
			TableID: "t1",
		}},
	}
}

// TestB02RawPutWaitsForAnotherRootsRecordLock is the core of the acceptance: a raw Put on a key
// another root holds exclusively must wait, and must be woken by that root's end.
func TestB02RawPutWaitsForAnotherRootsRecordLock(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	holder := f.begin()
	if err := holder.Put(rawSpace, rawKey, []byte("holder")); err != nil {
		t.Fatalf("the first raw write: %v", err)
	}

	waiter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- waiter.Put(rawSpace, rawKey, []byte("waiter")) }()

	f.awaitWait("the second raw write")
	select {
	case err := <-outcome:
		t.Fatalf("the second raw write returned (%v) instead of waiting for the holder", err)
	default:
	}
	if pending := f.manager.PendingRequests(waiter.ID); pending != 1 {
		t.Fatalf("the waiting root has %d queued requests, want 1", pending)
	}

	// The holder ending releases its owner, which is what has to wake the waiter.
	if err := holder.Rollback(); err != nil {
		t.Fatalf("holder rollback: %v", err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken raw write failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting raw write was never woken")
	}
	if _, err := waiter.Commit(ctx); err != nil {
		t.Fatalf("the woken raw write could not commit: %v", err)
	}
	f.requireIdle()
}

// TestB02RawDeleteTakesTheSameRecordLock proves Delete participates exactly like Put: it is the same
// write entry point and the same record identity.
func TestB02RawDeleteTakesTheSameRecordLock(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	seed := f.begin()
	if err := seed.Put(rawSpace, rawKey, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	deleter := f.begin()
	if err := deleter.Delete(rawSpace, rawKey); err != nil {
		t.Fatalf("the raw delete: %v", err)
	}

	waiter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- waiter.Put(rawSpace, rawKey, []byte("after-delete")) }()
	f.awaitWait("the raw write behind a raw delete")
	if err := deleter.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken raw write failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the raw write behind a raw delete was never woken")
	}
	_ = waiter.Rollback()
	f.requireIdle()
}

// TestB02RawWriteAndSQLShapedLockShareOneIdentity proves the two layers address one resource.
//
// The SQL component carries the table and index labels and the raw component does not; record
// identity is (Generation, Space, Key), so the labels cannot split the resource. Each direction is
// asserted: SQL holds and the raw write waits, then raw holds and the SQL-shaped request waits.
func TestB02RawWriteAndSQLShapedLockShareOneIdentity(t *testing.T) {
	t.Run("a raw write waits for the SQL-shaped lock", func(t *testing.T) {
		f := newRawLockFixture(t)
		ctx := context.Background()

		sqlRoot := f.begin()
		if _, op, _, err := sqlRoot.AcquireLock(ctx, sqlRoot.lockScopeID, sqlShapedBundle(), nil); err != nil {
			t.Fatalf("the SQL-shaped acquisition: %v", err)
		} else if op != lockmanager.LockOpGranted {
			t.Fatalf("the SQL-shaped acquisition op = %s, want GRANTED", op)
		}

		rawRoot := f.begin()
		outcome := make(chan error, 1)
		go func() { outcome <- rawRoot.Put(rawSpace, rawKey, []byte("raw")) }()
		f.awaitWait("the raw write behind a SQL-shaped lock")
		if err := sqlRoot.Rollback(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-outcome:
			if err != nil {
				t.Fatalf("the woken raw write failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the raw write behind a SQL-shaped lock was never woken")
		}
		_ = rawRoot.Rollback()
		f.requireIdle()
	})

	t.Run("the SQL-shaped lock waits for a raw write", func(t *testing.T) {
		f := newRawLockFixture(t)
		ctx := context.Background()

		rawRoot := f.begin()
		if err := rawRoot.Put(rawSpace, rawKey, []byte("raw")); err != nil {
			t.Fatalf("the raw write: %v", err)
		}

		sqlRoot := f.begin()
		// The acquisition is one call, so it is driven from another goroutine and the wait is
		// observed through the clock before the holder ends.
		type outcome struct {
			op  lockmanager.LockOp
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			// The two halves are what a caller does with the raw API: ask, and then wait for what it
			// was told to wait for. Returning on WAIT without waiting is the caller's mistake, and
			// the test would see it as "did not wait" below.
			_, op, wait, err := sqlRoot.AcquireLock(ctx, sqlRoot.lockScopeID, sqlShapedBundle(), nil)
			if err != nil {
				done <- outcome{err: err}
				return
			}
			if op != lockmanager.LockOpWait {
				done <- outcome{op: op}
				return
			}
			_, err = sqlRoot.WaitForLock(ctx, wait)
			done <- outcome{op: lockmanager.LockOpGranted, err: err}
		}()
		// The conflict is observable as the queued request, and that is the barrier: no timing is
		// involved in deciding whether the two layers met.
		f.awaitPending(sqlRoot.ID, 1)
		select {
		case got := <-done:
			t.Fatalf("the SQL-shaped acquisition did not wait for the raw holder: op=%s err=%v", got.op, got.err)
		default:
		}
		if err := rawRoot.Rollback(); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("the SQL-shaped acquisition failed: %v", got.err)
			}
			if got.op != lockmanager.LockOpGranted {
				t.Fatalf("the SQL-shaped acquisition resolved to %s, want GRANTED after the raw holder ended", got.op)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the SQL-shaped acquisition was never granted")
		}
		_ = sqlRoot.Rollback()
		f.requireIdle()
	})
}

// TestB02RawGetRemainsASnapshotRead proves the read path is untouched: a Get never takes a lock,
// never waits for one, and never sees another root's uncommitted write.
func TestB02RawGetRemainsASnapshotRead(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	seed := f.begin()
	if err := seed.Put(rawSpace, rawKey, []byte("published")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	holder := f.begin()
	if err := holder.Put(rawSpace, rawKey, []byte("uncommitted")); err != nil {
		t.Fatal(err)
	}

	reader := f.begin()
	value, found, err := reader.Get(rawSpace, rawKey)
	if err != nil || !found {
		t.Fatalf("Get = %q found=%v err=%v", value, found, err)
	}
	if string(value) != "published" {
		t.Fatalf("Get returned %q, want the published value: a read must not observe an uncommitted write", value)
	}
	if pending := f.manager.PendingRequests(reader.ID); pending != 0 {
		t.Fatalf("a plain read queued %d lock requests, want 0", pending)
	}
	_ = reader.Rollback()
	_ = holder.Rollback()
	f.requireIdle()
}

// TestB02RawWaitIsEndedByEpochReset proves a raw wait is cancellable through the manager's own
// lifecycle: an epoch change ends the owners and invalidates every queued request, so the caller
// fails with a definite error instead of waiting for ever.
func TestB02RawWaitIsEndedByEpochReset(t *testing.T) {
	f := newRawLockFixture(t)

	holder := f.begin()
	if err := holder.Put(rawSpace, rawKey, []byte("holder")); err != nil {
		t.Fatal(err)
	}

	waiter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- waiter.Put(rawSpace, rawKey, []byte("waiter")) }()
	f.awaitWait("the raw write that will be invalidated")

	if affected := f.store.ResetLockEpoch(1); affected == 0 {
		t.Fatal("the epoch reset affected no owner")
	}
	select {
	case err := <-outcome:
		if !errors.Is(err, lockmanager.ErrLockInvalidated) {
			t.Fatalf("the raw write returned %v, want ErrLockInvalidated", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the epoch reset did not end the raw wait")
	}
	// The invalidated handle cannot publish either, and nothing is left behind.
	if _, err := waiter.Commit(context.Background()); err == nil {
		t.Fatal("a transaction from an ended epoch committed")
	}
	f.requireIdle()
}

// TestB02RawWaitHoldsNoPersistenceLatch proves the wait happens before anything is staged: while
// one root is blocked on a key, another root can still write and commit an unrelated key. A
// store-wide latch held across the wait would show up here as a blocked third root.
func TestB02RawWaitHoldsNoPersistenceLatch(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	holder := f.begin()
	if err := holder.Put(rawSpace, rawKey, []byte("holder")); err != nil {
		t.Fatal(err)
	}

	waiter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- waiter.Put(rawSpace, rawKey, []byte("waiter")) }()
	f.awaitWait("the blocked raw write")

	// The third root touches a different key and must not be affected by the wait.
	third := f.begin()
	if err := third.Put("row/t2", []byte("other"), []byte("third")); err != nil {
		t.Fatalf("an unrelated write blocked behind a raw wait: %v", err)
	}
	if _, err := third.Commit(ctx); err != nil {
		t.Fatalf("an unrelated commit blocked behind a raw wait: %v", err)
	}

	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken raw write failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the raw write was never woken")
	}
	_ = waiter.Rollback()
	f.requireIdle()
}

// TestB02RawLockIsReleasedByCommitAndRollback covers the two ways a raw write ends its root: the
// lock must be gone in both cases, with no owner, grant or charge left behind.
func TestB02RawLockIsReleasedByCommitAndRollback(t *testing.T) {
	ctx := context.Background()

	t.Run("commit", func(t *testing.T) {
		f := newRawLockFixture(t)
		root := f.begin()
		if err := root.Put(rawSpace, rawKey, []byte("v")); err != nil {
			t.Fatal(err)
		}
		if mode, ok := f.manager.HeldMode(root.ID, sqlShapedRecord()); !ok || mode != lockmanager.ModeX {
			t.Fatalf("the raw write did not hold its record: %v/%v", mode, ok)
		}
		if _, err := root.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		f.requireIdle()
	})

	t.Run("rollback", func(t *testing.T) {
		f := newRawLockFixture(t)
		root := f.begin()
		if err := root.Put(rawSpace, rawKey, []byte("v")); err != nil {
			t.Fatal(err)
		}
		if mode, ok := f.manager.HeldMode(root.ID, sqlShapedRecord()); !ok || mode != lockmanager.ModeX {
			t.Fatalf("the raw write did not hold its record: %v/%v", mode, ok)
		}
		if err := root.Rollback(); err != nil {
			t.Fatal(err)
		}
		f.requireIdle()
	})
}

// TestB02ChildRawWriteLocksTheRoot proves a child's raw write is attributed to the root: the lock
// survives the child's merge or rollback and is released only when the root ends.
func TestB02ChildRawWriteLocksTheRoot(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	root := f.begin()
	child, err := root.ChildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Put(rawSpace, rawKey, []byte("from-child")); err != nil {
		t.Fatalf("the child's raw write: %v", err)
	}
	if mode, ok := f.manager.HeldMode(root.ID, sqlShapedRecord()); !ok || mode != lockmanager.ModeX {
		t.Fatalf("the child's raw write attributed %v/%v to the root, want X", mode, ok)
	}
	if _, err := child.Commit(ctx); err != nil {
		t.Fatalf("child commit: %v", err)
	}
	if mode, ok := f.manager.HeldMode(root.ID, sqlShapedRecord()); !ok || mode != lockmanager.ModeX {
		t.Fatalf("the root lost the child's lock on merge: %v/%v", mode, ok)
	}
	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	f.requireIdle()
}
