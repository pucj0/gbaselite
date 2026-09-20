package mvcc

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

var testStartedAt = time.Unix(1_700_000_000, 0)

func registerTestRoot(t *testing.T, m *TransactionManager, id string, readTS uint64) {
	t.Helper()
	if _, err := m.RegisterRoot(RootRegistration{ID: id, ReadTS: readTS, Generation: 1, StartedAt: testStartedAt}); err != nil {
		t.Fatalf("RegisterRoot(%s): %v", id, err)
	}
}

func registerTestChild(t *testing.T, m *TransactionManager, id, parentID string) {
	t.Helper()
	if _, err := m.RegisterChild(ChildRegistration{ID: id, ParentID: parentID, Generation: 1, StartedAt: testStartedAt}); err != nil {
		t.Fatalf("RegisterChild(%s of %s): %v", id, parentID, err)
	}
}

// managerRefCount reads the internal retention counter directly so a test can
// assert exact reference accounting rather than only the derived horizon. Same
// package, test-only, read under the manager lock.
func managerRefCount(t *testing.T, m *TransactionManager, readTS uint64) uint64 {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshotRefs[readTS]
}

func requireState(t *testing.T, m *TransactionManager, id string, want TransactionState) TransactionInfo {
	t.Helper()
	info, ok := m.Info(id)
	if !ok {
		t.Fatalf("transaction %s is not registered", id)
	}
	if info.State != want {
		t.Fatalf("transaction %s state = %s, want %s", id, info.State, want)
	}
	return info
}

func requireOldestReadTS(t *testing.T, m *TransactionManager, want uint64, wantOK bool) {
	t.Helper()
	oldest, ok := m.OldestReadTS()
	if ok != wantOK || ok && oldest != want {
		t.Fatalf("OldestReadTS() = (%d, %v), want (%d, %v)", oldest, ok, want, wantOK)
	}
}

func TestTransactionManagerRegistersRootAndChild(t *testing.T) {
	m := NewTransactionManager()

	registerTestRoot(t, m, "root", 7)
	root := requireState(t, m, "root", TransactionActive)
	if root.ParentID != "" {
		t.Fatalf("root parent = %q, want empty", root.ParentID)
	}
	if root.StartTS != 7 || root.ReadTS != 7 {
		t.Fatalf("root timestamps = (%d, %d), want (7, 7)", root.StartTS, root.ReadTS)
	}
	if root.HasCommitTS || root.CommitTS != 0 {
		t.Fatalf("new root reports a commit timestamp: %+v", root)
	}
	if root.Generation != 1 || !root.StartedAt.Equal(testStartedAt) {
		t.Fatalf("root diagnostics metadata lost: %+v", root)
	}

	registerTestChild(t, m, "child", "root")
	child := requireState(t, m, "child", TransactionActive)
	if child.ParentID != "root" {
		t.Fatalf("child parent = %q", child.ParentID)
	}
	if child.ReadTS != 7 || child.StartTS != 7 {
		t.Fatalf("child did not inherit the parent read timestamp: %+v", child)
	}

	registerTestChild(t, m, "grandchild", "child")
	grandchild := requireState(t, m, "grandchild", TransactionActive)
	if grandchild.ParentID != "child" || grandchild.ReadTS != 7 {
		t.Fatalf("nested child = %+v", grandchild)
	}

	if infos := m.ActiveTransactions(); len(infos) != 3 {
		t.Fatalf("registry holds %d transactions, want 3", len(infos))
	}
	stats := m.Stats()
	if stats.ActiveRoot != 1 || stats.ActiveChildren != 2 {
		t.Fatalf("stats = %+v, want 1 root and 2 children", stats)
	}
	// Only the root pins retention.
	if refs := managerRefCount(t, m, 7); refs != 1 {
		t.Fatalf("snapshotRefs[7] = %d, want 1", refs)
	}
	requireOldestReadTS(t, m, 7, true)
}

