package mvccadapter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_view_test.go is T032: the lock-aware finite-page iterator of a locked current scan.
//
// What it pins, in the order the task states it:
//
//   - the scan protects the access interval first and then walks bounded pages;
//   - no storage view, cursor or gate is held while the iterator waits for a lock, which is shown
//     by an unrelated transaction committing *while* the iterator is queued;
//   - a key is claimed and re-read after the wait, so a row that was rewritten is returned with its
//     latest valid version and a row that was deleted is skipped rather than returned as it was
//     listed;
//   - reverse scans, LIMIT and EOF follow the same bounded-page discipline;
//   - Close, context cancel and epoch reset leave no lock resource behind.
//
// A page size of 64 rows (see currentScanPageSize) is deliberately small relative to the fixtures
// here, so "paged" is exercised rather than assumed.

// t032Space is the namespace the scan fixtures live in.
const t032Space = "rows/t032-locking-view"

// t032Seed commits one key/value pair in its own transaction.
func t032Seed(t *testing.T, e *engine, key, value string) {
	t.Helper()
	tx := t018Begin(t, e)
	if err := tx.Put(t032Space, []byte(key), []byte(value)); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("seed commit %s: %v", key, err)
	}
}

// t032Scan opens one locked scan over the whole namespace through the neutral capability.
func t032Scan(t *testing.T, ctx context.Context, tx storageengine.Txn, mode storageengine.LockMode, r storageengine.KeyRange, limit int) storageengine.Iterator {
	t.Helper()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("locking capability: %v", err)
	}
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatalf("begin scope: %v", err)
	}
	it, err := locking.NewCurrentIterator(ctx, scope, storageengine.CurrentScanRequest{
		Space: t032Space,
		Range: r,
		Limit: limit,
		Mode:  mode,
	})
	if err != nil {
		t.Fatalf("new current iterator: %v", err)
	}
	t.Cleanup(func() { it.Close() })
	return it
}

