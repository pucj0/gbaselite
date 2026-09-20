package mvcc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// History retention: the store must keep every version a registered root
// transaction may still read, and must stop keeping them as soon as that
// transaction ends or its generation is replaced.
//
// Every test here is deterministic: no sleeps, no timing assumptions. Head
// positions and version counts are produced by explicit commits, and the GC
// horizon is read from the TransactionManager that CompactHistory itself uses.

// commitHistoryValue rewrites one row, so every call advances the commit
// sequence (and therefore the head) by exactly one.
func commitHistoryValue(t *testing.T, s *Store, value string) {
	t.Helper()
	tx := beginBaseline(t, s)
	mustPut(t, tx, "k", value)
	mustCommit(t, tx)
}

// advanceHistory writes values for the inclusive range [from, to], which leaves
// the head at exactly "to" when it starts from "from-1".
func advanceHistory(t *testing.T, s *Store, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		commitHistoryValue(t, s, fmt.Sprintf("v%03d", i))
	}
}

// openHistoryChildren builds a nested savepoint chain and keeps every layer
// registered, mirroring a session whose savepoints are stacked.
func openHistoryChildren(t *testing.T, parent *Tx, depth int) []*Tx {
	t.Helper()
	children := make([]*Tx, 0, depth)
	current := parent
	for i := 0; i < depth; i++ {
		child, err := current.Child()
		if err != nil {
			t.Fatalf("Child() at depth %d: %v", i, err)
		}
		children = append(children, child)
		current = child
	}
	t.Cleanup(func() {
		for i := len(children) - 1; i >= 0; i-- {
			_ = children[i].Rollback()
		}
	})
	return children
}