func TestTransactionManagerUnregisterIsIdempotent(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "root", 7)
	registerTestChild(t, m, "child", "root")

	m.Unregister("child")
	if _, ok := m.Info("child"); ok {
		t.Fatal("child survived unregister")
	}
	if refs := managerRefCount(t, m, 7); refs != 1 {
		t.Fatalf("child unregister changed the root pin: %d", refs)
	}
	for i := 0; i < 3; i++ {
		m.Unregister("child")
	}
	if refs := managerRefCount(t, m, 7); refs != 1 {
		t.Fatalf("repeated child unregister changed the root pin: %d", refs)
	}

	m.Unregister("root")
	if refs := managerRefCount(t, m, 7); refs != 0 {
		t.Fatalf("snapshotRefs[7] = %d after root unregister, want 0", refs)
	}
	requireOldestReadTS(t, m, 0, false)
	for i := 0; i < 3; i++ {
		m.Unregister("root")
		m.Unregister("never-registered")
	}
	if refs := managerRefCount(t, m, 7); refs != 0 {
		t.Fatalf("repeated unregister underflowed the reference: %d", refs)
	}
	if infos := m.ActiveTransactions(); len(infos) != 0 {
		t.Fatalf("registry leaked %d entries", len(infos))
	}
	stats := m.Stats()
	if stats.ActiveRoot != 0 || stats.ActiveChildren != 0 || stats.HasOldestReadTS {
		t.Fatalf("stats after cleanup = %+v", stats)
	}
}

func TestTransactionManagerDuplicateIDNeverReplaces(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "tx", 5)

	// Same ID, different metadata: the registered transaction must survive.
	_, err := m.RegisterRoot(RootRegistration{ID: "tx", ReadTS: 99, Generation: 2, StartedAt: testStartedAt.Add(time.Hour)})
	if !errors.Is(err, ErrTransactionIDCollision) {
		t.Fatalf("duplicate root error = %v, want ErrTransactionIDCollision", err)
	}
	original := requireState(t, m, "tx", TransactionActive)
	if original.ReadTS != 5 || original.Generation != 1 {
		t.Fatalf("duplicate registration replaced the transaction: %+v", original)
	}
	if refs := managerRefCount(t, m, 5); refs != 1 {
		t.Fatalf("snapshotRefs[5] = %d, want 1", refs)
	}
	if refs := managerRefCount(t, m, 99); refs != 0 {
		t.Fatalf("rejected registration pinned %d references", refs)
	}

	// The same ID cannot be reused as a child either.
	if _, err := m.RegisterChild(ChildRegistration{ID: "tx", ParentID: "tx", Generation: 1}); !errors.Is(err, ErrTransactionIDCollision) {
		t.Fatalf("duplicate child error = %v, want ErrTransactionIDCollision", err)
	}
	registerTestChild(t, m, "child", "tx")
	if _, err := m.RegisterRoot(RootRegistration{ID: "child", ReadTS: 6, Generation: 1}); !errors.Is(err, ErrTransactionIDCollision) {
		t.Fatalf("child ID reused as root: %v", err)
	}
	if _, err := m.RegisterChild(ChildRegistration{ID: "child", ParentID: "tx", Generation: 1}); !errors.Is(err, ErrTransactionIDCollision) {
		t.Fatalf("child ID registered twice: %v", err)
	}
	if refs := managerRefCount(t, m, 6); refs != 0 {
		t.Fatalf("rejected registrations pinned %d references", refs)
	}

	// An ID becomes reusable only after it is unregistered.
	m.Unregister("tx")
	registerTestRoot(t, m, "tx", 5)
	if refs := managerRefCount(t, m, 5); refs != 1 {
		t.Fatalf("re-registered root pinned %d references, want 1", refs)
	}
}

func TestTransactionManagerOldestReadTS(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "late", 40)
	registerTestRoot(t, m, "early", 10)
	registerTestRoot(t, m, "middle", 25)
	requireOldestReadTS(t, m, 10, true)

	// Child registrations never move the horizon.
	registerTestChild(t, m, "child", "late")
	requireOldestReadTS(t, m, 10, true)

	m.Unregister("early")
	requireOldestReadTS(t, m, 25, true)
	m.Unregister("middle")
	requireOldestReadTS(t, m, 40, true)
	m.Unregister("late")
	requireOldestReadTS(t, m, 0, false)

	// ReadTS=0 is a legal timestamp, never an unset sentinel.
	registerTestRoot(t, m, "zero", 0)
	oldest, ok := m.OldestReadTS()
	if !ok || oldest != 0 {
		t.Fatalf("OldestReadTS() = (%d, %v), want (0, true)", oldest, ok)
	}
	registerTestRoot(t, m, "positive", 3)
	requireOldestReadTS(t, m, 0, true)
	m.Unregister("zero")
	requireOldestReadTS(t, m, 3, true)
}

