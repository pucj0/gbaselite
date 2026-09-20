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
