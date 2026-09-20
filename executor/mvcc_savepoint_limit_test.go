package executor

import (
	"fmt"
	"strings"
	"testing"

	"gbaselite/storageengine"
)

// M-5 / T082: the MVCC savepoint path has its own layer ceiling, and the boundary has
// to be tested against the MVCC backend itself rather than inferred from the legacy
// engine fixture.
//
// The budget bounds the live MVCC savepoint *layers* of one user transaction, not the
// number of distinct savepoint names:
//
//   - exactly maxMVCCSavepoints layers are accepted;
//   - the next SAVEPOINT fails with the resource-limit error and changes nothing: the
//     transaction stays usable, no child is registered, and no layer or name is lost;
//   - replacing a name below the ceiling still moves the restore point and leaves the
//     older layer as an anonymous boundary;
//   - at the ceiling even re-issuing an *existing* name fails, because the replacement
//     would need a new layer, and the refusal must not anonymize the existing name;
//   - RELEASE only drops a name, so its layer keeps occupying a slot: repeated
//     SAVEPOINT/RELEASE cannot grow the chain past the ceiling;
//   - COMMIT and ROLLBACK both leave an empty registry (no orphan child).
//
// The MVCC path reports its own unexported resource-limit sentinel, so the stable
// cross-backend contract is the message ("at most 32 savepoint layers per transaction");
// ErrResourceLimit only exists inside the legacy test fixture, so nothing here assumes
// that value.

// requireLayerCount asserts the registry shape the savepoint stack must have: one live
// root transaction and exactly the expected number of live children.
func requireLayerCount(t *testing.T, diagnostics storageengine.TransactionDiagnostics, wantChildren int) {
	t.Helper()
	roots, children := 0, 0
	for _, info := range diagnostics.ActiveTransactions() {
		if info.State != storageengine.TransactionActive {
			t.Fatalf("a transaction reached a terminal state inside the savepoint stack: %+v", info)
		}
		if info.ParentID == "" {
			roots++
			continue
		}
		children++
	}
	if roots != 1 || children != wantChildren {
		t.Fatalf("registry = %d roots and %d children, want 1 root and %d children: %+v",
			roots, children, wantChildren, diagnostics.ActiveTransactions())
	}
	if stats := diagnostics.TransactionStats(); stats.ActiveRoot != 1 || stats.ActiveChildren != uint64(wantChildren) {
		t.Fatalf("stats = %+v, want 1 root and %d children", stats, wantChildren)
	}
}

// requireEmptySavepointRegistry asserts the whole savepoint chain was released.
func requireEmptySavepointRegistry(t *testing.T, diagnostics storageengine.TransactionDiagnostics) {
	t.Helper()
	if transactions := diagnostics.ActiveTransactions(); len(transactions) != 0 {
		t.Fatalf("registry leaked after the transaction ended: %+v", transactions)
	}
	if stats := diagnostics.TransactionStats(); stats.ActiveRoot != 0 || stats.ActiveChildren != 0 || stats.HasOldestReadTS {
		t.Fatalf("registry leaked retention: %+v", stats)
	}
}

// requireResourceLimit asserts the refusal is the savepoint layer budget: the MVCC path
// reports its own unexported sentinel, so the message is the stable part of the contract.
func requireResourceLimit(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("a savepoint beyond the layer ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "resource limit exceeded") ||
		!strings.Contains(err.Error(), fmt.Sprintf("at most %d savepoint layers", maxMVCCSavepoints)) {
		t.Fatalf("overflow error = %v, want the savepoint layer resource limit", err)
	}
}

func savepointNames(count int) []string {
	names := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		names = append(names, fmt.Sprintf("sp%02d", i))
	}
	return names
}

// resetUserTransaction ends a transaction a previous subtest left open, so every
// boundary case starts from the same session state.
func resetUserTransaction(t *testing.T, session *Session, run func(string) *Result) {
	t.Helper()
	if session.InTransaction() {
		run("ROLLBACK")
	}
}

