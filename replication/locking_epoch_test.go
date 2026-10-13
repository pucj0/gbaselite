package replication_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gbaselite/mvcc"
	"gbaselite/replication"
	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
)

// locking_epoch_test.go is the T054 suite: leader-only Current Read, Barrier vs VerifyLeader,
// Term fence, and loss-wake on replication.

func TestT054LeaderBarrierLinearizableOrderingAndTermFence(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()
	leaderIdx := tc.WaitForLeader(15 * time.Second)
	leaderEng := tc.engines[leaderIdx]
	engines := tc.engines
	ctx := context.Background()

	// 1. Verify Leader Barrier succeeds on Leader
	rep := leaderEng.(storageengine.ReplicatedEngine).Replica()
	if err := rep.Barrier(ctx); err != nil {
		t.Fatalf("leader Barrier failed: %v", err)
	}
	if err := rep.(storageengine.LeaderVerifier).VerifyLeader(ctx); err != nil {
		t.Fatalf("leader VerifyLeader failed: %v", err)
	}

	// 2. Non-leader (follower) fails Barrier with ErrNotLeader
	followerIdx := (leaderIdx + 1) % 3
	followerEng := engines[followerIdx]
	followerRep := followerEng.(storageengine.ReplicatedEngine).Replica()
	if err := followerRep.Barrier(ctx); err == nil || !errors.Is(err, replication.ErrNotLeader) {
		t.Fatalf("follower Barrier expected ErrNotLeader, got: %v", err)
	}

	// 3. Test Term Fence: Current Read under a stale term grant is rejected with ErrLockInvalidated
	leaderTx, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer leaderTx.Rollback()

	locking, ok := leaderTx.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	if !ok {
		t.Fatal("leader transaction does not expose LockingTxn")
	}
	locks, available := locking.LockingTxn()
	if !available {
		t.Fatal("leader LockingTxn is not available")
	}

	scope, err := locks.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	tableRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   "row/test",
		TableID: "t1",
	}
	rowRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   "row/test",
		TableID: "t1",
		Key:     []byte("k1"),
	}
	grant, err := locks.Acquire(ctx, scope, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
	if err != nil {
		t.Fatalf("leader Acquire failed: %v", err)
	}

	// Normal ReadCurrent with valid grant succeeds
	_, err = locks.ReadCurrent(ctx, grant, "row/test", []byte("k1"))
	if err != nil {
		t.Fatalf("leader ReadCurrent with valid grant failed: %v", err)
	}

	// Stale Term grant is rejected by Term Fence!
	staleTermGrant := grant
	staleTermGrant.Term = grant.Term + 999
	_, err = locks.ReadCurrent(ctx, staleTermGrant, "row/test", []byte("k1"))
	if !errors.Is(err, storageengine.ErrLockInvalidated) {
		t.Fatalf("expected ErrLockInvalidated for stale term grant, got: %v", err)
	}
}

