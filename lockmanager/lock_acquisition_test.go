package lockmanager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lock_acquisition_test.go drives the exported Manager façade.
//
// The other test files inspect internal state through unexported helpers; this one uses
// only what the backend adapter will use, so it doubles as the contract test for the
// package's public surface. It covers the cases the phase requires and that the matrix and
// queue tests do not: re-entrant acquisition, upgrade competition, duplicate release and
// illegal states, per-table and per-index resource separation, and concurrent safety.

// indexRecord builds a record lock on one encoded index key.
//
// It is deliberately spelled out rather than reused from the helpers: the identity fields
// are the point of these tests, and a helper that quietly filled them in would hide exactly
// what is being asserted.
func indexRecord(tableID, indexID string, key []byte) ResourceRef {
	return ResourceRef{Identity: Identity{
		Kind:    KindRecord,
		Mode:    ModeX,
		Space:   "index/" + indexID,
		TableID: tableID,
		IndexID: indexID,
		Key:     key,
	}}
}

// rowBundle pairs one record lock with the table intention its mode requires, which is the
// shape a real SQL write uses: the intention and the row protection are one atomic bundle.
func rowBundle(record ResourceRef) []ResourceRef {
	mode := record.Mode
	if mode == "" {
		mode = ModeX
	}
	return []ResourceRef{record, intentFor(mode, record.TableID)}
}

// indexRecordOf builds a record lock of a given mode on one encoded index key.
func indexRecordOf(mode Mode, tableID, indexID string, key []byte) ResourceRef {
	record := indexRecord(tableID, indexID, key)
	record.Mode = mode
	return record
}

// sharedBundle pairs a shared record lock with the IS it requires.
func sharedBundle(tableID, indexID string, key []byte) []ResourceRef {
	record := indexRecordOf(ModeS, tableID, indexID, key)
	return []ResourceRef{record, intentFor(ModeS, tableID)}
}

func ixIntent(tableID string) ResourceRef {
	return ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: tableID}}
}

func isIntent(tableID string) ResourceRef {
	return ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIS, Space: "table", TableID: tableID}}
}

// newPublicManager returns the exported façade over a fake clock.
func newPublicManager(t *testing.T, limits BudgetLimits) (*Manager, *FakeClock) {
	t.Helper()
	clock := newTestClock()
	m, err := NewManager(limits, 1, clock)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(m.Close)
	return m, clock
}

// publicOwner registers one owner and opens one statement scope through the façade.
type publicOwner struct {
	t       *testing.T
	manager *Manager
	rootID  string
	scopeID uint64
}

func newPublicOwner(t *testing.T, m *Manager, rootID string) publicOwner {
	t.Helper()
	if _, err := m.RegisterOwner(rootID, 1, 0, 10); err != nil {
		t.Fatalf("RegisterOwner(%s): %v", rootID, err)
	}
	scope, err := m.OpenScope(rootID, rootID+"-txn", 0, ScopeStatement, time.Time{})
	if err != nil {
		t.Fatalf("OpenScope(%s): %v", rootID, err)
	}
	return publicOwner{t: t, manager: m, rootID: rootID, scopeID: scope.ID}
}

func (o publicOwner) acquire(ctx context.Context, components ...ResourceRef) AcquireResult {
	o.t.Helper()
	result, err := o.manager.Acquire(ctx, o.rootID, o.scopeID, components)
	if err != nil {
		o.t.Fatalf("%s: acquire: %v", o.rootID, err)
	}
	return result
}

func (o publicOwner) grant(ctx context.Context, components ...ResourceRef) AcquireResult {
	o.t.Helper()
	result := o.acquire(ctx, components...)
	switch result.Op {
	case LockOpGranted:
		return result
	case LockOpReentrant:
		return result
	default:
		o.t.Fatalf("%s: acquisition is waiting, want a grant", o.rootID)
		return AcquireResult{}
	}
}

