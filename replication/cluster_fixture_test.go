package replication_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"gbaselite/replication"
	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
)

// cluster_fixture_test.go provides a reliable multi-node Raft cluster fixture that guarantees
// uniquely allocated peer addresses and clean teardown.

type testCluster struct {
	t       *testing.T
	peers   []replication.Peer
	dirs    []string
	engines []storageengine.Engine
}

func newTestCluster(t *testing.T, count int) *testCluster {
	t.Helper()

	// 1. Concurrently hold count listeners so the OS assigns distinct ports without collisions.
	listeners := make([]net.Listener, count)
	peers := make([]replication.Peer, count)
	seen := make(map[string]bool)

	for i := 0; i < count; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for j := 0; j < i; j++ {
				_ = listeners[j].Close()
			}
			t.Fatalf("failed to allocate listener for peer %d: %v", i, err)
		}
		listeners[i] = l
		addr := l.Addr().String()
		if seen[addr] {
			for j := 0; j <= i; j++ {
				_ = listeners[j].Close()
			}
			t.Fatalf("duplicate peer address allocated: %s", addr)
		}
		seen[addr] = true
		peers[i] = replication.Peer{ID: fmt.Sprint(i), Address: addr}
	}

	// Close all listeners so Raft transport can bind to them.
	for _, l := range listeners {
		_ = l.Close()
	}

	dirs := make([]string, count)
	engines := make([]storageengine.Engine, count)
	tc := &testCluster{
		t:       t,
		peers:   peers,
		dirs:    dirs,
		engines: engines,
	}

	t.Cleanup(func() {
		tc.Close()
	})

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
			t.Fatalf("failed to open engine %d: %v", i, err)
		}
	}

	return tc
}

func (tc *testCluster) Close() {
	for i, e := range tc.engines {
		if e != nil {
			_ = e.Close()
			tc.engines[i] = nil
		}
	}
}

func (tc *testCluster) WaitForLeader(timeout time.Duration) int {
	tc.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for i, eng := range tc.engines {
			if eng == nil {
				continue
			}
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
	tc.t.Fatal("failed to elect a leader within deadline")
	return -1
}
