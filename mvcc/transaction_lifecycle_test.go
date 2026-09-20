package mvcc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// Transaction lifecycle wiring: the semantic outcome (state, commit sequence, abort
// category) and the resource cleanup are separate steps, and only the outcome
// decides the terminal state. A successful commit must therefore survive every
// later cleanup, which is what the previous implementation could not guarantee: it
// ended every commit through Rollback.

func TestLifecycleRootWriteCommitPassesThroughCommitting(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "existing", "value")
	mustCommit(t, seed)

	// The gate parks the commit proposal, which is the exact boundary after the
	// transaction entered COMMITTING and before anything was published.
	gate := newPublicationGate(s, "commit")
	t.Cleanup(gate.releaseNow)
	tx, err := s.Begin(context.Background(), gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	if state := tx.State(); state != TransactionActive {
		t.Fatalf("new transaction state = %s, want ACTIVE", state)
	}
	putVisible(t, tx, "k", "value")

	done := make(chan commitOutcome, 1)
	go func() {
		sequence, commitErr := tx.Commit(context.Background())
		done <- commitOutcome{sequence: sequence, err: commitErr}
	}()
	awaitSignal(t, gate.parked, "the commit proposal")

	// The durable commit is in flight: COMMITTING, and no commit sequence yet. A
	// sequence set here would be a commit sequence for a commit that never happened.
	if state := tx.State(); state != TransactionCommitting {
		t.Fatalf("in-flight state = %s, want COMMITTING", state)
	}
	if info := tx.Info(); info.HasCommitTS || info.CommitTS != 0 {
		t.Fatalf("commit sequence was recorded before publication: %+v", info)
	}
	if registered, ok := s.txns.Info(tx.ID); !ok || registered.State != TransactionCommitting {
		t.Fatalf("registry state = %+v, want COMMITTING", registered)
	}
	if _, committed := s.Committed(tx.ID); committed {
		t.Fatal("a publication marker exists before publication")
	}

	gate.releaseNow()
	outcome := awaitCommit(t, done)
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	info := tx.Info()
	if info.State != TransactionCommitted {
		t.Fatalf("final state = %s, want COMMITTED", info.State)
	}
	if !info.HasCommitTS || info.CommitTS != outcome.sequence {
		t.Fatalf("commit sequence = (%d, %v), want (%d, true)", info.CommitTS, info.HasCommitTS, outcome.sequence)
	}
	if published, ok := s.Committed(tx.ID); !ok || published != outcome.sequence {
		t.Fatalf("publication marker = (%d, %v), want the returned sequence %d", published, ok, outcome.sequence)
	}
	if head := mustHead(t, s); head != outcome.sequence {
		t.Fatalf("head = %d, want %d", head, outcome.sequence)
	}
	requireCancelValue(t, s, "k", "value")
	// The registry entry is gone once the state is terminal, so no unbounded history
	// is retained.
	if _, ok := s.txns.Info(tx.ID); ok {
		t.Fatal("a committed transaction is still registered")
	}
}

// The local (non-proposer) path must produce the same outcome shape.
func TestLifecycleLocalCommitRecordsThePublishedSequence(t *testing.T) {
	for _, localWAL := range []bool{false, true} {
		name := "bounded"
		if localWAL {
			name = "local-wal"
		}
		t.Run(name, func(t *testing.T) {
			s := newCancelStore(t, localWAL)
			head := mustHead(t, s)
			tx := beginBaseline(t, s)
			putVisible(t, tx, "k", "value")
			sequence, err := tx.Commit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if sequence <= head {
				t.Fatalf("sequence = %d, want above head %d", sequence, head)
			}
			info := tx.Info()
			if info.State != TransactionCommitted || !info.HasCommitTS || info.CommitTS != sequence {
				t.Fatalf("lifecycle = %+v, want COMMITTED with commit sequence %d", info, sequence)
			}
			if info.AbortReason != "" {
				t.Fatalf("committed transaction carries abort reason %q", info.AbortReason)
			}
			if published, ok := s.Committed(tx.ID); !ok || published != sequence {
				t.Fatalf("publication marker = (%d, %v), want %d", published, ok, sequence)
			}
		})
	}
}

