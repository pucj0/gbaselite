package mvcc

import (
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
// label 鈥?a raw write has no label and therefore needs none, which is the asymmetry this file
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
