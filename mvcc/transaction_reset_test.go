package mvcc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

// A generation change purges the registry, so a transaction from the previous
// generation is no longer registered. Ending it must stay safe: no panic, no leaked
// staging file, no reference released twice, and no effect on the transactions of the
// new generation. The manager rejects the move as unknown, and the transaction's own
// snapshot still records the outcome it reached locally.

// staleTransactionStage grows a transaction's write set until it owns a staging
// database, so the cleanup being verified has something real to release.
func staleTransactionStage(t *testing.T, tx *Tx) string {
	t.Helper()
	value := bytes.Repeat([]byte{1}, 4096)
	for i := 0; i < 40; i++ {
		putVisible(t, tx, fmt.Sprintf("k%03d", i), string(value))
	}
	if tx.stage == nil {
		t.Fatal("the fixture did not open a staging database")
	}
	if _, err := os.Stat(tx.path); err != nil {
		t.Fatalf("staging file missing: %v", err)
	}
	return tx.path
}

func requireStagingReleased(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale cleanup leaked its staging file %s: %v", path, err)
	}
}

func TestStaleTransactionRollbackAfterResetIsSafe(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "existing", "committed")
	mustCommit(t, seed)

	stale := beginBaseline(t, s)
	path := staleTransactionStage(t, stale)
	image := captureStoreImage(t, s)
	if err := s.Restore(bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}
	// The restore purges the registry: the stale transaction holds no reference and
	// no entry, and its snapshot retention is gone with it.
	if stats := s.txns.Stats(); stats.ActiveRoot != 0 || stats.HasOldestReadTS {
		t.Fatalf("restore left retention behind: %+v", stats)
	}
	if _, ok := s.txns.Info(stale.ID); ok {
		t.Fatal("the stale transaction survived the restore")
	}

	// A transaction of the new generation owns the only retention reference.
	fresh := beginBaseline(t, s)
	requireHorizon(t, s, fresh.Snapshot, true)

	if err := stale.Rollback(); err != nil {
		t.Fatalf("stale rollback: %v", err)
	}
	if state := stale.State(); state != TransactionAborted {
		t.Fatalf("stale transaction state = %s, want ABORTED", state)
	}
	requireStagingReleased(t, path)
	if _, ok := s.txns.Info(stale.ID); ok {
		t.Fatal("stale rollback registered a new-generation entry")
	}
	// The new generation is untouched: same horizon, no extra counters.
	if oldest, ok := s.txns.OldestReadTS(); !ok || oldest != fresh.Snapshot {
		t.Fatalf("stale rollback moved the horizon to (%d, %v), want %d", oldest, ok, fresh.Snapshot)
	}
	if stats := s.txns.Stats(); stats.Aborted != 0 {
		t.Fatalf("a stale transaction was counted as an abort for this store: %+v", stats)
	}
	// Ending it again is still a no-op. A stale transaction reports the generation
	// change rather than ErrClosed, because the generation check comes first.
	if err := stale.Rollback(); err != nil {
		t.Fatalf("repeated stale rollback: %v", err)
	}
	if _, err := stale.Commit(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale commit after rollback = %v, want ErrConflict", err)
	}
	if state := stale.State(); state != TransactionAborted {
		t.Fatalf("repeated end changed the stale state to %s", state)
	}
}

func TestStaleTransactionCommitAfterResetIsSafe(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "existing", "committed")
	mustCommit(t, seed)
	head := mustHead(t, s)

	stale := beginBaseline(t, s)
	path := staleTransactionStage(t, stale)
	image := captureStoreImage(t, s)
	if err := s.Restore(bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}
	// Captured while the restored store still has no transaction of its own, so the
	// comparison below is not confused by the readers the data checks register.
	before := s.txns.Stats()
	oldestBefore, hasOldestBefore := s.txns.OldestReadTS()

	if _, err := stale.Commit(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale commit = %v, want ErrConflict", err)
	}
	info := stale.Info()
	if info.State != TransactionAborted {
		t.Fatalf("stale transaction state = %s, want ABORTED", info.State)
	}
	if info.HasCommitTS {
		t.Fatalf("stale transaction recorded a commit sequence: %+v", info)
	}
	requireStagingReleased(t, path)
	if _, committed := s.Committed(stale.ID); committed {
		t.Fatal("a stale commit published a marker")
	}
	// Nothing from the previous generation leaked into the restored store.
	afterHead, _, _ := storeMeta(t, s)
	if afterHead != head {
		t.Fatalf("stale commit moved head %d -> %d", head, afterHead)
	}
	if after := s.txns.Stats(); after != before {
		t.Fatalf("stale commit disturbed the new generation: %+v -> %+v", before, after)
	}
	if oldest, ok := s.txns.OldestReadTS(); ok != hasOldestBefore || (ok && oldest != oldestBefore) {
		t.Fatalf("stale commit moved the horizon to (%d, %v)", oldest, ok)
	}
	requireCancelValue(t, s, "existing", "committed")
	if _, _, ok, err := s.Get(^uint64(0), visibilitySpace, []byte("k000")); err != nil || ok {
		t.Fatalf("a stale staged write became visible: ok=%v err=%v", ok, err)
	}
}