func TestLifecycleReadOnlyAndEmptyCommitDoNotPublish(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "k", "value")
	mustCommit(t, seed)
	head := mustHead(t, s)
	sequence := s.localSeq

	reader := beginBaseline(t, s)
	if value := conflictRead(t, reader, "k"); value != "value" {
		t.Fatalf("read = %q", value)
	}
	if _, err := reader.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	info := reader.Info()
	if info.State != TransactionCommitted {
		t.Fatalf("read-only state = %s, want COMMITTED", info.State)
	}
	if info.HasCommitTS || info.CommitTS != 0 {
		t.Fatalf("read-only commit recorded a commit sequence: %+v", info)
	}
	if info.PointReads != 1 {
		t.Fatalf("read observation lost: %+v", info)
	}
	if _, committed := s.Committed(reader.ID); committed {
		t.Fatal("read-only commit published a marker")
	}
	if after := mustHead(t, s); after != head {
		t.Fatalf("head moved %d -> %d", head, after)
	}
	if s.localSeq != sequence {
		t.Fatalf("read-only commit allocated a sequence: %d -> %d", sequence, s.localSeq)
	}

	// An empty transaction has the same outcome, and its returned value stays the
	// read timestamp exactly as before.
	empty := beginBaseline(t, s)
	returned, err := empty.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if returned != empty.Snapshot {
		t.Fatalf("empty commit returned %d, want the read timestamp %d", returned, empty.Snapshot)
	}
	if info := empty.Info(); info.State != TransactionCommitted || info.HasCommitTS {
		t.Fatalf("empty commit lifecycle = %+v", info)
	}
	if after := mustHead(t, s); after != head {
		t.Fatalf("empty commit moved head %d -> %d", head, after)
	}
	if s.localSeq != sequence {
		t.Fatalf("empty commit allocated a sequence: %d -> %d", sequence, s.localSeq)
	}
}

func TestLifecycleChildCommitIsMergedNotCommitted(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)
	head := mustHead(t, s)

	parent := beginBaseline(t, s)
	putVisible(t, parent, "p", "parent")
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	putVisible(t, child, "c", "child")

	if _, err := child.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// A child commit is not a database commit: MERGED, no commit sequence, no
	// publication marker, and the head is untouched.
	info := child.Info()
	if info.State != TransactionMerged {
		t.Fatalf("child state = %s, want MERGED", info.State)
	}
	if info.HasCommitTS || info.CommitTS != 0 {
		t.Fatalf("child recorded a commit sequence: %+v", info)
	}
	if info.ParentID != parent.ID {
		t.Fatalf("child parent = %q, want %q", info.ParentID, parent.ID)
	}
	if _, committed := s.Committed(child.ID); committed {
		t.Fatal("a child merge published a marker")
	}
	if after := mustHead(t, s); after != head {
		t.Fatalf("child merge moved head %d -> %d", head, after)
	}
	// The writes and their diagnostic accounting are now the parent's.
	if value := conflictRead(t, parent, "c"); value != "child" {
		t.Fatalf("parent cannot see the merged write: %q", value)
	}
	if value := conflictRead(t, parent, "p"); value != "parent" {
		t.Fatalf("parent lost its own write: %q", value)
	}
	if parentInfo := parent.Info(); parentInfo.Writes != 2 {
		t.Fatalf("merged write set = %+v, want 2 writes", parentInfo)
	}
}