func (o publicOwner) wait(ctx context.Context, components ...ResourceRef) AcquireResult {
	o.t.Helper()
	result := o.acquire(ctx, components...)
	if result.Op != LockOpWait || result.Wait == nil {
		o.t.Fatalf("%s: acquisition op = %s, want WAIT", o.rootID, result.Op)
	}
	return result
}

// TestB02RecordSharedAndExclusiveMatrix drives the exported façade through the S/S, S/X and
// X/X cases and observes the outcome of each.
func TestB02RecordSharedAndExclusiveMatrix(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name          string
		first, second Mode
		wantWait      bool
	}{
		{"S then S coexists", ModeS, ModeS, false},
		{"S then X waits", ModeS, ModeX, true},
		{"X then S waits", ModeX, ModeS, true},
		{"X then X waits", ModeX, ModeX, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newPublicManager(t, BudgetLimits{})
			first := newPublicOwner(t, m, "root-a")
			second := newPublicOwner(t, m, "root-b")
			key := []byte("k1")

			firstWants := []ResourceRef{indexRecordOf(tc.first, "t1", "PRIMARY", key), intentFor(tc.first, "t1")}
			secondWants := []ResourceRef{indexRecordOf(tc.second, "t1", "PRIMARY", key), intentFor(tc.second, "t1")}
			first.grant(ctx, firstWants...)

			result := second.acquire(ctx, secondWants...)
			if tc.wantWait {
				if result.Op != LockOpWait {
					t.Fatalf("second acquisition op = %s, want WAIT", result.Op)
				}
				// The first owner's release completes the wait.
				if _, err := m.Release("root-a", firstWants[0]); err != nil {
					t.Fatal(err)
				}
				if _, err := m.Release("root-a", firstWants[1]); err != nil {
					t.Fatal(err)
				}
				if _, err := m.WaitForGrant(ctx, "root-b", result.Wait); err != nil {
					t.Fatalf("waiting for the grant: %v", err)
				}
			} else if result.Op != LockOpGranted {
				t.Fatalf("second acquisition op = %s, want GRANTED", result.Op)
			}
			// Both owners hold the shared lock in the S/S case.
			holders := map[string]bool{}
			for _, entry := range m.Snapshot() {
				if entry.Resource.Kind != KindRecord {
					continue
				}
				for _, root := range entry.HolderRoots {
					holders[root] = true
				}
			}
			// The first owner keeps its grant only when it was not asked to hand it over.
			// When the second request waited, the first owner released the row on purpose.
			if !tc.wantWait && !holders["root-a"] {
				t.Fatalf("root-a lost its grant: %+v", m.Snapshot())
			}
			if tc.wantWait && !holders["root-b"] {
				t.Fatalf("root-b never acquired after the wait: %+v", m.Snapshot())
			}
			if !tc.wantWait && !holders["root-b"] {
				t.Fatalf("root-b should hold the shared lock: %+v", m.Snapshot())
			}
		})
	}
}

// intentFor returns the table intention a record mode requires in the same bundle.
func intentFor(mode Mode, tableID string) ResourceRef {
	if mode == ModeX || mode == ModeInsertIntention {
		return ixIntent(tableID)
	}
	return isIntent(tableID)
}

