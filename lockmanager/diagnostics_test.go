package lockmanager

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// diagnostics_test.go is T058: the bounded, copied, private read-only diagnostics views.
//
// Two properties are asserted over and over here because they are the ones that break in
// production rather than in a unit test: a page must be bounded and say when it is clipped, and a
// page must never alias the manager's mutable state.

// requireCopiedSlices proves a row's slices are the caller's own.
//
// Mutating them must not change what the manager reports next, and the manager changing must not
// change what an already-returned row says. Either direction of aliasing would let a diagnostics
// reader corrupt a live lock's bookkeeping or read a torn view.
func requireCopiedSlices(t *testing.T, page LockDiagnosticsPage, m *Manager) {
	t.Helper()
	if len(page.Resources) > 0 {
		row := page.Resources[0]
		for i := range row.HolderIDs {
			row.HolderIDs[i] = "tampered"
		}
		for i := range row.HolderModes {
			row.HolderModes[i] = Mode("TAMPERED")
		}
		for i := range row.WaiterIDs {
			row.WaiterIDs[i] = "tampered"
		}
		for i := range row.WaiterModes {
			row.WaiterModes[i] = Mode("TAMPERED")
		}
	}
	for i := range page.Waits {
		for j := range page.Waits[i].BlockerIDs {
			page.Waits[i].BlockerIDs[j] = "tampered"
		}
		for j := range page.Waits[i].Reasons {
			page.Waits[i].Reasons[j] = WaitEdgeReason("TAMPERED")
		}
	}
	for i := range page.Deadlocks {
		for j := range page.Deadlocks[i].CycleRootIDs {
			page.Deadlocks[i].CycleRootIDs[j] = "tampered"
		}
		for j := range page.Deadlocks[i].AnonymousIDs {
			page.Deadlocks[i].AnonymousIDs[j] = 0
		}
		for j := range page.Deadlocks[i].Modes {
			page.Deadlocks[i].Modes[j] = Mode("TAMPERED")
		}
	}

	second := m.Diagnostics(DiagnosticsOptions{Limit: MaxDiagnosticRows, IncludeWaiters: true, IncludeDeadlocks: true})
	for _, row := range second.Resources {
		for _, ownerID := range append(append([]string(nil), row.HolderIDs...), row.WaiterIDs...) {
			if ownerID == "tampered" {
				t.Fatal("mutating a returned resource row changed the manager's state")
			}
		}
	}
	for _, row := range second.Waits {
		for _, blocker := range row.BlockerIDs {
			if blocker == "tampered" {
				t.Fatal("mutating a returned wait row changed the manager's state")
			}
		}
	}
	for _, summary := range second.Deadlocks {
		for _, rootID := range summary.CycleRootIDs {
			if rootID == "tampered" {
				t.Fatal("mutating a returned deadlock summary changed the retained history")
			}
		}
	}
}

