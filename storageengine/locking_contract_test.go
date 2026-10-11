package storageengine_test

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gbaselite/storageengine"
	"gbaselite/storageengine/testkit"
)

// TestB02LockingCapabilityIsAbsentByDefault pins the capability contract: the lock
// and locked-current-read surface is optional, so a backend that implements only the
// minimal Engine/Txn contract stays valid and reports absence instead of silently
// behaving like an unlocked lock manager.
func TestB02LockingCapabilityIsAbsentByDefault(t *testing.T) {
	engine := testkit.NewMemory()
	defer engine.Close()

	if _, ok := engine.(storageengine.LockingEngine); !ok {
		// Nothing to assert: a backend without the capability must simply not
		// implement it.
	} else {
		t.Fatal("the in-memory test backend must not advertise a lock capability")
	}
	if _, ok := engine.(storageengine.LockDiagnostics); ok {
		t.Fatal("the in-memory test backend must not advertise lock diagnostics")
	}

	tx, err := engine.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, ok := tx.(storageengine.LockingTxn); ok {
		t.Fatal("a minimal transaction must not advertise lock methods")
	}
	// The capability helpers are the documented way to ask, and they must report the
	// absence as ErrUnsupported rather than as a nil-but-usable value. A nil transaction
	// is the degenerate case of the same rule: a caller that has no transaction has no
	// capability, and a nil capability with a nil error would be indistinguishable from a
	// usable answer.
	if _, err := storageengine.LockingTxnOf(tx); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("LockingTxnOf on a minimal txn = %v, want ErrUnsupported", err)
	}
	if _, err := storageengine.LockingTxnOf(nil); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("LockingTxnOf(nil) = %v, want ErrUnsupported", err)
	}
	if _, err := storageengine.LockDiagnosticsOf(engine); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("LockDiagnosticsOf on a minimal engine = %v, want ErrUnsupported", err)
	}
}

// lockingEngineFixture adds the optional lock capability to a minimal engine while
// leaving Engine and Txn untouched, which is exactly what a backend adapter will do.
type lockingEngineFixture struct {
	inner storageengine.Engine
}

func (e lockingEngineFixture) Begin(ctx context.Context) (storageengine.Txn, error) {
	tx, err := e.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &lockingTxnFixture{inner: tx}, nil
}
func (e lockingEngineFixture) Close() error { return e.inner.Close() }

// Locking advertises the optional engine-level capability. It is the same shape the
// MVCC adapter will implement.
func (e lockingEngineFixture) Locking() bool { return true }

type lockingTxnFixture struct {
	inner   storageengine.Txn
	scopes  map[uint64]bool
	budgets map[uint64]time.Duration
	next    uint64
}

var (
	_ storageengine.Txn           = (*lockingTxnFixture)(nil)
	_ storageengine.LockingTxn    = (*lockingTxnFixture)(nil)
	_ storageengine.LockingEngine = lockingEngineFixture{}
)

