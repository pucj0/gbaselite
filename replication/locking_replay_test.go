package replication

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/mvcc"
	"gbaselite/storageengine"
)

// locking_replay_test.go satisfies T068:
// Proves Raft FSM Apply never waits for user locks, applies directly,
// persists and publishes the new version, and old user Root encounters ErrConflict.

func TestT068ReplicationFSMApplyNeverWaitsForUserLocks(t *testing.T) {
	dir := t.TempDir()
	store, err := mvcc.Open(filepath.Join(dir, "mvcc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 1. Attach real Lock Manager
	lm, err := lockmanager.NewManager(lockmanager.BudgetLimits{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lm.Close()
	store.AttachLocks(lm)

	ctx := context.Background()

	// 2. Transaction A begins and holds a lock by writing a key
	txA, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	if err := txA.Put("space1", []byte("locked_key"), []byte("user_val")); err != nil {
		t.Fatal(err)
	}

	// 3. Confirm Grant is actually held in LockManager
	if lm.Stats().ActiveOwners == 0 || lm.Stats().GrantedComponents == 0 {
		t.Fatal("expected user transaction A to hold active grant in lock manager")
	}

	// 4. FSM Apply arrives on the EXACT SAME key (replicated two-phase stage then commit)
	cmdStage := mvcc.Command{
		Kind: "stage",
		ID:   "tx-replicated",
		Ops: []mvcc.Op{
			{Space: "space1", Key: []byte("locked_key"), Value: []byte("replicated_val")},
		},
	}
	cmdCommit := mvcc.Command{
		Kind:     "commit",
		ID:       "tx-replicated",
		Snapshot: 1,
	}

	doneApply := make(chan error, 1)
	go func() {
		if _, err := store.Apply(2, cmdStage); err != nil {
			doneApply <- err
			return
		}
		_, err := store.Apply(3, cmdCommit)
		doneApply <- err
	}()

	// 5. FSM Apply must complete immediately and NEVER wait for user transaction A's lock
	select {
	case err := <-doneApply:
		if err != nil {
			t.Fatalf("FSM Apply failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FSM Apply was blocked by user lock; violation of Raft replay contract")
	}

	// 6. Check Apply's correct persistence and version publishing
	txRead, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatalf("store.Begin for read failed: %v", err)
	}
	defer txRead.Rollback()

	val, found, err := txRead.Get("space1", []byte("locked_key"))
	if err != nil {
		t.Fatalf("txRead.Get failed: %v", err)
	}
	if !found || string(val) != "replicated_val" {
		t.Fatalf("expected replicated_val, got found=%v, val=%s", found, string(val))
	}

	// 7. Old Root A encounters the newly published version (latest > snapshot) and is rejected with ErrConflict
	_, errCommit := txA.Commit(ctx)
	if !errors.Is(errCommit, storageengine.ErrConflict) && !errors.Is(errCommit, mvcc.ErrConflict) {
		t.Fatalf("expected ErrConflict on txA commit after replicated write, got: %v", errCommit)
	}
}