func TestStaleChildTransactionAfterResetIsSafe(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "existing", "committed")
	mustCommit(t, seed)

	parent := beginBaseline(t, s)
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	putVisible(t, child, "c", "child")
	image := captureStoreImage(t, s)
	if err := s.Restore(bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}
	// Captured before the data checks below register their own read transactions.
	before := s.txns.Stats()

	// A stale child must not merge into anything, and must not report MERGED.
	if _, err := child.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale child commit = %v, want ErrConflict", err)
	}
	if state := child.State(); state != TransactionAborted {
		t.Fatalf("stale child state = %s, want ABORTED", state)
	}
	if _, err := parent.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale parent commit = %v, want ErrConflict", err)
	}
	if state := parent.State(); state != TransactionAborted {
		t.Fatalf("stale parent state = %s, want ABORTED", state)
	}
	if _, ok := s.txns.Info(child.ID); ok {
		t.Fatal("a stale child re-entered the registry")
	}
	if _, ok := s.txns.Info(parent.ID); ok {
		t.Fatal("a stale parent re-entered the registry")
	}
	if after := s.txns.Stats(); after != before {
		t.Fatalf("stale transactions left registry state: %+v -> %+v", before, after)
	}
	requireInvisible(t, s, "c")
	requireCancelValue(t, s, "existing", "committed")
	requireNoOrphans(t, s)
}

