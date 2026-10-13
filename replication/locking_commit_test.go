package replication_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"gbaselite/replication"
	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
)

// locking_commit_test.go is the T055 replicated Raft commit path integration test.

func openReplicatedCluster(t *testing.T, count int) ([]storageengine.Engine, []replication.Peer, func()) {
	t.Helper()
	var peers []replication.Peer
	for i := 0; i < count; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, replication.Peer{ID: fmt.Sprint(i), Address: l.Addr().String()})
		l.Close()
	}

	dirs := make([]string, count)
	engines := make([]storageengine.Engine, count)
	cleanup := func() {
		for _, e := range engines {
			if e != nil {
				e.Close()
			}
		}
	}

	for i := 0; i < count; i++ {
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

	return engines, peers, cleanup
}

func waitForLeader(t *testing.T, engines []storageengine.Engine) int {
	t.Helper()
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
						return i
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("failed to elect a leader within deadline")
	return -1
}

// TestT055ReplicatedRaftCommitSuccessAndConflict verifies that Raft replicated commits
// enforce the exact same conflict validation (latest > readTS -> ErrConflict) as standalone MVCC,
// while correctly allocating commit sequences and markers.
func TestT055ReplicatedRaftCommitSuccessAndConflict(t *testing.T) {
	engines, _, cleanup := openReplicatedCluster(t, 3)
	defer cleanup()

	leaderIdx := waitForLeader(t, engines)
	leaderEng := engines[leaderIdx]
	ctx := context.Background()

	// 1. Initial seed commit through leader
	seedTx, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedTx.Put("row/t1", []byte("k1"), []byte("v0")); err != nil {
		t.Fatal(err)
	}
	seedSeq, err := seedTx.Commit(ctx)
	if err != nil {
		t.Fatalf("seed commit failed: %v", err)
	}
	if seedSeq == 0 {
		t.Fatal("seed commit allocated 0 sequence")
	}

	// 2. T1 begins on leader and reads k1
	t1, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer t1.Rollback()

	val, found, err := t1.Get("row/t1", []byte("k1"))
	if err != nil || !found || string(val) != "v0" {
		t.Fatalf("T1 read unexpected value: %s, %v", val, err)
	}

	// 3. T2 modifies k1 and commits through leader
	t2, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := t2.Put("row/t1", []byte("k1"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.Commit(ctx); err != nil {
		t.Fatalf("T2 commit failed: %v", err)
	}

	// 4. T1 puts to k1 and attempts commit -> must be rejected with ErrConflict!
	if err := t1.Put("row/t1", []byte("k1"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Commit(ctx); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("expected ErrConflict on replicated commit, got: %v", err)
	}
}

// TestT055ReplicatedThreeRootClasses verifies that all three root classes (empty/read-only,
// data-writing, and dependency-only Guard) operate consistently on replicated engines.
func TestT055ReplicatedThreeRootClasses(t *testing.T) {
	engines, _, cleanup := openReplicatedCluster(t, 3)
	defer cleanup()

	leaderIdx := waitForLeader(t, engines)
	leaderEng := engines[leaderIdx]
	ctx := context.Background()

	// 1. Read-only root
	readTx, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = readTx.Get("row/t1", []byte("nonexistent"))
	seq, err := readTx.Commit(ctx)
	if err != nil {
		t.Fatalf("read-only replicated commit failed: %v", err)
	}
	if seq != 0 {
		t.Fatalf("read-only replicated commit allocated sequence %d, want 0", seq)
	}

	// 2. Data-writing root
	writeTx, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTx.Put("row/t1", []byte("kw1"), []byte("vw1")); err != nil {
		t.Fatal(err)
	}
	writeSeq, err := writeTx.Commit(ctx)
	if err != nil {
		t.Fatalf("data-writing replicated commit failed: %v", err)
	}
	if writeSeq == 0 {
		t.Fatal("data-writing replicated commit allocated 0 sequence")
	}

	// 3. Dependency-only (Guard) root
	guardTx, err := leaderEng.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := guardTx.Guard("row/t1", []byte("kw1")); err != nil {
		t.Fatal(err)
	}
	guardSeq, err := guardTx.Commit(ctx)
	if err != nil {
		t.Fatalf("guard replicated commit failed: %v", err)
	}
	if guardSeq == 0 {
		t.Fatal("guard replicated commit allocated 0 sequence")
	}
}