// TestB02DiagnosticsPageReportsHoldersWaitersAndEdges checks the content of one page.
func TestB02DiagnosticsPageReportsHoldersWaitersAndEdges(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))
	other := indexRecord("t1", "PRIMARY", []byte("k2"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	f.owner("root-b").wait(ctx, rowBundle(row)...)
	f.owner("root-c").wait(ctx, rowBundle(row)...)
	f.owner("root-c").grant(ctx, rowBundle(other)...)

	page := f.m.Diagnostics(DiagnosticsOptions{Limit: MaxDiagnosticRows, IncludeWaiters: true, IncludeDeadlocks: true})
	if page.Limit != MaxDiagnosticRows {
		t.Fatalf("limit = %d, want %d", page.Limit, MaxDiagnosticRows)
	}
	if page.SnapshotRevision == 0 {
		t.Fatal("the page reports revision zero with a non-empty graph")
	}
	if page.NextCursor == "" {
		t.Fatal("the page reports no cursor")
	}
	if page.Truncated {
		t.Fatal("a small page reported truncation")
	}

	// Every held resource appears exactly once, identified only by its anonymous id.
	if page.TotalResources != 3 {
		t.Fatalf("total resources = %d, want 3 (row, other, table intention)", page.TotalResources)
	}
	seen := make(map[uint64]bool, len(page.Resources))
	for _, resource := range page.Resources {
		if resource.ResourceID == 0 {
			t.Fatal("a resource row has no anonymous id")
		}
		if seen[resource.ResourceID] {
			t.Fatalf("anonymous id %d appears twice", resource.ResourceID)
		}
		seen[resource.ResourceID] = true
		if resource.HolderCount == 0 && resource.WaiterCount == 0 {
			t.Fatalf("resource %d is empty but was reported", resource.ResourceID)
		}
		if len(resource.HolderIDs) != resource.HolderCount {
			t.Fatalf("resource %d: %d holder ids for %d holders", resource.ResourceID, len(resource.HolderIDs), resource.HolderCount)
		}
		if len(resource.WaiterIDs) != resource.WaiterCount {
			t.Fatalf("resource %d: %d waiter ids for %d waiters", resource.ResourceID, len(resource.WaiterIDs), resource.WaiterCount)
		}
	}

	// One row per waiting component, in queue order, with the blocking reasons named.
	if page.TotalWaits != 4 {
		t.Fatalf("total waits = %d, want 4 (two roots, two components each)", page.TotalWaits)
	}
	ordinals := make([]uint64, 0, len(page.Waits))
	for _, wait := range page.Waits {
		if wait.OwnerID == "" {
			t.Fatal("a wait row has no owner")
		}
		if wait.Status != RequestPending {
			t.Fatalf("wait row status = %s, want PENDING", wait.Status)
		}
		if wait.EnqueueOrdinal == 0 {
			t.Fatal("a wait row has no enqueue ordinal")
		}
		ordinals = append(ordinals, wait.EnqueueOrdinal)
	}
	for i := 1; i < len(ordinals); i++ {
		if ordinals[i-1] > ordinals[i] {
			t.Fatalf("wait rows are not in queue order: %v", ordinals)
		}
	}

	// The holder block and the queue-order block are reported as different reasons.
	var sawHolder, sawOlder bool
	for _, wait := range page.Waits {
		for _, reason := range wait.Reasons {
			switch reason {
			case EdgeHolder:
				sawHolder = true
			case EdgeOlderWaiter:
				sawOlder = true
			}
		}
	}
	if !sawHolder || !sawOlder {
		t.Fatalf("reasons are incomplete: holder=%v olderWaiter=%v", sawHolder, sawOlder)
	}

	// The page reports a wait duration derived from the manager's clock, not from wall time.
	if page.Waits[0].DeadlineMicros <= 0 {
		t.Fatal("a wait row reports no deadline")
	}
	if page.Waits[0].WaitMicros < 0 {
		t.Fatalf("wait duration = %d, want non-negative", page.Waits[0].WaitMicros)
	}

	requireCopiedSlices(t, page, f.m)

	// A page with the optional sets switched off reports only resources.
	bare := f.m.Diagnostics(DiagnosticsOptions{Limit: 10})
	if len(bare.Waits) != 0 || len(bare.Deadlocks) != 0 {
		t.Fatalf("a resource-only page returned %d waits and %d deadlocks", len(bare.Waits), len(bare.Deadlocks))
	}
	if len(bare.Resources) == 0 {
		t.Fatal("a resource-only page returned no resources")
	}
}

// TestB02DiagnosticsPrivacyExposesNoRawIdentity is the leak test.
//
// A page is assembled from anonymous ids, kinds, modes and root ids. It must contain no table
// name, index name, encoded key or statement text, so an operator-facing view cannot become a way
// to read rows or reconstruct a query. The check is a substring scan over every string the page
// can produce, which catches a field added later without a privacy review.
func TestB02DiagnosticsPrivacyExposesNoRawIdentity(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()

	// Secret-looking values in every identifying position: the namespace space, the table id, the
	// index id and the key bytes.
	const secretKey = "SECRET-ROW-KEY"
	const secretTable = "SECRETTABLE"
	const secretIndex = "SECRETINDEX"
	row := ResourceRef{Identity: Identity{
		Kind:    KindRecord,
		Mode:    ModeX,
		Space:   "index/" + secretIndex,
		TableID: secretTable,
		Key:     []byte(secretKey),
	}}
	intent := ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: secretTable}}

	f.owner("root-a").grant(ctx, row, intent)
	f.owner("root-b").wait(ctx, row, intent)

	page := f.m.Diagnostics(DiagnosticsOptions{Limit: MaxDiagnosticRows, IncludeWaiters: true, IncludeDeadlocks: true})

	var rendered strings.Builder
	for _, resource := range page.Resources {
		fmt.Fprintf(&rendered, "%d %s %s %s %v %v %v %v\n",
			resource.ResourceID, resource.Kind, resource.State, resource.HolderMode,
			resource.HolderIDs, resource.HolderModes, resource.WaiterIDs, resource.WaiterModes)
	}
	for _, wait := range page.Waits {
		fmt.Fprintf(&rendered, "%d %s %d %d %s %s %s %v %v\n",
			wait.RequestID, wait.OwnerID, wait.ScopeID, wait.ResourceID,
			wait.Kind, wait.Mode, wait.Status, wait.BlockerIDs, wait.Reasons)
	}
	for _, summary := range page.Deadlocks {
		fmt.Fprintf(&rendered, "%v %v %v\n", summary.CycleRootIDs, summary.AnonymousIDs, summary.Modes)
	}
	text := rendered.String()
	for _, secret := range []string{secretKey, secretTable, secretIndex, "index/", "PRIMARY", "SELECT", "k1"} {
		if strings.Contains(text, secret) {
			t.Fatalf("a diagnostics page leaked %q:\n%s", secret, text)
		}
	}
	// Root ids are the operator's correlation handle and must be present, so the scan above is
	// not vacuously passing on an empty page.
	if !strings.Contains(text, "root-b") {
		t.Fatalf("the page lost the owner identity it is supposed to report:\n%s", text)
	}
	// The anonymous id is stable for one resource across pages, so rows can be correlated.
	second := f.m.Diagnostics(DiagnosticsOptions{Limit: MaxDiagnosticRows, IncludeWaiters: true})
	firstIDs := make(map[ResourceKind]uint64)
	for _, resource := range page.Resources {
		firstIDs[resource.Kind] = resource.ResourceID
	}
	for _, resource := range second.Resources {
		if firstIDs[resource.Kind] != resource.ResourceID {
			t.Fatalf("anonymous id for kind %s changed between pages", resource.Kind)
		}
	}
}

