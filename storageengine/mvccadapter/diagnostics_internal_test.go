package mvccadapter

import (
	"context"
	"gbaselite/mvcc"
	"gbaselite/storageengine"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Internal tests reach the concrete engine so they can install a gated proposer and
// compare the translated diagnostics against the MVCC registry they came from.
// Translation is the adapter's only responsibility here; the semantics of the
// registry itself are tested in the mvcc package.

// gateProposer parks the first "commit" proposal. Parking on that boundary is the
// only way to observe COMMITTING deterministically: it is the window after the
// transaction entered COMMITTING and before anything was published.
type gateProposer struct {
	inner   mvcc.Proposer
	parked  chan struct{}
	release chan struct{}
	once    sync.Once
	free    sync.Once
}

func newGateProposer(inner mvcc.Proposer) *gateProposer {
	return &gateProposer{inner: inner, parked: make(chan struct{}), release: make(chan struct{})}
}

func (g *gateProposer) Barrier(ctx context.Context) error { return g.inner.Barrier(ctx) }

func (g *gateProposer) Propose(ctx context.Context, command mvcc.Command) (mvcc.Result, error) {
	if command.Kind == "commit" {
		g.once.Do(func() { close(g.parked) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return mvcc.Result{}, ctx.Err()
		}
	}
	return g.inner.Propose(ctx, command)
}

// releaseNow is idempotent so it is safe to call from a cleanup function.
func (g *gateProposer) releaseNow() { g.free.Do(func() { close(g.release) }) }

func openRaw(t *testing.T) *mvcc.Store {
	t.Helper()
	raw, err := mvcc.OpenWithOptions(filepath.Join(t.TempDir(), "versioned"), mvcc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return raw
}

func awaitParked(t *testing.T, parked <-chan struct{}) {
	t.Helper()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the commit proposal was never parked")
	}
}

func awaitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the commit never returned")
		return nil
	}
}