// t032Collect drains one iterator into a key鈫抳alue map.
func t032Collect(t *testing.T, it storageengine.Iterator) map[string]string {
	t.Helper()
	out := map[string]string{}
	for it.Next() {
		out[string(it.Key())] = string(it.Value())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	return out
}

// TestT032CurrentScanPagesWithoutHoldingAView proves the finite-page discipline from the outside.
//
// The iterator is stopped after its first row. It must hold no storage view at that moment: an
// unrelated transaction writing and committing a *different* namespace has to succeed, and so does
// a write to another key of the scanned namespace (which the scan's own interval, not a storage
// view, is what could ever block). A scan that kept its bbolt read transaction open across the
// consumer would fail this immediately.
func TestT032CurrentScanPagesWithoutHoldingAView(t *testing.T) {
	e := t018OpenEngine(t)
	ctx := context.Background()

	// More rows than one page, so the iterator really has to come back for a second page.
	for i := 0; i < currentScanPageSize+5; i++ {
		t032Seed(t, e, fmt.Sprintf("k%03d", i), fmt.Sprintf("v%03d", i))
	}

	scanner := t018Begin(t, e)
	it := t032Scan(t, ctx, scanner, storageengine.LockModeS, storageengine.KeyRange{}, 0)

	if !it.Next() {
		t.Fatalf("the first page yielded no row: %v", it.Err())
	}
	first := string(it.Key())

	// While the iterator is paused between two rows, an unrelated writer must not be blocked by it.
	writer := t018Begin(t, e)
	if err := writer.Put("rows/t032-unrelated", []byte("other"), []byte("value")); err != nil {
		t.Fatalf("an unrelated namespace was blocked by a paused locked scan: %v", err)
	}
	if _, err := writer.Commit(ctx); err != nil {
		t.Fatalf("an unrelated commit was blocked by a paused locked scan: %v", err)
	}
	// A key of the scanned namespace that is *outside* the visited rows is not claimed by a paginated
	// scan either: the scan claims a row when it visits it, not the whole namespace.
	other := t018Begin(t, e)
	if err := other.Put(t032Space, []byte("zzz-outside"), []byte("later")); err != nil {
		t.Fatalf("a key beyond the scanned rows was blocked: %v", err)
	}
	if _, err := other.Commit(ctx); err != nil {
		t.Fatalf("a key beyond the scanned rows could not commit: %v", err)
	}

	// The scan continues to the end and saw every seeded row exactly once.
	seen := map[string]string{first: ""}
	for it.Next() {
		key := string(it.Key())
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("the scan yielded %q twice", key)
		}
		seen[key] = string(it.Value())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	for i := 0; i < currentScanPageSize+5; i++ {
		key := fmt.Sprintf("k%03d", i)
		if _, ok := seen[key]; !ok {
			t.Fatalf("the scan missed %q (saw %d rows)", key, len(seen))
		}
	}
	if err := scanner.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// TestT032CurrentScanWaitsThenReReadsTheLatestVersion proves that a claimed key is re-read under the
// claim, and that the wait holds nothing of the storage.
//
// The holder has staged a *new* value for one row and keeps its transaction open. The scan must
// queue on that row (observable in the engine's own lock state, not inferred from timing), which is
// also the moment the test proves the scan holds no storage view by committing an unrelated write
// from a third transaction. Only after the holder commits does the scan continue, and the row it
// returns is the holder's committed value 鈥?never the value that was listed before the wait.
func TestT032CurrentScanWaitsThenReReadsTheLatestVersion(t *testing.T) {
	e := t018OpenEngine(t)
	ctx := context.Background()

	t032Seed(t, e, "k1", "one")
	t032Seed(t, e, "k2", "before")
	t032Seed(t, e, "k3", "three")

	holder := t018Begin(t, e)
	if err := holder.Put(t032Space, []byte("k2"), []byte("after")); err != nil {
		t.Fatalf("holder write: %v", err)
	}

	scanner := t018Begin(t, e)
	it := t032Scan(t, ctx, scanner, storageengine.LockModeS, storageengine.KeyRange{}, 0)

	// k1 is free; k2 is held by the open holder. The first row must arrive *before* the scan has to
	// wait on k2: a scan that claimed a whole page up front would block here instead of returning
	// the row it can already serve.
	if !it.Next() {
		t.Fatalf("the scan yielded no first row: %v", it.Err())
	}
	if got := string(it.Key()); got != "k1" {
		t.Fatalf("first row = %q, want k1", got)
	}
	consumed := map[string]string{"k1": string(it.Value())}
	type outcome struct {
		key, value string
		err        error
	}
	advanced := make(chan outcome, 32)
	go func() {
		for it.Next() {
			advanced <- outcome{key: string(it.Key()), value: string(it.Value())}
		}
		advanced <- outcome{err: it.Err()}
	}()

	// The scan is queued on the held row: that is the engine's own lock state, so the test does not
	// have to guess how long a wait would take.
	t032AwaitPending(t, e, scanner.ID(), 1)

	// While the scan waits, nothing of the storage is held by it: an unrelated transaction can write
	// and commit. This is the property the finite-page design exists for.
	unrelated := t018Begin(t, e)
	if err := unrelated.Put("rows/t032-during-wait", []byte("k"), []byte("v")); err != nil {
		t.Fatalf("an unrelated write was blocked while a locked scan waited: %v", err)
	}
	if _, err := unrelated.Commit(ctx); err != nil {
		t.Fatalf("an unrelated commit was blocked while a locked scan waited: %v", err)
	}

	// The holder commits: the scan's wait ends and it must observe the committed value.
	if _, err := holder.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	got := map[string]string{}
	for {
		select {
		case next := <-advanced:
			if next.err != nil {
				t.Fatalf("scan failed after the wait: %v", next.err)
			}
			if next.key == "" {
				goto drained
			}
			got[next.key] = next.value
		case <-time.After(10 * time.Second):
			t.Fatal("the scan never continued after the holder committed")
		}
	}
drained:
	if got["k2"] != "after" {
		t.Fatalf("k2 = %q, want the value committed by the holder while the scan waited", got["k2"])
	}
	if consumed["k1"] != "one" {
		t.Fatalf("the first row read %q, want one", consumed["k1"])
	}
	if got["k3"] != "three" {
		t.Fatalf("the scan lost rows: consumed=%v later=%v", consumed, got)
	}
	if len(got) != 2 {
		t.Fatalf("the scan returned %d rows after the wait, want k2 and k3: %v", len(got), got)
	}
	if err := scanner.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// TestT032CurrentScanSkipsARowDeletedWhileWaiting proves a listed key is not returned after it was
// deleted: the listed value was never observed under the claim, so returning it would report a row
// the current read never saw.
func TestT032CurrentScanSkipsARowDeletedWhileWaiting(t *testing.T) {
	e := t018OpenEngine(t)
	ctx := context.Background()

	t032Seed(t, e, "k1", "one")
	t032Seed(t, e, "k2", "two")
	t032Seed(t, e, "k3", "three")

	holder := t018Begin(t, e)
	if err := holder.Delete(t032Space, []byte("k2")); err != nil {
		t.Fatalf("holder delete: %v", err)
	}

	scanner := t018Begin(t, e)
	it := t032Scan(t, ctx, scanner, storageengine.LockModeS, storageengine.KeyRange{}, 0)
	if !it.Next() || string(it.Key()) != "k1" {
		t.Fatalf("first row = %q (%v), want k1", string(it.Key()), it.Err())
	}
	advanced := make(chan string, 32)
	go func() {
		for it.Next() {
			advanced <- string(it.Key())
		}
		advanced <- ""
	}()
	t032AwaitPending(t, e, scanner.ID(), 1)
	if _, err := holder.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	var keys []string
	for {
		select {
		case key := <-advanced:
			if key == "" {
				goto done
			}
			keys = append(keys, key)
		case <-time.After(10 * time.Second):
			t.Fatal("the scan never continued after the deletion committed")
		}
	}
done:
	for _, key := range keys {
		if key == "k2" {
			t.Fatalf("the scan returned %q, which was deleted while the scan waited: %v", key, keys)
		}
	}
	if len(keys) != 1 || keys[0] != "k3" {
		t.Fatalf("the scan continued with %v, want only k3", keys)
	}
	if err := scanner.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// TestT032CurrentScanReverseLimitEOFAndClose pins the remaining access shapes: a reverse scan
// returns the same members in the opposite order, LIMIT bounds the output without unbounded work,
// EOF is stable, and Close is idempotent and safe before the first Next.
func TestT032CurrentScanReverseLimitEOFAndClose(t *testing.T) {
	e := t018OpenEngine(t)
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c", "d", "e"} {
		t032Seed(t, e, key, "v-"+key)
	}

	// Forward, unbounded.
	forward := t018Begin(t, e)
	forwardIt := t032Scan(t, ctx, forward, storageengine.LockModeS, storageengine.KeyRange{}, 0)
	got := t032Collect(t, forwardIt)
	if len(got) != 5 {
		t.Fatalf("forward scan saw %d rows, want 5: %v", len(got), got)
	}
	// EOF is stable: a Next after the end keeps reporting false and Err stays nil.
	if forwardIt.Next() || forwardIt.Err() != nil {
		t.Fatalf("EOF is not stable: next=%v err=%v", forwardIt.Next(), forwardIt.Err())
	}
	if err := forwardIt.Close(); err != nil {
		t.Fatalf("close after EOF: %v", err)
	}
	if err := forwardIt.Close(); err != nil {
		t.Fatalf("close is not idempotent: %v", err)
	}

	// Reverse keeps the same members.
	reverse := t018Begin(t, e)
	reverseIt := t032Scan(t, ctx, reverse, storageengine.LockModeS, storageengine.KeyRange{Reverse: true}, 0)
	var order []string
	for reverseIt.Next() {
		order = append(order, string(reverseIt.Key()))
	}
	if err := reverseIt.Err(); err != nil {
		t.Fatalf("reverse scan: %v", err)
	}
	if want := []string{"e", "d", "c", "b", "a"}; len(order) != len(want) {
		t.Fatalf("reverse scan saw %v, want %v", order, want)
	} else {
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("reverse scan order = %v, want %v", order, want)
			}
		}
	}

	// LIMIT bounds the output.
	limited := t018Begin(t, e)
	limitedIt := t032Scan(t, ctx, limited, storageengine.LockModeS, storageengine.KeyRange{}, 2)
	if got := t032Collect(t, limitedIt); len(got) != 2 {
		t.Fatalf("a LIMIT 2 scan saw %d rows, want 2: %v", len(got), got)
	}
	// A bounded range with both endpoints.
	bounded := t018Begin(t, e)
	boundedIt := t032Scan(t, ctx, bounded, storageengine.LockModeS, storageengine.KeyRange{
		Lower: []byte("b"), Upper: []byte("d"), LowerInclusive: true, UpperInclusive: true,
	}, 0)
	if got := t032Collect(t, boundedIt); len(got) != 3 {
		t.Fatalf("a bounded scan saw %v, want b, c and d", got)
	}

	// Close before the first Next is safe and leaves nothing behind.
	early := t018Begin(t, e)
	earlyIt := t032Scan(t, ctx, early, storageengine.LockModeS, storageengine.KeyRange{}, 0)
	if err := earlyIt.Close(); err != nil {
		t.Fatalf("close before the first row: %v", err)
	}
	if earlyIt.Next() {
		t.Fatal("a closed iterator yielded a row")
	}

	for _, tx := range []storageengine.Txn{forward, reverse, limited, bounded, early} {
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	t018RequireNoResidue(t, e)
}

// TestT032CurrentScanRefusesAnEmptyMode proves the scan does not guess its strength: without an
// explicit S or X it fails closed instead of silently taking one of them.
func TestT032CurrentScanRefusesAnEmptyMode(t *testing.T) {
	e := t018OpenEngine(t)
	scanner := t018Begin(t, e)
	locking, err := storageengine.LockingTxnOf(scanner)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := locking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locking.NewCurrentIterator(context.Background(), scope, storageengine.CurrentScanRequest{Space: t032Space}); err == nil {
		t.Fatal("a locked scan without a record mode was accepted")
	}
	if _, err := locking.NewCurrentIterator(context.Background(), scope, storageengine.CurrentScanRequest{Space: t032Space, Mode: storageengine.LockModeGap}); err == nil {
		t.Fatal("a locked scan with an interval mode was accepted as a record scan")
	}
	if err := scanner.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// TestT032CurrentScanCleansUpOnCancelAndEpochReset proves the terminal paths release everything: a
// cancelled context ends the scan with an error, an epoch change ends a *waiting* scan with an
// error instead of a value, and in both cases the engine's lock table and ledger return to idle.
func TestT032CurrentScanCleansUpOnCancelAndEpochReset(t *testing.T) {
	t.Run("context cancel during a wait", func(t *testing.T) {
		e := t018OpenEngine(t)
		t032Seed(t, e, "k1", "before")

		holder := t018Begin(t, e)
		if err := holder.Put(t032Space, []byte("k1"), []byte("held")); err != nil {
			t.Fatal(err)
		}

		scanner := t018Begin(t, e)
		ctx, cancel := context.WithCancel(context.Background())
		it := t032Scan(t, ctx, scanner, storageengine.LockModeS, storageengine.KeyRange{}, 0)
		done := make(chan error, 1)
		go func() {
			for it.Next() {
			}
			done <- it.Err()
		}()
		t032AwaitPending(t, e, scanner.ID(), 1)
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a cancelled locked scan reported no error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a cancelled locked scan never ended")
		}
		if err := holder.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := scanner.Rollback(); err != nil {
			t.Fatal(err)
		}
		t018RequireNoResidue(t, e)
	})

	t.Run("epoch reset during a wait", func(t *testing.T) {
		e := t018OpenEngine(t)
		t032Seed(t, e, "k1", "before")

		holder := t018Begin(t, e)
		if err := holder.Put(t032Space, []byte("k1"), []byte("held")); err != nil {
			t.Fatal(err)
		}

		scanner := t018Begin(t, e)
		it := t032Scan(t, context.Background(), scanner, storageengine.LockModeS, storageengine.KeyRange{}, 0)
		values := make(chan string, 4)
		errs := make(chan error, 1)
		go func() {
			for it.Next() {
				values <- string(it.Value())
			}
			errs <- it.Err()
		}()
		t032AwaitPending(t, e, scanner.ID(), 1)

		// The epoch moves on while the scan waits. The waiting request is invalidated, so the scan
		// must fail rather than return the value it was about to read.
		if affected := e.store.ResetLockEpoch(1); affected == 0 {
			t.Fatal("the epoch reset affected no owner")
		}
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("a locked scan from an ended epoch reported no error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an epoch change did not end the waiting locked scan")
		}
		select {
		case value := <-values:
			t.Fatalf("an ended scan yielded %q", value)
		default:
		}
		t018RequireNoResidue(t, e)
	})
}

// TestT035ScopeDeadlineReachesTheLockManager proves the session's lock-wait bound is carried into the
// lock manager rather than being clamped to the contract default somewhere on the way.
//
// The executor expresses the budget as the scope's context deadline, and the manager's rule is that a
// scope deadline is the bound when it is set. This test asserts the middle link: the scope the
// adapter opened really carries the caller's deadline. Without it a session asking for 300 seconds
// would be cut off at 50 with nothing in the observable state to show why.
func TestT035ScopeDeadlineReachesTheLockManager(t *testing.T) {
	e := t018OpenEngine(t)
	tx := t018Begin(t, e)
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(300*time.Second))
	defer cancel()
	scopeID, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatalf("begin scope: %v", err)
	}
	scope, ok := e.locks.Scope(tx.ID(), scopeID)
	if !ok {
		t.Fatalf("the scope %d is not registered on %s", scopeID, tx.ID())
	}
	if scope.ContextDeadline.IsZero() {
		t.Fatal("the scope carries no deadline, so the session budget could not be honoured")
	}
	if remaining := time.Until(scope.ContextDeadline); remaining < 250*time.Second {
		t.Fatalf("the scope deadline is %v away, want the caller's ~300s budget", remaining)
	}
	// A context without a deadline leaves the scope without one, so the manager's default applies.
	plain := t018Begin(t, e)
	plainLocking, err := storageengine.LockingTxnOf(plain)
	if err != nil {
		t.Fatal(err)
	}
	plainScope, err := plainLocking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if registered, ok := e.locks.Scope(plain.ID(), plainScope); !ok {
		t.Fatalf("the plain scope %d is not registered", plainScope)
	} else if !registered.ContextDeadline.IsZero() {
		t.Fatalf("a context without a deadline produced the scope deadline %v", registered.ContextDeadline)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := plain.Rollback(); err != nil {
		t.Fatal(err)
	}
	t018RequireNoResidue(t, e)
}

// t032AwaitPending blocks until one root has exactly the expected number of queued lock requests,
// which is the engine's own view of the wait rather than a timing assumption.
func t032AwaitPending(t *testing.T, e *engine, rootID string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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