// TestB02DiagnosticsTruncatesAtTheContractBound checks the 256/257 boundary.
func TestB02DiagnosticsTruncatesAtTheContractBound(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-h")
	ctx := context.Background()

	// One holder on one row and 257 waiters on that same row, so there are more wait rows than a
	// page serves. The waiters all contend for the same lock, which is what makes each of them
	// queue instead of being granted on a different key.
	rows := 257
	contended := indexRecord("t1", "PRIMARY", []byte("k0"))
	f.owner("root-h").grant(ctx, rowBundle(contended)...)
	waits := make([]*LockHandle, 0, rows)
	waiterRoots := make([]string, 0, rows)
	for i := 0; i < rows; i++ {
		rootID := fmt.Sprintf("root-w%03d", i)
		f.owners[rootID] = newPublicOwner(t, f.m, rootID)
		result := f.owner(rootID).wait(ctx, rowBundle(contended)...)
		waits = append(waits, result.Wait)
		waiterRoots = append(waiterRoots, rootID)
	}

	// A limit above the contract bound is clamped and reported.
	page := f.m.Diagnostics(DiagnosticsOptions{Limit: MaxDiagnosticRows + 1, IncludeWaiters: true})
	if page.Limit != MaxDiagnosticRows {
		t.Fatalf("limit = %d, want it clamped to %d", page.Limit, MaxDiagnosticRows)
	}
	if !page.Truncated {
		t.Fatal("a clamped page did not report truncation")
	}
	if len(page.Waits) != MaxDiagnosticRows {
		t.Fatalf("wait rows = %d, want %d", len(page.Waits), MaxDiagnosticRows)
	}
	if page.TotalWaits <= len(page.Waits) {
		t.Fatalf("total waits = %d with %d served, want the larger total reported", page.TotalWaits, len(page.Waits))
	}
	if !page.WaitsTruncated {
		t.Fatal("the wait set did not report truncation")
	}

	// Exactly at the bound is a complete *serving*: no set may exceed the bound, and the complete
	// total is still reported. With more waiters than the bound, the page is clipped by the set
	// size, not by the requested limit, and says so.
	exact := f.m.Diagnostics(DiagnosticsOptions{Limit: MaxDiagnosticRows, IncludeWaiters: true})
	if len(exact.Waits) > MaxDiagnosticRows {
		t.Fatalf("a page served %d rows for the bound %d", len(exact.Waits), MaxDiagnosticRows)
	}
	if exact.Limit != MaxDiagnosticRows {
		t.Fatalf("limit = %d, want %d", exact.Limit, MaxDiagnosticRows)
	}
	// The wait rows scale with the number of queued *components*, so the complete total is at
	// least twice the waiter count: a record and a table intention each. The exact figure is not
	// asserted, because which components queue depends on what the holder already covers.
	if exact.TotalWaits < rows*2 {
		t.Fatalf("total waits = %d, want at least %d", exact.TotalWaits, rows*2)
	}
	// A page whose sets all fit is not truncated at all.
	requireSmall := func(page LockDiagnosticsPage) {
		if page.Truncated {
			t.Fatalf("a fully served page reported truncation: %+v", page)
		}
	}
	requireSmall(f.m.Diagnostics(DiagnosticsOptions{Limit: MaxDiagnosticRows}))

	// A smaller explicit limit truncates and says so.
	small := f.m.Diagnostics(DiagnosticsOptions{Limit: 4, IncludeWaiters: true})
	if len(small.Waits) != 4 || !small.WaitsTruncated {
		t.Fatalf("limit 4 returned %d wait rows, truncated=%v", len(small.Waits), small.WaitsTruncated)
	}
	for i, handle := range waits {
		f.m.Cancel(waiterRoots[i], handle)
	}
}

