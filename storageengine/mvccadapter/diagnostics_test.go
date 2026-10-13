package mvccadapter_test

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
	"gbaselite/storageengine/testkit"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Transaction diagnostics are an optional capability: the memory test double must
// not claim them, the MVCC-backed engine must expose them for both the plain and the
// local-WAL configuration, and the capability must be detected explicitly rather
// than assumed from Engine or from another capability.

func openDiagnosed(t *testing.T, options storageengine.Options) (storageengine.Engine, storageengine.TransactionDiagnostics) {
	t.Helper()
	e, err := mvccadapter.Open(t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	d, ok := e.(storageengine.TransactionDiagnostics)
	if !ok {
		t.Fatal("the MVCC adapter does not expose transaction diagnostics")
	}
	return e, d
}

func TestDiagnosticsCapabilityIsDetectedAndOptional(t *testing.T) {
	if _, ok := testkit.NewMemory().(storageengine.TransactionDiagnostics); ok {
		t.Fatal("the memory backend reported diagnostics it does not have")
	}
	for _, configuration := range []struct {
		name    string
		options storageengine.Options
	}{
		{"mvcc", storageengine.Options{}},
		{"mvcc-wal", storageengine.Options{LocalWAL: true}},
	} {
		t.Run(configuration.name, func(t *testing.T) {
			e, d := openDiagnosed(t, configuration.options)
			if stats := d.TransactionStats(); stats != (storageengine.TransactionStats{}) {
				t.Fatalf("fresh engine stats = %+v, want zero", stats)
			}
			// A non-nil empty slice keeps len(), range and JSON encoding uniform.
			if transactions := d.ActiveTransactions(); transactions == nil || len(transactions) != 0 {
				t.Fatalf("fresh engine transactions = %v", transactions)
			}
			// Diagnostics is a separate capability and must not be implied or lost.
			if _, ok := e.(storageengine.Diagnostics); !ok {
				t.Fatal("the pending-bytes capability disappeared")
			}
			if _, ok := e.(storageengine.TransactionDiagnostics); !ok {
				t.Fatal("the capability was not re-detectable")
			}
		})
	}
}

// findTransaction selects one registry entry by ID, which is the only lookup
// stable across the unspecified iteration order.
func findTransaction(t *testing.T, transactions []storageengine.TransactionInfo, id string) storageengine.TransactionInfo {
	t.Helper()
	for _, info := range transactions {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("transaction %s is not registered: %+v", id, transactions)
	return storageengine.TransactionInfo{}
}

func TestDiagnosticsReportLiveRootTransaction(t *testing.T) {
	e, d := openDiagnosed(t, storageengine.Options{})
	seed := begin(t, e)
	if err := seed.Table("t").Put([]byte("seed"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	commit(t, seed)
	head, err := e.(storageengine.RevisionReader).Head()
	if err != nil {
		t.Fatal(err)
	}

	tx := begin(t, e)
	if err = tx.Table("t").Put([]byte("a"), []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = tx.Table("t").Get([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if err = tx.Guard("t", []byte("guarded")); err != nil {
		t.Fatal(err)
	}

	info := findTransaction(t, d.ActiveTransactions(), tx.ID())
	if info.ParentID != "" {
		t.Fatalf("a root transaction reported a parent: %+v", info)
	}
	if info.State != storageengine.TransactionActive {
		t.Fatalf("state = %q, want %q", info.State, storageengine.TransactionActive)
	}
	if info.StartTS != tx.Snapshot() || info.ReadTS != tx.Snapshot() {
		t.Fatalf("timestamps = (%d, %d), want the begin snapshot %d", info.StartTS, info.ReadTS, tx.Snapshot())
	}
	if info.StartTS != head {
		t.Fatalf("StartTS = %d, want the head at Begin %d", info.StartTS, head)
	}
	if info.HasCommitTS || info.CommitTS != 0 {
		t.Fatalf("an uncommitted transaction reported a commit sequence: %+v", info)
	}
	if info.StartedAt.IsZero() {
		t.Fatalf("StartedAt was not reported: %+v", info)
	}
	if info.Writes != 1 || info.WriteBytes <= 0 {
		t.Fatalf("write set counters = (%d, %d)", info.Writes, info.WriteBytes)
	}
	if info.PointReads != 1 {
		t.Fatalf("PointReads = %d, want 1", info.PointReads)
	}
	if info.PointDependencies != 1 || info.RangeDependencies != 0 {
		t.Fatalf("dependency counters = (%d, %d)", info.PointDependencies, info.RangeDependencies)
	}
	if info.AbortReason != "" {
		t.Fatalf("a live transaction reported an abort reason %q", info.AbortReason)
	}

	stats := d.TransactionStats()
	if stats.ActiveRoot != 1 || stats.ActiveChildren != 0 {
		t.Fatalf("active counts = (%d, %d)", stats.ActiveRoot, stats.ActiveChildren)
	}
	if !stats.HasOldestReadTS || stats.OldestReadTS != tx.Snapshot() {
		t.Fatalf("horizon = (%d, %v), want the live snapshot %d", stats.OldestReadTS, stats.HasOldestReadTS, tx.Snapshot())
	}
}

func TestDiagnosticsReportChildTransactionAndRootOnlyHorizon(t *testing.T) {
	e, d := openDiagnosed(t, storageengine.Options{})
	seed := begin(t, e)
	if err := seed.Table("t").Put([]byte("seed"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	commit(t, seed)

	parent := begin(t, e)
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Table("t").Put([]byte("c"), []byte("child")); err != nil {
		t.Fatal(err)
	}

	info := findTransaction(t, d.ActiveTransactions(), child.ID())
	if info.ParentID != parent.ID() {
		t.Fatalf("child parent = %q, want %q", info.ParentID, parent.ID())
	}
	if info.State != storageengine.TransactionActive {
		t.Fatalf("child state = %q", info.State)
	}
	// A child inherits its parent's snapshot instead of taking a new one, so it
	// reports the parent's timestamps. They are not the child's own begin revision.
	if info.StartTS != parent.Snapshot() || info.ReadTS != parent.Snapshot() {
		t.Fatalf("child timestamps = (%d, %d), want the parent snapshot %d", info.StartTS, info.ReadTS, parent.Snapshot())
	}
	if info.HasCommitTS {
		t.Fatalf("a child reported a commit sequence: %+v", info)
	}

	stats := d.TransactionStats()
	if stats.ActiveRoot != 1 || stats.ActiveChildren != 1 {
		t.Fatalf("active counts = (%d, %d)", stats.ActiveRoot, stats.ActiveChildren)
	}
	// Only roots pin history: the child must not move the horizon.
	if !stats.HasOldestReadTS || stats.OldestReadTS != parent.Snapshot() {
		t.Fatalf("horizon = (%d, %v), want the root snapshot %d", stats.OldestReadTS, stats.HasOldestReadTS, parent.Snapshot())
	}

	if _, err = child.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, remaining := range d.ActiveTransactions() {
		if remaining.ID == child.ID() {
			t.Fatal("a merged child stayed registered")
		}
	}
	if stats = d.TransactionStats(); stats.ActiveRoot != 1 || stats.ActiveChildren != 0 {
		t.Fatalf("active counts after merge = (%d, %d)", stats.ActiveRoot, stats.ActiveChildren)
	}
}

func TestDiagnosticsDropTerminalTransactionsAndCountOutcomes(t *testing.T) {
	e, d := openDiagnosed(t, storageengine.Options{})
	ctx := context.Background()
	before := d.TransactionStats()

	committed := begin(t, e)
	if err := committed.Table("t").Put([]byte("a"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	commit(t, committed)

	rolledBack := begin(t, e)
	if err := rolledBack.Table("t").Put([]byte("b"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := rolledBack.Rollback(); err != nil {
		t.Fatal(err)
	}

	// A conflict is an abort too, and it is counted separately as well.
	//
	// The two writers cannot overlap on one key any more: a raw write takes the record lock, so a
	// second writer on the same key would wait instead of conflicting. Both transactions therefore
	// start on one snapshot and the first one ends before the second writes: the second write finds
	// the lock free and its commit then meets the same optimistic conflict the overlap used to
	// produce. What the counters see is unchanged — one commit and one conflict.
	winner := begin(t, e)
	loser := begin(t, e)
	if err := winner.Table("t").Put([]byte("a"), []byte("winner")); err != nil {
		t.Fatal(err)
	}
	commit(t, winner)
	if err := loser.Table("t").Put([]byte("a"), []byte("loser")); err != nil {
		t.Fatal(err)
	}
	if _, err := loser.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("loser commit = %v, want ErrConflict", err)
	}

	after := d.TransactionStats()
	if after.Committed != before.Committed+2 {
		t.Fatalf("Committed = %d, want %d", after.Committed, before.Committed+2)
	}
	// Ending without a commit is counted as an abort, and so is a conflict: the
	// losing transaction aborted, and it is counted once as a conflict as well.
	if after.Aborted != before.Aborted+2 {
		t.Fatalf("Aborted = %d, want %d", after.Aborted, before.Aborted+2)
	}
	if after.Conflicts != before.Conflicts+1 {
		t.Fatalf("Conflicts = %d, want %d", after.Conflicts, before.Conflicts+1)
	}
	if after.ActiveRoot != 0 || after.ActiveChildren != 0 || after.HasOldestReadTS {
		t.Fatalf("terminal transactions stayed registered: %+v", after)
	}
	if transactions := d.ActiveTransactions(); len(transactions) != 0 {
		t.Fatalf("terminal transactions stayed registered: %+v", transactions)
	}
}

func TestDiagnosticsSnapshotIsCallerOwnedAndStable(t *testing.T) {
	e, d := openDiagnosed(t, storageengine.Options{})
	tx := begin(t, e)
	if err := tx.Table("t").Put([]byte("a"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	first := d.ActiveTransactions()
	if len(first) != 1 {
		t.Fatalf("transactions = %+v", first)
	}
	second := d.ActiveTransactions()
	// The returned slice is the caller's: mutating it must not reach the registry,
	// and two calls must not share backing storage.
	first[0] = storageengine.TransactionInfo{ID: "mutated", State: storageengine.TransactionUnknown}
	first = append(first, storageengine.TransactionInfo{ID: "appended"})
	if second[0].ID != tx.ID() {
		t.Fatalf("a previous snapshot changed to %+v", second)
	}
	if stats := d.TransactionStats(); stats.ActiveRoot != 1 {
		t.Fatalf("mutating a snapshot changed the registry: %+v", stats)
	}
	if third := d.ActiveTransactions(); third[0].ID != tx.ID() || len(third) != 1 {
		t.Fatalf("transactions = %+v", third)
	}
}

func TestDiagnosticsReportNoKeysValuesOrSQL(t *testing.T) {
	const payload = "SECRET-ROW-PAYLOAD"
	e, d := openDiagnosed(t, storageengine.Options{})
	tx := begin(t, e)
	if err := tx.Table("secret-space").Put([]byte("secret-key"), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := tx.GuardRange("secret-space", storageengine.KeyRange{Lower: []byte("secret-key")}); err != nil {
		t.Fatal(err)
	}
	rendered := fmt.Sprintf("%+v %+v", d.ActiveTransactions(), d.TransactionStats())
	for _, secret := range []string{payload, "secret-key", "secret-space"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, rendered)
		}
	}
	// State is a fixed classification, not free text, so these four fields are the
	// only places text of any kind can reach the DTO. A fifth one added later fails
	// this test until it is justified.
	var text []string
	for i := 0; i < reflect.TypeOf(storageengine.TransactionInfo{}).NumField(); i++ {
		field := reflect.TypeOf(storageengine.TransactionInfo{}).Field(i)
		if field.Type.Kind() == reflect.String {
			text = append(text, field.Name)
		}
	}
	if fmt.Sprint(text) != "[ID ParentID State AbortReason]" {
		t.Fatalf("textual fields = %v", text)
	}
	for i := 0; i < reflect.TypeOf(storageengine.TransactionStats{}).NumField(); i++ {
		if kind := reflect.TypeOf(storageengine.TransactionStats{}).Field(i).Type.Kind(); kind == reflect.String || kind == reflect.Slice || kind == reflect.Map {
			t.Fatalf("stats field %s can carry row data", reflect.TypeOf(storageengine.TransactionStats{}).Field(i).Name)
		}
	}
}

// TestDiagnosticsDTOsCannotHoldReadState pins boundedness at the boundary callers
// actually see: neither DTO may contain a collection, so no amount of reading inside a
// transaction can grow the diagnostics a caller receives. Only one small value per
// registered transaction crosses the boundary.
func TestDiagnosticsDTOsCannotHoldReadState(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(storageengine.TransactionInfo{}), reflect.TypeOf(storageengine.TransactionStats{})} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			switch field.Type.Kind() {
			case reflect.Map, reflect.Slice, reflect.Array, reflect.Interface, reflect.Func, reflect.Chan, reflect.Pointer:
				t.Fatalf("%s.%s is %s: diagnostics must not be able to hold per-key state", typ, field.Name, field.Type)
			}
		}
	}
}

func TestDiagnosticsRemainReadableDuringConcurrentCommits(t *testing.T) {
	e, d := openDiagnosed(t, storageengine.Options{})
	stop := make(chan struct{})
	var observer sync.WaitGroup
	observer.Add(1)
	go func() {
		defer observer.Done()
		var last storageengine.TransactionStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			current := d.TransactionStats()
			if current.Committed < last.Committed || current.Aborted < last.Aborted || current.Conflicts < last.Conflicts {
				t.Errorf("counters moved backwards: %+v -> %+v", last, current)
				return
			}
			last = current
			for _, info := range d.ActiveTransactions() {
				switch info.State {
				case storageengine.TransactionActive, storageengine.TransactionCommitting:
				case storageengine.TransactionUnknown:
					t.Errorf("live transaction classified as UNKNOWN: %+v", info)
					return
				default:
					// A terminal entry may still be registered for an instant.
				}
				// The state and the commit sequence are published together, so an
				// uncommitted state can never carry one.
				if info.HasCommitTS && info.State != storageengine.TransactionCommitted {
					t.Errorf("commit sequence visible in state %q: %+v", info.State, info)
					return
				}
			}
		}
	}()

	before := d.TransactionStats()
	for i := 0; i < 20; i++ {
		tx := begin(t, e)
		if err := tx.Table("t").Put([]byte(fmt.Sprintf("k%02d", i)), []byte("v")); err != nil {
			t.Fatal(err)
		}
		if i%4 == 0 {
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		commit(t, tx)
	}
	close(stop)
	observer.Wait()

	after := d.TransactionStats()
	if after.Committed != before.Committed+15 || after.Aborted != before.Aborted+5 {
		t.Fatalf("counters = %+v, want +15 committed and +5 aborted from %+v", after, before)
	}
	if after.ActiveRoot != 0 || after.ActiveChildren != 0 {
		t.Fatalf("registry leaked: %+v", after)
	}
}

// TestT060LockDiagnosticsCapability verifies T060:
// 1. LockDiagnostics capability is optional and detected via LockDiagnosticsOf;
// 2. Returns neutral DTOs with anonymous Resource IDs;
// 3. Page limit clamped to MaxLockDiagnosticRows (256);
// 4. Zero sensitive data (no keys, values, spaces, SQL);
// 5. Returns independent copies.
func TestT060LockDiagnosticsCapability(t *testing.T) {
	e, _ := openDiagnosed(t, storageengine.Options{})
	defer e.Close()

	diag, err := storageengine.LockDiagnosticsOf(e)
	if err != nil {
		t.Fatalf("expected LockDiagnostics capability, got error: %v", err)
	}

	ctx := context.Background()

	// 1. Initial empty pages
	locksPage, err := diag.LocksPage("", 10)
	if err != nil {
		t.Fatalf("LocksPage failed: %v", err)
	}
	if len(locksPage.Rows) != 0 {
		t.Fatalf("expected 0 lock rows initially, got %d", len(locksPage.Rows))
	}

	waitsPage, err := diag.WaitPage("", 10)
	if err != nil {
		t.Fatalf("WaitPage failed: %v", err)
	}
	if len(waitsPage.Rows) != 0 {
		t.Fatalf("expected 0 wait rows initially, got %d", len(waitsPage.Rows))
	}

	deadlocksPage, err := diag.DeadlocksPage("", 10)
	if err != nil {
		t.Fatalf("DeadlocksPage failed: %v", err)
	}
	if len(deadlocksPage.Rows) != 0 {
		t.Fatalf("expected 0 deadlock rows initially, got %d", len(deadlocksPage.Rows))
	}

	stats, err := diag.LockStats()
	if err != nil {
		t.Fatalf("LockStats failed: %v", err)
	}
	if stats.ActiveOwners != 0 || stats.GrantedComponents != 0 {
		t.Fatalf("expected clean initial stats, got %+v", stats)
	}

	// 2. Acquire a lock: tx1 holds X on k1
	tx1, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()

	locking, ok := tx1.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	if !ok {
		t.Fatal("transaction does not expose LockingTxn")
	}
	ltx1, _ := locking.LockingTxn()
	scope1, err := ltx1.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	tableRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   "row/t1",
		TableID: "t1",
	}
	rowRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   "row/t1",
		TableID: "t1",
		Key:     []byte("secret_key_1"),
	}
	grant, err := ltx1.Acquire(ctx, scope1, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	// Verify LocksPage shows the granted lock with anonymous ResourceID
	locksPage, err = diag.LocksPage("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(locksPage.Rows) == 0 {
		t.Fatal("expected at least 1 lock row, got 0")
	}
	found := false
	for _, row := range locksPage.Rows {
		if row.RootID == grant.OwnerID {
			found = true
			if row.Mode != storageengine.LockModeX && row.Mode != storageengine.LockModeIX {
				t.Fatalf("expected LockModeX or IX, got %s", row.Mode)
			}
			if row.Resource == 0 {
				t.Fatal("expected nonzero anonymous Resource ID")
			}
		}
	}
	if !found {
		t.Fatalf("did not find tx1 (%s) in LocksPage: %+v", grant.OwnerID, locksPage.Rows)
	}

	// 3. Page limit clamped to MaxLockDiagnosticRows (256)
	overLimitPage, err := diag.LocksPage("", 300)
	if err != nil {
		t.Fatal(err)
	}
	if overLimitPage.Limit != storageengine.MaxLockDiagnosticRows {
		t.Fatalf("expected limit clamped to %d, got %d", storageengine.MaxLockDiagnosticRows, overLimitPage.Limit)
	}

	// 4. Mutation of returned page does not affect backend
	if len(locksPage.Rows) > 0 {
		locksPage.Rows[0].RootID = "MUTATED"
		freshPage, _ := diag.LocksPage("", 10)
		for _, r := range freshPage.Rows {
			if r.RootID == "MUTATED" {
				t.Fatal("backend state was mutated by modifying returned DTO")
			}
		}
	}
}
