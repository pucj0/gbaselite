package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gbaselite/mvcc"
	"github.com/hashicorp/raft"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestThreeNodeFailoverAndMinority(t *testing.T) {
	var peers []Peer
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, Peer{ID: fmt.Sprint(i), Address: l.Addr().String()})
		l.Close()
	}
	nodes := make([]*Node, 3)
	stores := make([]*mvcc.Store, 3)
	dirs := make([]string, 3)
	defer func() {
		for _, n := range nodes {
			if n != nil {
				n.Close()
			}
		}
		for _, s := range stores {
			if s != nil {
				s.Close()
			}
		}
	}()
	open := func(i int) {
		var err error
		stores[i], err = mvcc.Open(filepath.Join(dirs[i], "data"))
		if err != nil {
			t.Fatal(err)
		}
		nodes[i], err = Open(stores[i], Options{ID: peers[i].ID, Bind: peers[i].Address, Peers: peers, Directory: filepath.Join(dirs[i], "raft"), Bootstrap: i == 0})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range nodes {
		dirs[i] = t.TempDir()
		open(i)
	}
	leader := func() int {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			for i, n := range nodes {
				if n != nil && n.Status().State == "Leader" {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					err := n.Barrier(ctx)
					cancel()
					if err == nil {
						return i
					}
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("no quorum leader")
		return -1
	}
	put := func(i int, k, v string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := stores[i].Begin(ctx, nodes[i])
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = tx.Put("rows", []byte(k), []byte(v)); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	first := leader()
	// A verification must not append a barrier log or wait for FSM apply.
	beforeVerify := nodes[first].raft.LastIndex()
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), time.Second)
	if err := nodes[first].VerifyLeader(verifyCtx); err != nil {
		t.Fatal(err)
	}
	verifyCancel()
	if nodes[first].raft.LastIndex() != beforeVerify {
		t.Fatal("leader verification appended a Raft log entry")
	}
	canceled, cancelVerify := context.WithCancel(context.Background())
	cancelVerify()
	if err := nodes[first].VerifyLeader(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for i, n := range nodes {
		if i != first {
			if err := n.VerifyLeader(context.Background()); err == nil {
				t.Fatal("follower verified leadership")
			}
		}
	}
	put(first, "a", "confirmed")
	if err := nodes[first].raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	nodes[first].Close()
	nodes[first] = nil
	stores[first].Close()
	stores[first] = nil
	second := leader()
	put(second, "b", "after-election")
	open(first)
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _, ok, err := stores[first].Get(^uint64(0), "rows", []byte("b"))
		if err != nil {
			t.Fatal(err)
		}
		if ok && string(v) == "after-election" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restarted node did not catch up")
		}
		time.Sleep(25 * time.Millisecond)
	}
	for i, s := range stores {
		v, _, ok, err := s.Get(^uint64(0), "rows", []byte("a"))
		if err != nil || !ok || string(v) != "confirmed" {
			t.Fatalf("node %d lost confirmed data: %s %v", i, v, err)
		}
	}
	for i, n := range nodes {
		if i != second {
			n.Close()
			nodes[i] = nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := nodes[second].VerifyLeader(ctx); err == nil {
		t.Fatal("minority verified leadership")
	}
	if err := nodes[second].Barrier(ctx); err == nil {
		t.Fatal("minority accepted linearizable operation")
	}
}

func TestRejectPreviousTermCommit(t *testing.T) {
	s, err := mvcc.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	node := &Node{store: s}
	command, _ := json.Marshal(mvcc.Command{Kind: "commit", ID: "old", Term: 3})
	result := node.Apply(&raft.Log{Index: 4, Term: 4, Data: command}).(mvcc.Result)
	if result.Err() != mvcc.ErrConflict {
		t.Fatalf("old term accepted: %+v", result)
	}
}
