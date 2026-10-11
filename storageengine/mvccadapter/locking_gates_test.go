package mvccadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_gates_test.go closes the last two Phase 5 gates:
//
//   - the two neighbour probes a locked read uses to bound an absent key's gap happen at two different
//     moments, so a concurrent INSERT or DELETE can land between them. These tests pin what those stale
//     bounds still guarantee: the absent key's own position is always protected, and stale bounds can
//     only ever be *wider* than the true gap, never a hole around the key the statement reported missing;
//   - closing a statement scope must leave no pending request, no wait edge, no phantom waiter and no
//     ledger charge.
//
// Everything here drives the neutral locking capability, so the sequence under test is the one the SQL
// path runs, not a second implementation of it.

// t032Table is the table label the gate components carry, so the bundles below are the same shape the
// SQL path builds (a labelled record or interval plus its table intention).
const t032Table = "t032"

// t032Locking returns the neutral locking capability of one adapter transaction.
func t032Locking(t *testing.T, tx *txn) storageengine.LockingTxn {
	t.Helper()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("locking capability: %v", err)
	}
	return locking
}

// t032Scope opens one statement scope with the contract's per-wait budget.
func t032Scope(t *testing.T, ctx context.Context, locking storageengine.LockingTxn) uint64 {
	t.Helper()
	scope, err := locking.BeginLockScopeBudget(ctx, storageengine.LockScopeStatement, 5*time.Second)
	if err != nil {
		t.Fatalf("begin scope: %v", err)
	}
	return scope
}

// t05RecordClaim is the per-record claim of one key: the record plus the table intention in one bundle,
// exactly as the locked read builds it.
func t05RecordClaim(key string, mode storageengine.LockMode) []storageengine.LockComponent {
	intent := storageengine.LockModeIX
	if mode == storageengine.LockModeS {
		intent = storageengine.LockModeIS
	}
	return []storageengine.LockComponent{
		{Mode: mode, Kind: storageengine.LockComponentRecord, Space: t032Space, TableID: t032Table, Key: []byte(key)},
		{Mode: intent, Kind: storageengine.LockComponentTableIntent, Space: t032Space, TableID: t032Table},
	}
}

// t05InsertionClaim is what another session's INSERT of one key asks for.
func t05InsertionClaim(key string) []storageengine.LockComponent {
	return []storageengine.LockComponent{
		{Mode: storageengine.LockModeInsertIntention, Kind: storageengine.LockComponentRecord, Space: t032Space, TableID: t032Table, Key: []byte(key)},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: t032Space, TableID: t032Table, Key: []byte(key)},
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: t032Space, TableID: t032Table},
	}
}

// t05IntervalClaim is the gap around one absent key, bounded by the two neighbour keys.
func t05IntervalClaim(previous, next []byte) []storageengine.LockComponent {
	return []storageengine.LockComponent{
		{Mode: storageengine.LockModeGap, Kind: storageengine.LockComponentInterval, Space: t032Space, TableID: t032Table,
			Lower: previous, Upper: next},
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: t032Space, TableID: t032Table},
	}
}

// t05InsertBlocked reports whether one insertion claim is refused without waiting.
//
// A refusal is the observable form of "the gap protects this position": the insertion intention is
// inside the interval, so the manager answers ErrConflict instead of granting it.
func t05InsertBlocked(t *testing.T, ctx context.Context, locking storageengine.LockingTxn, scope uint64, key string) bool {
	t.Helper()
	_, err := locking.Acquire(ctx, scope, storageengine.LockRequest{
		Components: t05InsertionClaim(key),
		ShouldWait: func(context.Context) bool { return false },
	})
	if err == nil {
		return false
	}
	if !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the insertion claim of %s failed with %v, want the neutral conflict", key, err)
	}
	return true
}

