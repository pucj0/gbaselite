package replication

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/mvcc"
	"net"
	"path/filepath"
	"testing"
)

// A replicated client whose wait was interrupted must still learn the definitive
// outcome. The FSM applies commands through Store.Apply, which never observes a
// client context, so once the publication marker exists the commit is durable no
// matter what the caller's context says. Reporting a cancellation there would
// invite a retry of a transaction that already committed.
//
// The test needs no election: the durable-marker lookup answers before any quorum
// interaction, and the unpublished case is decided by the context check that
// precedes leadership verification.
func TestReplicatedCommitReportsDurablePublicationDespiteCancellation(t *testing.T) {
	var peers []Peer
	for i := 0; i < 3; i++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, Peer{ID: fmt.Sprint(i), Address: listener.Addr().String()})
		listener.Close()
	}
	root := t.TempDir()
	store, err := mvcc.Open(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := Open(store, Options{ID: peers[0].ID, Bind: peers[0].Address, Peers: peers, Directory: filepath.Join(root, "raft"), Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	// Commit through the store: this is exactly the durable state a replicated FSM
	// produces when it applies the commit command.
	ctx := context.Background()
	tx, err := store.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := tx.Put("rows", []byte("k"), []byte("committed")); err != nil {
		t.Fatal(err)
	}
	sequence, err := tx.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, committed := store.Committed(tx.ID); !committed {
		t.Fatal("committed transaction has no publication marker")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	result, err := node.Propose(cancelled, mvcc.Command{Kind: "commit", ID: tx.ID, Snapshot: tx.Snapshot})
	if err != nil {
		t.Fatalf("commit replay error = %v, want the committed outcome", err)
	}
	if result.Sequence != sequence {
		t.Fatalf("replay sequence = %d, want %d", result.Sequence, sequence)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("replay result error = %v", err)
	}

	// Only a durable publication outranks the context: a transaction that never
	// published still reports the cancellation.
	if _, err := node.Propose(cancelled, mvcc.Command{Kind: "commit", ID: "never-published", Snapshot: tx.Snapshot}); !errors.Is(err, context.Canceled) {
		t.Fatalf("unpublished commit error = %v, want context.Canceled", err)
	}
}
