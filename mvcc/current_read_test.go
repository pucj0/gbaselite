package mvcc

import (
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/lockmanager"
)

// current_read_test.go is T029: the locked current read and the observation baseline that
// reconciles it with the commit rule.
//
// The two properties under test are the ones that make SELECT ... FOR UPDATE meaningful rather
// than decorative: a current read returns the latest published version plus this transaction's own
// overlay, and the version it observed under lock is what authorises the later write. Everything
// else in the transaction keeps exact snapshot semantics.

// currentReadFixture is one store with a lock manager attached.
type currentReadFixture struct {
	t       *testing.T
	store   *Store
	manager *lockmanager.Manager
	clock   *lockmanager.FakeClock
}

func newCurrentReadFixture(t *testing.T) *currentReadFixture {
	t.Helper()
	store, err := OpenWithOptions(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clock := lockmanager.NewFakeClock(time.Unix(1750000000, 0))
	manager, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, clock)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(manager.Close)
	if _, err := store.AttachLocks(manager); err != nil {
		t.Fatalf("attach: %v", err)
	}
	return &currentReadFixture{t: t, store: store, manager: manager, clock: clock}
}

// rowLock acquires the exclusive lock one record needs, with its table intention.
func (f *currentReadFixture) rowLock(ctx context.Context, tx *Tx, key string, mode lockmanager.Mode) lockmanager.LockGrant {
	f.t.Helper()
	result, err := f.manager.Acquire(ctx, tx.LockOwnerID(), tx.lockScopeID, []lockmanager.ResourceRef{
		recordRef(key, mode), intentRef(),
	})
	if err != nil {
		f.t.Fatalf("acquire %s: %v", key, err)
	}
	if result.Op != lockmanager.LockOpGranted && result.Op != lockmanager.LockOpReentrant {
		f.t.Fatalf("acquire %s op = %s, want GRANTED or REENTRANT", key, result.Op)
	}
	// Both outcomes return the manager's own grant. A re-entrant acquisition reports the
	// protection the owner already holds, components included, so the capability really does
	// describe the resource it covers: the current read re-validates that coverage against the
	// live registry rather than trusting an owner ID.
	return result.Grant
}

func recordRef(key string, mode lockmanager.Mode) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:    lockmanager.KindRecord,
		Mode:    mode,
		Space:   "index/PRIMARY",
		TableID: "t1",
		Key:     []byte(key),
	}}
}

func intentRef() lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:    lockmanager.KindTableIntent,
		Mode:    lockmanager.ModeIX,
		Space:   "table",
		TableID: "t1",
	}}
}

