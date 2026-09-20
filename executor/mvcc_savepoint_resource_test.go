package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