// TestB02ReentrantAcquisitionIsFree proves repeated acquisition by the same transaction is
// a no-op that costs nothing: the same bundle taken twice reports REENTRANT, keeps one
// grant, and does not grow the accounting.
func TestB02ReentrantAcquisitionIsFree(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	owner := newPublicOwner(t, m, "root-a")
	bundle := []ResourceRef{indexRecord("t1", "PRIMARY", []byte("k1")), ixIntent("t1")}

	first := owner.grant(ctx, bundle...)
	if first.Op != LockOpGranted {
		t.Fatalf("first acquisition op = %s, want GRANTED", first.Op)
	}
	afterFirst, _ := m.Ledger().Usage("root-a")

	// Repeating the exact bundle is a no-op.
	second := owner.acquire(ctx, bundle...)
	if second.Op != LockOpReentrant {
		t.Fatalf("repeated acquisition op = %s, want REENTRANT", second.Op)
	}
	// Repeating it with the weaker mode is also satisfied by what is held.
	weaker := []ResourceRef{{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "index/PRIMARY", TableID: "t1", IndexID: "PRIMARY", Key: []byte("k1")}}, isIntent("t1")}
	if got := owner.acquire(ctx, weaker...); got.Op != LockOpReentrant {
		t.Fatalf("weaker repeated acquisition op = %s, want REENTRANT", got.Op)
	}
	afterRepeat, _ := m.Ledger().Usage("root-a")
	if afterFirst.Components != afterRepeat.Components || afterFirst.MemoryBytes != afterRepeat.MemoryBytes {
		t.Fatalf("a repeated acquisition changed the accounting: %+v -> %+v", afterFirst, afterRepeat)
	}
	// Exactly one grant is recorded for the row.
	if mode, ok := m.HeldMode("root-a", bundle[0]); !ok || mode != ModeX {
		t.Fatalf("held mode = %v/%v, want X", mode, ok)
	}
	if got := holderCountOf(m.Snapshot(), bundle[0]); got != 1 {
		t.Fatalf("row holders = %d, want 1", got)
	}
}

// TestB02UpgradeCompetition covers the two upgrade directions.
//
// It is the case the contract calls out: an upgrade must wait for the other holders, must
// keep the S it already holds while it waits, and must not deadlock with a competing
// upgrade from the other owner.
func TestB02UpgradeCompetition(t *testing.T) {
	t.Run("S to X waits for the other shared holder and keeps S", func(t *testing.T) {
		ctx := context.Background()
		m, _ := newPublicManager(t, BudgetLimits{})
		a := newPublicOwner(t, m, "root-a")
		b := newPublicOwner(t, m, "root-b")
		row := indexRecord("t1", "PRIMARY", []byte("k1"))

		a.grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
		b.grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
		// root-a upgrades. Its S is retained while the upgrade waits.
		upgrade := a.wait(ctx, rowBundle(row)...)
		if mode, ok := m.HeldMode("root-a", row); !ok || mode != ModeS {
			t.Fatalf("during the upgrade root-a holds %v/%v, want the retained S", mode, ok)
		}
		// root-b releasing its shared lock lets the upgrade through. root-b holds one claim,
		// so one release is enough.
		if _, err := m.Release("root-b", row); err != nil {
			t.Fatal(err)
		}
		if _, err := m.WaitForGrant(ctx, "root-a", upgrade.Wait); err != nil {
			t.Fatalf("upgrade wait: %v", err)
		}
		if mode, ok := m.HeldMode("root-a", row); !ok || mode != ModeX {
			t.Fatalf("after the upgrade root-a holds %v/%v, want X", mode, ok)
		}
	})

	t.Run("competing upgrades both queue and then resolve", func(t *testing.T) {
		ctx := context.Background()
		m, _ := newPublicManager(t, BudgetLimits{})
		a := newPublicOwner(t, m, "root-a")
		b := newPublicOwner(t, m, "root-b")
		c := newPublicOwner(t, m, "root-c")
		row := indexRecord("t1", "PRIMARY", []byte("k1"))

		a.grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
		b.grant(ctx, sharedBundle("t1", "PRIMARY", []byte("k1"))...)
		// Both upgrade; each is blocked by the other's S, so both queue. This is the
		// double-upgrade shape a deadlock detector must be able to see later.
		first := a.wait(ctx, rowBundle(row)...)
		second := b.wait(ctx, rowBundle(row)...)
		if got := m.PendingRequests("root-a"); got != 1 {
			t.Fatalf("root-a pending = %d, want 1", got)
		}
		if got := m.PendingRequests("root-b"); got != 1 {
			t.Fatalf("root-b pending = %d, want 1", got)
		}
		// A third owner collides with the shared lock root-a still holds, so it queues behind
		// both upgrades. It is withdrawn immediately because nothing will release the row for
		// it in this test.
		third := c.acquire(ctx, rowBundle(row)...)
		if third.Op != LockOpWait {
			t.Fatalf("third acquisition op = %s, want WAIT", third.Op)
		}
		if status := m.Cancel("root-c", third.Wait); status != RequestCanceled {
			t.Fatalf("third cancel = %s, want CANCELED", status)
		}
		// Cancelling one upgrade withdraws that request, and its already-granted components
		// are retracted. It does not end the owner, so the shared lock the owner holds from
		// its earlier request is still in place and still blocks the other upgrade.
		if status := m.Cancel("root-b", second.Wait); status != RequestCanceled {
			t.Fatalf("cancel status = %s, want CANCELED", status)
		}
		if got := m.PendingRequests("root-b"); got != 0 {
			t.Fatalf("root-b pending = %d after cancel, want 0", got)
		}
		if mode, ok := m.HeldMode("root-b", row); !ok || mode != ModeS {
			t.Fatalf("root-b holds %v/%v after cancelling only its upgrade, want the S it still owns", mode, ok)
		}
		// Releasing the shared lock is what lets the surviving upgrade through. This is the
		// contract's rule: a deadlock detector asks the victim's root to end, and a plain
		// cancellation only withdraws the request it names.
		if _, err := m.Release("root-b", row); err != nil {
			t.Fatal(err)
		}
		if _, err := m.WaitForGrant(ctx, "root-a", first.Wait); err != nil {
			t.Fatalf("root-a upgrade wait: %v", err)
		}
		if mode, ok := m.HeldMode("root-a", row); !ok || mode != ModeX {
			t.Fatalf("root-a holds %v/%v, want X", mode, ok)
		}
		if got := m.PendingRequests("root-c"); got != 0 {
			t.Fatalf("root-c pending = %d, want its withdrawn request gone", got)
		}
	})
}