func TestLifecycleChildMergeThenParentRollbackDiscardsEverything(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)

	parent := beginBaseline(t, s)
	putVisible(t, parent, "p", "parent")
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	putVisible(t, child, "c", "child")
	if _, err := child.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := parent.Rollback(); err != nil {
		t.Fatal(err)
	}
	if state := parent.State(); state != TransactionAborted {
		t.Fatalf("parent state = %s, want ABORTED", state)
	}
	if info := parent.Info(); info.HasCommitTS {
		t.Fatalf("aborted parent recorded a commit sequence: %+v", info)
	}
	requireInvisible(t, s, "p")
	requireInvisible(t, s, "c")
	if _, committed := s.Committed(parent.ID); committed {
		t.Fatal("an aborted parent has a publication marker")
	}
}

// A failed merge must not report MERGED: the child's writes were discarded.
func TestLifecycleFailedMergeEndsAborted(t *testing.T) {
	s := baselineStore(t)
	parent := beginBaseline(t, s)
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	putVisible(t, child, "c", "child")
	// The parent's stage is gone, so the merge cannot complete.
	if err := parent.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Commit(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("child commit error = %v, want ErrClosed", err)
	}
	if state := child.State(); state == TransactionMerged {
		t.Fatal("a failed merge reported MERGED")
	}
	if state := child.State(); state != TransactionAborted {
		t.Fatalf("child state = %s, want ABORTED", state)
	}
	requireInvisible(t, s, "c")
}

func TestLifecycleRollbackRecordsAborted(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	putVisible(t, tx, "k", "value")
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	info := tx.Info()
	if info.State != TransactionAborted {
		t.Fatalf("state = %s, want ABORTED", info.State)
	}
	if info.HasCommitTS || info.CommitTS != 0 {
		t.Fatalf("aborted transaction recorded a commit sequence: %+v", info)
	}
	requireInvisible(t, s, "k")

	// A rollback before any write is the same outcome, and a repeated rollback is
	// idempotent: it cannot move the state anywhere.
	empty := beginBaseline(t, s)
	if err := empty.Rollback(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := empty.Rollback(); err != nil {
			t.Fatalf("repeated rollback %d: %v", i, err)
		}
	}
	if info := empty.Info(); info.State != TransactionAborted {
		t.Fatalf("repeated rollback changed the state to %s", info.State)
	}
}

// A conflict fails the commit after the transaction already entered COMMITTING, and
// the aborted outcome must carry a short category rather than a payload.
func TestLifecycleFailedCommitHasNoCommitSequence(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "secret-key", "value")
	mustCommit(t, seed)
	head := mustHead(t, s)

	// Both writers must share one snapshot, so the loser's write really conflicts.
	loser, err := s.Begin(ctx, newPublicationGate(s, "never"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loser.Rollback() })
	putVisible(t, loser, "secret-key", "loser")

	winner := beginBaseline(t, s)
	putVisible(t, winner, "secret-key", "winner")
	mustCommit(t, winner)

	if _, err := loser.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicted commit error = %v, want ErrConflict", err)
	}
	info := loser.Info()
	if info.State != TransactionAborted {
		t.Fatalf("state = %s, want ABORTED", info.State)
	}
	if info.HasCommitTS || info.CommitTS != 0 {
		t.Fatalf("failed commit recorded a commit sequence: %+v", info)
	}
	if info.AbortReason != "conflict" {
		t.Fatalf("abort reason = %q, want the conflict category", info.AbortReason)
	}
	if strings.Contains(info.AbortReason, "secret") || strings.Contains(info.AbortReason, "value") {
		t.Fatalf("abort reason leaked transaction payload: %q", info.AbortReason)
	}
	if _, committed := s.Committed(loser.ID); committed {
		t.Fatal("a failed commit published a marker")
	}
	if after := mustHead(t, s); after <= head {
		t.Fatalf("head = %d, want the winner's commit above %d", after, head)
	}
}