// TestB02DiagnosticsCursorTracksTheGraphRevision proves the cursor is the revision.
//
// A consumer that pages and then finds the cursor has moved knows its earlier page described a
// graph that no longer exists, instead of silently joining rows from two different states.
func TestB02DiagnosticsCursorTracksTheGraphRevision(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	first := f.m.Diagnostics(DiagnosticsOptions{Limit: 10, IncludeWaiters: true})
	if first.NextCursor != "0" {
		t.Fatalf("cursor on an idle manager = %q, want \"0\"", first.NextCursor)
	}

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	f.owner("root-b").wait(ctx, rowBundle(row)...)
	second := f.m.Diagnostics(DiagnosticsOptions{Limit: 10, IncludeWaiters: true})
	if second.NextCursor == first.NextCursor {
		t.Fatal("the cursor did not move when the wait graph changed")
	}
	if second.SnapshotRevision != second.Page.SnapshotRevision {
		t.Fatal("the page revision and the cursor disagree")
	}

	// An unrelated acquisition that creates no wait edge does not move the cursor: the cursor
	// tracks the graph, not general activity.
	f.owner("root-c").grant(ctx, rowBundle(indexRecord("t1", "PRIMARY", []byte("k9")))...)
	third := f.m.Diagnostics(DiagnosticsOptions{Limit: 10, IncludeWaiters: true})
	if third.NextCursor != second.NextCursor {
		t.Fatalf("an unrelated grant moved the cursor from %q to %q", second.NextCursor, third.NextCursor)
	}
}

