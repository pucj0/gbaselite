package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gbaselite/storageengine"
)

// HIGH-1 regression: ROLLBACK TO SAVEPOINT discards the target layer and every layer
// above it, and those child transactions must end their own lifecycle. Only
// storageengine.Txn rollback releases the staging database and its file — dropping the
// entries from the diagnostics registry does not, because the registry owns diagnostics
// state while Tx cleanup owns the bolt handle and the <store>/transactions/*.tmp file.
//
// The fixtures below force the discarded layers to spill past the 128 KiB in-memory
// buffer, so a leak leaves a real staging file behind. Each test asserts the release
// immediately after ROLLBACK TO, not after the user transaction ends.

// spillRows pushes a layer's write set past the in-memory buffer, so the layer owns a
// real staging database.
const spillRows = 40

func spillIntoCurrentLayer(t *testing.T, run func(string) *Result, startID int) {
	t.Helper()
	payload := strings.Repeat("x", 20000)
	for i := 0; i < spillRows; i++ {
		run(fmt.Sprintf("INSERT INTO big VALUES(%d, '%s')", startID+i, payload))
	}
}

// stagingFiles lists the staging databases of one store directory.
func stagingFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "versioned", "transactions"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tmp") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// openSavepointStore opens a real MVCC store on dir and returns a run helper bound to
// one session in database "probe".
func openSavepointStore(t *testing.T, dir string) (*Engine, *Session, func(string) *Result) {
	t.Helper()
	e, err := OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	run := func(q string) *Result {
		t.Helper()
		result, err := e.Execute(session, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return result
	}
	run("CREATE DATABASE probe")
	run("USE probe")
	run("CREATE TABLE big(id INT PRIMARY KEY, v VARCHAR(20000))")
	return e, session, run
}

// requireDiscardedRangeCleared proves that no abandoned layer stays reachable through
// the savepoint slice's backing array, so its transaction can be collected.
func requireDiscardedRangeCleared(t *testing.T, session *Session) {
	t.Helper()
	backing := session.savepoints[:cap(session.savepoints)]
	for i := len(session.savepoints); i < len(backing); i++ {
		if backing[i].tx != nil || backing[i].parent != nil || backing[i].name != "" {
			t.Fatalf("discarded layer %d is still reachable through the backing array: %+v", i, backing[i])
		}
	}
}

func TestMVCCRollbackToSavepointReleasesDiscardedLayers(t *testing.T) {
	dir := t.TempDir()
	e, session, run := openSavepointStore(t, dir)
	t.Cleanup(func() { _ = e.Close() })
	diagnostics := transactionDiagnostics(t, e)

	run("BEGIN")
	run("SAVEPOINT a")
	run("SAVEPOINT b")
	spillIntoCurrentLayer(t, run, 0)

	// The fixture must really spill: the discarded subtree owns a staging file.
	if spilled := stagingFiles(t, dir); len(spilled) == 0 {
		t.Fatal("the fixture did not spill to a staging database")
	}
	requireLayerCount(t, diagnostics, 2)
	before := diagnostics.TransactionStats()
	if before.ActiveRoot != 1 || before.ActiveChildren != 2 || !before.HasOldestReadTS {
		t.Fatalf("fixture registry = %+v", before)
	}

	run("ROLLBACK TO SAVEPOINT a")

	// Immediately after the statement: the discarded layer is gone, the target is a
	// fresh ACTIVE child, the root gained no second pin, and every staging file the
	// discarded subtree owned is released — which also proves the handle was closed.
	requireLayerCount(t, diagnostics, 1)
	requireDiscardedRangeCleared(t, session)
	if after := stagingFiles(t, dir); len(after) != 0 {
		t.Fatalf("discarded layers left staging files behind: %v", after)
	}
	after := diagnostics.TransactionStats()
	if after.ActiveRoot != 1 || after.ActiveChildren != 1 || !after.HasOldestReadTS {
		t.Fatalf("registry after ROLLBACK TO = %+v", after)
	}
	if after.OldestReadTS != before.OldestReadTS {
		t.Fatalf("ROLLBACK TO moved the horizon %d -> %d", before.OldestReadTS, after.OldestReadTS)
	}
	// The discarded layers now reach their own terminal state, so the two of them are
	// counted as aborted outcomes while nothing new is counted as committed.
	if after.Committed != before.Committed {
		t.Fatalf("ROLLBACK TO changed Committed: %d -> %d", before.Committed, after.Committed)
	}
	if after.Aborted != before.Aborted+2 {
		t.Fatalf("Aborted = %d, want %d (the target and the layer above it)", after.Aborted, before.Aborted+2)
	}

	// The transaction continues normally and ends cleanly.
	run("INSERT INTO big VALUES(999, 'after')")
	run("COMMIT")
	requireEmptySavepointRegistry(t, diagnostics)
	if after := stagingFiles(t, dir); len(after) != 0 {
		t.Fatalf("staging files survived COMMIT: %v", after)
	}
	// Resource release must not change what the rollback means: every write made in the
	// discarded layers is gone, and only the row written after it survives.
	result, err := e.Execute(&Session{CurrentDatabase: "probe"}, "SELECT COUNT(*) FROM big")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(result.Rows); got != "[[1]]" {
		t.Fatalf("rows after COMMIT = %s, want only the row written after ROLLBACK TO", got)
	}
}

func TestMVCCRollbackToSavepointReleasesEveryDiscardedLayer(t *testing.T) {
	dir := t.TempDir()
	e, session, run := openSavepointStore(t, dir)
	defer e.Close()
	diagnostics := transactionDiagnostics(t, e)

	run("BEGIN")
	run("SAVEPOINT a")
	run("SAVEPOINT b")
	spillIntoCurrentLayer(t, run, 0)
	run("SAVEPOINT c")
	spillIntoCurrentLayer(t, run, 100)
	if spilled := stagingFiles(t, dir); len(spilled) < 2 {
		t.Fatalf("the fixture did not spill in both discarded layers: %v", spilled)
	}
	requireLayerCount(t, diagnostics, 3)

	// Both b and c are discarded, so both staging databases must be released even though
	// only the target's rollback is directly observable through the registry.
	run("ROLLBACK TO SAVEPOINT a")
	requireLayerCount(t, diagnostics, 1)
	requireDiscardedRangeCleared(t, session)
	if after := stagingFiles(t, dir); len(after) != 0 {
		t.Fatalf("a multi-layer discard left staging files: %v", after)
	}
	run("ROLLBACK")
	requireEmptySavepointRegistry(t, diagnostics)
}

func TestMVCCRollbackToSavepointAllowsReopen(t *testing.T) {
	dir := t.TempDir()
	e, _, run := openSavepointStore(t, dir)
	run("BEGIN")
	run("SAVEPOINT a")
	run("SAVEPOINT b")
	spillIntoCurrentLayer(t, run, 0)
	run("ROLLBACK TO SAVEPOINT a")
	run("ROLLBACK")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// The same directory must reopen in this process. A staging handle abandoned by
	// ROLLBACK TO would still hold its file, and the open-time sweep of
	// <store>/transactions would fail on Windows with "used by another process".
	reopened, err := OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatalf("reopen after ROLLBACK TO failed: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if after := stagingFiles(t, dir); len(after) != 0 {
		t.Fatalf("reopen left staging files: %v", after)
	}
}

func TestMVCCRollbackToSavepointDoesNotAccumulateStages(t *testing.T) {
	dir := t.TempDir()
	e, session, run := openSavepointStore(t, dir)
	defer e.Close()
	diagnostics := transactionDiagnostics(t, e)

	run("BEGIN")
	run("SAVEPOINT outer")
	for round := 0; round < 3; round++ {
		run("SAVEPOINT inner")
		spillIntoCurrentLayer(t, run, round*100)
		run("ROLLBACK TO SAVEPOINT outer")

		// Each round discards its spilled inner layer completely: the chain stays at one
		// child and no staging file survives into the next round.
		requireLayerCount(t, diagnostics, 1)
		requireDiscardedRangeCleared(t, session)
		if files := stagingFiles(t, dir); len(files) != 0 {
			t.Fatalf("round %d left %d staging files: %v", round, len(files), files)
		}
	}
	run("ROLLBACK")
	requireEmptySavepointRegistry(t, diagnostics)
	if files := stagingFiles(t, dir); len(files) != 0 {
		t.Fatalf("staging files survived the transaction: %v", files)
	}
}

// HIGH-2 regression: after a real generation change (RESTORE MVCC FROM in another
// session, or a raft snapshot install on a replica) the fresh target child cannot be
// created, so ROLLBACK TO fails. The session must still reference a live, reachable
// transaction — the surviving parent — so the remaining chain, including the root and
// its staging file, can still be reclaimed by ROLLBACK or by a disconnect.
//
// The failure is driven through SQL, never by stubbing Child(): two sessions on one
// engine, a real BACKUP/RESTORE pair, and enough writes that both the root and the
// savepoint layer own a real staging database.

type savepointRestoreFixture struct {
	e       *Engine
	dir     string
	root    storageengine.Txn
	session *Session
	run     func(string) *Result
}

// newRestoreFixture creates the HIGH-2 scenario: session A holds a backup, session B
// holds an open chain whose root and savepoint layer both spilled to staging, and the
// restore has already invalidated B's generation.
func newRestoreFixture(t *testing.T) savepointRestoreFixture {
	t.Helper()
	dir := t.TempDir()
	e, _, runA := openSavepointStore(t, dir)
	backup := filepath.Join(dir, "snapshot.mvcc")
	runA("BACKUP MVCC TO '" + backup + "'")

	sessionB := &Session{CurrentDatabase: "probe"}
	runB := func(q string) *Result {
		t.Helper()
		result, err := e.Execute(sessionB, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return result
	}
	runB("BEGIN")
	root := sessionB.transaction
	if root == nil {
		t.Fatal("BEGIN did not open a transaction")
	}
	spillIntoCurrentLayer(t, runB, 0) // the root owns a staging file
	runB("SAVEPOINT s0")
	spillIntoCurrentLayer(t, runB, 100) // s0 owns a staging file

	if files := stagingFiles(t, dir); len(files) < 2 {
		t.Fatalf("fixture did not spill in both the root and s0: %v", files)
	}
	runA("RESTORE MVCC FROM '" + backup + "'")
	return savepointRestoreFixture{e: e, dir: dir, root: root, session: sessionB, run: runB}
}

// requireFailedRollbackToSavepoint drives the failing ROLLBACK TO and asserts the
// fallback-parent contract: the discarded layer is released, the session still points
// at the surviving root, and the live layer count never exceeds the ceiling.
func requireFailedRollbackToSavepoint(t *testing.T, f savepointRestoreFixture, diagnostics storageengine.TransactionDiagnostics) {
	t.Helper()
	before := stagingFiles(t, f.dir)
	if _, err := f.e.Execute(f.session, "ROLLBACK TO SAVEPOINT s0"); !errors.Is(err, storageengine.ErrClosed) {
		t.Fatalf("ROLLBACK TO after a generation change = %v, want ErrClosed", err)
	}
	// The discarded savepoint layer is released immediately, even though the statement
	// failed: its staging file is gone while the root's is still legitimately held.
	after := stagingFiles(t, f.dir)
	if len(after) != len(before)-1 {
		t.Fatalf("staging files = %v, want the discarded layer released from %v", after, before)
	}
	// The fallback invariant: the session must reference the surviving root, never the
	// closed discarded layer, so a later ROLLBACK/CloseSession can still reach it.
	if f.session.transaction == nil {
		t.Fatal("the session lost its transaction reference after a failed ROLLBACK TO")
	}
	if f.session.transaction != f.root {
		t.Fatalf("session.transaction = %v, want the surviving root %v", f.session.transaction, f.root)
	}
	if len(f.session.savepoints) != 0 {
		t.Fatalf("savepoints = %d, want the discarded chain removed", len(f.session.savepoints))
	}
	if stats := diagnostics.TransactionStats(); stats.ActiveChildren > maxMVCCSavepoints {
		t.Fatalf("live savepoint children = %d, above the ceiling", stats.ActiveChildren)
	}
}

func TestMVCCRollbackToSavepointAfterRestoreKeepsRootReclaimable(t *testing.T) {
	t.Run("explicit rollback reclaims the root", func(t *testing.T) {
		f := newRestoreFixture(t)
		diagnostics := transactionDiagnostics(t, f.e)
		requireFailedRollbackToSavepoint(t, f, diagnostics)

		// The stale root is still reachable, so an explicit ROLLBACK must perform its
		// local resource cleanup even though the generation moved.
		f.run("ROLLBACK")
		if f.session.transaction != nil {
			t.Fatalf("session.transaction = %v after ROLLBACK, want nil", f.session.transaction)
		}
		if len(f.session.savepoints) != 0 {
			t.Fatalf("savepoints = %v after ROLLBACK, want empty", f.session.savepoints)
		}
		requireEmptySavepointRegistry(t, diagnostics)
		if files := stagingFiles(t, f.dir); len(files) != 0 {
			t.Fatalf("ROLLBACK left staging files: %v", files)
		}

		// Windows: an unreleased staging handle would make this fail with a sharing
		// violation on the still-mapped <id>.tmp.
		if err := f.e.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenWithOptions(f.dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
		if err != nil {
			t.Fatalf("in-process reopen after ROLLBACK failed: %v", err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("disconnect reclaims the root", func(t *testing.T) {
		f := newRestoreFixture(t)
		diagnostics := transactionDiagnostics(t, f.e)
		requireFailedRollbackToSavepoint(t, f, diagnostics)

		// A disconnect (COM_QUIT, KILL, COM_RESET_CONNECTION) must reclaim the whole
		// remaining user transaction, root included, without a manual ROLLBACK.
		f.e.CloseSession(f.session)
		if f.session.transaction != nil {
			t.Fatalf("session.transaction = %v after CloseSession, want nil", f.session.transaction)
		}
		if len(f.session.savepoints) != 0 {
			t.Fatalf("savepoints = %v after CloseSession, want empty", f.session.savepoints)
		}
		requireEmptySavepointRegistry(t, diagnostics)
		if files := stagingFiles(t, f.dir); len(files) != 0 {
			t.Fatalf("CloseSession left staging files: %v", files)
		}
		if err := f.e.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenWithOptions(f.dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
		if err != nil {
			t.Fatalf("in-process reopen after CloseSession failed: %v", err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

// The same generation change also hits createSavepoint: a refused SAVEPOINT must not
// have anonymized an existing savepoint name, because the child is created before any
// name is touched.
func TestMVCCCreateSavepointAfterRestoreKeepsExistingName(t *testing.T) {
	dir := t.TempDir()
	e, _, runA := openSavepointStore(t, dir)
	defer e.Close()
	backup := filepath.Join(dir, "snapshot.mvcc")
	runA("BACKUP MVCC TO '" + backup + "'")

	sessionB := &Session{CurrentDatabase: "probe"}
	runB := func(q string) *Result {
		t.Helper()
		result, err := e.Execute(sessionB, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return result
	}
	runB("BEGIN")
	runB("SAVEPOINT keep")
	layers := len(sessionB.savepoints)

	runA("RESTORE MVCC FROM '" + backup + "'")

	// Re-issuing the existing name now fails on the closed parent, and the failure must
	// leave the name in place.
	if _, err := e.Execute(sessionB, "SAVEPOINT keep"); !errors.Is(err, storageengine.ErrClosed) {
		t.Fatalf("SAVEPOINT after a generation change = %v, want ErrClosed", err)
	}
	if len(sessionB.savepoints) != layers {
		t.Fatalf("savepoints = %d, want the refused SAVEPOINT to append nothing", len(sessionB.savepoints))
	}
	if name := sessionB.savepoints[0].name; !strings.EqualFold(name, "keep") {
		t.Fatalf("the existing savepoint name became %q, want it untouched", name)
	}
	// Behavioural proof: the name still resolves, so the failure is the generation
	// error and not "SAVEPOINT does not exist".
	if _, err := e.Execute(sessionB, "ROLLBACK TO SAVEPOINT keep"); errors.Is(err, errMVCCSavepointNotFound) {
		t.Fatalf("ROLLBACK TO lost the existing name: %v", err)
	}
	e.CloseSession(sessionB)
	if files := stagingFiles(t, dir); len(files) != 0 {
		t.Fatalf("cleanup left staging files: %v", files)
	}
}