// COMMITTING -> ABORTED, observed deterministically while the commit is parked.
func TestLifecycleCommittingToAbortedOnConflict(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "k", "seed")
	mustCommit(t, seed)

	// Both writers share one snapshot, so the parked commit is guaranteed to
	// conflict once it is released.
	gate := newPublicationGate(s, "commit")
	t.Cleanup(gate.releaseNow)
	loser, err := s.Begin(ctx, gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { loser.Rollback() })
	putVisible(t, loser, "k", "loser")

	winner := beginBaseline(t, s)
	putVisible(t, winner, "k", "winner")
	mustCommit(t, winner)

	done := make(chan commitOutcome, 1)
	go func() {
		sequence, commitErr := loser.Commit(ctx)
		done <- commitOutcome{sequence: sequence, err: commitErr}
	}()
	awaitSignal(t, gate.parked, "the commit proposal")
	if state := loser.State(); state != TransactionCommitting {
		t.Fatalf("in-flight state = %s, want COMMITTING", state)
	}
	if info := loser.Info(); info.HasCommitTS {
		t.Fatal("a commit sequence was recorded before the conflict was decided")
	}

	gate.releaseNow()
	outcome := awaitCommit(t, done)
	if !errors.Is(outcome.err, ErrConflict) {
		t.Fatalf("commit error = %v, want ErrConflict", outcome.err)
	}
	info := loser.Info()
	if info.State != TransactionAborted || info.AbortReason != "conflict" {
		t.Fatalf("terminal lifecycle = %+v, want ABORTED/conflict", info)
	}
	if info.HasCommitTS {
		t.Fatalf("conflicted commit kept a commit sequence: %+v", info)
	}
	requireCancelValue(t, s, "k", "winner")
}

// The regression this wiring exists for: cleanup must never rewrite a decided
// terminal state.
func TestLifecycleCleanupNeverRewritesATerminalState(t *testing.T) {
	ctx := context.Background()

	t.Run("committed root survives repeated rollback", func(t *testing.T) {
		s := baselineStore(t)
		tx := beginBaseline(t, s)
		putVisible(t, tx, "k", "value")
		sequence := mustCommit(t, tx)
		for i := 0; i < 3; i++ {
			if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback %d after commit: %v", i, err)
			}
		}
		info := tx.Info()
		if info.State != TransactionCommitted {
			t.Fatalf("state = %s after cleanup, want COMMITTED", info.State)
		}
		if !info.HasCommitTS || info.CommitTS != sequence {
			t.Fatalf("cleanup lost the commit sequence: %+v", info)
		}
		if info.AbortReason != "" {
			t.Fatalf("committed transaction carries abort reason %q", info.AbortReason)
		}
		if _, committed := s.Committed(tx.ID); !committed {
			t.Fatal("cleanup removed the publication marker")
		}
		requireCancelValue(t, s, "k", "value")
	})

	t.Run("read-only commit survives repeated rollback", func(t *testing.T) {
		s := baselineStore(t)
		tx := beginBaseline(t, s)
		if _, err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if info := tx.Info(); info.State != TransactionCommitted || info.HasCommitTS {
			t.Fatalf("read-only lifecycle after cleanup = %+v", info)
		}
	})

	t.Run("merged child survives repeated rollback", func(t *testing.T) {
		s := baselineStore(t)
		parent := beginBaseline(t, s)
		child, err := parent.Child()
		if err != nil {
			t.Fatal(err)
		}
		putVisible(t, child, "c", "child")
		if _, err := child.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if err := child.Rollback(); err != nil {
				t.Fatalf("rollback %d after merge: %v", i, err)
			}
		}
		info := child.Info()
		if info.State != TransactionMerged {
			t.Fatalf("merged child became %s after cleanup", info.State)
		}
		if info.HasCommitTS {
			t.Fatalf("merged child recorded a commit sequence: %+v", info)
		}
		if value := conflictRead(t, parent, "c"); value != "child" {
			t.Fatalf("cleanup discarded the merged write: %q", value)
		}
	})

	// A generation change must not let a later cleanup rewrite a decided outcome: the
	// transaction is stale, so only its own snapshot can be updated.
	t.Run("stale transaction cannot rewrite its committed outcome", func(t *testing.T) {
		s := baselineStore(t)
		tx := beginBaseline(t, s)
		putVisible(t, tx, "k", "value")
		sequence := mustCommit(t, tx)
		image := captureStoreImage(t, s)
		if err := s.Restore(bytes.NewReader(image)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		info := tx.Info()
		if info.State != TransactionCommitted || !info.HasCommitTS || info.CommitTS != sequence {
			t.Fatalf("a stale cleanup rewrote the outcome: %+v", info)
		}
	})
}