func TestMVCCSavepointLayerLimit(t *testing.T) {
	e, session, run := rangeTestEngine(t)
	diagnostics := transactionDiagnostics(t, e)
	run("CREATE TABLE layers(id INT PRIMARY KEY)")

	t.Run("the limit is exact and the overflow changes nothing", func(t *testing.T) {
		resetUserTransaction(t, session, run)
		run("BEGIN")
		run("INSERT INTO layers VALUES(1)")
		names := savepointNames(maxMVCCSavepoints)
		for _, name := range names {
			run("SAVEPOINT " + name)
		}
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		// One more layer must fail closed with the resource-limit message.
		_, err := e.Execute(session, "SAVEPOINT overflow")
		requireResourceLimit(t, err)
		// The refused statement registered nothing and discarded nothing.
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		// The transaction is still usable, and a rollback to the last layer discards
		// exactly the writes made above it.
		run("INSERT INTO layers VALUES(2)")
		run("ROLLBACK TO SAVEPOINT " + names[len(names)-1])
		run("INSERT INTO layers VALUES(3)")
		run("COMMIT")
		requireEmptySavepointRegistry(t, diagnostics)

		result, queryErr := e.Execute(&Session{CurrentDatabase: "test"}, "SELECT id FROM layers ORDER BY id")
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		if got := fmt.Sprint(result.Rows); got != "[[1] [3]]" {
			t.Fatalf("committed rows = %s, want the writes below and above the rolled back layer only", got)
		}
	})

	t.Run("replacement below the ceiling keeps the documented semantics", func(t *testing.T) {
		resetUserTransaction(t, session, run)
		run("DELETE FROM layers")
		run("BEGIN")
		run("INSERT INTO layers VALUES(1)")
		run("SAVEPOINT a")
		run("INSERT INTO layers VALUES(2)")
		run("SAVEPOINT b")
		run("INSERT INTO layers VALUES(3)")

		// Re-issuing "a" below the ceiling moves the restore point: the older a becomes
		// an anonymous boundary and b keeps its name and its layer.
		run("SAVEPOINT a")
		requireLayerCount(t, diagnostics, 3)
		run("INSERT INTO layers VALUES(4)")
		run("ROLLBACK TO SAVEPOINT a")
		run("INSERT INTO layers VALUES(5)")
		run("COMMIT")
		requireEmptySavepointRegistry(t, diagnostics)

		// Only the write above the newest "a" was discarded; the writes below it,
		// including the one made inside b, survived the replacement.
		result, queryErr := e.Execute(&Session{CurrentDatabase: "test"}, "SELECT id FROM layers ORDER BY id")
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		if got := fmt.Sprint(result.Rows); got != "[[1] [2] [3] [5]]" {
			t.Fatalf("committed rows = %s, want replacement to keep every layer below the new restore point", got)
		}
	})

	t.Run("replacement at the ceiling fails closed and keeps the name", func(t *testing.T) {
		resetUserTransaction(t, session, run)
		run("DELETE FROM layers")
		run("BEGIN")
		names := savepointNames(maxMVCCSavepoints)
		for _, name := range names {
			run("SAVEPOINT " + name)
		}
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		// Re-issuing an existing name at the ceiling needs a new layer, so it must be
		// refused -- and the refusal must not anonymize the name it was asked to replace.
		_, err := e.Execute(session, "SAVEPOINT "+names[1])
		requireResourceLimit(t, err)
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		// The name is still a usable rollback target, which is the observable proof that
		// the refused statement mutated nothing. A name that had been anonymized would
		// instead report that the savepoint does not exist.
		run("ROLLBACK TO SAVEPOINT " + names[1])
		// Rolling back to a lower layer discards every layer above it, which is the
		// documented truncation: the chain is now the layers below the target plus the
		// fresh target layer, and its descendants were released from the registry.
		requireLayerCount(t, diagnostics, 2)

		// The transaction is still usable and commits cleanly with no orphan.
		run("INSERT INTO layers VALUES(1)")
		run("COMMIT")
		requireEmptySavepointRegistry(t, diagnostics)
	})

	t.Run("repeated same-name savepoints cannot grow the chain", func(t *testing.T) {
		resetUserTransaction(t, session, run)
		run("DELETE FROM layers")
		run("BEGIN")
		// Each SAVEPOINT "repeated" anonymizes the previous one, so the name count stays
		// at one while the live layer chain grows to the ceiling and stops there.
		for i := 0; i < maxMVCCSavepoints; i++ {
			run("SAVEPOINT repeated")
		}
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		_, err := e.Execute(session, "SAVEPOINT repeated")
		requireResourceLimit(t, err)
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		// The newest "repeated" is still the restore point, and ROLLBACK releases
		// everything.
		run("INSERT INTO layers VALUES(1)")
		run("ROLLBACK TO SAVEPOINT repeated")
		run("ROLLBACK")
		requireEmptySavepointRegistry(t, diagnostics)
	})

	t.Run("release cannot free a layer slot", func(t *testing.T) {
		resetUserTransaction(t, session, run)
		run("DELETE FROM layers")
		run("BEGIN")
		// RELEASE drops the name, not the child layer, so the released layers keep
		// occupying the budget even though no savepoint name is active.
		for i := 1; i <= maxMVCCSavepoints; i++ {
			name := fmt.Sprintf("s%02d", i)
			run("SAVEPOINT " + name)
			run("RELEASE SAVEPOINT " + name)
		}
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		_, err := e.Execute(session, "SAVEPOINT extra")
		requireResourceLimit(t, err)
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		run("INSERT INTO layers VALUES(1)")
		run("COMMIT")
		requireEmptySavepointRegistry(t, diagnostics)
	})

	t.Run("rollback releases every layer", func(t *testing.T) {
		resetUserTransaction(t, session, run)
		run("BEGIN")
		for _, name := range savepointNames(maxMVCCSavepoints) {
			run("SAVEPOINT " + name)
		}
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)
		run("ROLLBACK")
		requireEmptySavepointRegistry(t, diagnostics)
	})
}