// TestB02DuplicateReleaseAndIllegalState covers releases that should change nothing and
// operations against unknown or ended owners and scopes.
func TestB02DuplicateReleaseAndIllegalState(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	a.grant(ctx, rowBundle(row)...)
	// A first release drops the grant, a second reports false and changes nothing.
	if released, err := m.Release("root-a", row); err != nil || !released {
		t.Fatalf("first release = %v/%v, want true", released, err)
	}
	afterFirst, _ := m.Ledger().Usage("root-a")
	if released, err := m.Release("root-a", row); err != nil || released {
		t.Fatalf("second release = %v/%v, want false and no error", released, err)
	}
	afterSecond, _ := m.Ledger().Usage("root-a")
	if afterFirst.Components != afterSecond.Components {
		t.Fatalf("a duplicate release changed the accounting: %+v -> %+v", afterFirst, afterSecond)
	}
	if _, err := m.Release("root-a", ixIntent("t1")); err != nil {
		t.Fatalf("releasing the table intention: %v", err)
	}
	// Releasing a resource the owner never held is likewise a no-op.
	if released, err := m.Release("root-a", indexRecord("t1", "PRIMARY", []byte("never"))); err != nil || released {
		t.Fatalf("release of an unheld resource = %v/%v, want false", released, err)
	}
	// Releasing for an unknown owner is refused, not silently accepted.
	if _, err := m.Release("root-missing", row); err != ErrOwnerUnknown {
		t.Fatalf("release for an unknown owner = %v, want ErrOwnerUnknown", err)
	}

	// An unknown scope and an unknown owner are refused before any state changes.
	if _, err := m.Acquire(ctx, "root-missing", a.scopeID, []ResourceRef{row}); err != ErrOwnerUnknown {
		t.Fatalf("acquire for an unknown owner = %v, want ErrOwnerUnknown", err)
	}
	if _, err := m.Acquire(ctx, "root-a", 99999, []ResourceRef{row}); err != ErrScopeUnknown {
		t.Fatalf("acquire with an unknown scope = %v, want ErrScopeUnknown", err)
	}
	// Take a grant, then close the scope that asked for it. A closed scope may not issue new
	// requests, but the grant it took belongs to the root and survives.
	a.grant(ctx, rowBundle(row)...)
	if _, err := m.CloseScope("root-a", a.scopeID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, "root-a", a.scopeID, []ResourceRef{row}); err != ErrScopeUnknown {
		t.Fatalf("acquire with a closed scope = %v, want ErrScopeUnknown", err)
	}
	// A closed scope's grant is still held by the root, which is the whole point of keeping
	// ownership at the root.
	if mode, ok := m.HeldMode("root-a", row); !ok || mode != ModeX {
		t.Fatalf("the closed scope released a grant: %v/%v", mode, ok)
	}
	// A duplicate owner registration is refused.
	if _, err := m.RegisterOwner("root-a", 1, 0, 10); err != ErrOwnerExists {
		t.Fatalf("duplicate registration = %v, want ErrOwnerExists", err)
	}
	// A bundle that fails validation leaves nothing behind.
	if _, err := m.Acquire(ctx, "root-a", a.scopeID, []ResourceRef{row}); err == nil {
		t.Fatal("expected the closed scope to refuse")
	}
	if _, err := m.Acquire(ctx, "root-a", a.scopeID, nil); err == nil {
		t.Fatal("an empty bundle must be refused")
	}
}