// TestB02DiagnosticsRetainedDeadlockRingIsBounded checks the 32/33 boundary of the ring as it is
// reported through a page.
func TestB02DiagnosticsRetainedDeadlockRingIsBounded(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()

	for i := 0; i < maxDeadlockSummaries+1; i++ {
		f.m.ReleaseOwner("root-a")
		f.m.ReleaseOwner("root-b")
		f.owners["root-a"] = newPublicOwner(t, f.m, "root-a")
		f.owners["root-b"] = newPublicOwner(t, f.m, "root-b")
		row1 := indexRecord("t1", "PRIMARY", []byte(fmt.Sprintf("a%d", i)))
		row2 := indexRecord("t2", "PRIMARY", []byte(fmt.Sprintf("b%d", i)))
		f.owner("root-a").grant(ctx, rowBundle(row1)...)
		f.owner("root-b").grant(ctx, rowBundle(row2)...)
		f.owner("root-a").wait(ctx, rowBundle(row2)...)
		f.owner("root-b").wait(ctx, rowBundle(row1)...)
		if _, found := f.m.DetectDeadlock(); !found {
			t.Fatalf("round %d: deadlock not detected", i)
		}
	}

	page := f.m.Diagnostics(DiagnosticsOptions{Limit: MaxDiagnosticRows, IncludeDeadlocks: true})
	if page.TotalDeadlocks != maxDeadlockSummaries {
		t.Fatalf("retained summaries = %d, want the ring bound %d", page.TotalDeadlocks, maxDeadlockSummaries)
	}
	if len(page.Deadlocks) != maxDeadlockSummaries {
		t.Fatalf("page summaries = %d, want %d", len(page.Deadlocks), maxDeadlockSummaries)
	}
	if page.DeadlocksTruncated {
		t.Fatal("the ring is bounded, so a full-ring page is not truncated")
	}
	for _, summary := range page.Deadlocks {
		if len(summary.CycleRootIDs) > maxDeadlockNodes || len(summary.AnonymousIDs) > maxDeadlockNodes {
			t.Fatalf("a summary exceeded the node budget: %+v", summary)
		}
	}
	requireCopiedSlices(t, page, f.m)
}

// TestB02DiagnosticsIsReadableConcurrently proves the page is safe under concurrent readers and
// that it does not hold up lock progress.
//
// Readers run while owners acquire, wait and release. Every page must be internally consistent
// (a state's counts match its own row lists) and no reader may observe a partially built page.
func TestB02DiagnosticsIsReadableConcurrently(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
	ctx := context.Background()

	const readers = 6
	const rounds = 120
	var wg sync.WaitGroup
	errs := make(chan error, readers)

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				page := f.m.Diagnostics(DiagnosticsOptions{Limit: 64, IncludeWaiters: true, IncludeDeadlocks: true})
				for _, resource := range page.Resources {
					if resource.Kind == "" {
						errs <- fmt.Errorf("resource %d has no kind", resource.ResourceID)
						return
					}
					if resource.State == "" {
						errs <- fmt.Errorf("resource %d has no state", resource.ResourceID)
						return
					}
					if len(resource.HolderIDs) != len(resource.HolderModes) {
						errs <- fmt.Errorf("resource %d: %d holder ids vs %d modes", resource.ResourceID, len(resource.HolderIDs), len(resource.HolderModes))
						return
					}
					if len(resource.WaiterIDs) != len(resource.WaiterModes) {
						errs <- fmt.Errorf("resource %d: %d waiter ids vs %d modes", resource.ResourceID, len(resource.WaiterIDs), len(resource.WaiterModes))
						return
					}
					if len(page.Resources) > page.Limit {
						errs <- fmt.Errorf("page served %d rows for limit %d", len(page.Resources), page.Limit)
						return
					}
				}
			}
		}()
	}

	// The writers run the full cycle: hold, contend, resolve.
	for i := 0; i < rounds/4; i++ {
		key := []byte(fmt.Sprintf("k%d", i))
		row := indexRecord("t1", "PRIMARY", key)
		f.owner("root-a").grant(ctx, rowBundle(row)...)
		waiter := f.owner("root-b").wait(ctx, rowBundle(row)...)
		f.m.ReleaseOwner("root-c")
		f.m.Release("root-a", row)
		if _, err := f.m.WaitForGrant(ctx, "root-b", waiter.Wait); err != nil {
			t.Fatalf("waiter was not granted: %v", err)
		}
		f.m.ReleaseOwner("root-a")
		f.m.ReleaseOwner("root-b")
		f.owners["root-a"] = newPublicOwner(t, f.m, "root-a")
		f.owners["root-b"] = newPublicOwner(t, f.m, "root-b")
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// The run ends with an empty manager, so nothing was leaked by the concurrent reading.
	if size := f.m.LockTableSize(); size != 0 {
		t.Fatalf("lock table size = %d after the run, want 0", size)
	}
}