// t05ProtectAbsent runs the absent branch of the locked read: claim the key, read it, and if it is
// absent bound its gap by the two neighbours. Kept here so the stale-bound behaviour can be driven
// deterministically, because the SQL path cannot be paused between its two probes.
func t05ProtectAbsent(t *testing.T, ctx context.Context, locking storageengine.LockingTxn, scope uint64, key string) {
	t.Helper()
	grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{Components: t05RecordClaim(key, storageengine.LockModeX)})
	if err != nil {
		t.Fatalf("claim %s: %v", key, err)
	}
	observation, err := locking.ReadCurrent(ctx, grant, t032Space, []byte(key))
	if err != nil {
		t.Fatalf("read %s under its claim: %v", key, err)
	}
	if observation.Exists {
		t.Fatalf("%s already exists, so it has no absent-key gap to protect", key)
	}
	previous, next, err := locking.KeysAround(ctx, t032Space, []byte(key))
	if err != nil {
		t.Fatalf("keys around %s: %v", key, err)
	}
	if _, err := locking.Acquire(ctx, scope, storageengine.LockRequest{Components: t05IntervalClaim(previous, next)}); err != nil {
		t.Fatalf("protect the gap around %s: %v", key, err)
	}
}

// TestP5StaleBoundsAfterAConcurrentInsert proves a key committed between the two neighbour probes cannot
// open a hole around the absent key: the stale bounds make the protected interval wider than the true
// gap, and the absent key's position stays inside it.
func TestP5StaleBoundsAfterAConcurrentInsert(t *testing.T) {
	e := t018OpenEngine(t)
	ctx := context.Background()
	t032Seed(t, e, "k010", "v")
	t032Seed(t, e, "k100", "v")

	scanner := t018Begin(t, e)
	scannerLocking := t032Locking(t, scanner)
	scannerScope := t032Scope(t, ctx, scannerLocking)

	// The probes run here: the neighbours of k042 are k010 and k100.
	previous, next, err := scannerLocking.KeysAround(ctx, t032Space, []byte("k042"))
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != "k010" || string(next) != "k100" {
		t.Fatalf("neighbours of k042 = %q,%q, want k010,k100", previous, next)
	}
	// A concurrent session inserts a key between the probes' answers and commits it: the true gap for
	// k042 is now (k010,k050), while the scanner's bounds still say (k010,k100).
	t032Seed(t, e, "k050", "v")

	// The scanner protects the stale interval, exactly as the real path does.
	if _, err := scannerLocking.Acquire(ctx, scannerScope, storageengine.LockRequest{Components: t05IntervalClaim(previous, next)}); err != nil {
		t.Fatalf("protect the stale interval: %v", err)
	}

	writer := t018Begin(t, e)
	writerLocking := t032Locking(t, writer)
	writerScope := t032Scope(t, ctx, writerLocking)
	// The absent key's position is inside the stale interval, so it is still protected: this must never
	// depend on the two probes agreeing.
	if !t05InsertBlocked(t, ctx, writerLocking, writerScope, "k042") {
		t.Fatal("an insert at the absent key was allowed while its gap was protected by stale bounds")
	}
	// A position the concurrent insert created is inside the stale interval too: wider protection is
	// conservative, never unsafe.
	if !t05InsertBlocked(t, ctx, writerLocking, writerScope, "k060") {
		t.Fatal("an insert inside the stale interval was allowed")
	}
	// Beyond the stale upper bound the protection ends, which is what keeps the gap bounded rather than
	// covering the rest of the namespace.
	if t05InsertBlocked(t, ctx, writerLocking, writerScope, "k150") {
		t.Fatal("an insert beyond the stale upper bound was blocked")
	}
	_ = writer.Rollback()
	_ = scanner.Rollback()
	t018RequireNoResidue(t, e)
}