// TestB02ResourceSeparationByTableAndIndex proves the resource identity separates the
// physical resources the contract names: a table, an index namespace, and an encoded key.
//
// The point is that a business primary-key string is never the whole identity: the same key
// bytes in a different table or a different index are different resources, and a lock on
// one does not block the other.
func TestB02ResourceSeparationByTableAndIndex(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")

	key := []byte{0x00, 0x01}
	// The same key bytes in three different physical resources.
	primaryT1 := indexRecord("t1", "PRIMARY", key)
	uniqueT1 := indexRecord("t1", "u1", key)
	primaryT2 := indexRecord("t2", "PRIMARY", key)

	a.grant(ctx, rowBundle(primaryT1)...)
	// A different index namespace of the same table is a different resource.
	if got := b.acquire(ctx, rowBundle(uniqueT1)...); got.Op == LockOpWait {
		t.Fatal("a different index namespace must not be blocked by the primary index lock")
	}

	// A different table is a different resource even with the same index name and key.
	if got := b.acquire(ctx, rowBundle(primaryT2)...); got.Op == LockOpWait {
		t.Fatal("a different table must not be blocked by another table's lock")
	}
	// The identical resource still conflicts.
	if got := b.acquire(ctx, rowBundle(primaryT1)...); got.Op != LockOpWait {
		t.Fatalf("the identical resource op = %s, want WAIT", got.Op)
	}
	// And a different key in the same index does not.
	if got := b.acquire(ctx, rowBundle(indexRecord("t1", "PRIMARY", []byte{0x00, 0x02}))...); got.Op == LockOpWait {
		t.Fatal("a different encoded key must not be blocked")
	}

	// The identity the manager built must carry the labels, not just the key.
	stored := m.Snapshot()
	found := false
	for _, entry := range stored {
		if entry.Resource.Kind != KindRecord {
			continue
		}
		if entry.Resource.TableID == "t1" && entry.Resource.IndexID == "PRIMARY" && string(entry.Resource.Key) == string(key) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the locked resource lost its table/index identity: %+v", stored)
	}
}

