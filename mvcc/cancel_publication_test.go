package mvcc

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Context cancellation against the durable commit point.
//
// The publication marker is the commit point, so every commit path must satisfy:
//
//	observed cancellation before publication -> error, no marker, invisible writes
//	versions installed without a marker      -> never visible to any reader
//	publication already durable              -> definitive committed outcome
//
// Nothing here sleeps. Each test aims a cancellation at a storage boundary the
// path itself defines (a cancelled context, the reserved-versus-applied install
// high-water mark, or a proposer that cancels right after the publication), so the
// outcome is deterministic rather than a guess about timing.

// cancelPath publishes one transaction through one physical commit path and
// reports the transaction ID and the outcome the caller would see.
type cancelPath struct {
	name     string
	localWAL bool
	// installWindow is true when versions are installed in steps separate from the
	// publication marker, so a cancellation can land between them.
	installWindow bool
	commit        func(t *testing.T, s *Store, commitCtx context.Context, script conflictScript) (string, uint64, error)
}

// beginCancelTx opens a root transaction with a live context, because Begin itself
// honours cancellation: the test controls the commit's context, not the begin's.
func beginCancelTx(t *testing.T, s *Store, proposer Proposer) *Tx {
	t.Helper()
	tx, err := s.Begin(context.Background(), proposer)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// padCancelStream grows the write set past MaxChunkBytes, which is the condition
// Tx.Commit uses to select the streaming path.
func padCancelStream(t *testing.T, tx *Tx, id string) {
	t.Helper()
	padding := make([]byte, 4096)
	for i := 0; i < 20; i++ {
		if err := tx.Put("__cancel_pad", []byte(fmt.Sprintf("pad-%s-%02d", id, i)), padding); err != nil {
			t.Fatal(err)
		}
	}
}

var cancelPaths = []cancelPath{
	{
		name: "local-bounded",
		commit: func(t *testing.T, s *Store, commitCtx context.Context, script conflictScript) (string, uint64, error) {
			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			script(t, tx)
			sequence, err := tx.Commit(commitCtx)
			return tx.ID, sequence, err
		},
	},
	{
		name: "local-group",
		commit: func(t *testing.T, s *Store, commitCtx context.Context, script conflictScript) (string, uint64, error) {
			id := randomID()
			request := &localCommitRequest{ctx: commitCtx, generation: s.generation.Load(), snapshot: mustHead(t, s), id: id, ops: captureConflictOps(t, s, script), done: make(chan struct{})}
			s.commitLocalGroup([]*localCommitRequest{request})
			return id, request.index, request.err
		},
	},
	{
		name:          "local-streaming",
		installWindow: true,
		commit: func(t *testing.T, s *Store, commitCtx context.Context, script conflictScript) (string, uint64, error) {
			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			script(t, tx)
			padCancelStream(t, tx, "stream")
			sequence, err := tx.Commit(commitCtx)
			return tx.ID, sequence, err
		},
	},
	{
		name:     "local-wal",
		localWAL: true,
		commit: func(t *testing.T, s *Store, commitCtx context.Context, script conflictScript) (string, uint64, error) {
			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			script(t, tx)
			sequence, err := tx.Commit(commitCtx)
			return tx.ID, sequence, err
		},
	},
	{
		name:          "local-wal-streaming",
		localWAL:      true,
		installWindow: true,
		commit: func(t *testing.T, s *Store, commitCtx context.Context, script conflictScript) (string, uint64, error) {
			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			script(t, tx)
			padCancelStream(t, tx, "walstream")
			sequence, err := tx.Commit(commitCtx)
			return tx.ID, sequence, err
		},
	},
	{
		// A proposer that is not the store selects the staged path, which is the
		// same validate-then-publish sequence a replicated client uses.
		name: "replicated-client",
		commit: func(t *testing.T, s *Store, commitCtx context.Context, script conflictScript) (string, uint64, error) {
			tx := beginCancelTx(t, s, &forwardingProposer{store: s})
			defer tx.Rollback()
			script(t, tx)
			sequence, err := tx.Commit(commitCtx)
			return tx.ID, sequence, err
		},
	},
}

func newCancelStore(t *testing.T, localWAL bool) *Store {
	t.Helper()
	s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: localWAL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func requireInvisible(t *testing.T, s *Store, key string) {
	t.Helper()
	tx := beginBaseline(t, s)
	if value, ok, err := tx.Get(visibilitySpace, []byte(key)); err != nil || ok || len(value) != 0 {
		t.Fatalf("key %q = (%q, %v, %v), want invisible", key, value, ok, err)
	}
}

// requireCancelValue reads one row through a fresh transaction in the namespace
// these tests write to.
func requireCancelValue(t *testing.T, s *Store, key, want string) {
	t.Helper()
	tx := beginBaseline(t, s)
	if value := conflictRead(t, tx, key); value != want {
		t.Fatalf("key %q = %q, want %q", key, value, want)
	}
}

// Case A and B: a cancellation observed before publication leaves an aborted
// transaction with an unset commit sequence and invisible writes.
func TestCancelBeforePublicationAbortsEveryPath(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, path := range cancelPaths {
		t.Run(path.name, func(t *testing.T) {
			s := newCancelStore(t, path.localWAL)
			head := mustHead(t, s)
			id, sequence, err := path.commit(t, s, cancelled, putConflict("k", "value"))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("commit error = %v, want context.Canceled", err)
			}
			if sequence != 0 {
				t.Fatalf("cancelled commit reported sequence %d", sequence)
			}
			if _, committed := s.Committed(id); committed {
				t.Fatalf("cancelled transaction %s has a publication marker", id)
			}
			if after := mustHead(t, s); after != head {
				t.Fatalf("cancelled commit moved head %d -> %d", head, after)
			}
			requireInvisible(t, s, "k")

			// The aborted write must never resurface: a later commit of the same key
			// is what readers see.
			winner := beginBaseline(t, s)
			putVisible(t, winner, "k", "winner")
			mustCommit(t, winner)
			requireCancelValue(t, s, "k", "winner")
			if _, committed := s.Committed(id); committed {
				t.Fatalf("aborted transaction %s became committed later", id)
			}
		})
	}
}

// Case B in its strongest form: versions really are installed, and they still never
// become visible because the publication marker is missing.
func TestCancelDuringInstallKeepsVersionsUnpublished(t *testing.T) {
	ctx := context.Background()

	t.Run("local-streaming", func(t *testing.T) {
		for _, localWAL := range []bool{false, true} {
			s := newCancelStore(t, localWAL)
			seed := beginBaseline(t, s)
			putVisible(t, seed, "k", "old")
			mustCommit(t, seed)
			before := physicalVersionCount(t, s, visibilitySpace, "k")

			tx := beginCancelTx(t, s, nil)
			defer tx.Rollback()
			putVisible(t, tx, "k", "abandoned")
			padCancelStream(t, tx, "mid")
			interrupted := &afterInstalledContext{Context: ctx, store: s}
			sequence, err := tx.Commit(interrupted)

			if localWAL {
				// The local WAL path decides durability by replacing the WAL file and
				// only then installs versions. Nothing is installed before that
				// decision, so an install-aware cancellation is never observed here and
				// the commit is reported as committed: the WAL file is this path's
				// durable commit point, and completing the install afterwards is
				// recovery rather than publication.
				if err != nil {
					t.Fatalf("wal commit error = %v, want the committed outcome", err)
				}
				if sequence == 0 {
					t.Fatal("wal commit reported no sequence")
				}
				if _, committed := s.Committed(tx.ID); !committed {
					t.Fatal("wal commit has no publication marker")
				}
				requireCancelValue(t, s, "k", "abandoned")
				continue
			}

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("commit error = %v, want context.Canceled", err)
			}
			if sequence != 0 {
				t.Fatalf("cancelled commit reported sequence %d", sequence)
			}
			// The version was installed for real, so the assertion is not vacuous.
			if installed := physicalVersionCount(t, s, visibilitySpace, "k"); installed <= before {
				t.Fatalf("no version was installed before the cancellation (%d -> %d)", before, installed)
			}
			if _, committed := s.Committed(tx.ID); committed {
				t.Fatal("interrupted transaction has a publication marker")
			}
			// Nobody can see the abandoned version.
			requireCancelValue(t, s, "k", "old")
			value, _, ok, err := s.Get(^uint64(0), visibilitySpace, []byte("k"))
			if err != nil || !ok || string(value) != "old" {
				t.Fatalf("newest revision = (%q, %v, %v), want old", value, ok, err)
			}
		}
	})

	t.Run("replicated-client", func(t *testing.T) {
		s := newCancelStore(t, false)
		seed := beginBaseline(t, s)
		putVisible(t, seed, "k", "old")
		mustCommit(t, seed)
		before := physicalVersionCount(t, s, visibilitySpace, "k")

		id := randomID()
		ops := captureConflictOps(t, s, putConflict("k", "abandoned"))
		if _, err := s.Propose(ctx, Command{Kind: "stage", ID: id, Ops: ops}); err != nil {
			t.Fatal(err)
		}
		index := s.localSeq + 1
		command := Command{Kind: "commit", ID: id, Snapshot: mustHead(t, s)}
		interrupted := &afterInstalledContext{Context: ctx, store: s}
		if _, err := s.commitContext(interrupted, index, command); !errors.Is(err, context.Canceled) {
			t.Fatalf("staged commit error = %v, want context.Canceled", err)
		}
		if installed := physicalVersionCount(t, s, visibilitySpace, "k"); installed <= before {
			t.Fatalf("no version was installed before the cancellation (%d -> %d)", before, installed)
		}
		if _, committed := s.Committed(id); committed {
			t.Fatal("interrupted staged transaction has a publication marker")
		}
		requireCancelValue(t, s, "k", "old")
		// The installed version stays unpublished. A later commit is what readers
		// see, and visibility skips the abandoned version even when it holds a higher
		// physical sequence than the new commit, because it has no marker.
		if err := s.clearPending(id); err != nil {
			t.Fatal(err)
		}
		winner := beginBaseline(t, s)
		putVisible(t, winner, "k", "newer")
		if sequence := mustCommit(t, winner); sequence == 0 {
			t.Fatal("later commit reported no sequence")
		}
		requireCancelValue(t, s, "k", "newer")
		value, _, ok, err := s.Get(^uint64(0), visibilitySpace, []byte("k"))
		if err != nil || !ok || string(value) != "newer" {
			t.Fatalf("newest revision = (%q, %v, %v), want newer", value, ok, err)
		}
		if _, committed := s.Committed(id); committed {
			t.Fatal("the abandoned version became committed")
		}
	})
}