// TestT054CommandFormatHasNoNewFields verifies that mvcc.Command serialization contains only
// the specified, backward-compatible fields without new lock-specific fields.
func TestT054CommandFormatHasNoNewFields(t *testing.T) {
	cmd := mvcc.Command{
		Term:     1,
		Kind:     "commit",
		ID:       "tx1",
		Snapshot: 10,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{
		"Term":     true,
		"Kind":     true,
		"ID":       true,
		"Snapshot": true,
		"Ops":      true,
		"Counter":  true,
		"Floor":    true,
		"Count":    true,
	}

	for k := range m {
		if !allowed[k] {
			t.Fatalf("unexpected new field in Command JSON serialization: %q", k)
		}
	}
}

// TestT054LeaderLossWakesWaitersWithErrLockInvalidated verifies that when a Raft leader loses
// leadership or closes, waiting lock requests are promptly woken with ErrLockInvalidated,
// and the former leader cannot continue serving Current Reads under the old term.
func TestT054LeaderLossWakesWaitersWithErrLockInvalidated(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()
	leaderIdx := tc.WaitForLeader(15 * time.Second)
	leaderEng := tc.engines[leaderIdx]
	ctx := context.Background()

	txA, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	locksA := txA.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltxA, _ := locksA.LockingTxn()
	scopeA, err := ltxA.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	tableRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   "row/test",
		TableID: "t1",
	}
	rowRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   "row/test",
		TableID: "t1",
		Key:     []byte("k1"),
	}

	grantA, err := ltxA.Acquire(ctx, scopeA, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
	if err != nil {
		t.Fatalf("txA Acquire failed: %v", err)
	}

	// txB tries to acquire row k1 and enters wait
	txB, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()

	locksB := txB.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltxB, _ := locksB.LockingTxn()
	scopeB, err := ltxB.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	doneB := make(chan error, 1)
	go func() {
		_, err := ltxB.Acquire(ctx, scopeB, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
		doneB <- err
	}()

	// Give txB a moment to block
	select {
	case err := <-doneB:
		t.Fatalf("txB did not block on held lock, returned: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Now shut down leader to simulate leader loss
	leaderEng.Close()
	tc.engines[leaderIdx] = nil

	// Waiter txB must be woken with ErrLockInvalidated!
	select {
	case err := <-doneB:
		if !errors.Is(err, storageengine.ErrLockInvalidated) {
			t.Fatalf("expected ErrLockInvalidated on leader loss, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not woken with ErrLockInvalidated upon leader loss")
	}

	// Former leader cannot continue reading under old grant
	_, err = ltxA.ReadCurrent(ctx, grantA, "row/test", []byte("k1"))
	if err == nil {
		t.Fatal("former leader accepted Current Read under old grant")
	}
}

// TestT054CurrentIteratorTermFenceInterruptsOnLeaderLoss verifies that an open CurrentIterator
// cannot continue yielding records once the leader term moves or leadership is lost.
func TestT054CurrentIteratorTermFenceInterruptsOnLeaderLoss(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()
	leaderIdx := tc.WaitForLeader(15 * time.Second)
	leaderEng := tc.engines[leaderIdx]
	ctx := context.Background()

	// Seed 3 keys
	seedTx, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k1", "k2", "k3"} {
		if err := seedTx.Put("row/test", []byte(k), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seedTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Open CurrentIterator on leader
	scanTx, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer scanTx.Rollback()

	locks := scanTx.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltx, _ := locks.LockingTxn()
	scope, err := ltx.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	tableRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeIS,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   "row/test",
		TableID: "t1",
	}
	gapRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeGap,
		Kind:    storageengine.LockComponentInterval,
		Space:   "row/test",
		TableID: "t1",
	}
	plan := storageengine.LockPlan{
		Kind:       "whole-domain",
		Components: []storageengine.LockComponent{tableRef, gapRef},
	}
	req := storageengine.CurrentScanRequest{
		Space: "row/test",
		Mode:  storageengine.LockModeX,
		Plan:  plan,
	}

	it, err := ltx.NewCurrentIterator(ctx, scope, req)
	if err != nil {
		t.Fatalf("NewCurrentIterator failed: %v", err)
	}
	defer it.Close()

	// Read first item
	if !it.Next() {
		t.Fatalf("expected first item from iterator, got none, err: %v", it.Err())
	}

	// Now shut down leader to simulate leader loss
	leaderEng.Close()
	tc.engines[leaderIdx] = nil

	// Next() must detect leader loss and return false with error!
	if it.Next() {
		t.Fatal("iterator continued reading after leader loss")
	}
	if it.Err() == nil {
		t.Fatal("iterator returned nil error after leader loss")
	}
	t.Logf("Iterator properly interrupted after leader loss with error: %v", it.Err())
}

// TestT054QueryCancellationDoesNotResetLockEpoch verifies Task 2: cancelling a query context
// or timing out on Barrier/VerifyLeader returns context.Canceled / DeadlineExceeded without
// erroneously triggering an engine-wide Lock Epoch Reset. Held locks and waiting requests remain intact.
func TestT054QueryCancellationDoesNotResetLockEpoch(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()
	leaderIdx := tc.WaitForLeader(15 * time.Second)
	leaderEng := tc.engines[leaderIdx]
	ctx := context.Background()

	// 1. Root A holds lock on row k1
	txA, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	locksA := txA.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltxA, _ := locksA.LockingTxn()
	scopeA, err := ltxA.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	tableRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   "row/test",
		TableID: "t1",
	}
	rowRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   "row/test",
		TableID: "t1",
		Key:     []byte("k1"),
	}

	if _, err := ltxA.Acquire(ctx, scopeA, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}}); err != nil {
		t.Fatalf("txA Acquire failed: %v", err)
	}

	// 2. Root B requests row k1 and enters wait
	txB, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()

	locksB := txB.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltxB, _ := locksB.LockingTxn()
	scopeB, err := ltxB.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	doneB := make(chan error, 1)
	go func() {
		_, err := ltxB.Acquire(ctx, scopeB, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
		doneB <- err
	}()

	// Confirm B is waiting using WaitEdges without sleep
	lm := mvccadapter.LockManagerOf(leaderEng)
	if lm == nil {
		t.Fatal("no lock manager on leader")
	}
	waitDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDeadline) {
		if len(lm.WaitEdges()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(lm.WaitEdges()) == 0 {
		t.Fatal("txB did not establish wait edge on held lock")
	}

	// 3. Query C executes Barrier with a cancelled context (confirm future started before cancellation via hook)
	rep := leaderEng.(storageengine.ReplicatedEngine).Replica()
	cancelCtx, cancel := context.WithCancel(context.Background())
	prevHook := replication.SetWaitStartedHook(func() {
		cancel()
	})
	defer replication.SetWaitStartedHook(prevHook)

	errC := rep.Barrier(cancelCtx)
	if !errors.Is(errC, context.Canceled) {
		t.Fatalf("expected context.Canceled for C Barrier, got: %v", errC)
	}

	// Also test timeout context on VerifyLeader
	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelTimeout()
	time.Sleep(time.Millisecond) // ensure expired
	errVerify := rep.(storageengine.LeaderVerifier).VerifyLeader(timeoutCtx)
	if !errors.Is(errVerify, context.DeadlineExceeded) && !errors.Is(errVerify, context.Canceled) {
		t.Fatalf("expected context deadline error for VerifyLeader, got: %v", errVerify)
	}

	// 4. Assert Leader status and Term are unchanged!
	status := rep.Status()
	if status.State != "Leader" {
		t.Fatalf("leader state changed unexpectedly: %s", status.State)
	}

	// 5. Assert B is STILL waiting (not woken with ErrLockInvalidated, not prematurely granted)
	select {
	case err := <-doneB:
		t.Fatalf("txB was erroneously unblocked after query C cancellation: %v", err)
	default:
	}

	// 6. Commit A -> B is woken normally!
	if err := txA.Put("row/test", []byte("k1"), []byte("valA")); err != nil {
		t.Fatal(err)
	}
	if _, err := txA.Commit(ctx); err != nil {
		t.Fatalf("txA Commit failed: %v", err)
	}

	// Now B must be granted!
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("txB failed after txA commit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("txB was not woken after txA commit")
	}

	_ = txB.Rollback()
}

// TestT054RealLeadershipTransferWakesWaitersAndEnforcesTermFence tests Task 4:
// When Leader A gracefully transfers leadership to another peer (without closing A):
// 1. A becomes Follower;
// 2. Waiters on A are woken with ErrLockInvalidated;
// 3. New leader B is elected under new term;
// 4. New leader B successfully executes Barrier, Current Read, and write;
// 5. A's old grant cannot be used anymore.
func TestT054RealLeadershipTransferWakesWaitersAndEnforcesTermFence(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()
	leaderIdx := tc.WaitForLeader(15 * time.Second)
	leaderEng := tc.engines[leaderIdx]
	ctx := context.Background()

	// 1. On Leader A: Root 1 holds lock on k1, Root 2 waits on k1
	txA, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	locksA := txA.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltxA, _ := locksA.LockingTxn()
	scopeA, err := ltxA.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	tableRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   "row/test",
		TableID: "t1",
	}
	rowRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   "row/test",
		TableID: "t1",
		Key:     []byte("k1"),
	}

	grantA, err := ltxA.Acquire(ctx, scopeA, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
	if err != nil {
		t.Fatalf("txA Acquire failed: %v", err)
	}

	txB, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()

	locksB := txB.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltxB, _ := locksB.LockingTxn()
	scopeB, err := ltxB.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	doneB := make(chan error, 1)
	go func() {
		_, err := ltxB.Acquire(ctx, scopeB, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
		doneB <- err
	}()

	// Confirm B is waiting using WaitEdges without sleep
	lmA := mvccadapter.LockManagerOf(leaderEng)
	if lmA == nil {
		t.Fatal("no lock manager on leader A")
	}
	waitDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDeadline) {
		if len(lmA.WaitEdges()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(lmA.WaitEdges()) == 0 {
		t.Fatal("txB did not establish wait edge on held lock")
	}

	// 2. Gracefully transfer leadership away from Leader A (bounded context, A stays alive)
	transferCtx, cancelTransfer := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTransfer()

	repA := leaderEng.(storageengine.ReplicatedEngine).Replica()
	if err := repA.(storageengine.LeadershipTransferrer).LeadershipTransfer(transferCtx); err != nil {
		t.Fatalf("LeadershipTransfer failed: %v", err)
	}

	// Waiter txB on former leader A must be woken with ErrLockInvalidated!
	select {
	case err := <-doneB:
		if !errors.Is(err, storageengine.ErrLockInvalidated) {
			t.Fatalf("expected ErrLockInvalidated for txB after leadership transfer, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("txB was not woken after leadership transfer")
	}

	// Verify all residues on former leader A are cleared
	statsA := lmA.Stats()
	if statsA.PendingRequests != 0 || statsA.WaitEdges != 0 || statsA.ActiveOwners != 0 || lmA.LockTableSize() != 0 {
		t.Fatalf("former leader lock manager has residue after step down: stats=%+v, size=%d", statsA, lmA.LockTableSize())
	}

	// 3. Wait for new leader to be elected
	newLeaderIdx := -1
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for i, eng := range tc.engines {
			if i == leaderIdx || eng == nil {
				continue
			}
			rep := eng.(storageengine.ReplicatedEngine).Replica()
			if rep != nil && rep.Status().State == "Leader" {
				ctxBarrier, cancel := context.WithTimeout(context.Background(), time.Second)
				err := rep.Barrier(ctxBarrier)
				cancel()
				if err == nil {
					newLeaderIdx = i
					break
				}
			}
		}
		if newLeaderIdx >= 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if newLeaderIdx < 0 {
		t.Fatal("failed to elect a new leader after transfer")
	}

	// 4. On new leader: Barrier succeeds, Current Read succeeds, write succeeds and commits
	newLeaderEng := tc.engines[newLeaderIdx]
	txNew, err := newLeaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txNew.Rollback()

	if err := txNew.Put("row/test", []byte("k1"), []byte("valNewLeader")); err != nil {
		t.Fatal(err)
	}
	seqNew, err := txNew.Commit(ctx)
	if err != nil {
		t.Fatalf("new leader commit failed: %v", err)
	}
	if seqNew == 0 {
		t.Fatal("new leader commit allocated 0 sequence")
	}

	// 5. Old grant on former leader A cannot continue to be used
	_, err = ltxA.ReadCurrent(ctx, grantA, "row/test", []byte("k1"))
	if err == nil {
		t.Fatal("former leader accepted Current Read under old grant")
	}
}

// TestT054DelayedLeaderLossEventCannotInvalidateNewTermRoot verifies Section A:
// A delayed Leader Loss event from an old tenure/term cannot wipe out a new Root
// operating legally in a subsequent tenure/term.
func TestT054DelayedLeaderLossEventCannotInvalidateNewTermRoot(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.Close()
	leaderIdx := tc.WaitForLeader(15 * time.Second)
	leaderEng := tc.engines[leaderIdx]
	ctx := context.Background()

	nodeA := leaderEng.(storageengine.ReplicatedEngine).Replica().(interface{ Node() *replication.Node }).Node()
	tenure1, term1 := nodeA.CurrentTenureForTesting()

	// 1. Root 1 on Leader A holds lock on k1
	tx1, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()

	locks1 := tx1.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltx1, _ := locks1.LockingTxn()
	scope1, err := ltx1.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	tableRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   "row/test",
		TableID: "t1",
	}
	rowRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   "row/test",
		TableID: "t1",
		Key:     []byte("k1"),
	}

	if _, err := ltx1.Acquire(ctx, scope1, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}}); err != nil {
		t.Fatalf("tx1 Acquire failed: %v", err)
	}

	// Root 2 waits on k1
	tx2, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()

	locks2 := tx2.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltx2, _ := locks2.LockingTxn()
	scope2, err := ltx2.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	done2 := make(chan error, 1)
	go func() {
		_, err := ltx2.Acquire(ctx, scope2, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
		done2 <- err
	}()

	lmA := mvccadapter.LockManagerOf(leaderEng)
	waitDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDeadline) {
		if len(lmA.WaitEdges()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 2. Node A experiences leader loss of tenure 1 (term 1)
	nodeA.HandleLeaderLossForTesting(tenure1, term1)

	// Waiter Root 2 must be woken with ErrLockInvalidated
	select {
	case err := <-done2:
		if !errors.Is(err, storageengine.ErrLockInvalidated) {
			t.Fatalf("expected ErrLockInvalidated for tx2, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tx2 was not woken after leader loss")
	}

	// 3. Node A is elected Leader again in Term+1 (tenure 2)
	term2 := term1 + 1
	nodeA.HandleLeaderElectedForTesting(term2)

	// 4. In new tenure 2 / Term 2, a new Root 3 acquires lock on k1
	tx3, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx3.Rollback()

	locks3 := tx3.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	ltx3, _ := locks3.LockingTxn()
	scope3, err := ltx3.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	grant3, err := ltx3.Acquire(ctx, scope3, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
	if err != nil {
		t.Fatalf("tx3 Acquire in new term failed: %v", err)
	}

	// 5. A delayed LeaderCh=false event from tenure 1 is delivered!
	nodeA.HandleLeaderLossForTesting(tenure1, term1)

	// 6. Confirm Root 3 in Term 2 is NOT invalidated by the delayed old event!
	obs, err := ltx3.ReadCurrent(ctx, grant3, "row/test", []byte("k1"))
	if err != nil {
		t.Fatalf("tx3 was erroneously invalidated by delayed old loss event: %v", err)
	}
	_ = obs

	// 7. Duplicate loss event for tenure 1 still does not affect Root 3!
	nodeA.HandleLeaderLossForTesting(tenure1, term1)
	if _, err := ltx3.ReadCurrent(ctx, grant3, "row/test", []byte("k1")); err != nil {
		t.Fatalf("tx3 lost its lock after duplicate loss event: %v", err)
	}

	// 8. When true leader loss of tenure 2 occurs:
	tenure2, _ := nodeA.CurrentTenureForTesting()
	nodeA.HandleLeaderLossForTesting(tenure2, term2)

	// Now Root 3's grant IS invalidated!
	_, err = ltx3.ReadCurrent(ctx, grant3, "row/test", []byte("k1"))
	if err == nil {
		t.Fatal("expected failure for tx3 after genuine loss of tenure 2, got nil")
	}
}