func TestTransactionManagerChildrenNeverPinSnapshot(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "root-a", 12)
	// The savepoint ceiling is 32 layers; every layer must stay unpinned.
	for i := 0; i < 32; i++ {
		registerTestChild(t, m, fmt.Sprintf("sp-%02d", i), "root-a")
	}
	registerTestChild(t, m, "nested", "sp-00")
	if refs := managerRefCount(t, m, 12); refs != 1 {
		t.Fatalf("one root plus 33 children pinned %d references, want 1", refs)
	}

	// A second root sharing the same read timestamp pins exactly once more.
	registerTestRoot(t, m, "root-b", 12)
	if refs := managerRefCount(t, m, 12); refs != 2 {
		t.Fatalf("two roots at the same read timestamp pinned %d references, want 2", refs)
	}

	for i := 0; i < 32; i++ {
		m.Unregister(fmt.Sprintf("sp-%02d", i))
	}
	m.Unregister("nested")
	if refs := managerRefCount(t, m, 12); refs != 2 {
		t.Fatalf("child cleanup changed root pins: %d", refs)
	}
	m.Unregister("root-b")
	if refs := managerRefCount(t, m, 12); refs != 1 {
		t.Fatalf("snapshotRefs[12] = %d after one root left, want 1", refs)
	}
	requireOldestReadTS(t, m, 12, true)
	m.Unregister("root-a")
	if refs := managerRefCount(t, m, 12); refs != 0 {
		t.Fatalf("snapshotRefs[12] = %d after both roots left, want 0", refs)
	}
	requireOldestReadTS(t, m, 0, false)
}

func TestTransactionManagerUnregisterRootDropsDescendants(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "root", 5)
	registerTestChild(t, m, "child", "root")
	registerTestChild(t, m, "grandchild", "child")
	registerTestRoot(t, m, "other", 9)

	m.Unregister("root")
	if _, ok := m.Info("child"); ok {
		t.Fatal("descendant child leaked after its root was unregistered")
	}
	if _, ok := m.Info("grandchild"); ok {
		t.Fatal("descendant grandchild leaked after its root was unregistered")
	}
	if _, ok := m.Info("other"); !ok {
		t.Fatal("unrelated root was removed")
	}
	if refs := managerRefCount(t, m, 5); refs != 0 {
		t.Fatalf("snapshotRefs[5] = %d, want 0", refs)
	}
	if refs := managerRefCount(t, m, 9); refs != 1 {
		t.Fatalf("unrelated root pin = %d, want 1", refs)
	}
	requireOldestReadTS(t, m, 9, true)
}

// TestTransactionManagerTransitionTable pins the exhaustive table: which edges
// exist and which transaction kind may make them. It is the reference the manager
// enforcement tests below rely on.
func TestTransactionManagerTransitionTable(t *testing.T) {
	legal := map[[2]TransactionState]transitionKind{
		{TransactionActive, TransactionCommitting}:    transitionRootOnly,
		{TransactionActive, TransactionCommitted}:     transitionRootOnly,
		{TransactionActive, TransactionAborted}:       transitionAny,
		{TransactionActive, TransactionMerged}:        transitionChildOnly,
		{TransactionCommitting, TransactionCommitted}: transitionRootOnly,
		{TransactionCommitting, TransactionAborted}:   transitionAny,
		{TransactionCommitted, TransactionCommitted}:  transitionAny,
		{TransactionAborted, TransactionAborted}:      transitionAny,
		{TransactionMerged, TransactionMerged}:        transitionAny,
	}
	all := []TransactionState{
		TransactionUnset, TransactionActive, TransactionCommitting,
		TransactionCommitted, TransactionAborted, TransactionMerged,
	}
	for _, from := range all {
		for _, to := range all {
			want := legal[[2]TransactionState{from, to}] // absent key is transitionIllegal
			if got := transactionTransitionKind(from, to); got != want {
				t.Errorf("transactionTransitionKind(%s, %s) = %d, want %d", from, to, got, want)
			}
			if got := transactionTransitionAllowed(from, to); got != (want != transitionIllegal) {
				t.Errorf("transactionTransitionAllowed(%s, %s) = %v, want %v", from, to, got, want != transitionIllegal)
			}
		}
	}

	for state, want := range map[TransactionState]bool{
		TransactionUnset:      false,
		TransactionActive:     false,
		TransactionCommitting: false,
		TransactionCommitted:  true,
		TransactionAborted:    true,
		TransactionMerged:     true,
	} {
		if got := state.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", state, got, want)
		}
	}
	for state, want := range map[TransactionState]string{
		TransactionUnset:      "UNSET",
		TransactionActive:     "ACTIVE",
		TransactionCommitting: "COMMITTING",
		TransactionCommitted:  "COMMITTED",
		TransactionAborted:    "ABORTED",
		TransactionMerged:     "MERGED",
	} {
		if got := state.String(); got != want {
			t.Errorf("TransactionState(%d).String() = %q, want %q", state, got, want)
		}
	}
}