// TestP5StaleBoundsAfterAConcurrentDelete covers the other direction: the upper neighbour is deleted
// between the probes. The absent key stays protected, and the protection does not silently widen into
// the whole namespace behind the caller's back.
func TestP5StaleBoundsAfterAConcurrentDelete(t *testing.T) {
	e := t018OpenEngine(t)
	ctx := context.Background()
	t032Seed(t, e, "k010", "v")
	t032Seed(t, e, "k100", "v")

	scanner := t018Begin(t, e)
	scannerLocking := t032Locking(t, scanner)
	scannerScope := t032Scope(t, ctx, scannerLocking)
	previous, next, err := scannerLocking.KeysAround(ctx, t032Space, []byte("k042"))
	if err != nil {
		t.Fatal(err)
	}
	deleter := t018Begin(t, e)
	if err := deleter.Delete(t032Space, []byte("k100")); err != nil {
		t.Fatal(err)
	}
	if _, err := deleter.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := scannerLocking.Acquire(ctx, scannerScope, storageengine.LockRequest{Components: t05IntervalClaim(previous, next)}); err != nil {
		t.Fatalf("protect the stale interval: %v", err)
	}

	writer := t018Begin(t, e)
	writerLocking := t032Locking(t, writer)
	writerScope := t032Scope(t, ctx, writerLocking)
	// The statement reported k042 missing; nothing may insert it while its claim stands.
	if !t05InsertBlocked(t, ctx, writerLocking, writerScope, "k042") {
		t.Fatal("an insert at the absent key was allowed after its upper neighbour was deleted")
	}
	// Beyond the stale bound the protection ends: the decision was about k042, and its position is what
	// the claim covers.
	if t05InsertBlocked(t, ctx, writerLocking, writerScope, "k150") {
		t.Fatal("an insert beyond the stale upper bound was blocked")
	}
	_ = writer.Rollback()
	_ = scanner.Rollback()
	t018RequireNoResidue(t, e)
}

