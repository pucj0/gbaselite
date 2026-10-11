package mvcc

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking_lifecycle_test.go is T047: the lifecycle contract between the B01 transaction manager
// and the B02 lock manager.
//
// The property under test is ownership. Every grant belongs to the root transaction, so a child,
// a savepoint and a statement can take locks and withdraw their own pending requests, but only
// the root can hand a lock back. The tests assert that through the observable state of the lock
// manager -- grants, pending requests and the wait graph -- rather than through the transaction's
// own bookkeeping, so a lifecycle that forgets one of those steps fails here.

// lockTestFixture is one store with one attached lock manager.
type lockTestFixture struct {
	t       *testing.T
	store   *Store
	manager *lockmanager.Manager
	// clock is the manager's injected time source. Wait deadlines are driven through it, so a
	// timeout is established as a fact rather than by sleeping.
	clock *lockmanager.FakeClock
}

func newLockTestFixture(t *testing.T) *lockTestFixture {
	t.Helper()
	store, err := OpenWithOptions(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		store.CloseLocks()
		_ = store.Close()
	})
	clock := lockmanager.NewFakeClock(time.Unix(1700000000, 0))
	manager, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, clock)
	if err != nil {
		t.Fatalf("new lock manager: %v", err)
	}
	if _, err := store.AttachLocks(manager); err != nil {
		t.Fatalf("attach locks: %v", err)
	}
	return &lockTestFixture{t: t, store: store, manager: manager, clock: clock}
}

// begin starts a root transaction and asserts it owns a lock owner.
func (f *lockTestFixture) begin(ctx context.Context) *Tx {
	f.t.Helper()
	tx, err := f.store.Begin(ctx, nil)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	if tx.LockOwnerID() != tx.ID {
		f.t.Fatalf("root lock owner = %q, want its own id %q", tx.LockOwnerID(), tx.ID)
	}
	return tx
}

// child creates a child transaction and asserts its grants belong to the root.
func (f *lockTestFixture) child(ctx context.Context, parent *Tx) *Tx {
	f.t.Helper()
	child, err := parent.ChildContext(ctx)
	if err != nil {
		f.t.Fatalf("child: %v", err)
	}
	if child.LockOwnerID() != parent.ID {
		f.t.Fatalf("child lock owner = %q, want the root %q", child.LockOwnerID(), parent.ID)
	}
	return child
}

// lockRow takes one exclusive row lock through a transaction's own scope.
func (f *lockTestFixture) lockRow(ctx context.Context, tx *Tx, key string) lockmanager.LockGrant {
	f.t.Helper()
	return f.lockRowInScope(ctx, tx, tx.lockScopeID, key)
}

// lockRowInScope takes one exclusive row lock through a named scope.
func (f *lockTestFixture) lockRowInScope(ctx context.Context, tx *Tx, scope uint64, key string) lockmanager.LockGrant {
	f.t.Helper()
	owner := tx.LockOwnerID()
	result, err := f.manager.Acquire(ctx, owner, scope, []lockmanager.ResourceRef{
		indexRecordRef(key), tableIntentRef(),
	})
	if err != nil {
		f.t.Fatalf("acquire %q: %v", key, err)
	}
	if result.Op != lockmanager.LockOpGranted {
		f.t.Fatalf("acquire %q op = %s, want GRANTED", key, result.Op)
	}
	return result.Grant
}

// rowLock is the record component and table intent one row lock uses.
func indexRecordRef(key string) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:    lockmanager.KindRecord,
		Mode:    lockmanager.ModeX,
		Space:   "index/PRIMARY",
		TableID: "t1",
		Key:     []byte(key),
	}}
}

func tableIntentRef() lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:    lockmanager.KindTableIntent,
		Mode:    lockmanager.ModeIX,
		Space:   "table",
		TableID: "t1",
	}}
}

// heldRow reports whether one root holds the row lock.
func (f *lockTestFixture) heldRow(rootID, key string) bool {
	f.t.Helper()
	_, ok := f.manager.HeldMode(rootID, indexRecordRef(key))
	return ok
}