// Case C: once the publication is durable, a cancellation must not turn the
// outcome into a rollback. The first form is a replay of an already published
// commit with a cancelled context; the second cancels the context at the exact
// boundary between publication and Commit inspecting its result.
func TestCancelAfterPublicationReportsCommittedOutcome(t *testing.T) {
	ctx := context.Background()

	t.Run("replay with a cancelled context", func(t *testing.T) {
		s := newCancelStore(t, false)
		tx := beginCancelTx(t, s, &forwardingProposer{store: s})
		defer tx.Rollback()
		putVisible(t, tx, "k", "committed")
		sequence, err := tx.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, committed := s.Committed(tx.ID); !committed {
			t.Fatal("committed transaction has no publication marker")
		}
		head := mustHead(t, s)

		// A client whose wait was interrupted retries the same commit command. The
		// durable marker must decide the outcome, not the context.
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		replayed, err := s.Propose(cancelled, Command{Kind: "commit", ID: tx.ID, Snapshot: tx.Snapshot})
		if err != nil {
			t.Fatalf("replay error = %v, want the committed outcome", err)
		}
		if replayed.Sequence != sequence {
			t.Fatalf("replay sequence = %d, want %d", replayed.Sequence, sequence)
		}
		if after := mustHead(t, s); after != head {
			t.Fatalf("replay moved head %d -> %d", head, after)
		}
		requireCancelValue(t, s, "k", "committed")
	})

	t.Run("cancelled between publication and result inspection", func(t *testing.T) {
		s := newCancelStore(t, false)
		commitCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		proposer := &publicationCancellingProposer{store: s, cancel: cancel}
		tx := beginCancelTx(t, s, proposer)
		defer tx.Rollback()
		putVisible(t, tx, "k", "committed")

		sequence, err := tx.Commit(commitCtx)
		if err != nil {
			t.Fatalf("commit reported %v after a durable publication", err)
		}
		if sequence == 0 {
			t.Fatal("durable commit reported no sequence")
		}
		if !proposer.fired {
			t.Fatal("the test never cancelled after publication; the boundary was not reached")
		}
		if err := commitCtx.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("context was not cancelled at the boundary: %v", err)
		}
		for _, kind := range proposer.kinds {
			if kind == "abort" {
				t.Fatalf("a durable commit proposed an abort: %v", proposer.kinds)
			}
		}
		if _, committed := s.Committed(tx.ID); !committed {
			t.Fatal("committed transaction lost its publication marker")
		}
		requireCancelValue(t, s, "k", "committed")
	})
}