func TestLifecycleDoubleCommitIsRejected(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	putVisible(t, tx, "k", "value")
	sequence := mustCommit(t, tx)
	head := mustHead(t, s)

	if _, err := tx.Commit(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("second commit error = %v, want ErrClosed", err)
	}
	info := tx.Info()
	if info.State != TransactionCommitted || !info.HasCommitTS || info.CommitTS != sequence {
		t.Fatalf("second commit changed the lifecycle: %+v", info)
	}
	if after := mustHead(t, s); after != head {
		t.Fatalf("second commit moved head %d -> %d", head, after)
	}
	// A rolled back transaction is closed too.
	aborted := beginBaseline(t, s)
	if err := aborted.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := aborted.Commit(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("commit after rollback error = %v, want ErrClosed", err)
	}
	if state := aborted.State(); state != TransactionAborted {
		t.Fatalf("commit after rollback changed the state to %s", state)
	}
}

// closed and the lifecycle state are written by the same finish step, so they can
// never disagree.
func TestLifecycleClosedMatchesTerminalState(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)

	committed := beginBaseline(t, s)
	putVisible(t, committed, "k", "value")
	mustCommit(t, committed)
	aborted := beginBaseline(t, s)
	if err := aborted.Rollback(); err != nil {
		t.Fatal(err)
	}
	parent := beginBaseline(t, s)
	merged, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := merged.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	active := beginBaseline(t, s)

	// A terminal state must be closed, and a live one must not: both are written by
	// the same transition step.
	for _, probe := range []struct {
		name string
		tx   *Tx
		want TransactionState
	}{
		{"committed", committed, TransactionCommitted},
		{"aborted", aborted, TransactionAborted},
		{"merged", merged, TransactionMerged},
		{"active", active, TransactionActive},
	} {
		tx := probe.tx
		if state := tx.State(); state != probe.want {
			t.Fatalf("%s state = %s, want %s", probe.name, state, probe.want)
		}
		if terminal := tx.State().Terminal(); terminal != tx.closed {
			t.Fatalf("%s: state %s terminal=%v disagrees with closed=%v", probe.name, tx.State(), terminal, tx.closed)
		}
		info := tx.Info()
		if info.State != probe.want {
			t.Fatalf("%s: snapshot state = %s", probe.name, info.State)
		}
	}
}

// A durable publication decides the outcome even when the caller's context is
// cancelled at that exact boundary: the transaction ends COMMITTED.
func TestLifecycleCancellationAfterPublicationEndsCommitted(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)
	commitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	proposer := &publicationCancellingProposer{store: s, cancel: cancel}
	tx, err := s.Begin(ctx, proposer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	putVisible(t, tx, "k", "value")

	sequence, err := tx.Commit(commitCtx)
	if err != nil {
		t.Fatalf("commit reported %v after a durable publication", err)
	}
	if !proposer.fired {
		t.Fatal("the test never cancelled after publication")
	}
	info := tx.Info()
	if info.State != TransactionCommitted {
		t.Fatalf("state = %s after a durable publication, want COMMITTED", info.State)
	}
	if !info.HasCommitTS || info.CommitTS != sequence {
		t.Fatalf("commit sequence = (%d, %v), want (%d, true)", info.CommitTS, info.HasCommitTS, sequence)
	}
	requireCancelValue(t, s, "k", "value")
}