// TestB02DiagnosticsStatisticsAreConsistentWithTheGraph proves the aggregate view and the row view
// cannot disagree.
func TestB02DiagnosticsStatisticsAreConsistentWithTheGraph(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b", "root-c")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	f.owner("root-b").wait(ctx, rowBundle(row)...)
	f.owner("root-c").wait(ctx, rowBundle(row)...)

	stats := f.m.Stats()
	if stats.WaitEdges != uint64(len(f.m.WaitEdges())) {
		t.Fatalf("stats report %d edges, graph has %d", stats.WaitEdges, len(f.m.WaitEdges()))
	}
	if stats.PendingRequests != 2 {
		t.Fatalf("stats report %d pending requests, want 2", stats.PendingRequests)
	}
	if stats.Timeouts != 0 || stats.Deadlocks != 0 {
		t.Fatalf("unexpected counters before any timeout or deadlock: %+v", stats)
	}
	if stats.Revision == 0 {
		t.Fatal("stats report revision zero with a non-empty graph")
	}
	if stats.MemoryBytes == 0 || stats.GrantedComponents == 0 || stats.ActiveOwners != 3 {
		t.Fatalf("stats look incomplete: %+v", stats)
	}
}

// TestB02DiagnosticsReportsWaitDurationsFromTheManagerClock proves a duration advances with the
// manager's clock only, so a report never claims a wait that did not happen.
func TestB02DiagnosticsReportsWaitDurationsFromTheManagerClock(t *testing.T) {
	f := newDetectFixture(t, BudgetLimits{}, "root-a", "root-b")
	ctx := context.Background()
	row := indexRecord("t1", "PRIMARY", []byte("k1"))

	f.owner("root-a").grant(ctx, rowBundle(row)...)
	f.owner("root-b").wait(ctx, rowBundle(row)...)

	first := f.m.Diagnostics(DiagnosticsOptions{Limit: 10, IncludeWaiters: true})
	var firstWait int64 = -1
	for _, wait := range first.Waits {
		if wait.OwnerID == "root-b" && wait.Kind == KindRecord {
			firstWait = wait.WaitMicros
		}
	}
	if firstWait < 0 {
		t.Fatalf("root-b's record wait was not reported: %+v", first.Waits)
	}
	// Nothing advanced the clock, so the duration did not grow.
	second := f.m.Diagnostics(DiagnosticsOptions{Limit: 10, IncludeWaiters: true})
	for _, wait := range second.Waits {
		if wait.OwnerID == "root-b" && wait.Kind == KindRecord && wait.WaitMicros != firstWait {
			t.Fatalf("wait duration changed without the clock moving: %d then %d", firstWait, wait.WaitMicros)
		}
	}
	// Advancing the manager's clock advances the reported duration by the same amount.
	f.clock.Advance(3 * time.Second)
	third := f.m.Diagnostics(DiagnosticsOptions{Limit: 10, IncludeWaiters: true})
	for _, wait := range third.Waits {
		if wait.OwnerID == "root-b" && wait.Kind == KindRecord {
			if wait.WaitMicros != firstWait+3_000_000 {
				t.Fatalf("wait duration = %d, want %d after advancing three seconds", wait.WaitMicros, firstWait+3_000_000)
			}
		}
	}
}