func TestTransactionManagerLegalMoves(t *testing.T) {
	t.Run("root durable commit", func(t *testing.T) {
		m := NewTransactionManager()
		registerTestRoot(t, m, "root", 100)
		if err := m.Transition("root", TransactionCommitting, TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
		requireState(t, m, "root", TransactionCommitting)
		if err := m.Transition("root", TransactionCommitted, TransitionOptions{CommitTS: 42, HasCommitTS: true}); err != nil {
			t.Fatal(err)
		}
		info := requireState(t, m, "root", TransactionCommitted)
		if !info.HasCommitTS || info.CommitTS != 42 {
			t.Fatalf("durable commit timestamp = (%d, %v), want (42, true)", info.CommitTS, info.HasCommitTS)
		}
	})

	t.Run("root empty or read-only commit skips COMMITTING", func(t *testing.T) {
		m := NewTransactionManager()
		registerTestRoot(t, m, "root", 100)
		if err := m.Transition("root", TransactionCommitted, TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
		info := requireState(t, m, "root", TransactionCommitted)
		if info.HasCommitTS || info.CommitTS != 0 {
			t.Fatalf("read-only commit allocated a version: %+v", info)
		}
	})

	t.Run("root abort from ACTIVE and from COMMITTING", func(t *testing.T) {
		m := NewTransactionManager()
		registerTestRoot(t, m, "early", 1)
		if err := m.Transition("early", TransactionAborted, TransitionOptions{Reason: "canceled"}); err != nil {
			t.Fatal(err)
		}
		if info := requireState(t, m, "early", TransactionAborted); info.AbortReason != "canceled" {
			t.Fatalf("abort reason = %q", info.AbortReason)
		}

		registerTestRoot(t, m, "late", 1)
		if err := m.Transition("late", TransactionCommitting, TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := m.Transition("late", TransactionAborted, TransitionOptions{Reason: "conflict", Conflict: true}); err != nil {
			t.Fatal(err)
		}
		stats := m.Stats()
		if stats.Aborted != 2 || stats.Conflicts != 1 {
			t.Fatalf("stats = %+v, want 2 aborted and 1 conflict", stats)
		}
	})

	t.Run("child merge and child abort", func(t *testing.T) {
		m := NewTransactionManager()
		registerTestRoot(t, m, "root", 4)
		registerTestChild(t, m, "merged", "root")
		if err := m.Transition("merged", TransactionMerged, TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
		info := requireState(t, m, "merged", TransactionMerged)
		if info.HasCommitTS {
			t.Fatal("a merged child reported a durable commit timestamp")
		}
		registerTestChild(t, m, "rolled", "root")
		if err := m.Transition("rolled", TransactionAborted, TransitionOptions{Reason: "statement failed"}); err != nil {
			t.Fatal(err)
		}
		requireState(t, m, "rolled", TransactionAborted)
	})

	t.Run("a repeated commit report is idempotent", func(t *testing.T) {
		m := NewTransactionManager()
		registerTestRoot(t, m, "root", 4)
		if err := m.Transition("root", TransactionCommitted, TransitionOptions{CommitTS: 8, HasCommitTS: true}); err != nil {
			t.Fatal(err)
		}
		// A repeated terminal report must not re-record the outcome.
		if err := m.Transition("root", TransactionCommitted, TransitionOptions{CommitTS: 9, HasCommitTS: true}); err != nil {
			t.Fatal(err)
		}
		info := requireState(t, m, "root", TransactionCommitted)
		if info.CommitTS != 8 {
			t.Fatalf("repeated terminal move rewrote the commit timestamp: %d", info.CommitTS)
		}
		if stats := m.Stats(); stats.Committed != 1 {
			t.Fatalf("repeated terminal move double-counted commits: %+v", stats)
		}
	})

	t.Run("a repeated rollback report is idempotent", func(t *testing.T) {
		m := NewTransactionManager()
		registerTestRoot(t, m, "root", 4)
		registerTestChild(t, m, "child", "root")
		if err := m.Transition("child", TransactionAborted, TransitionOptions{Reason: "first"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Transition("child", TransactionAborted, TransitionOptions{Reason: "second", Conflict: true}); err != nil {
			t.Fatal(err)
		}
		if info := requireState(t, m, "child", TransactionAborted); info.AbortReason != "first" {
			t.Fatalf("repeated abort rewrote the reason: %q", info.AbortReason)
		}
		stats := m.Stats()
		if stats.Aborted != 1 || stats.Conflicts != 0 {
			t.Fatalf("repeated abort double-counted: %+v", stats)
		}
	})
}

func TestTransactionManagerIllegalMoves(t *testing.T) {
	commit := func(t *testing.T, m *TransactionManager, id string) {
		t.Helper()
		if err := m.Transition(id, TransactionCommitted, TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	committing := func(t *testing.T, m *TransactionManager, id string) {
		t.Helper()
		if err := m.Transition(id, TransactionCommitting, TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	merge := func(t *testing.T, m *TransactionManager, id string) {
		t.Helper()
		if err := m.Transition(id, TransactionMerged, TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	abort := func(t *testing.T, m *TransactionManager, id string) {
		t.Helper()
		if err := m.Transition(id, TransactionAborted, TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		root  bool
		setup func(*testing.T, *TransactionManager, string)
		to    TransactionState
	}{
		{"active-to-active", true, nil, TransactionActive},
		{"root-to-merged", true, nil, TransactionMerged},
		{"child-to-committing", false, nil, TransactionCommitting},
		{"child-to-committed", false, nil, TransactionCommitted},
		{"committing-to-committing", true, committing, TransactionCommitting},
		{"committing-to-merged", true, committing, TransactionMerged},
		{"committed-to-active", true, commit, TransactionActive},
		{"committed-to-committing", true, commit, TransactionCommitting},
		{"committed-to-aborted", true, commit, TransactionAborted},
		{"committed-to-merged", true, commit, TransactionMerged},
		{"merged-to-active", false, merge, TransactionActive},
		{"merged-to-committed", false, merge, TransactionCommitted},
		{"aborted-to-active", true, abort, TransactionActive},
		{"aborted-to-committed", true, abort, TransactionCommitted},
		{"aborted-to-merged", false, abort, TransactionMerged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewTransactionManager()
			id := "tx"
			if c.root {
				registerTestRoot(t, m, id, 100)
			} else {
				registerTestRoot(t, m, "parent", 100)
				registerTestChild(t, m, id, "parent")
			}
			if c.setup != nil {
				c.setup(t, m, id)
			}
			if err := m.Transition(id, c.to, TransitionOptions{}); !errors.Is(err, ErrIllegalTransactionMove) {
				t.Fatalf("Transition(%s) error = %v, want ErrIllegalTransactionMove", c.to, err)
			}
		})
	}

	t.Run("unknown transaction", func(t *testing.T) {
		m := NewTransactionManager()
		for _, to := range []TransactionState{TransactionCommitting, TransactionCommitted, TransactionAborted, TransactionMerged} {
			if err := m.Transition("missing", to, TransitionOptions{}); !errors.Is(err, ErrUnknownTransaction) {
				t.Fatalf("Transition(%s) on an unknown ID = %v, want ErrUnknownTransaction", to, err)
			}
		}
	})
}

func TestTransactionManagerTransitionOptionRules(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "root", 2)
	registerTestChild(t, m, "child", "root")

	// A durable commit timestamp only belongs to a root COMMITTED move.
	if err := m.Transition("child", TransactionMerged, TransitionOptions{CommitTS: 5, HasCommitTS: true}); !errors.Is(err, ErrIllegalTransactionMove) {
		t.Fatalf("child merge with commit timestamp = %v", err)
	}
	if err := m.Transition("root", TransactionAborted, TransitionOptions{CommitTS: 5, HasCommitTS: true}); !errors.Is(err, ErrIllegalTransactionMove) {
		t.Fatalf("abort with commit timestamp = %v", err)
	}
	// Conflict and reason markers only belong to an ABORTED move.
	if err := m.Transition("child", TransactionMerged, TransitionOptions{Conflict: true}); !errors.Is(err, ErrIllegalTransactionMove) {
		t.Fatalf("merge with conflict marker = %v", err)
	}
	if err := m.Transition("root", TransactionCommitted, TransitionOptions{Reason: "why"}); !errors.Is(err, ErrIllegalTransactionMove) {
		t.Fatalf("commit with abort reason = %v", err)
	}
	// None of the rejected moves changed any state.
	requireState(t, m, "root", TransactionActive)
	requireState(t, m, "child", TransactionActive)
	requireOldestReadTS(t, m, 2, true)
}

func TestTransactionManagerTerminalMoveReleasesRetention(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "root", 30)

	// COMMITTING still pins: the transaction may still need its snapshot.
	if err := m.Transition("root", TransactionCommitting, TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	if refs := managerRefCount(t, m, 30); refs != 1 {
		t.Fatalf("COMMITTING released retention early: %d", refs)
	}
	if err := m.Transition("root", TransactionCommitted, TransitionOptions{CommitTS: 9, HasCommitTS: true}); err != nil {
		t.Fatal(err)
	}
	if refs := managerRefCount(t, m, 30); refs != 0 {
		t.Fatalf("COMMITTED kept %d references", refs)
	}
	requireOldestReadTS(t, m, 0, false)
	// The entry stays registered until it is unregistered, but it is no longer
	// active; the two counts are deliberately different.
	if infos := m.ActiveTransactions(); len(infos) != 1 {
		t.Fatalf("terminal entry left the registry early: %d entries", len(infos))
	}
	if stats := m.Stats(); stats.ActiveRoot != 0 || stats.Committed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	m.Unregister("root")
	if refs := managerRefCount(t, m, 30); refs != 0 {
		t.Fatalf("unregister after a terminal move re-released: %d", refs)
	}

	// The abort path releases retention too.
	registerTestRoot(t, m, "canceled", 31)
	if err := m.Transition("canceled", TransactionAborted, TransitionOptions{Reason: "canceled"}); err != nil {
		t.Fatal(err)
	}
	if refs := managerRefCount(t, m, 31); refs != 0 {
		t.Fatalf("ABORTED kept %d references", refs)
	}
	m.Unregister("canceled")
	if refs := managerRefCount(t, m, 31); refs != 0 {
		t.Fatalf("unregister after abort underflowed the reference: %d", refs)
	}
}

func TestTransactionManagerRegistrationValidation(t *testing.T) {
	m := NewTransactionManager()
	if _, err := m.RegisterRoot(RootRegistration{ReadTS: 1}); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("empty root ID = %v", err)
	}
	if _, err := m.RegisterChild(ChildRegistration{ParentID: "root"}); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("empty child ID = %v", err)
	}
	if _, err := m.RegisterChild(ChildRegistration{ID: "c"}); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("missing parent ID = %v", err)
	}
	if _, err := m.RegisterChild(ChildRegistration{ID: "c", ParentID: "absent"}); !errors.Is(err, ErrUnknownTransaction) {
		t.Fatalf("unknown parent = %v", err)
	}

	registerTestRoot(t, m, "root", 3)
	if err := m.Transition("root", TransactionCommitting, TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RegisterChild(ChildRegistration{ID: "c", ParentID: "root"}); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("child of a COMMITTING parent = %v", err)
	}
	if err := m.Transition("root", TransactionCommitted, TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RegisterChild(ChildRegistration{ID: "c", ParentID: "root"}); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("child of a COMMITTED parent = %v", err)
	}
	if infos := m.ActiveTransactions(); len(infos) != 1 {
		t.Fatalf("rejected registrations left %d entries", len(infos))
	}
}

func TestTransactionManagerSnapshotsAreCopies(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "root", 100)
	registerTestChild(t, m, "child", "root")
	if err := m.Transition("root", TransactionCommitting, TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	beforeStats := m.Stats()
	before := sortedTransactionInfo(m.ActiveTransactions())

	// Tamper with everything the snapshot exposes.
	returned := m.ActiveTransactions()
	for i := range returned {
		returned[i] = TransactionInfo{
			ID:          "tampered",
			ParentID:    "tampered",
			StartTS:     1,
			ReadTS:      1,
			CommitTS:    1,
			HasCommitTS: true,
			State:       TransactionMerged,
			Generation:  99,
			StartedAt:   time.Time{},
			AbortReason: "tampered",
		}
	}
	returned[0] = TransactionInfo{}

	if after := sortedTransactionInfo(m.ActiveTransactions()); !reflect.DeepEqual(after, before) {
		t.Fatalf("a diagnostics snapshot aliased manager state:\n got %+v\nwant %+v", after, before)
	}
	if info := requireState(t, m, "root", TransactionCommitting); info.ID != "root" || info.ReadTS != 100 {
		t.Fatalf("point snapshot was writable: %+v", info)
	}
	if stats := m.Stats(); !reflect.DeepEqual(stats, beforeStats) {
		t.Fatalf("stats changed through a snapshot: %+v != %+v", stats, beforeStats)
	}
}

func sortedTransactionInfo(infos []TransactionInfo) []TransactionInfo {
	out := append([]TransactionInfo(nil), infos...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func TestTransactionManagerStatsCounters(t *testing.T) {
	m := NewTransactionManager()
	registerTestRoot(t, m, "root", 10)
	registerTestChild(t, m, "child-a", "root")
	registerTestChild(t, m, "child-b", "root")

	stats := m.Stats()
	if stats.ActiveRoot != 1 || stats.ActiveChildren != 2 || stats.Committed != 0 || stats.Aborted != 0 || stats.Conflicts != 0 {
		t.Fatalf("initial stats = %+v", stats)
	}
	if !stats.HasOldestReadTS || stats.OldestReadTS != 10 {
		t.Fatalf("horizon = (%d, %v)", stats.OldestReadTS, stats.HasOldestReadTS)
	}

	if err := m.Transition("child-a", TransactionMerged, TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	if stats = m.Stats(); stats.ActiveChildren != 1 {
		t.Fatalf("merged child still counted active: %+v", stats)
	}
	if err := m.Transition("root", TransactionCommitted, TransitionOptions{CommitTS: 11, HasCommitTS: true}); err != nil {
		t.Fatal(err)
	}
	stats = m.Stats()
	if stats.ActiveRoot != 0 || stats.ActiveChildren != 1 || stats.Committed != 1 {
		t.Fatalf("stats after commit = %+v", stats)
	}
	if stats.HasOldestReadTS {
		t.Fatalf("a committed root still pins the horizon: %+v", stats)
	}
}

// TestTransactionManagerConcurrentAccess drives registration, state moves,
// unregistration and diagnostics from many goroutines at once. It must never
// panic, deadlock or corrupt the reference accounting. Run under -race in CI to
// turn it into a data-race check.
func TestTransactionManagerConcurrentAccess(t *testing.T) {
	m := NewTransactionManager()
	const workers = 32

	done := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				_ = m.ActiveTransactions()
				_ = m.Stats()
				_, _ = m.OldestReadTS()
			}
		}()
	}

	var writers sync.WaitGroup
	for i := 0; i < workers; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			root := fmt.Sprintf("worker-%02d", i)
			child := root + "-child"
			if _, err := m.RegisterRoot(RootRegistration{ID: root, ReadTS: uint64(i), Generation: 1}); err != nil {
				t.Errorf("RegisterRoot(%s): %v", root, err)
				return
			}
			if _, err := m.RegisterChild(ChildRegistration{ID: child, ParentID: root, Generation: 1}); err != nil {
				t.Errorf("RegisterChild(%s): %v", child, err)
				return
			}
			if err := m.Transition(root, TransactionCommitting, TransitionOptions{}); err != nil {
				t.Errorf("Transition(%s, COMMITTING): %v", root, err)
				return
			}
			if err := m.Transition(root, TransactionCommitted, TransitionOptions{CommitTS: uint64(i) + 1, HasCommitTS: true}); err != nil {
				t.Errorf("Transition(%s, COMMITTED): %v", root, err)
				return
			}
			m.Unregister(child)
			m.Unregister(root)
		}(i)
	}
	writers.Wait()
	close(done)
	readers.Wait()

	if infos := m.ActiveTransactions(); len(infos) != 0 {
		t.Fatalf("registry leaked %d entries", len(infos))
	}
	stats := m.Stats()
	if stats.ActiveRoot != 0 || stats.ActiveChildren != 0 || stats.HasOldestReadTS {
		t.Fatalf("stats after concurrent cleanup = %+v", stats)
	}
	if stats.Committed != workers {
		t.Fatalf("committed = %d, want %d", stats.Committed, workers)
	}
	m.mu.RLock()
	remaining := len(m.snapshotRefs)
	m.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("%d snapshot references survived concurrent cleanup", remaining)
	}
}