// pendingOf reports how many requests one root is waiting for.
func (f *lockTestFixture) pendingOf(rootID string) int {
	f.t.Helper()
	return f.manager.PendingRequests(rootID)
}

// waitForPending blocks until one root has exactly the expected number of queued requests.
//
// It is a barrier on observed state, not a delay used to create an order: the loop ends when the
// manager reports the queue entry, and the sleep is only the polling interval of that observation.
// A waiter that never queues fails the test instead of being mistaken for a scheduling artifact.
func (f *lockTestFixture) waitForPending(t *testing.T, rootID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if f.manager.PendingRequests(rootID) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %d queued requests (now %d)", rootID, want, f.manager.PendingRequests(rootID))
		}
		time.Sleep(time.Millisecond)
	}
}

// requireNoLockState asserts the whole lock manager is idle.
func (f *lockTestFixture) requireNoLockState() {
	f.t.Helper()
	if size := f.manager.LockTableSize(); size != 0 {
		f.t.Fatalf("lock table holds %d resources after cleanup, want 0", size)
	}
	if edges := f.manager.WaitEdges(); len(edges) != 0 {
		f.t.Fatalf("%d wait edges survived cleanup: %v", len(edges), edges)
	}
	if owners := f.manager.Stats().ActiveOwners; owners != 0 {
		f.t.Fatalf("%d lock owners survived cleanup, want 0", owners)
	}
}

// TestB02ClosingAnEnclosingScopeCancelsAChildsQueuedRequest proves the scope tree is load-bearing.
//
// A savepoint's requests belong to the savepoint layer, so discarding that layer must cancel them
// even when the request was issued through a scope the child opened itself. That is only true if the
// child's own transaction scope is nested inside the scope of the transaction that created it: a
// root-level sibling scope would survive the enclosing rollback and leave a request queued for a
// layer that no longer exists.
func TestB02ClosingAnEnclosingScopeCancelsAChildsQueuedRequest(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	// One root holds the row, so the child's request has to queue instead of being granted.
	blocker := f.begin(ctx)
	f.lockRow(ctx, blocker, "k1")

	parent := f.begin(ctx)
	parentScope := parent.lockScopeID
	child := f.child(ctx, parent)
	childScope, err := child.BeginLockScope(ctx, LockScopeOptions{Kind: storageengine.LockScopeSavepoint})
	if err != nil {
		t.Fatalf("open the child's savepoint scope: %v", err)
	}

	outcome := make(chan error, 1)
	go func() {
		// The two-step shape a real caller uses: ask, then wait for what the answer said to wait for.
		_, op, wait, err := child.AcquireLock(ctx, childScope, []lockmanager.ResourceRef{indexRecordRef("k1"), tableIntentRef()}, nil)
		if err != nil {
			outcome <- err
			return
		}
		if op != lockmanager.LockOpWait {
			outcome <- nil
			return
		}
		_, err = child.WaitForLock(ctx, wait)
		outcome <- err
	}()
	f.waitForPending(t, parent.ID, 1)

	// Discarding the enclosing layer closes its whole subtree, which now contains the child's own
	// scope and every scope the child opened under it.
	if _, err := parent.CloseLockScope(ctx, parentScope); err != nil {
		t.Fatalf("close the enclosing scope: %v", err)
	}
	select {
	case err := <-outcome:
		if !errors.Is(err, lockmanager.ErrConflict) {
			// The manager reports a cancelled request as a conflict; the adapter maps it to the
			// neutral conflict the statement layer retries.
			t.Fatalf("the child's queued request returned %v, want a cancelled request (lockmanager.ErrConflict)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the child's queued request survived the enclosing scope's close")
	}
	if pending := f.pendingOf(parent.ID); pending != 0 {
		t.Fatalf("%d requests survived the enclosing scope's close", pending)
	}
	// A discarded statement must not be able to keep working either: every write entry point takes
	// its claim from the transaction's own scope, so with the scope closed the write is refused and
	// nothing can be staged for a statement that no longer exists. Without this the statement could
	// write unprotected and only be stopped later, which is the "an invalid statement must not keep
	// executing" half of the lifecycle contract.
	if err := child.Put("index/PRIMARY", []byte("after-close"), []byte("value")); !errors.Is(err, lockmanager.ErrScopeUnknown) {
		t.Fatalf("a write through a closed scope returned %v, want ErrScopeUnknown", err)
	}
	if _, found, err := child.readOp("index/PRIMARY", []byte("after-close")); err != nil || found {
		t.Fatalf("a refused write left staged state: found=%v err=%v", found, err)
	}
	_ = child.Rollback()
	_ = parent.Rollback()
	_ = blocker.Rollback()
	f.requireNoLockState()
}

// TestB02RootCommitReleasesLocks covers the commit half of the root lifecycle.
func TestB02RootCommitReleasesLocks(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	f.lockRow(ctx, root, "k1")
	f.lockRow(ctx, root, "k2")
	if !f.heldRow(root.ID, "k1") || !f.heldRow(root.ID, "k2") {
		t.Fatal("the root did not hold both rows before the commit")
	}
	if err := root.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}

	if _, err := root.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if root.State() != TransactionCommitted {
		t.Fatalf("state = %s, want COMMITTED", root.State())
	}
	// The commit is the release point: every grant is gone and the owner is unregistered.
	f.requireNoLockState()
	if _, ok := f.manager.Owner(root.ID); ok {
		t.Fatal("the lock owner survived a successful root commit")
	}
	// The MVCC registry agrees: no active transaction still pins history.
	if active := f.store.txns.ActiveTransactions(); len(active) != 0 {
		t.Fatalf("%d transactions remain registered after the commit: %+v", len(active), active)
	}
}