// The lifecycle outcomes feed the manager statistics that survive the registry entry.
func TestLifecycleOutcomesReachManagerStatistics(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)
	before := s.txns.Stats()

	committed := beginBaseline(t, s)
	putVisible(t, committed, "a", "value")
	mustCommit(t, committed)
	if stats := s.txns.Stats(); stats.Committed != before.Committed+1 {
		t.Fatalf("Committed = %d, want %d", stats.Committed, before.Committed+1)
	}

	aborted := beginBaseline(t, s)
	if err := aborted.Rollback(); err != nil {
		t.Fatal(err)
	}
	afterAbort := s.txns.Stats()
	if afterAbort.Aborted != before.Aborted+1 {
		t.Fatalf("Aborted = %d, want %d", afterAbort.Aborted, before.Aborted+1)
	}

	// A merge is neither a commit nor an abort: it must not move either counter.
	parent := beginBaseline(t, s)
	merged, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := merged.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := s.txns.Stats(); stats.Committed != afterAbort.Committed || stats.Aborted != afterAbort.Aborted {
		t.Fatalf("a merge moved the terminal counters: %+v -> %+v", afterAbort, stats)
	}

	if err := parent.Rollback(); err != nil {
		t.Fatal(err)
	}
	stats := s.txns.Stats()
	if stats.Committed != before.Committed+1 {
		t.Fatalf("Committed = %d, want %d", stats.Committed, before.Committed+1)
	}
	if stats.Aborted != before.Aborted+2 {
		t.Fatalf("Aborted = %d, want %d", stats.Aborted, before.Aborted+2)
	}
	if stats.ActiveRoot != 0 || stats.ActiveChildren != 0 || stats.HasOldestReadTS {
		t.Fatalf("registry leaked after terminal outcomes: %+v", stats)
	}
}

// The diagnostics accessors are read while the owning goroutine commits. The
// lifecycle fields are published under their own lock, so a concurrent snapshot sees
// a coherent move; without a race detector this test asserts the observed states are
// legal, and under -race in CI it checks the synchronization itself.
func TestLifecycleDiagnosticsSnapshotUnderConcurrentCommit(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "k", "seed")
	mustCommit(t, seed)

	tx := beginBaseline(t, s)
	putVisible(t, tx, "k", "value")

	start := make(chan struct{})
	done := make(chan commitOutcome, 1)
	go func() {
		<-start
		sequence, err := tx.Commit(context.Background())
		done <- commitOutcome{sequence: sequence, err: err}
	}()

	stop := make(chan struct{})
	rank := map[TransactionState]int{
		TransactionActive:     0,
		TransactionCommitting: 1,
		TransactionCommitted:  2,
		TransactionAborted:    3,
	}
	var observer sync.WaitGroup
	observer.Add(1)
	go func() {
		defer observer.Done()
		seen := -1
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, ok := rank[tx.State()]; !ok {
				t.Errorf("concurrent State() reported an illegal state %s", tx.State())
				return
			}
			// Each accessor takes its own snapshot, so reads from one iteration may
			// straddle the transition. What must hold is that the state never moves
			// backwards, and that a commit sequence is never visible before the move
			// to COMMITTED, since both fields are published under one lock.
			info := tx.Info()
			current, ok := rank[info.State]
			if !ok {
				t.Errorf("concurrent snapshot reported an illegal state %s", info.State)
				return
			}
			if current < seen {
				t.Errorf("concurrent snapshot moved backwards from rank %d to %s", seen, info.State)
				return
			}
			seen = current
			if info.State != TransactionCommitted && info.HasCommitTS {
				t.Errorf("commit sequence visible before the commit finished: %+v", info)
				return
			}
		}
	}()

	close(start)
	outcome := awaitCommit(t, done)
	close(stop)
	observer.Wait()
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	info := tx.Info()
	if info.State != TransactionCommitted || !info.HasCommitTS || info.CommitTS != outcome.sequence {
		t.Fatalf("final lifecycle = %+v, want COMMITTED with %d", info, outcome.sequence)
	}
}