// TestP5AbsentKeyProtectionUnderConcurrency races a real insertion against the absent-key protection and
// asserts the safety invariant: whenever the insert succeeds, the reader must have observed the row. A
// single run cannot choose the interleaving, so the invariant is asserted and the repetition is what
// makes the race likely.
func TestP5AbsentKeyProtectionUnderConcurrency(t *testing.T) {
	for attempt := 0; attempt < 8; attempt++ {
		func() {
			e := t018OpenEngine(t)
			ctx := context.Background()
			t032Seed(t, e, "k010", "v")
			t032Seed(t, e, "k100", "v")

			scanner := t018Begin(t, e)
			scannerLocking := t032Locking(t, scanner)
			scannerScope := t032Scope(t, ctx, scannerLocking)
			// Both transactions are created here, on the test goroutine: the helper reports failures with
			// t.Fatalf, which is only legal from the goroutine running the test.
			inserter := t018Begin(t, e)
			inserterLocking := t032Locking(t, inserter)
			inserterScope := t032Scope(t, ctx, inserterLocking)

			inserted := make(chan error, 1)
			go func() {
				_, err := inserterLocking.Acquire(ctx, inserterScope, storageengine.LockRequest{
					Components: t05InsertionClaim("k042"),
					ShouldWait: func(context.Context) bool { return false },
				})
				inserted <- err
			}()

			t05ProtectAbsent(t, ctx, scannerLocking, scannerScope, "k042")
			// What the reader's decision rests on: it re-reads the key under its claim.
			grant, err := scannerLocking.Acquire(ctx, scannerScope, storageengine.LockRequest{Components: t05RecordClaim("k042", storageengine.LockModeS)})
			if err != nil {
				t.Fatalf("re-claim for the re-read: %v", err)
			}
			observation, err := scannerLocking.ReadCurrent(ctx, grant, t032Space, []byte("k042"))
			if err != nil {
				t.Fatalf("re-read under the claim: %v", err)
			}
			select {
			case insertErr := <-inserted:
				if insertErr == nil && !observation.Exists {
					t.Fatal("the insert at the absent key succeeded while the reader still observed it missing: the claim lost the position")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the racing insert never finished")
			}
			_ = inserter.Rollback()
			_ = scanner.Rollback()
			t018RequireNoResidue(t, e)
		}()
	}
}

// TestP5ScopeCloseLeavesNoPendingRequestOrLedgerCharge closes a statement scope while one of its requests
// is queued and checks every accounting view the lock manager offers: nothing pending, no wait edge, no
// ledger waiter or edge charge, and no grant a phantom waiter could collect later.
func TestP5ScopeCloseLeavesNoPendingRequestOrLedgerCharge(t *testing.T) {
	e := t018OpenEngine(t)
	ctx := context.Background()
	t032Seed(t, e, "k1", "v")
	t032Seed(t, e, "k2", "v")

	holder := t018Begin(t, e)
	holderLocking := t032Locking(t, holder)
	holderScope := t032Scope(t, ctx, holderLocking)
	if _, err := holderLocking.Acquire(ctx, holderScope, storageengine.LockRequest{Components: t05RecordClaim("k2", storageengine.LockModeX)}); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	waiter := t018Begin(t, e)
	waiterLocking := t032Locking(t, waiter)
	waiterScope := t032Scope(t, ctx, waiterLocking)
	// The baseline is taken with both owners registered: what must be released is the *request's*
	// accounting, not the owners' own fixed charge, which lives until their transactions end.
	baselineTable := e.locks.LockTableSize()
	baselineLedger := e.locks.Ledger().Totals()
	queued := make(chan error, 1)
	go func() {
		_, err := waiterLocking.Acquire(ctx, waiterScope, storageengine.LockRequest{Components: t05RecordClaim("k2", storageengine.LockModeX)})
		queued <- err
	}()
	t032AwaitPending(t, e, waiter.ID(), 1)
	// The waiting request is real state: it is charged to the ledger and it has a wait edge.
	if e.locks.Ledger().Waiters() == 0 {
		t.Fatal("a queued request was not charged to the ledger")
	}
	if edges := e.locks.WaitEdges(); len(edges) == 0 {
		t.Fatal("a queued request produced no wait edge")
	}

	// The scope closes under the waiting request, which is what ROLLBACK TO SAVEPOINT and a cancelled
	// statement do.
	if err := waiterLocking.CloseLockScope(ctx, waiterScope); err != nil {
		t.Fatalf("close scope: %v", err)
	}
	select {
	case err := <-queued:
		if err == nil {
			t.Fatal("a request whose scope closed while queued reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("closing the scope did not withdraw the queued request")
	}

	if stats := e.locks.Stats(); stats.PendingRequests != 0 || stats.WaitEdges != 0 {
		t.Fatalf("the closed scope left lock state behind: %+v", stats)
	}
	if edges := e.locks.WaitEdges(); len(edges) != 0 {
		t.Fatalf("the closed scope left %d wait edges: %v", len(edges), edges)
	}
	if ledger := e.locks.Ledger(); ledger.Waiters() != 0 || ledger.Edges() != 0 {
		t.Fatalf("the closed scope left ledger charges: waiters=%d edges=%d", ledger.Waiters(), ledger.Edges())
	}
	if size := e.locks.LockTableSize(); size > baselineTable {
		t.Fatalf("the lock table grew from %d to %d entries", baselineTable, size)
	}
	if totals := e.locks.Ledger().Totals(); totals != baselineLedger {
		t.Fatalf("the ledger did not return to its baseline: %+v vs %+v", totals, baselineLedger)
	}

	// Releasing the holder must not hand a grant to the withdrawn request: a phantom waiter would take
	// the row and block every later writer.
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	after := t018Begin(t, e)
	afterLocking := t032Locking(t, after)
	afterScope := t032Scope(t, ctx, afterLocking)
	if _, err := afterLocking.Acquire(ctx, afterScope, storageengine.LockRequest{Components: t05RecordClaim("k2", storageengine.LockModeX)}); err != nil {
		t.Fatalf("the row was not free after the holder ended, so a phantom waiter took it: %v", err)
	}
	_ = waiter.Rollback()
	_ = after.Rollback()
	t018RequireNoResidue(t, e)
}