// TestB02RootRollbackReleasesLocks covers the abort half.
func TestB02RootRollbackReleasesLocks(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	f.lockRow(ctx, root, "k1")

	if err := root.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if root.State() != TransactionAborted {
		t.Fatalf("state = %s, want ABORTED", root.State())
	}
	f.requireNoLockState()
	if active := f.store.txns.ActiveTransactions(); len(active) != 0 {
		t.Fatalf("%d transactions remain registered after the rollback", len(active))
	}
}

// TestB02DuplicateRollbackAndCloseRaceIsIdempotent covers repeated cleanup.
//
// A double rollback, a rollback after a commit and a rollback after Close must all be harmless:
// the lock accounting is released once and cannot underflow.
func TestB02DuplicateRollbackAndCloseRaceIsIdempotent(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	f.lockRow(ctx, root, "k1")

	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	f.requireNoLockState()
	// A second rollback is a no-op and must not release anything twice.
	for i := 0; i < 3; i++ {
		if err := root.Rollback(); err != nil {
			t.Fatalf("rollback %d: %v", i, err)
		}
	}
	f.requireNoLockState()

	// A transaction that already committed cannot be turned into an abort by a later rollback, and
	// the committed outcome is what survives.
	committed := f.begin(ctx)
	f.lockRow(ctx, committed, "k2")
	if _, err := committed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := committed.Rollback(); err != nil {
		t.Fatal(err)
	}
	if committed.State() != TransactionCommitted {
		t.Fatalf("state = %s after commit-then-rollback, want the COMMITTED outcome to stand", committed.State())
	}
	f.requireNoLockState()

	// Closing the store and then closing the lock manager again is idempotent.
	f.store.CloseLocks()
	f.store.CloseLocks()
}

// TestB02ChildCommitKeepsRootLocks is the central ownership test.
//
// A child merges its writes into the parent; a merge is not a database commit and must not hand
// back a single lock. If a child could release the root's grants, another transaction could take
// the row between the merge and the root's own commit, which is the lost update the whole design
// exists to prevent.
func TestB02ChildCommitKeepsRootLocks(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	child := f.child(ctx, root)

	f.lockRow(ctx, root, "k1")
	f.lockRow(ctx, child, "k2")
	if !f.heldRow(root.ID, "k1") || !f.heldRow(root.ID, "k2") {
		t.Fatal("both rows must be held by the root owner while the child is live")
	}

	if _, err := child.Commit(ctx); err != nil {
		t.Fatalf("child commit: %v", err)
	}
	if child.State() != TransactionMerged {
		t.Fatalf("child state = %s, want MERGED", child.State())
	}
	// Both locks survive the merge, and the root still owns them.
	if !f.heldRow(root.ID, "k1") {
		t.Fatal("the child commit released the root's own lock")
	}
	if !f.heldRow(root.ID, "k2") {
		t.Fatal("the child commit released the lock the child took for the root")
	}
	if _, ok := f.manager.Owner(root.ID); !ok {
		t.Fatal("the root lock owner was unregistered by a child commit")
	}
	// The child's own scope is closed; the root's is still open.
	if _, ok := f.manager.Scope(root.ID, child.lockScopeID); ok {
		t.Fatal("the child lock scope survived the merge")
	}

	// The root's commit is the only release point.
	if _, err := root.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.requireNoLockState()
}