// TestB02ConcurrentAcquireAndReleaseIsSafe drives the exported façade from many goroutines.
//
// The race detector is unavailable on this host (no C toolchain), so this test does not
// claim to prove the absence of a data race. What it proves is the property a race would
// destroy: with a deterministic amount of concurrent traffic, the accounting returns to
// exactly zero and no grant is lost or duplicated.
func TestB02ConcurrentAcquireAndReleaseIsSafe(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{MemoryBytes: 64 << 20, RootRequests: 1 << 20, Waiters: 1 << 16, GraphWork: 1 << 16})

	const owners = 8
	const goroutines = 8
	const rounds = 60

	holder := newPublicOwner(t, m, "root-holder")
	row := indexRecord("t1", "PRIMARY", []byte("contended"))
	holder.grant(ctx, rowBundle(row)...)

	var granted, blocked int64
	start := NewBarrier()
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			owner := newPublicOwner(t, m, fmt.Sprintf("root-%d", worker))
			if !start.Wait(ctx) {
				return
			}
			for round := 0; round < rounds; round++ {
				// Every worker contends for the same row and also takes a private row, so
				// the test exercises both a contended and an uncontended resource at once.
				private := indexRecord("t1", "PRIMARY", []byte(fmt.Sprintf("private-%d-%d", worker, round)))
				if got := owner.acquire(ctx, rowBundle(private)...); got.Op == LockOpWait {
					// The private key is never contended, so waiting would be a bug.
					t.Errorf("worker %d waited on a private key", worker)
					return
				}
				contended := owner.acquire(ctx, rowBundle(row)...)
				if contended.Op == LockOpWait {
					atomic.AddInt64(&blocked, 1)
					// Withdraw immediately: the holder owns the row for the whole test.
					if status := m.Cancel(owner.rootID, contended.Wait); status != RequestCanceled {
						t.Errorf("worker %d cancel = %s, want CANCELED", worker, status)
					}
				} else {
					atomic.AddInt64(&granted, 1)
					if _, err := m.Release(owner.rootID, row); err != nil {
						t.Errorf("worker %d release: %v", worker, err)
					}
				}
			}
			m.ReleaseOwner(owner.rootID)
		}(g)
	}
	start.Arrive()
	wg.Wait()

	if atomically(&blocked) == 0 {
		t.Fatal("the test premise failed: nothing was ever blocked")
	}
	// Every worker released its owner, and the holder still owns the row.
	m.ReleaseOwner("root-holder")
	if size := m.LockTableSize(); size != 0 {
		t.Fatalf("lock table = %d entries after releasing every owner, want 0", size)
	}
	requireBudgetZero(t, m.Ledger())
	if stats := m.Stats(); stats.PendingRequests != 0 || stats.GrantedComponents != 0 {
		t.Fatalf("stats after a full release: %+v", stats)
	}
	t.Logf("concurrent traffic: granted=%d blocked=%d", atomically(&granted), atomically(&blocked))
}

func atomically(v *int64) int64 { return atomic.LoadInt64(v) }

