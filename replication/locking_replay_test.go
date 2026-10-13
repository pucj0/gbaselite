package replication

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gbaselite/mvcc"
)

// locking_replay_test.go satisfies T068:
// Proves Raft FSM Apply never waits for user locks and applies directly.

func TestT068ReplicationFSMApplyNeverWaitsForUserLocks(t *testing.T) {
	dir := t.TempDir()
	store, err := mvcc.Open(filepath.Join(dir, "mvcc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()

	// 1. Transaction A begins and holds a lock by writing a key
	txA, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()

	if err := txA.Put("space1", []byte("locked_key"), []byte("user_val")); err != nil {
		t.Fatal(err)
	}

	// 2. FSM Apply arrives concurrently on the SAME key
	cmd := mvcc.Command{
		Kind: "commit",
		ID:   "tx-replicated",
		Ops: []mvcc.Op{
			{Space: "space1", Key: []byte("locked_key"), Value: []byte("replicated_val")},
		},
	}

	doneApply := make(chan error, 1)
	go func() {
		_, err := store.Apply(1, cmd)
		doneApply <- err
	}()

	// 3. FSM Apply must complete immediately and NEVER wait for user transaction A's lock
	select {
	case err := <-doneApply:
		if err != nil {
			t.Fatalf("FSM Apply failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FSM Apply was blocked by user lock; violation of Raft replay contract")
	}

	// Clean up
	_ = txA.Rollback()
}