// The replicated client resolves a durable publication before it consults the
// context, which is what makes an interrupted wait safe to accept as committed.
func TestReplicatedClientNeverReportsRollbackForPublishedCommit(t *testing.T) {
	s := newCancelStore(t, false)
	tx := beginCancelTx(t, s, &forwardingProposer{store: s})
	defer tx.Rollback()
	putVisible(t, tx, "k", "committed")
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// A stage replay for a committed transaction is an idempotent no-op, but only a
	// commit command resolves the durable marker ahead of the context: the stage
	// step is an internal proposal that Tx.Commit issues once, never a retried one.
	if result, err := s.Propose(context.Background(), Command{Kind: "stage", ID: tx.ID}); err != nil || result.Err() != nil {
		t.Fatalf("stage replay = %+v, %v", result, err)
	}
	// An unknown transaction still honours the cancellation: only a durable
	// publication outranks it.
	if _, err := s.Propose(cancelled, Command{Kind: "commit", ID: randomID(), Snapshot: tx.Snapshot}); !errors.Is(err, context.Canceled) {
		t.Fatalf("unknown commit error = %v, want context.Canceled", err)
	}
	// And so does a transaction that only staged, without publishing.
	staged := randomID()
	if _, err := s.Propose(context.Background(), Command{Kind: "stage", ID: staged, Ops: []Op{{Space: visibilitySpace, Key: []byte("k2"), Value: []byte("v")}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(cancelled, Command{Kind: "commit", ID: staged, Snapshot: tx.Snapshot}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled staged commit error = %v, want context.Canceled", err)
	}
	if _, committed := s.Committed(staged); committed {
		t.Fatal("a cancelled staged commit published a marker")
	}
}

// Group commit cancellation is per request: a cancelled member is skipped and the
// others still publish.
func TestCancelWithinCommitGroupIsPerRequest(t *testing.T) {
	for _, localWAL := range []bool{false, true} {
		name := "group"
		if localWAL {
			name = "wal-group"
		}
		t.Run(name, func(t *testing.T) {
			s := newCancelStore(t, localWAL)
			head := mustHead(t, s)
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()

			ids := []string{randomID(), randomID(), randomID()}
			contexts := []context.Context{context.Background(), cancelled, context.Background()}
			requests := make([]*localCommitRequest, len(ids))
			for i := range ids {
				requests[i] = &localCommitRequest{
					ctx:        contexts[i],
					generation: s.generation.Load(),
					snapshot:   head,
					id:         ids[i],
					ops:        captureConflictOps(t, s, putConflict(fmt.Sprintf("k%d", i), "value")),
					done:       make(chan struct{}),
				}
			}
			s.commitLocalGroup(requests)

			if requests[0].err != nil || requests[2].err != nil {
				t.Fatalf("uncancelled members failed: %v %v", requests[0].err, requests[2].err)
			}
			if !errors.Is(requests[1].err, context.Canceled) {
				t.Fatalf("cancelled member error = %v, want context.Canceled", requests[1].err)
			}
			for i, id := range ids {
				_, committed := s.Committed(id)
				if want := i != 1; committed != want {
					t.Fatalf("member %d committed = %v, want %v", i, committed, want)
				}
			}
			if after := mustHead(t, s); after != head+2 {
				t.Fatalf("head = %d, want %d", after, head+2)
			}
			requireCancelValue(t, s, "k0", "value")
			requireCancelValue(t, s, "k2", "value")
			requireInvisible(t, s, "k1")
		})
	}
}

func TestCommittedIsThePublicationAuthority(t *testing.T) {
	s := newCancelStore(t, false)
	if _, ok := s.Committed(""); ok {
		t.Fatal("an empty transaction ID resolved a marker")
	}
	if _, ok := s.Committed(randomID()); ok {
		t.Fatal("an unknown transaction ID resolved a marker")
	}

	tx := beginCancelTx(t, s, nil)
	defer tx.Rollback()
	putVisible(t, tx, "k", "value")
	if _, ok := s.Committed(tx.ID); ok {
		t.Fatal("an uncommitted transaction resolved a marker")
	}
	sequence, err := tx.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	published, ok := s.Committed(tx.ID)
	if !ok || published != sequence {
		t.Fatalf("Committed = (%d, %v), want (%d, true)", published, ok, sequence)
	}

	// An aborted transaction never gets a marker.
	aborted := beginCancelTx(t, s, nil)
	putVisible(t, aborted, "k2", "value")
	if err := aborted.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Committed(aborted.ID); ok {
		t.Fatal("a rolled back transaction resolved a marker")
	}
}

// forwardingProposer sends commands straight to the store while remaining a
// different proposer value, which is what selects the staged commit path.
type forwardingProposer struct {
	store *Store
	kinds []string
}

func (p *forwardingProposer) Barrier(ctx context.Context) error { return p.store.Barrier(ctx) }

func (p *forwardingProposer) Propose(ctx context.Context, command Command) (Result, error) {
	p.kinds = append(p.kinds, command.Kind)
	return p.store.Propose(ctx, command)
}

// publicationCancellingProposer cancels the caller's context immediately after the
// store published a commit command: the exact boundary where the transaction is
// durable but Commit has not inspected the result yet.
type publicationCancellingProposer struct {
	store  *Store
	cancel context.CancelFunc
	kinds  []string
	fired  bool
}

func (p *publicationCancellingProposer) Barrier(ctx context.Context) error {
	return p.store.Barrier(ctx)
}

func (p *publicationCancellingProposer) Propose(ctx context.Context, command Command) (Result, error) {
	p.kinds = append(p.kinds, command.Kind)
	result, err := p.store.Propose(ctx, command)
	if command.Kind == "commit" && err == nil && result.Err() == nil {
		p.fired = true
		p.cancel()
	}
	return result, err
}