// M-6: a stale transaction must fail closed on writes too. Get, Commit and Rollback were
// already covered; Put and Delete must reject the new generation in exactly the same way
// as a closed transaction (ErrClosed through checkOpen/write), and nothing about the
// refused write may reach the new generation: no staging database is opened, no registry
// entry appears, the head does not move, retention is untouched, and neither the refused
// value nor a refused delete becomes visible.
func TestStaleTransactionRejectsWritesAfterReset(t *testing.T) {
	t.Run("a stale root without a stage never opens one", func(t *testing.T) {
		s := baselineStore(t)
		seed := beginBaseline(t, s)
		putVisible(t, seed, "existing", "committed")
		mustCommit(t, seed)
		head := mustHead(t, s)

		stale := beginBaseline(t, s)
		image := captureStoreImage(t, s)
		if err := s.Restore(bytes.NewReader(image)); err != nil {
			t.Fatal(err)
		}
		oldestBefore, hasOldestBefore := s.txns.OldestReadTS()

		if stale.stage != nil || len(stale.buffered) != 0 || stale.stagedBytes != 0 {
			t.Fatalf("the fixture is not a write-free transaction: stage=%v buffered=%d bytes=%d",
				stale.stage != nil, len(stale.buffered), stale.stagedBytes)
		}
		if err := stale.Put(visibilitySpace, []byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
			t.Fatalf("stale Put = %v, want ErrClosed", err)
		}
		if err := stale.Delete(visibilitySpace, []byte("existing")); !errors.Is(err, ErrClosed) {
			t.Fatalf("stale Delete = %v, want ErrClosed", err)
		}
		// Fail closed: the refused writes created no staging state and no accounting.
		if stale.stage != nil || len(stale.buffered) != 0 || stale.stagedBytes != 0 {
			t.Fatalf("a refused write created staging state: stage=%v buffered=%d bytes=%d",
				stale.stage != nil, len(stale.buffered), stale.stagedBytes)
		}
		if _, ok := s.txns.Info(stale.ID); ok {
			t.Fatal("a refused write registered the stale transaction")
		}
		if after := mustHead(t, s); after != head {
			t.Fatalf("a refused write moved head %d -> %d", head, after)
		}
		if oldest, ok := s.txns.OldestReadTS(); ok != hasOldestBefore || (ok && oldest != oldestBefore) {
			t.Fatalf("a refused write changed retention to (%d, %v)", oldest, ok)
		}
		if stats := s.txns.Stats(); stats.ActiveRoot != 0 || stats.ActiveChildren != 0 || stats.HasOldestReadTS {
			t.Fatalf("a refused write left registry state: %+v", stats)
		}
		if _, _, ok, err := s.Get(^uint64(0), visibilitySpace, []byte("k")); err != nil || ok {
			t.Fatalf("a refused stale write became visible: ok=%v err=%v", ok, err)
		}
		if _, _, ok, err := s.Get(^uint64(0), visibilitySpace, []byte("existing")); err != nil || !ok {
			t.Fatalf("a refused stale delete removed committed data: ok=%v err=%v", ok, err)
		}
	})

	t.Run("a stale root keeps its existing stage untouched", func(t *testing.T) {
		s := baselineStore(t)
		seed := beginBaseline(t, s)
		putVisible(t, seed, "existing", "committed")
		mustCommit(t, seed)
		head := mustHead(t, s)

		stale := beginBaseline(t, s)
		path := staleTransactionStage(t, stale)
		stage, stagedBytes := stale.stage, stale.stagedBytes
		infoBefore := stale.Info()
		image := captureStoreImage(t, s)
		if err := s.Restore(bytes.NewReader(image)); err != nil {
			t.Fatal(err)
		}

		if err := stale.Put(visibilitySpace, []byte("late"), []byte("v")); !errors.Is(err, ErrClosed) {
			t.Fatalf("stale Put with an open stage = %v, want ErrClosed", err)
		}
		if err := stale.Delete(visibilitySpace, []byte("existing")); !errors.Is(err, ErrClosed) {
			t.Fatalf("stale Delete with an open stage = %v, want ErrClosed", err)
		}
		// The existing staging database is neither replaced nor grown.
		if stale.stage != stage {
			t.Fatal("a refused write replaced the staging database")
		}
		if stale.stagedBytes != stagedBytes {
			t.Fatalf("a refused write changed the write-set accounting: %d -> %d", stagedBytes, stale.stagedBytes)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("a refused write removed the staging file: %v", err)
		}
		if after := stale.Info(); after != infoBefore {
			t.Fatalf("a refused write changed the transaction snapshot: %+v -> %+v", infoBefore, after)
		}
		if after := mustHead(t, s); after != head {
			t.Fatalf("a refused write moved head %d -> %d", head, after)
		}
		// The stale transaction still ends safely and releases what it held.
		if err := stale.Rollback(); err != nil {
			t.Fatalf("stale rollback = %v", err)
		}
		requireStagingReleased(t, path)
		if _, ok := s.txns.Info(stale.ID); ok {
			t.Fatal("stale cleanup registered a new-generation entry")
		}
	})

	t.Run("a stale child rejects writes as well", func(t *testing.T) {
		s := baselineStore(t)
		seed := beginBaseline(t, s)
		putVisible(t, seed, "existing", "committed")
		mustCommit(t, seed)

		parent := beginBaseline(t, s)
		child, err := parent.Child()
		if err != nil {
			t.Fatal(err)
		}
		image := captureStoreImage(t, s)
		if err := s.Restore(bytes.NewReader(image)); err != nil {
			t.Fatal(err)
		}
		before := s.txns.Stats()

		if err := child.Put(visibilitySpace, []byte("c"), []byte("child")); !errors.Is(err, ErrClosed) {
			t.Fatalf("stale child Put = %v, want ErrClosed", err)
		}
		if err := child.Delete(visibilitySpace, []byte("existing")); !errors.Is(err, ErrClosed) {
			t.Fatalf("stale child Delete = %v, want ErrClosed", err)
		}
		if err := parent.Put(visibilitySpace, []byte("p"), []byte("parent")); !errors.Is(err, ErrClosed) {
			t.Fatalf("stale parent Put = %v, want ErrClosed", err)
		}
		if err := parent.Delete(visibilitySpace, []byte("existing")); !errors.Is(err, ErrClosed) {
			t.Fatalf("stale parent Delete = %v, want ErrClosed", err)
		}
		// Neither the child nor the parent opened a stage or reached the registry.
		if child.stage != nil || len(child.buffered) != 0 || child.stagedBytes != 0 {
			t.Fatalf("a refused child write created staging state: stage=%v buffered=%d bytes=%d",
				child.stage != nil, len(child.buffered), child.stagedBytes)
		}
		if parent.stage != nil || len(parent.buffered) != 0 || parent.stagedBytes != 0 {
			t.Fatalf("a refused parent write created staging state: stage=%v buffered=%d bytes=%d",
				parent.stage != nil, len(parent.buffered), parent.stagedBytes)
		}
		if _, ok := s.txns.Info(child.ID); ok {
			t.Fatal("a refused child write re-entered the registry")
		}
		if _, ok := s.txns.Info(parent.ID); ok {
			t.Fatal("a refused parent write re-entered the registry")
		}
		if after := s.txns.Stats(); after != before {
			t.Fatalf("refused writes left registry state: %+v -> %+v", before, after)
		}
		requireInvisible(t, s, "c")
		requireCancelValue(t, s, "existing", "committed")
		requireNoOrphans(t, s)
	})
}
