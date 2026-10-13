package replication_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"gbaselite/mvcc"
	"gbaselite/replication"
	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
)

// locking_epoch_test.go is the T054 suite: leader-only Current Read, Barrier vs VerifyLeader,
// Term fence, and loss-wake on replication.

func TestT054LeaderBarrierLinearizableOrderingAndTermFence(t *testing.T) {
	var peers []replication.Peer
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, replication.Peer{ID: fmt.Sprint(i), Address: l.Addr().String()})
		l.Close()
	}

	dirs := make([]string, 3)
	engines := make([]storageengine.Engine, 3)
	defer func() {
		for _, e := range engines {
			if e != nil {
				e.Close()
			}
		}
	}()

	openNode := func(i int) {
		dirs[i] = t.TempDir()
		opts := storageengine.Options{
			Replication: &storageengine.ReplicationOptions{
				ID:        peers[i].ID,
				Bind:      peers[i].Address,
				Advertise: peers[i].Address,
				Directory: filepath.Join(dirs[i], "raft"),
				Bootstrap: i == 0,
			},
		}
		for _, p := range peers {
			opts.Replication.Peers = append(opts.Replication.Peers, storageengine.Peer{ID: p.ID, Address: p.Address})
		}
		var err error
		engines[i], err = mvccadapter.Open(filepath.Join(dirs[i], "data"), opts)
		if err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 3; i++ {
		openNode(i)
	}

	// Wait for leader election
	leaderIdx := -1
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for i, eng := range engines {
			if repEng, ok := eng.(storageengine.ReplicatedEngine); ok {
				rep := repEng.Replica()
				if rep != nil && rep.Status().State == "Leader" {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					err := rep.Barrier(ctx)
					cancel()
					if err == nil {
						leaderIdx = i
						break
					}
				}
			}
		}
		if leaderIdx >= 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if leaderIdx < 0 {
		t.Fatal("failed to elect a leader within deadline")
	}

	leaderEng := engines[leaderIdx]
	ctx := context.Background()

	// 1. Verify Leader Barrier succeeds on Leader
	rep := leaderEng.(storageengine.ReplicatedEngine).Replica()
	if err := rep.Barrier(ctx); err != nil {
		t.Fatalf("leader Barrier failed: %v", err)
	}
	if err := rep.(storageengine.LeaderVerifier).VerifyLeader(ctx); err != nil {
		t.Fatalf("leader VerifyLeader failed: %v", err)
	}

	// 2. Non-leader (follower) fails Barrier with ErrNotLeader
	followerIdx := (leaderIdx + 1) % 3
	followerEng := engines[followerIdx]
	followerRep := followerEng.(storageengine.ReplicatedEngine).Replica()
	if err := followerRep.Barrier(ctx); err == nil || !errors.Is(err, replication.ErrNotLeader) {
		t.Fatalf("follower Barrier expected ErrNotLeader, got: %v", err)
	}

	// 3. Test Term Fence: Current Read under a stale term grant is rejected with ErrLockInvalidated
	leaderTx, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer leaderTx.Rollback()

	locking, ok := leaderTx.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	if !ok {
		t.Fatal("leader transaction does not expose LockingTxn")
	}
	locks, available := locking.LockingTxn()
	if !available {
		t.Fatal("leader LockingTxn is not available")
	}

	scope, err := locks.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}

	tableRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   "row/test",
		TableID: "t1",
	}
	rowRef := storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   "row/test",
		TableID: "t1",
		Key:     []byte("k1"),
	}
	grant, err := locks.Acquire(ctx, scope, storageengine.LockRequest{Components: []storageengine.LockComponent{tableRef, rowRef}})
	if err != nil {
		t.Fatalf("leader Acquire failed: %v", err)
	}

	// Normal ReadCurrent with valid grant succeeds
	_, err = locks.ReadCurrent(ctx, grant, "row/test", []byte("k1"))
	if err != nil {
		t.Fatalf("leader ReadCurrent with valid grant failed: %v", err)
	}

	// Stale Term grant is rejected by Term Fence!
	staleTermGrant := grant
	staleTermGrant.Term = grant.Term + 999
	_, err = locks.ReadCurrent(ctx, staleTermGrant, "row/test", []byte("k1"))
	if !errors.Is(err, storageengine.ErrLockInvalidated) {
		t.Fatalf("expected ErrLockInvalidated for stale term grant, got: %v", err)
	}
}

// TestT054CommandFormatHasNoNewFields verifies that mvcc.Command serialization contains only
// the specified, backward-compatible fields without new lock-specific fields.
func TestT054CommandFormatHasNoNewFields(t *testing.T) {
	cmd := mvcc.Command{
		Term:     1,
		Kind:     "commit",
		ID:       "tx1",
		Snapshot: 10,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{
		"Term":     true,
		"Kind":     true,
		"ID":       true,
		"Snapshot": true,
		"Ops":      true,
		"Counter":  true,
		"Floor":    true,
		"Count":    true,
	}

	for k := range m {
		if !allowed[k] {
			t.Fatalf("unexpected new field in Command JSON serialization: %q", k)
		}
	}
}