// TestB02WaitHonoursContextAndDeliversOneStatus proves the blocking wait path: a queued
// caller is woken by the holder's release, and a caller whose context ends first is told
// so without consuming a grant that arrived at the same moment.
func TestB02WaitHonoursContextAndDeliversOneStatus(t *testing.T) {
	t.Run("release wakes the waiter", func(t *testing.T) {
		ctx := context.Background()
		m, _ := newPublicManager(t, BudgetLimits{})
		a := newPublicOwner(t, m, "root-a")
		b := newPublicOwner(t, m, "root-b")
		row := indexRecord("t1", "PRIMARY", []byte("k1"))
		a.grant(ctx, rowBundle(row)...)
		result := b.wait(ctx, rowBundle(row)...)

		done := make(chan error, 1)
		go func() {
			_, err := m.WaitForGrant(ctx, "root-b", result.Wait)
			done <- err
		}()
		if _, err := m.Release("root-a", row); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("waiting for the grant: %v", err)
			}
		case <-time.After(5 * time.Second):
			// A bounded guard, not a scheduling device: the release above is what must wake
			// the waiter, so a timeout here is a real failure rather than a slow machine.
			t.Fatal("the waiter was not woken by the holder's release")
		}
	})

	t.Run("context cancelled before the release withdraws the request", func(t *testing.T) {
		m, _ := newPublicManager(t, BudgetLimits{})
		ownerCtx := context.Background()
		a := newPublicOwner(t, m, "root-a")
		b := newPublicOwner(t, m, "root-b")
		row := indexRecord("t1", "PRIMARY", []byte("k1"))
		a.grant(ownerCtx, rowBundle(row)...)
		result := b.wait(ownerCtx, rowBundle(row)...)

		waitCtx, cancel := context.WithCancel(ownerCtx)
		cancel()
		if _, err := m.WaitForGrant(waitCtx, "root-b", result.Wait); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled wait reported %v, want context.Canceled", err)
		}
		// The request is withdrawn as well as the caller woken. A queued entry whose caller has
		// gone away would keep blocking every later request on that resource while nobody is
		// waiting for it, so the withdrawal is part of the cancellation, not a separate step.
		if got := m.PendingRequests("root-b"); got != 0 {
			t.Fatalf("root-b pending = %d, want the request withdrawn", got)
		}
		if status := m.WaitStatus("root-b", result.Wait); status != RequestContextCanceled {
			t.Fatalf("status = %s, want CONTEXT_CANCELED", status)
		}
		// Releasing the holder grants nobody, because the only waiter is gone.
		if _, err := m.Release("root-a", row); err != nil {
			t.Fatal(err)
		}
		if _, ok := m.HeldMode("root-b", row); ok {
			t.Fatal("a cancelled waiter was granted the lock")
		}
	})
}

// TestB02ReleaseRetractsOnlyItsOwnClaim is the regression test for the claim accounting.
//
// A grant is not "owner to mode". It is the strongest of the claims the owner's requests
// hold on the resource, and each request owns its own claim. Without that, releasing the
// shared lock a transaction took first would silently drop the exclusive mode it upgraded
// to, and a waiting upgrade would not offer exclusive protection at all. Both directions are
// asserted here.
func TestB02ReleaseRetractsOnlyItsOwnClaim(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")
	c := newPublicOwner(t, m, "root-c")
	key := []byte("k1")
	row := indexRecord("t1", "PRIMARY", key)

	// root-a and root-b both hold S on the row.
	a.grant(ctx, sharedBundle("t1", "PRIMARY", key)...)
	b.grant(ctx, sharedBundle("t1", "PRIMARY", key)...)

	// root-a's upgrade is blocked by root-b's S. Its own S is retained, and the waiting
	// upgrade already offers X, which is what blocks root-c immediately.
	upgrade := a.wait(ctx, rowBundle(row)...)
	if mode, ok := m.HeldMode("root-a", row); !ok || mode != ModeS {
		t.Fatalf("the retained mode is %v/%v, want S", mode, ok)
	}
	young := c.acquire(ctx, sharedBundle("t1", "PRIMARY", key)...)
	if young.Op != LockOpWait {
		t.Fatalf("a young shared request op = %s, want WAIT behind the waiting upgrade", young.Op)
	}
	if status := m.Cancel("root-c", young.Wait); status != RequestCanceled {
		t.Fatalf("young request status = %s, want CANCELED while blocked", status)
	}

	// Releasing root-a's shared claim must not retract the exclusive claim its upgrade holds:
	// the upgrade is still waiting and still protects the row.
	released, err := m.Release("root-a", row)
	if err != nil || !released {
		t.Fatalf("release = %v/%v, want true", released, err)
	}
	if mode, ok := m.OfferedMode("root-a", row); !ok || mode != ModeX {
		t.Fatalf("root-a offers %v/%v after releasing only its S claim, want the pending X", mode, ok)
	}
	if mode, ok := m.HeldMode("root-b", row); !ok || mode != ModeS {
		t.Fatalf("root-b lost its claim: %v/%v", mode, ok)
	}

	// root-b releasing lets root-a's upgrade complete.
	if _, err := m.Release("root-b", row); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitForGrant(ctx, "root-a", upgrade.Wait); err != nil {
		t.Fatalf("upgrade wait: %v", err)
	}
	if mode, ok := m.HeldMode("root-a", row); !ok || mode != ModeX {
		t.Fatalf("root-a holds %v/%v after the upgrade, want X", mode, ok)
	}
}