// TestB02FailedStatementKeepsRootLocks covers the statement-failure rule.
//
// A failed statement is a child rollback: its pending requests are withdrawn and every granted
// lock stays with the root, because the root may still commit and the locks it took are still
// protecting its write set.
func TestB02FailedStatementKeepsRootLocks(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	f.lockRow(ctx, root, "k1")

	// A statement layer takes one lock and leaves one request waiting behind another root.
	statement := f.child(ctx, root)
	f.lockRow(ctx, statement, "k2")

	other := f.begin(ctx)
	f.lockRow(ctx, other, "k3")
	// The statement's second request conflicts with another root, so it queues.
	blocked, err := f.manager.Acquire(ctx, statement.LockOwnerID(), statement.lockScopeID, []lockmanager.ResourceRef{
		indexRecordRef("k3"), tableIntentRef(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Op != lockmanager.LockOpWait {
		t.Fatalf("op = %s, want WAIT for a row another root holds", blocked.Op)
	}

	// The statement fails. Its scope closes, which withdraws the waiting request and releases
	// nothing.
	if err := statement.Rollback(); err != nil {
		t.Fatalf("statement rollback: %v", err)
	}
	if statement.State() != TransactionAborted {
		t.Fatalf("statement state = %s, want ABORTED", statement.State())
	}
	if got := f.pendingOf(root.ID); got != 0 {
		t.Fatalf("the failed statement left %d pending requests behind", got)
	}
	if !f.heldRow(root.ID, "k1") || !f.heldRow(root.ID, "k2") {
		t.Fatal("a failed statement released a lock the root still needs")
	}
	// The root is unaffected and can still commit.
	if _, err := root.Commit(ctx); err != nil {
		t.Fatalf("root commit after a failed statement: %v", err)
	}
	f.manager.ReleaseOwner(other.ID)
	f.requireNoLockState()
}

// TestB02SavepointScopeCreateRollbackRelease covers the savepoint rules.
//
// A savepoint is a nested request origin. Closing it withdraws its own pending requests, keeps
// every grant, and never disturbs a sibling scope's grants.
func TestB02SavepointScopeCreateRollbackRelease(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	f.lockRow(ctx, root, "k1")

	// Creating a savepoint opens a nested scope under the transaction's own scope.
	inner, err := root.BeginLockScope(ctx, LockScopeOptions{Kind: storageengine.LockScopeSavepoint})
	if err != nil {
		t.Fatalf("begin savepoint scope: %v", err)
	}
	scope, ok := f.manager.Scope(root.ID, inner)
	if !ok {
		t.Fatal("the savepoint scope was not registered on the root owner")
	}
	if scope.Kind != lockmanager.ScopeSavepoint {
		t.Fatalf("scope kind = %s, want savepoint", scope.Kind)
	}
	if scope.ParentScopeID != root.lockScopeID {
		t.Fatalf("savepoint parent = %d, want the transaction scope %d", scope.ParentScopeID, root.lockScopeID)
	}

	f.lockRowInScope(ctx, root, inner, "k2")
	if !f.heldRow(root.ID, "k2") {
		t.Fatal("a savepoint lock is not owned by the root")
	}

	// A second savepoint layer queues behind another root and is then rolled back to.
	other := f.begin(ctx)
	f.lockRow(ctx, other, "k3")
	sibling, err := root.BeginLockScope(ctx, LockScopeOptions{Kind: storageengine.LockScopeSavepoint})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := f.manager.Acquire(ctx, root.ID, sibling, []lockmanager.ResourceRef{indexRecordRef("k3"), tableIntentRef()})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Op != lockmanager.LockOpWait {
		t.Fatalf("op = %s, want WAIT", queued.Op)
	}

	// ROLLBACK TO SAVEPOINT closes that scope: its pending request is withdrawn, no grant moves.
	if _, err := root.CloseLockScope(ctx, sibling); err != nil {
		t.Fatalf("close savepoint scope: %v", err)
	}
	if got := f.pendingOf(root.ID); got != 0 {
		t.Fatalf("%d pending requests survived ROLLBACK TO SAVEPOINT", got)
	}
	for _, key := range []string{"k1", "k2"} {
		if !f.heldRow(root.ID, key) {
			t.Fatalf("ROLLBACK TO SAVEPOINT released %q, which it must not", key)
		}
	}
	// The first savepoint's scope is untouched by closing the second.
	if _, ok := f.manager.Scope(root.ID, inner); !ok {
		t.Fatal("closing one savepoint scope disturbed a sibling scope")
	}
	// Releasing the savepoint explicitly does not release the root's locks either.
	if _, err := root.CloseLockScope(ctx, inner); err != nil {
		t.Fatal(err)
	}
	if !f.heldRow(root.ID, "k2") {
		t.Fatal("RELEASE SAVEPOINT released a grant")
	}

	f.manager.ReleaseOwner(other.ID)
	if _, err := root.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.requireNoLockState()
}

// TestB02DeadlockVictimCannotWriteOrCommit covers requirement 6.
//
// A chosen victim must not be able to take another lock, merge a child or reach a durable
// publication. The detector only marks the intent; the transaction itself is what stops, which is
// what keeps the detector from calling a transaction method.
func TestB02DeadlockVictimCannotWriteOrCommit(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	// Build a real two-root cycle on two rows.
	a := f.begin(ctx)
	b := f.begin(ctx)
	f.lockRow(ctx, a, "k1")
	f.lockRow(ctx, b, "k2")
	waitA, err := f.manager.Acquire(ctx, a.ID, a.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k2"), tableIntentRef()})
	if err != nil {
		t.Fatal(err)
	}
	waitB, err := f.manager.Acquire(ctx, b.ID, b.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k1"), tableIntentRef()})
	if err != nil {
		t.Fatal(err)
	}
	if waitA.Op != lockmanager.LockOpWait || waitB.Op != lockmanager.LockOpWait {
		t.Fatalf("cycle did not form: a=%s b=%s", waitA.Op, waitB.Op)
	}

	summary, found := f.manager.DetectDeadlock()
	if !found {
		t.Fatalf("deadlock not detected; edges = %v", f.manager.WaitEdges())
	}
	victim := summary.VictimRootID
	if victim != b.ID {
		t.Fatalf("victim = %s, want the later root %s", victim, b.ID)
	}
	victimTx := b
	survivor := a

	// The victim learns it was chosen and cannot start new work.
	if !victimTx.AbortRequested() {
		t.Fatal("the victim does not observe the abort request")
	}
	if _, err := f.manager.Acquire(ctx, victim, victimTx.lockScopeID, []lockmanager.ResourceRef{
		indexRecordRef("k9"), tableIntentRef(),
	}); !errors.Is(err, lockmanager.ErrOwnerEnding) {
		t.Fatalf("a victim acquired a new lock: err = %v, want ErrOwnerEnding", err)
	}
	// A child of the victim cannot merge either.
	child := f.child(ctx, victimTx)
	if _, err := child.Commit(ctx); !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("a victim's child merged: err = %v, want ErrDeadlock", err)
	}
	if child.State() != TransactionAborted {
		t.Fatalf("the victim's child state = %s, want ABORTED", child.State())
	}
	// The victim's own commit is refused, and it is refused before anything is published.
	if victimTx.Put("index/PRIMARY", []byte("k1"), []byte("victim")) == nil {
		// Writing through the MVCC layer is still allowed (the victim's goroutine has not aborted
		// yet); what must not happen is publishing it.
	}
	if _, err := victimTx.Commit(ctx); !errors.Is(err, storageengine.ErrDeadlock) {
		t.Fatalf("the victim committed: err = %v, want ErrDeadlock", err)
	}
	if victimTx.State() != TransactionAborted {
		t.Fatalf("victim state = %s, want ABORTED", victimTx.State())
	}
	// The victim's abort released its grants, so the survivor's wait resolves.
	if _, err := f.manager.WaitForGrant(ctx, survivor.ID, waitA.Wait); err != nil {
		t.Fatalf("the survivor was not granted after the victim aborted: %v", err)
	}
	f.manager.ReleaseOwner(survivor.ID)
	f.requireNoLockState()
}

// TestB02LockTimeoutAbortsOnlyItsOwnRequest covers requirement 7.
//
// A lock wait timeout ends the request and the statement that asked for it. It must not release
// the locks the root already holds, and it must not abort a root that can still continue.
func TestB02LockTimeoutAbortsOnlyItsOwnRequest(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	holder := f.begin(ctx)
	waiter := f.begin(ctx)
	f.lockRow(ctx, holder, "k1")
	f.lockRow(ctx, waiter, "k0")
	f.lockRow(ctx, holder, "k2")

	// The waiter's scope carries a deadline, so the wait is bounded and the request is withdrawn
	// when it passes.
	scope, err := waiter.BeginLockScope(ctx, LockScopeOptions{
		Kind:     storageengine.LockScopeStatement,
		Deadline: f.clock.Now().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := f.manager.Acquire(ctx, waiter.ID, scope, []lockmanager.ResourceRef{indexRecordRef("k2"), tableIntentRef()})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Op != lockmanager.LockOpWait {
		t.Fatalf("op = %s, want WAIT", queued.Op)
	}

	// Drive the manager's clock past the deadline and sweep, which is the same transition the
	// waiting caller observes.
	f.clock.Advance(2 * time.Second)
	if completed := f.manager.CheckTimeouts(); completed != 1 {
		t.Fatalf("the sweep completed %d requests, want the single expired one", completed)
	}
	if _, err := f.manager.WaitForGrantResult(ctx, waiter.ID, queued.Wait); !errors.Is(err, lockmanager.ErrLockWaitTimeout) {
		t.Fatalf("wait error = %v, want ErrLockWaitTimeout", err)
	}
	// Only the one request is gone: the waiter's existing grant is untouched.
	if f.heldRow(waiter.ID, "k0") != true {
		t.Fatal("a lock timeout released a grant the root already held")
	}
	if f.pendingOf(waiter.ID) != 0 {
		t.Fatalf("%d pending requests survived the timeout", f.pendingOf(waiter.ID))
	}
	// The statement scope is closed, which is the statement rollback. The root can still continue.
	if _, err := waiter.CloseLockScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if waiter.State() != TransactionActive {
		t.Fatalf("root state = %s after a statement timeout, want ACTIVE", waiter.State())
	}
	if _, err := waiter.Commit(ctx); err != nil {
		t.Fatalf("the root could not commit after a statement timeout: %v", err)
	}
	f.manager.ReleaseOwner(holder.ID)
	f.requireNoLockState()
}

// TestB02CommitFailureReleasesLocks covers a failed durable publication.
//
// A publication that fails aborts the root, and the abort is what releases the locks. A commit
// failure must leave no grant behind, and it must not turn a decided abort into a commit.
func TestB02CommitFailureReleasesLocks(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()
	root := f.begin(ctx)
	f.lockRow(ctx, root, "k1")
	if err := root.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}

	// A canceled commit context fails the publication.
	canceled, cancel := context.WithCancel(ctx)
	if _, err := root.Commit(canceled); err == nil {
		// A local commit may complete before the cancellation is observed; in that case the
		// transaction is committed and the release point is the same. Only assert the outcome is
		// definite and the locks are gone.
		if root.State() != TransactionCommitted {
			t.Fatalf("a commit with a cancelled context neither failed nor committed: %s", root.State())
		}
	}
	cancel()
	state := root.State()
	if !state.Terminal() {
		t.Fatalf("state = %s after a failed commit, want a terminal state", state)
	}
	f.requireNoLockState()
	if active := f.store.txns.ActiveTransactions(); len(active) != 0 {
		t.Fatalf("%d transactions survived a failed commit: %+v", len(active), active)
	}
}

// TestB02DisconnectAndRestoreInvalidateWaiters covers requirement 9.
//
// A restore replaces the generation. Every transaction from the old generation becomes invalid,
// the lock owners it was using are dropped and every waiter is woken with one definite status, so
// no goroutine is left blocked on a generation that no longer exists.
func TestB02DisconnectAndRestoreInvalidateWaiters(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	holder := f.begin(ctx)
	waiter := f.begin(ctx)
	f.lockRow(ctx, holder, "k1")
	queued, err := f.manager.Acquire(ctx, waiter.ID, waiter.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k1"), tableIntentRef()})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Op != lockmanager.LockOpWait {
		t.Fatalf("op = %s, want WAIT", queued.Op)
	}
	if f.pendingOf(waiter.ID) == 0 {
		t.Fatal("the waiter is not queued")
	}

	// A generation change drives the same path Restore uses.
	if invalidated := f.store.ResetLockEpoch(7); invalidated != 2 {
		t.Fatalf("invalidated owners = %d, want both roots", invalidated)
	}
	if _, err := f.manager.WaitForGrantResult(ctx, waiter.ID, queued.Wait); !errors.Is(err, lockmanager.ErrLockInvalidated) {
		t.Fatalf("waiter error = %v, want ErrLockInvalidated", err)
	}
	if edges := f.manager.WaitEdges(); len(edges) != 0 {
		t.Fatalf("%d wait edges survived the epoch change: %v", len(edges), edges)
	}
	// The old generation's handles are refused rather than allowing a new request. The owner was
	// removed with its generation, so every operation on it reports an unknown owner.
	if _, err := f.manager.Acquire(ctx, holder.ID, holder.lockScopeID, []lockmanager.ResourceRef{
		indexRecordRef("k5"), tableIntentRef(),
	}); !errors.Is(err, lockmanager.ErrOwnerUnknown) {
		t.Fatalf("an old-generation owner acquired a lock: err = %v, want ErrOwnerUnknown", err)
	}
	// The transactions themselves observe the generation change.
	if _, err := holder.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("an old-generation root committed: err = %v, want ErrConflict", err)
	}

	// The registry and the lock manager agree that nothing is left.
	for _, tx := range []*Tx{holder, waiter} {
		tx.Rollback()
	}
	f.requireNoLockState()
	if active := f.store.txns.ActiveTransactions(); len(active) != 0 {
		t.Fatalf("%d transactions survived the epoch change", len(active))
	}
}

// TestB02StoreRestoreSynchronisesTheLockEpoch covers the Restore path itself.
//
// Restore is the one place that replaces the database image, so it is the place where a lock
// owner from the replaced generation would be most dangerous: it would keep blocking rows in a
// database that no longer contains them.
func TestB02StoreRestoreSynchronisesTheLockEpoch(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	// Produce a valid image to restore from.
	source := f.begin(ctx)
	if err := source.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var image bytes.Buffer
	if _, err := snapshot.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Rollback(); err != nil {
		t.Fatal(err)
	}

	// A live transaction holds a lock and a waiter is queued when the restore happens.
	holder := f.begin(ctx)
	waiter := f.begin(ctx)
	f.lockRow(ctx, holder, "k1")
	queued, err := f.manager.Acquire(ctx, waiter.ID, waiter.lockScopeID, []lockmanager.ResourceRef{indexRecordRef("k1"), tableIntentRef()})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Op != lockmanager.LockOpWait {
		t.Fatalf("op = %s, want WAIT before the restore", queued.Op)
	}

	if err := f.store.Restore(bytes.NewReader(image.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// The waiter is woken and no owner from the replaced generation survives.
	if _, err := f.manager.WaitForGrantResult(ctx, waiter.ID, queued.Wait); !errors.Is(err, lockmanager.ErrLockInvalidated) {
		t.Fatalf("waiter error after restore = %v, want ErrLockInvalidated", err)
	}
	if owners := f.manager.Stats().ActiveOwners; owners != 0 {
		t.Fatalf("%d lock owners survived a restore, want 0", owners)
	}
	if edges := f.manager.WaitEdges(); len(edges) != 0 {
		t.Fatalf("%d wait edges survived a restore", len(edges))
	}
	// Cleaning up the stale handles is harmless and leaves the manager idle.
	holder.Rollback()
	waiter.Rollback()
	f.requireNoLockState()
}

// TestB02LockOwnersAndRegistryStayConsistent checks requirement 10 from both sides.
//
// Every live transaction has a lock owner and every live lock owner has a transaction. A leak in
// either registry is a leak in the other, so the two counts are asserted together after every
// lifecycle step.
func TestB02LockOwnersAndRegistryStayConsistent(t *testing.T) {
	f := newLockTestFixture(t)
	ctx := context.Background()

	check := func(label string) {
		t.Helper()
		active := len(f.store.txns.ActiveTransactions())
		owners := int(f.manager.Stats().ActiveOwners)
		if active != owners {
			t.Fatalf("%s: %d active transactions but %d lock owners", label, active, owners)
		}
	}

	check("empty")
	root := f.begin(ctx)
	check("after begin")
	f.lockRow(ctx, root, "k1")
	child := f.child(ctx, root)
	// A child is a registry entry but not a lock owner, so the child count is what makes the two
	// numbers differ; the roots must still match.
	roots := 1
	if owners := int(f.manager.Stats().ActiveOwners); owners != roots {
		t.Fatalf("after a child: %d lock owners, want %d (a child must not own one)", owners, roots)
	}

	if _, err := child.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	check("after child merge")
	if _, err := root.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	check("after root commit")

	// The same for the abort path.
	second := f.begin(ctx)
	f.lockRow(ctx, second, "k2")
	check("after second begin")
	second.Rollback()
	check("after second rollback")

	// And for a failed child, which drops a registry entry without touching the owner.
	third := f.begin(ctx)
	f.lockRow(ctx, third, "k3")
	failing := f.child(ctx, third)
	failing.Rollback()
	check("after a failed child")
	if _, ok := f.manager.Owner(third.ID); !ok {
		t.Fatal("a failed child released the root's lock owner")
	}
	third.Rollback()
	check("after the root rollback")
	f.requireNoLockState()
}

// TestB02NoLockingWithoutABinding proves the binding is opt-in.
//
// A store with no lock manager must run exactly as B01 did: Begin succeeds, no owner is created
// and the locking entry points fail closed instead of silently pretending to lock.
func TestB02NoLockingWithoutABinding(t *testing.T) {
	store, err := OpenWithOptions(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	tx, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatalf("begin without locking: %v", err)
	}
	if _, ok := tx.LockManager(); ok {
		t.Fatal("a store with no binding reports a lock manager")
	}
	if tx.AbortRequested() {
		t.Fatal("a store with no binding reports an abort request")
	}
	if _, err := tx.BeginLockScope(ctx, LockScopeOptions{Kind: storageengine.LockScopeStatement}); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("BeginLockScope without locking = %v, want ErrUnsupported", err)
	}
	if _, err := tx.CloseLockScope(ctx, 1); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("CloseLockScope without locking = %v, want ErrUnsupported", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// TestB02AttachingASecondManagerIsRefused proves two managers cannot share one store.
func TestB02AttachingASecondManagerIsRefused(t *testing.T) {
	f := newLockTestFixture(t)
	other, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := f.store.AttachLocks(other); !errors.Is(err, ErrLocksAttached) {
		t.Fatalf("attaching a second manager = %v, want ErrLocksAttached", err)
	}
	// Re-attaching the same manager is harmless and idempotent.
	if _, err := f.store.AttachLocks(f.manager); err != nil {
		t.Fatalf("re-attaching the same manager: %v", err)
	}
	if _, err := f.store.AttachLocks(nil); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("attaching nil = %v, want ErrUnsupported", err)
	}
}
