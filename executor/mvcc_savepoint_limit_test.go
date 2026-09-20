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
// Contract pinned here (current implementation, mvcc 的 savepoint_mvcc.go):
//
//   - exactly maxMVCCSavepoints named layers are accepted;
//   - one more named savepoint fails with the resource-limit error and changes nothing:
//     the transaction stays usable, no child is registered, and no layer is lost;
//   - re-issuing a name that already exists at the ceiling is allowed and moves the
//     restore point to the end: the older layer becomes an anonymous boundary, which is
//     why the registry then holds maxMVCCSavepoints+1 children;
//   - COMMIT and ROLLBACK both leave an empty registry (no orphan child).
//
// The MVCC path reports its own unexported resource-limit sentinel, so the stable
// cross-backend contract is the message ("at most 32 savepoints per transaction");
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

func savepointNames(count int) []string {
	names := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		names = append(names, fmt.Sprintf("sp%02d", i))
	}
	return names
}

func TestMVCCSavepointLayerLimit(t *testing.T) {
	e, session, run := rangeTestEngine(t)
	diagnostics := transactionDiagnostics(t, e)
	run("CREATE TABLE layers(id INT PRIMARY KEY)")

	t.Run("the limit is exact and the overflow changes nothing", func(t *testing.T) {
		run("BEGIN")
		run("INSERT INTO layers VALUES(1)")
		names := savepointNames(maxMVCCSavepoints)
		for _, name := range names {
			run("SAVEPOINT " + name)
		}
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		// One more named layer must fail closed with the resource-limit message.
		_, err := e.Execute(session, "SAVEPOINT overflow")
		if err == nil {
			t.Fatal("a savepoint beyond the ceiling was accepted")
		}
		if !strings.Contains(err.Error(), "resource limit exceeded") ||
			!strings.Contains(err.Error(), fmt.Sprintf("at most %d savepoints", maxMVCCSavepoints)) {
			t.Fatalf("overflow error = %v, want the savepoint resource limit", err)
		}
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

	t.Run("replacement at the ceiling keeps working", func(t *testing.T) {
		run("DELETE FROM layers")
		run("BEGIN")
		names := savepointNames(maxMVCCSavepoints)
		for _, name := range names {
			run("SAVEPOINT " + name)
		}
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)

		// Re-issuing an existing name at the ceiling is the documented way to move the
		// restore point: the old layer stays as an anonymous boundary.
		run("SAVEPOINT " + names[0])
		requireLayerCount(t, diagnostics, maxMVCCSavepoints+1)

		// The other names still resolve, and the transaction still commits cleanly.
		run("INSERT INTO layers VALUES(1)")
		run("ROLLBACK TO SAVEPOINT " + names[1])
		run("INSERT INTO layers VALUES(2)")
		run("COMMIT")
		requireEmptySavepointRegistry(t, diagnostics)
	})

	t.Run("rollback releases every layer", func(t *testing.T) {
		run("BEGIN")
		for _, name := range savepointNames(maxMVCCSavepoints) {
			run("SAVEPOINT " + name)
		}
		requireLayerCount(t, diagnostics, maxMVCCSavepoints)
		run("ROLLBACK")
		requireEmptySavepointRegistry(t, diagnostics)
	})
}