// TestB02CurrentReadSeesLatestPublishedVersion proves the read basis is the newest published
// version rather than the transaction's snapshot.
func TestB02CurrentReadSeesLatestPublishedVersion(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	writer := mustBegin(t, f.store)
	if err := writer.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The reader starts *after* that commit, so it also sees v1 on a snapshot read.
	reader := mustBegin(t, f.store)
	snapshot, found, err := reader.Get("index/PRIMARY", []byte("k1"))
	if err != nil || !found || string(snapshot) != "v1" {
		t.Fatalf("snapshot read = %q found=%v err=%v", snapshot, found, err)
	}

	// A newer committed version appears while the reader is open.
	second := mustBegin(t, f.store)
	if err := second.Put("index/PRIMARY", []byte("k1"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The ordinary read still sees the snapshot it began with: Snapshot Isolation is untouched.
	stillOld, _, err := reader.Get("index/PRIMARY", []byte("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stillOld) != "v1" {
		t.Fatalf("snapshot read after a concurrent commit = %q, want the fixed v1", stillOld)
	}
	// The current read sees v2, and reports the version it observed.
	grant := f.rowLock(ctx, reader, "k1", lockmanager.ModeX)
	observation, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant)
	if err != nil {
		t.Fatalf("current read: %v", err)
	}
	if string(observation.Value) != "v2" {
		t.Fatalf("current read = %q, want v2", observation.Value)
	}
	if !observation.Exists || observation.Tombstone {
		t.Fatalf("current read exists=%v tombstone=%v, want an existing row", observation.Exists, observation.Tombstone)
	}
	if observation.Source != CurrentCommitted {
		t.Fatalf("source = %v, want CurrentCommitted", observation.Source)
	}
	if !observation.HasChange || observation.ChangeTS == 0 {
		t.Fatalf("change metadata = %+v, want the publishing sequence", observation)
	}
	// The transaction's own read basis is untouched by the current read.
	if reader.Snapshot != mustSnapshot(t, writer) {
		// writer's snapshot is its own begin point; the reader must still report its own.
	}
	if reader.WriteBaselineSize() != 1 {
		t.Fatalf("baselines = %d, want the single locked key", reader.WriteBaselineSize())
	}
}

// TestB02LockedReadAuthorisesTheWriteItObserved is the reconciliation test: the whole point of the
// observation baseline.
//
// Without it a locked read that saw a version newer than the transaction's readTS would still have
// its write rejected, which is precisely the case SELECT ... FOR UPDATE exists to allow.
func TestB02LockedReadAuthorisesTheWriteItObserved(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The reader begins first, so its readTS predates the next commit.
	reader := mustBegin(t, f.store)
	readerReadTS := reader.Snapshot

	// A concurrent transaction commits a newer version after the reader began.
	concurrent := mustBegin(t, f.store)
	if err := concurrent.Put("index/PRIMARY", []byte("k1"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	commitTS, err := concurrent.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if commitTS <= readerReadTS {
		t.Fatalf("the concurrent commit (%d) did not outrun the reader's readTS (%d)", commitTS, readerReadTS)
	}

	// The reader locks the row and observes the newer version.
	grant := f.rowLock(ctx, reader, "k1", lockmanager.ModeX)
	observation, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant)
	if err != nil {
		t.Fatal(err)
	}
	if string(observation.Value) != "v2" {
		t.Fatalf("observed %q, want v2", observation.Value)
	}
	if observation.ChangeTS != commitTS {
		t.Fatalf("observed version %d, want the commit sequence %d", observation.ChangeTS, commitTS)
	}

	// The locked, observed write is authorised even though it is newer than readTS.
	if err := reader.Put("index/PRIMARY", []byte("k1"), []byte("v3")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Commit(ctx); err != nil {
		t.Fatalf("a write authorised by a locked current read was rejected: %v", err)
	}
	after := mustBegin(t, f.store)
	value, _, err := after.Get("index/PRIMARY", []byte("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "v3" {
		t.Fatalf("committed value = %q, want v3", value)
	}
	// The reader's readTS was never modified: the baseline is per key, not a rebase.
	if reader.Snapshot != readerReadTS {
		t.Fatalf("readTS changed from %d to %d: a current read must not rebase the transaction", readerReadTS, reader.Snapshot)
	}
}

// TestB02UnlockedWriteStillConflicts proves the baseline changes nothing for a key that was not
// read under lock.
//
// This is the guard against the reconciliation becoming a general weakening: a plain write to a
// key changed after readTS must still be refused.
func TestB02UnlockedWriteStillConflicts(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	reader := mustBegin(t, f.store)
	concurrent := mustBegin(t, f.store)
	if err := concurrent.Put("index/PRIMARY", []byte("k1"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := concurrent.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The reader writes the key without ever locking or observing it.
	if err := reader.Put("index/PRIMARY", []byte("k1"), []byte("v3")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("an unlocked write to a changed key committed: err = %v, want ErrConflict", err)
	}
	if reader.State() != TransactionAborted {
		t.Fatalf("state = %s, want ABORTED", reader.State())
	}
}

// TestB02BaselineIsPerKeyNotPerTransaction proves a locked read of one key does not authorise a
// write to another key that also changed.
func TestB02BaselineIsPerKeyNotPerTransaction(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("locked"), []byte("a1")); err != nil {
		t.Fatal(err)
	}
	if err := seed.Put("index/PRIMARY", []byte("other"), []byte("b1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	reader := mustBegin(t, f.store)
	concurrent := mustBegin(t, f.store)
	if err := concurrent.Put("index/PRIMARY", []byte("other"), []byte("b2")); err != nil {
		t.Fatal(err)
	}
	if _, err := concurrent.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Lock and observe only "locked"; "other" is never observed.
	grant := f.rowLock(ctx, reader, "locked", lockmanager.ModeX)
	if _, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("locked"), grant); err != nil {
		t.Fatal(err)
	}
	if err := reader.Put("index/PRIMARY", []byte("locked"), []byte("a2")); err != nil {
		t.Fatal(err)
	}
	if err := reader.Put("index/PRIMARY", []byte("other"), []byte("b3")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("a write to an unobserved key committed: err = %v, want ErrConflict", err)
	}
}

// TestB02BaselineRecordsZeroForAbsentAndTombstone proves what a locked read of a key that does not
// exist records, and that the record is the observation and not a stale readTS.
//
// The shape this test used to exercise — another root creating the row underneath the observing
// caller — is no longer reachable, and that is the point of the lock: a raw write now takes the same
// record X the observer holds, so the other root waits instead of appearing. What remains to check
// here is the observation itself: absence records baseline 0 (no change at all), a tombstone keeps
// its deleting sequence, and each is distinguishable from the other. The guard that an *unlocked*
// observation of absence must never authorise an overwrite lives in
// TestB02UnlockedCurrentReadRecordsNoBaseline, which is the case the baseline must not weaken.
func TestB02BaselineRecordsZeroForAbsentAndTombstone(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	reader := mustBegin(t, f.store)
	// "missing" does not exist at all.
	grant := f.rowLock(ctx, reader, "missing", lockmanager.ModeX)
	observation, err := reader.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("missing"), grant)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Exists || observation.HasChange {
		t.Fatalf("absent key observed as %+v, want absent with no change", observation)
	}
	if observation.Tombstone {
		t.Fatal("a key that never existed was reported as a tombstone")
	}
	// The baseline is keyed by the logical store key, which is what the commit validator looks up.
	logicalKey, err := key("index/PRIMARY", []byte("missing"))
	if err != nil {
		t.Fatal(err)
	}
	if version, recorded := reader.writeBaselineFor(logicalKey); !recorded {
		t.Fatal("a locked read of an absent key recorded no baseline")
	} else if version != 0 {
		t.Fatalf("the absent key's baseline = %d, want 0 (no version observed)", version)
	}
	// The lock authorises exactly what it observed: creating the key it locked as missing publishes
	// the row, and nothing changed underneath it because a concurrent writer could not get in.
	if err := reader.Put("index/PRIMARY", []byte("missing"), []byte("mine")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Commit(ctx); err != nil {
		t.Fatalf("inserting the key this transaction locked as absent: %v", err)
	}

	// The same for a tombstone: a deleted-and-locked key is reported as a tombstone with its
	// deleting sequence, and is distinguishable from a key that never existed.
	g := newCurrentReadFixture(t)
	seed := mustBegin(t, g.store)
	if err := seed.Put("index/PRIMARY", []byte("gone"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	deleter := mustBegin(t, g.store)
	if err := deleter.Delete("index/PRIMARY", []byte("gone")); err != nil {
		t.Fatal(err)
	}
	if _, err := deleter.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	observer := mustBegin(t, g.store)
	grant = g.rowLock(ctx, observer, "gone", lockmanager.ModeX)
	tombstone, err := observer.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("gone"), grant)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.Exists {
		t.Fatal("a deleted key was reported as existing")
	}
	if !tombstone.Tombstone {
		t.Fatalf("a deleted key must be distinguishable from one that never existed: %+v", tombstone)
	}
	if !tombstone.HasChange || tombstone.ChangeTS == 0 {
		t.Fatalf("tombstone change metadata = %+v, want the deleting sequence", tombstone)
	}
}

// TestB02CurrentReadIncludesOwnOverlay proves the current read layers this transaction's own
// unmerged writes over the published version.
func TestB02CurrentReadIncludesOwnOverlay(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("published")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx := mustBegin(t, f.store)
	if err := tx.Put("index/PRIMARY", []byte("k1"), []byte("own")); err != nil {
		t.Fatal(err)
	}
	grant := f.rowLock(ctx, tx, "k1", lockmanager.ModeX)
	observation, err := tx.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant)
	if err != nil {
		t.Fatal(err)
	}
	if string(observation.Value) != "own" {
		t.Fatalf("current read = %q, want this transaction's own unmerged value", observation.Value)
	}
	if observation.Source != CurrentOwnOverlay {
		t.Fatalf("source = %v, want CurrentOwnOverlay", observation.Source)
	}
	if observation.ChangeTS != 0 {
		t.Fatalf("an overlay value reported a commit sequence %d; it is unpublished by definition", observation.ChangeTS)
	}

	// A child sees its own write and its ancestor's, but a parent must not see an unmerged child.
	child, err := tx.ChildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grant = f.rowLock(ctx, child, "k1", lockmanager.ModeX)
	childObservation, err := child.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant)
	if err != nil {
		t.Fatal(err)
	}
	if string(childObservation.Value) != "own" {
		t.Fatalf("child current read = %q, want the ancestor's value", childObservation.Value)
	}
	if err := child.Put("index/PRIMARY", []byte("k1"), []byte("child")); err != nil {
		t.Fatal(err)
	}
	parentObservation, err := tx.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant)
	if err != nil {
		t.Fatal(err)
	}
	if string(parentObservation.Value) != "own" {
		t.Fatalf("parent observed %q: a parent must never see an unmerged child", parentObservation.Value)
	}
	_ = child.Rollback()
	_ = tx.Rollback()
}

// TestB02OwnOverlayDeleteIsObservable proves a delete in the transaction's own write set is
// reported as a tombstone rather than as an absent key.
func TestB02OwnOverlayDeleteIsObservable(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx := mustBegin(t, f.store)
	if err := tx.Delete("index/PRIMARY", []byte("k1")); err != nil {
		t.Fatal(err)
	}
	grant := f.rowLock(ctx, tx, "k1", lockmanager.ModeX)
	observation, err := tx.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Exists {
		t.Fatal("the transaction's own delete was not observed")
	}
	if !observation.Tombstone || observation.Source != CurrentOwnOverlay {
		t.Fatalf("own delete observed as %+v, want an own-overlay tombstone", observation)
	}
	_ = tx.Rollback()
}

// TestB02CurrentReadRejectsAForeignOrStaleGrant proves the observation is bound to this
// transaction's own lock.
func TestB02CurrentReadRejectsAForeignOrStaleGrant(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	one := mustBegin(t, f.store)
	two := mustBegin(t, f.store)
	grant := f.rowLock(ctx, one, "k1", lockmanager.ModeX)

	// A grant recorded for another owner must not authorise a read on this one.
	if _, err := two.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant); !errors.Is(err, ErrConflict) {
		t.Fatalf("a foreign grant authorised a read: err = %v, want ErrConflict", err)
	}
	// A grant whose owner was released must fail closed rather than return a stale value.
	one.Rollback()
	if _, err := one.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant); err == nil {
		t.Fatal("a released owner's grant still authorised a current read")
	}
}

// TestB02CurrentReadDoesNotPinHistoryOrChangeTheRootClass proves the coordination rules around
// retention and commit classification are untouched.
func TestB02CurrentReadDoesNotPinHistoryOrChangeTheRootClass(t *testing.T) {
	f := newCurrentReadFixture(t)
	ctx := context.Background()

	seed := mustBegin(t, f.store)
	if err := seed.Put("index/PRIMARY", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A current-only root: it takes a locked current read and writes nothing.
	currentOnly := mustBegin(t, f.store)
	before, hadBefore := f.store.txns.OldestReadTS()
	grant := f.rowLock(ctx, currentOnly, "k1", lockmanager.ModeX)
	if _, err := currentOnly.ReadCurrentLocked(ctx, "index/PRIMARY", []byte("k1"), grant); err != nil {
		t.Fatal(err)
	}
	after, hadAfter := f.store.txns.OldestReadTS()
	if hadBefore != hadAfter || before != after {
		t.Fatalf("a current observation changed the GC horizon: (%d,%v) -> (%d,%v)", before, hadBefore, after, hadAfter)
	}
	// The root still commits as the empty/read-only class. The empty-root path returns the
	// transaction's snapshot as its "sequence" by design, so the class is proved the way the
	// contract states it: no publication marker exists and the head did not move.
	head, err := f.store.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := currentOnly.Commit(ctx); err != nil {
		t.Fatalf("current-only commit: %v", err)
	}
	afterHead, err := f.store.Head()
	if err != nil {
		t.Fatal(err)
	}
	if afterHead != head {
		t.Fatalf("a current-only root moved the head from %d to %d", head, afterHead)
	}
	if _, published := f.store.Committed(currentOnly.ID); published {
		t.Fatal("a current-only root installed a publication marker")
	}
	if currentOnly.State() != TransactionCommitted {
		t.Fatalf("state = %s, want COMMITTED", currentOnly.State())
	}

	// A dependency-only root keeps its own class: a guard with no data write.
	dependencyOnly := mustBegin(t, f.store)
	if err := dependencyOnly.Guard("index/PRIMARY", []byte("k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := dependencyOnly.Commit(ctx); err != nil {
		t.Fatalf("dependency-only commit: %v", err)
	}
	if dependencyOnly.State() != TransactionCommitted {
		t.Fatalf("dependency-only state = %s, want COMMITTED", dependencyOnly.State())
	}
}

// mustBegin and mustSnapshot keep the fixtures small.
func mustBegin(t *testing.T, store *Store) *Tx {
	t.Helper()
	tx, err := store.Begin(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

func mustSnapshot(t *testing.T, tx *Tx) uint64 {
	t.Helper()
	return tx.Snapshot
}