// TestB02ReentrantUpgradeWhileWaitingAdoptsThePendingClaim proves that asking again for the
// very same upgrade while it is already queued is a no-op rather than a second claim: a
// duplicate claim would need two releases to retract and would keep the row blocked for one
// release too long.
func TestB02ReentrantUpgradeWhileWaitingAdoptsThePendingClaim(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	a := newPublicOwner(t, m, "root-a")
	b := newPublicOwner(t, m, "root-b")
	c := newPublicOwner(t, m, "root-c")
	key := []byte("k1")
	row := indexRecord("t1", "PRIMARY", key)

	a.grant(ctx, sharedBundle("t1", "PRIMARY", key)...)
	b.grant(ctx, sharedBundle("t1", "PRIMARY", key)...)
	first := a.wait(ctx, rowBundle(row)...)

	// The same request again is a no-op: the protection is already queued, so the transaction
	// still owns exactly one exclusive claim and no second request is created.
	second := a.acquire(ctx, rowBundle(row)...)
	if second.Op != LockOpReentrant {
		t.Fatalf("duplicate upgrade op = %s, want REENTRANT because the upgrade is already queued", second.Op)
	}
	if got := m.PendingRequests("root-a"); got != 1 {
		t.Fatalf("root-a pending = %d, want the single upgrade", got)
	}
	// The original upgrade is untouched and still completes when root-b releases.
	if _, err := m.Release("root-b", row); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitForGrant(ctx, "root-a", first.Wait); err != nil {
		t.Fatalf("original upgrade wait: %v", err)
	}
	if mode, ok := m.HeldMode("root-a", row); !ok || mode != ModeX {
		t.Fatalf("root-a holds %v/%v, want X", mode, ok)
	}
	// The row is handed to the next owner once root-a's claims are gone. root-a holds one
	// shared claim and one exclusive claim, so both are released; a duplicate upgrade would
	// have added a third.
	waiting := c.wait(ctx, rowBundle(row)...)
	if _, err := m.Release("root-a", row); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("root-a", row); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitForGrant(ctx, "root-c", waiting.Wait); err != nil {
		t.Fatalf("root-c wait after releasing root-a's claims: %v", err)
	}
}

// TestB02NoGlobalTransactionGate is the anti-regression test for the rule the constitution
// states outright: a global transaction mutex must never substitute for row-level
// concurrency. It holds a lock on one row and proves many other rows and tables proceed.
func TestB02NoGlobalTransactionGate(t *testing.T) {
	ctx := context.Background()
	m, _ := newPublicManager(t, BudgetLimits{})
	holder := newPublicOwner(t, m, "root-holder")
	holder.grant(ctx, rowBundle(indexRecord("t1", "PRIMARY", []byte("held")))...)

	// Many independent owners, rows, tables and indexes must all be granted immediately.
	other := newPublicOwner(t, m, "root-other")
	for i := 0; i < 64; i++ {
		row := indexRecord("t1", "PRIMARY", []byte(fmt.Sprintf("free-%d", i)))
		if got := other.acquire(ctx, rowBundle(row)...); got.Op == LockOpWait {
			t.Fatalf("row %d was blocked by a global gate", i)
		}
	}
	for _, table := range []string{"t2", "t3", "t4"} {
		if got := other.acquire(ctx, rowBundle(indexRecord(table, "PRIMARY", []byte("held")))...); got.Op == LockOpWait {
			t.Fatalf("table %s was blocked by a global gate", table)
		}
	}
	// Only the identical resource waits.
	if got := other.acquire(ctx, rowBundle(indexRecord("t1", "PRIMARY", []byte("held")))...); got.Op != LockOpWait {
		t.Fatalf("the identical resource op = %s, want WAIT", got.Op)
	}
}