func TestDiagnosticsExposeCommittingStateThroughTheAdapter(t *testing.T) {
	ctx := context.Background()
	raw := openRaw(t)
	gate := newGateProposer(raw)
	t.Cleanup(gate.releaseNow)
	e := &engine{store: raw, proposer: gate}

	tx, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	if err := tx.Put("t", []byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if stats := e.TransactionStats(); stats.ActiveRoot != 1 {
		t.Fatalf("active before commit = %+v", stats)
	}

	done := make(chan error, 1)
	go func() {
		_, commitErr := tx.Commit(ctx)
		done <- commitErr
	}()
	awaitParked(t, gate.parked)

	// The durable commit is in flight: the adapter must report COMMITTING and must
	// not report a commit sequence, which would claim a commit that has not
	// happened yet. This is the state that previously had no representation.
	info := findInfo(t, e.ActiveTransactions(), tx.ID())
	if info.State != storageengine.TransactionCommitting {
		t.Fatalf("in-flight state = %q, want %q", info.State, storageengine.TransactionCommitting)
	}
	if info.HasCommitTS || info.CommitTS != 0 {
		t.Fatalf("commit sequence visible before publication: %+v", info)
	}
	if stats := e.TransactionStats(); stats.ActiveRoot != 1 || !stats.HasOldestReadTS {
		t.Fatalf("in-flight stats = %+v", stats)
	}

	gate.releaseNow()
	if err := awaitDone(t, done); err != nil {
		t.Fatal(err)
	}
	if transactions := e.ActiveTransactions(); len(transactions) != 0 {
		t.Fatalf("a committed transaction stayed registered: %+v", transactions)
	}
	if stats := e.TransactionStats(); stats.Committed != 1 || stats.ActiveRoot != 0 || stats.HasOldestReadTS {
		t.Fatalf("stats after commit = %+v", stats)
	}
}

func findInfo(t *testing.T, transactions []storageengine.TransactionInfo, id string) storageengine.TransactionInfo {
	t.Helper()
	for _, info := range transactions {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("transaction %s is not registered: %+v", id, transactions)
	return storageengine.TransactionInfo{}
}

// TestDiagnosticsTranslateEveryRegistryField checks the translation itself: each
// field of the neutral DTO carries the value of the registry entry it came from, for
// a live root and its child, with counters charged by real work.
func TestDiagnosticsTranslateEveryRegistryField(t *testing.T) {
	ctx := context.Background()
	raw := openRaw(t)
	e := &engine{store: raw}

	seed, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = seed.Put("t", []byte("seed"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	parent, err := e.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Put("t", []byte("c"), []byte("child")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = child.Get("t", []byte("c")); err != nil {
		t.Fatal(err)
	}
	if err = child.Guard("t", []byte("g")); err != nil {
		t.Fatal(err)
	}

	expected := raw.ActiveTransactions()
	translated := e.ActiveTransactions()
	if len(translated) != len(expected) || len(translated) != 2 {
		t.Fatalf("translated %d entries from %d", len(translated), len(expected))
	}
	// Both accessors are read-only, so the registry content must be identical before
	// and after the translation.
	if again := raw.ActiveTransactions(); len(again) != len(expected) {
		t.Fatalf("translation changed the registry: %d -> %d", len(expected), len(again))
	}
	for _, registry := range expected {
		translatedInfo := findInfo(t, translated, registry.ID)
		if want := transactionInfo(registry); translatedInfo != want {
			t.Fatalf("translation of %s = %+v, want %+v", registry.ID, translatedInfo, want)
		}
		if translatedInfo.State == storageengine.TransactionUnknown {
			t.Fatalf("a registered transaction was classified UNKNOWN: %+v", translatedInfo)
		}
		if translatedInfo.StartedAt.IsZero() {
			t.Fatalf("StartedAt was dropped: %+v", translatedInfo)
		}
	}
	childInfo := findInfo(t, translated, child.ID())
	if childInfo.ParentID != parent.ID() || childInfo.Writes != 1 || childInfo.PointReads != 1 || childInfo.PointDependencies != 1 {
		t.Fatalf("child translation = %+v", childInfo)
	}
	if parentInfo := findInfo(t, translated, parent.ID()); parentInfo.StartTS != parent.Snapshot() || parentInfo.ReadTS != parent.Snapshot() {
		t.Fatalf("parent translation = %+v", parentInfo)
	}

	if got, want := e.TransactionStats(), raw.TransactionStats(); got != (storageengine.TransactionStats{
		ActiveRoot:      want.ActiveRoot,
		ActiveChildren:  want.ActiveChildren,
		Committed:       want.Committed,
		Aborted:         want.Aborted,
		Conflicts:       want.Conflicts,
		OldestReadTS:    want.OldestReadTS,
		HasOldestReadTS: want.HasOldestReadTS,
	}) {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}

// TestDiagnosticsStateMappingIsTotalAndFailsClosed pins the classification of every
// backend lifecycle state, including the ones that are not reachable from a
// deterministic public call, and the fail-closed default.
func TestDiagnosticsStateMappingIsTotalAndFailsClosed(t *testing.T) {
	known := map[mvcc.TransactionState]storageengine.TransactionState{
		mvcc.TransactionActive:     storageengine.TransactionActive,
		mvcc.TransactionCommitting: storageengine.TransactionCommitting,
		mvcc.TransactionCommitted:  storageengine.TransactionCommitted,
		mvcc.TransactionAborted:    storageengine.TransactionAborted,
		mvcc.TransactionMerged:     storageengine.TransactionMerged,
		// A transaction that was never registered is not ACTIVE: the adapter must not
		// claim a state the commit path never confirmed.
		mvcc.TransactionUnset: storageengine.TransactionUnknown,
		// An unknown future state fails closed rather than defaulting to a usable one.
		mvcc.TransactionState(200): storageengine.TransactionUnknown,
	}
	for state, want := range known {
		if got := transactionState(state); got != want {
			t.Fatalf("state %d mapped to %q, want %q", state, got, want)
		}
	}
	if got := transactionState(0); got == storageengine.TransactionActive {
		t.Fatal("an unregistered state was reported as ACTIVE")
	}
	// The neutral states are a stable, documented vocabulary.
	for raw, want := range map[storageengine.TransactionState]string{
		storageengine.TransactionActive:     "ACTIVE",
		storageengine.TransactionCommitting: "COMMITTING",
		storageengine.TransactionCommitted:  "COMMITTED",
		storageengine.TransactionAborted:    "ABORTED",
		storageengine.TransactionMerged:     "MERGED",
		storageengine.TransactionUnknown:    "UNKNOWN",
	} {
		if string(raw) != want {
			t.Fatalf("state %q is not the documented %q", raw, want)
		}
	}
}