// historyVersionCount reports how many physical versions of one key the nested
// layout still holds, so a test can prove GC actually collected something.
func historyVersionCount(t *testing.T, s *Store, space, rowKey string) int {
	t.Helper()
	encoded, err := key(space, []byte(rowKey))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := s.db.View(func(tx *bolt.Tx) error {
		rows := tx.Bucket(dataBucket).Bucket(encoded)
		if rows == nil {
			return nil
		}
		return rows.ForEach(func(_, _ []byte) error { count++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func requireHorizon(t *testing.T, s *Store, want uint64, wantOK bool) {
	t.Helper()
	oldest, ok := s.txns.OldestReadTS()
	if ok != wantOK || ok && oldest != want {
		t.Fatalf("GC horizon = (%d, %v), want (%d, %v)", oldest, ok, want, wantOK)
	}
}

func requireRegistryEmpty(t *testing.T, s *Store) {
	t.Helper()
	if infos := s.txns.ActiveTransactions(); len(infos) != 0 {
		t.Fatalf("registry leaked %d entries: %+v", len(infos), infos)
	}
	if stats := s.txns.Stats(); stats.ActiveRoot != 0 || stats.ActiveChildren != 0 {
		t.Fatalf("active counts leaked: %+v", stats)
	}
}

// requireNoOrphans asserts the registry holds no child whose parent is missing,
// which is what a leaked savepoint layer would look like.
func requireNoOrphans(t *testing.T, s *Store) {
	t.Helper()
	infos := s.txns.ActiveTransactions()
	registered := make(map[string]bool, len(infos))
	for _, info := range infos {
		registered[info.ID] = true
	}
	for _, info := range infos {
		if info.ParentID != "" && !registered[info.ParentID] {
			t.Fatalf("registry orphan: %s references unregistered parent %s", info.ID, info.ParentID)
		}
	}
}

func captureStoreImage(t *testing.T, s *Store) []byte {
	t.Helper()
	snapshot, err := s.Snapshot()
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
	return image.Bytes()
}

// Case A: a long root transaction protects the history it still needs while
// other transactions push the head far past its snapshot, and its savepoint
// children never change the oldest root read timestamp.
func TestHistoryRetentionProtectsLongRootTransaction(t *testing.T) {
	s := baselineStore(t)
	ctx := context.Background()
	advanceHistory(t, s, 1, 100)
	if head := mustHead(t, s); head != 100 {
		t.Fatalf("head = %d, want 100", head)
	}
	if versions := historyVersionCount(t, s, baselineSpace, "k"); versions != 100 {
		t.Fatalf("versions at head 100 = %d, want 100", versions)
	}

	long := beginBaseline(t, s)
	if long.Snapshot != 100 {
		t.Fatalf("long transaction read timestamp = %d, want 100", long.Snapshot)
	}
	requireValue(t, long, "k", "v100")

	children := openHistoryChildren(t, long, 8)
	requireHorizon(t, s, 100, true)
	if stats := s.txns.Stats(); stats.ActiveRoot != 1 || stats.ActiveChildren != uint64(len(children)) {
		t.Fatalf("registry = %+v, want 1 root and %d children", stats, len(children))
	}

	advanceHistory(t, s, 101, 200)
	if head := mustHead(t, s); head != 200 {
		t.Fatalf("head after concurrent commits = %d, want 200", head)
	}
	// Concurrent commits do not move the pinned horizon, and the children still
	// do not add a second pin.
	requireHorizon(t, s, 100, true)

	if err := s.CompactHistory(ctx); err != nil {
		t.Fatal(err)
	}
	// Compaction ran: it dropped every version older than the anchor at the
	// horizon and kept the anchor itself plus every newer version.
	if versions := historyVersionCount(t, s, baselineSpace, "k"); versions != 200-100+1 {
		t.Fatalf("versions after compaction = %d, want %d", versions, 200-100+1)
	}
	// The protected version is still readable through the long transaction and
	// through each of its savepoint layers.
	requireValue(t, long, "k", "v100")
	for i, child := range children {
		value, ok, err := child.Get(baselineSpace, []byte("k"))
		if err != nil || !ok || string(value) != "v100" {
			t.Fatalf("savepoint layer %d read %q, %v, %v; want v100", i, value, ok, err)
		}
	}
	fresh := beginBaseline(t, s)
	requireValue(t, fresh, "k", "v200")
}

// Case B: once the long transaction ends, by commit or by rollback, its
// retention is released and the GC horizon may move forward.
func TestHistoryRetentionReleasedWhenTransactionEnds(t *testing.T) {
	ctx := context.Background()
	for _, end := range []string{"rollback", "commit"} {
		t.Run(end, func(t *testing.T) {
			s := baselineStore(t)
			advanceHistory(t, s, 1, 20)
			long := beginBaseline(t, s)
			openHistoryChildren(t, long, 4)
			advanceHistory(t, s, 21, 40)
			requireHorizon(t, s, 20, true)

			// While the transaction runs, versions from its snapshot onward stay.
			if err := s.CompactHistory(ctx); err != nil {
				t.Fatal(err)
			}
			if versions := historyVersionCount(t, s, baselineSpace, "k"); versions != 40-20+1 {
				t.Fatalf("versions with a live long transaction = %d, want %d", versions, 40-20+1)
			}

			if end == "rollback" {
				if err := long.Rollback(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := long.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			// Ending the transaction releases the pin and leaves no registry entry,
			// including the savepoint layers it owned.
			requireHorizon(t, s, 0, false)
			requireRegistryEmpty(t, s)
			requireNoOrphans(t, s)

			if err := s.CompactHistory(ctx); err != nil {
				t.Fatal(err)
			}
			if versions := historyVersionCount(t, s, baselineSpace, "k"); versions != 1 {
				t.Fatalf("versions after the transaction ended = %d, want 1", versions)
			}
			fresh := beginBaseline(t, s)
			requireValue(t, fresh, "k", "v040")
		})
	}
}

// Case C: with several roots the oldest read timestamp wins, and the horizon
// only moves when that oldest root ends.
func TestHistoryRetentionOldestRootWins(t *testing.T) {
	s := baselineStore(t)
	advanceHistory(t, s, 1, 10)
	first := beginBaseline(t, s)
	if first.Snapshot != 10 {
		t.Fatalf("first read timestamp = %d, want 10", first.Snapshot)
	}
	openHistoryChildren(t, first, 3)
	advanceHistory(t, s, 11, 15)
	second := beginBaseline(t, s)
	if second.Snapshot != 15 {
		t.Fatalf("second read timestamp = %d, want 15", second.Snapshot)
	}
	requireHorizon(t, s, 10, true)

	// A third root joins at the newest snapshot, then the newest root leaves:
	// the oldest pin is unchanged.
	third := beginBaseline(t, s)
	if err := second.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireHorizon(t, s, 10, true)

	// Only ending the oldest root moves the horizon to the next one.
	if err := first.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireHorizon(t, s, 15, true)
	// The ended root took its savepoint layers with it and left no orphan.
	infos := s.txns.ActiveTransactions()
	if len(infos) != 1 || infos[0].ID != third.ID {
		t.Fatalf("registry after the oldest root ended = %+v", infos)
	}
	requireNoOrphans(t, s)

	if err := third.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireHorizon(t, s, 0, false)
	requireRegistryEmpty(t, s)
}

// Case D: a generation change invalidates the old transactions and purges their
// retention, so a restore cannot pin the new generation's GC forever.
func TestHistoryRetentionPurgedByGenerationChange(t *testing.T) {
	s := baselineStore(t)
	ctx := context.Background()
	advanceHistory(t, s, 1, 10)
	stale := beginBaseline(t, s)
	if stale.Snapshot != 10 {
		t.Fatalf("stale read timestamp = %d, want 10", stale.Snapshot)
	}
	openHistoryChildren(t, stale, 3)
	advanceHistory(t, s, 11, 20)
	requireHorizon(t, s, 10, true)

	image := captureStoreImage(t, s)
	if err := s.Restore(bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}

	// The old transaction is invalid...
	if _, _, err := stale.Get(baselineSpace, []byte("k")); err == nil {
		t.Fatal("stale transaction still reads after a generation change")
	}
	if _, err := stale.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale commit error = %v, want ErrConflict", err)
	}
	// ...and its snapshot retention is gone, so it cannot hold the new
	// generation's history alive.
	requireHorizon(t, s, 0, false)
	requireRegistryEmpty(t, s)

	// A new transaction pins normally against the restored generation.
	fresh := beginBaseline(t, s)
	if fresh.Snapshot != 20 {
		t.Fatalf("post-restore read timestamp = %d, want 20", fresh.Snapshot)
	}
	requireHorizon(t, s, 20, true)

	// Rolling back the stale transaction must not release the new pin.
	if err := stale.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireHorizon(t, s, 20, true)
	requireNoOrphans(t, s)

	if err := fresh.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireHorizon(t, s, 0, false)
	requireRegistryEmpty(t, s)
	// GC may now advance all the way to the restored head.
	if err := s.CompactHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if versions := historyVersionCount(t, s, baselineSpace, "k"); versions != 1 {
		t.Fatalf("versions after the restored generation was released = %d, want 1", versions)
	}
}

// Requirement 6: a disconnect is an ordered rollback of every savepoint layer
// followed by the outermost transaction (executor.rollbackSessionTransaction).
// Retention must survive until the outermost rollback and be released by it.
func TestHistoryRetentionReleasedByDisconnectRollback(t *testing.T) {
	s := baselineStore(t)
	advanceHistory(t, s, 1, 5)
	root := beginBaseline(t, s)
	layers := openHistoryChildren(t, root, 4)
	requireHorizon(t, s, root.Snapshot, true)

	for i := len(layers) - 1; i >= 0; i-- {
		if err := layers[i].Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	// The root still reads, so its history is still pinned.
	requireHorizon(t, s, root.Snapshot, true)
	requireValue(t, root, "k", "v005")

	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireHorizon(t, s, 0, false)
	requireRegistryEmpty(t, s)
	requireNoOrphans(t, s)
}

// Requirement 8: no negative reference, no double release, no orphan registry and
// no leaked child, including when a transaction ends twice and when a savepoint
// layer is rolled back while deeper layers still exist.
func TestHistoryRetentionNoDoubleReleaseOrOrphan(t *testing.T) {
	s := baselineStore(t)
	ctx := context.Background()
	advanceHistory(t, s, 1, 5)

	root := beginBaseline(t, s)
	openHistoryChildren(t, root, 3)
	requireHorizon(t, s, root.Snapshot, true)

	// Commit ends through Rollback; the two extra rollbacks must not release the
	// reference a second time.
	if _, err := root.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := root.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireHorizon(t, s, 0, false)
	requireRegistryEmpty(t, s)
	requireNoOrphans(t, s)

	// ROLLBACK TO shape: rolling back a layer drops the layers above it instead of
	// orphaning them, and leaves the root's pin untouched.
	second := beginBaseline(t, s)
	layers := openHistoryChildren(t, second, 3)
	if err := layers[0].Rollback(); err != nil {
		t.Fatal(err)
	}
	infos := s.txns.ActiveTransactions()
	if len(infos) != 1 || infos[0].ID != second.ID {
		t.Fatalf("layers above the rolled-back savepoint leaked: %+v", infos)
	}
	requireNoOrphans(t, s)
	requireHorizon(t, s, second.Snapshot, true)

	if err := second.Rollback(); err != nil {
		t.Fatal(err)
	}
	second.Rollback()
	requireHorizon(t, s, 0, false)
	requireRegistryEmpty(t, s)

	// The registration path rejects a duplicate ID instead of replacing a live
	// transaction, so a collision cannot create a phantom retention reference.
	live := beginBaseline(t, s)
	if _, err := s.txns.RegisterRoot(RootRegistration{ID: live.ID, ReadTS: 999}); !errors.Is(err, ErrTransactionIDCollision) {
		t.Fatalf("duplicate ID error = %v, want ErrTransactionIDCollision", err)
	}
	requireHorizon(t, s, live.Snapshot, true)
	if err := live.Rollback(); err != nil {
		t.Fatal(err)
	}
	requireHorizon(t, s, 0, false)
	requireRegistryEmpty(t, s)
}