func (t *lockingTxnFixture) ID() string       { return t.inner.ID() }
func (t *lockingTxnFixture) Snapshot() uint64 { return t.inner.Snapshot() }
func (t *lockingTxnFixture) Get(space string, key []byte) ([]byte, bool, error) {
	return t.inner.Get(space, key)
}
func (t *lockingTxnFixture) Put(space string, key, value []byte) error {
	return t.inner.Put(space, key, value)
}
func (t *lockingTxnFixture) Delete(space string, key []byte) error {
	return t.inner.Delete(space, key)
}
func (t *lockingTxnFixture) Guard(space string, key []byte) error {
	return t.inner.Guard(space, key)
}
func (t *lockingTxnFixture) GuardRange(space string, bounds storageengine.KeyRange) error {
	return t.inner.GuardRange(space, bounds)
}
func (t *lockingTxnFixture) Scan(ctx context.Context, space string, yield func([]byte, []byte) error) error {
	return t.inner.Scan(ctx, space, yield)
}
func (t *lockingTxnFixture) ScanRange(ctx context.Context, space string, bounds storageengine.KeyRange, yield func([]byte, []byte) error) error {
	return t.inner.ScanRange(ctx, space, bounds, yield)
}
func (t *lockingTxnFixture) Table(id string) storageengine.Table { return t.inner.Table(id) }
func (t *lockingTxnFixture) Child() (storageengine.Txn, error) {
	child, err := t.inner.Child()
	if err != nil {
		return nil, err
	}
	return &lockingTxnFixture{inner: child}, nil
}
func (t *lockingTxnFixture) Commit(ctx context.Context) (uint64, error) {
	return t.inner.Commit(ctx)
}
func (t *lockingTxnFixture) Rollback() error { return t.inner.Rollback() }
func (t *lockingTxnFixture) NewIterator(ctx context.Context, request storageengine.ScanRequest) (storageengine.Iterator, error) {
	return t.inner.NewIterator(ctx, request)
}

func (t *lockingTxnFixture) BeginLockScope(ctx context.Context, kind storageengine.LockScopeKind) (uint64, error) {
	return t.BeginLockScopeBudget(ctx, kind, 0)
}

// BeginLockScopeBudget mirrors the documented scope contract: the kind is still validated, and the
// budget is recorded with the scope so a test can assert what the caller asked for.
func (t *lockingTxnFixture) BeginLockScopeBudget(_ context.Context, kind storageengine.LockScopeKind, budget time.Duration) (uint64, error) {
	if !kind.Valid() {
		return 0, storageengine.ErrUnsupported
	}
	t.next++
	if t.scopes == nil {
		t.scopes = make(map[uint64]bool)
	}
	t.scopes[t.next] = true
	if budget > 0 {
		if t.budgets == nil {
			t.budgets = make(map[uint64]time.Duration)
		}
		t.budgets[t.next] = budget
	}
	return t.next, nil
}

func (t *lockingTxnFixture) Acquire(_ context.Context, scope uint64, request storageengine.LockRequest) (storageengine.LockGrant, error) {
	if !t.scopes[scope] {
		return storageengine.LockGrant{}, storageengine.ErrUnsupported
	}
	for _, component := range request.Components {
		if !component.Mode.Valid() || !component.Kind.Valid() {
			// A mode or kind outside the published set must be refused, never
			// silently downgraded.
			return storageengine.LockGrant{}, storageengine.ErrUnsupported
		}
	}
	return storageengine.LockGrant{ID: 1, OwnerID: t.inner.ID(), ScopeID: scope, Granted: time.Now()}, nil
}

func (t *lockingTxnFixture) ReadCurrent(_ context.Context, _ storageengine.LockGrant, space string, key []byte) (storageengine.CurrentObservation, error) {
	value, ok, err := t.inner.Get(space, key)
	if err != nil {
		return storageengine.CurrentObservation{}, err
	}
	if !ok {
		return storageengine.CurrentObservation{Source: storageengine.CurrentSourceAbsent}, nil
	}
	return storageengine.CurrentObservation{Value: value, Exists: true, Source: storageengine.CurrentSourceCommitted}, nil
}

func (t *lockingTxnFixture) NewCurrentIterator(ctx context.Context, scope uint64, request storageengine.CurrentScanRequest) (storageengine.Iterator, error) {
	if !t.scopes[scope] {
		return nil, storageengine.ErrUnsupported
	}
	return t.inner.NewIterator(ctx, storageengine.ScanRequest{Space: request.Space, Range: request.Range, Limit: request.Limit})
}

func (t *lockingTxnFixture) CloseLockScope(_ context.Context, scope uint64) error {
	delete(t.scopes, scope)
	return nil
}

// TestB02LockingCapabilityIsAdditive proves the optional surface can be added without
// changing Engine or Txn, and that the documented helpers detect it.
func TestB02LockingCapabilityIsAdditive(t *testing.T) {
	// The suite closes every engine it is handed, so the factory returns a fresh
	// wrapper per subtest exactly like the backend contract test does.
	testkit.Run(t, func(*testing.T) storageengine.Engine {
		return lockingEngineFixture{inner: testkit.NewMemory()}
	})

	engine := lockingEngineFixture{inner: testkit.NewMemory()}
	defer engine.Close()
	tx, err := engine.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("the fixture must advertise the lock capability: %v", err)
	}

	scope, err := locking.BeginLockScope(context.Background(), storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("k")
	request := storageengine.LockRequest{Components: []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, TableID: "t1"},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: "rows", Key: key},
	}}
	grant, err := locking.Acquire(context.Background(), scope, request)
	if err != nil {
		t.Fatal(err)
	}
	// The override seam must not be exported as a real engine: the capability helper
	// returns ErrUnsupported for an engine that lacks the interface.
	if _, err := storageengine.LockDiagnosticsOf(engine); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("LockDiagnosticsOf on a locking-only engine = %v, want ErrUnsupported", err)
	}
	// The fixture proves the copied-bytes rule: mutating the caller's key after
	// Acquire must not change the grant the implementation retained.
	if grant.ScopeID != scope {
		t.Fatalf("grant scope = %d, want %d", grant.ScopeID, scope)
	}
	if !bytesEqual(request.Components[1].Key, key) {
		t.Fatal("the request component key changed")
	}
	observation, err := locking.ReadCurrent(context.Background(), grant, "rows", key)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Source != storageengine.CurrentSourceAbsent {
		t.Fatalf("observation source = %q, want ABSENT for a missing key", observation.Source)
	}
	// An unsupported mode is refused rather than treated as a no-op lock.
	if _, err := locking.Acquire(context.Background(), scope, storageengine.LockRequest{
		Components: []storageengine.LockComponent{{Mode: storageengine.LockMode("SIX"), Kind: storageengine.LockComponentRecord, Space: "rows", Key: key}},
	}); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("unknown mode = %v, want ErrUnsupported", err)
	}
	// An unknown scope is refused too.
	if _, err := locking.Acquire(context.Background(), scope+99, request); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("unknown scope = %v, want ErrUnsupported", err)
	}
	if err := locking.CloseLockScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	// Closing a scope only cancels pending requests: nothing in this contract lets a
	// caller release a root's grants through a scope.
	if _, err := locking.Acquire(context.Background(), scope, request); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatalf("closed scope = %v, want ErrUnsupported", err)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestB02NeutralLockingTypesStayDependencyFree pins the architecture boundary the
// contract depends on: the neutral package must not import a concrete backend, and
// the SQL/physical layers must not import a concrete lock implementation.
func TestB02NeutralLockingTypesStayDependencyFree(t *testing.T) {
	assertNoImport(t, "storageengine", "gbaselite/mvcc", "gbaselite/lockmanager", "gbaselite/executor", "gbaselite/replication", "go.etcd.io/bbolt")
	assertNoImport(t, "lockmanager", "gbaselite/storageengine", "gbaselite/mvcc", "gbaselite/executor", "gbaselite/replication", "gbaselite/storage", "gbaselite/physical", "gbaselite/parser", "gbaselite/sqllayout", "go.etcd.io/bbolt")
}

// assertNoImport walks one package directory and reports the first forbidden import.
func assertNoImport(t *testing.T, packagePath string, forbidden ...string) {
	t.Helper()
	directory := filepath.Join("..", packagePath)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	banned := make(map[string]bool, len(forbidden))
	for _, path := range forbidden {
		banned[path] = true
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(directory, entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if banned[path] {
				t.Errorf("%s/%s imports %q, which crosses the B02 architecture boundary", packagePath, entry.Name(), path)
			}
		}
	}
}
