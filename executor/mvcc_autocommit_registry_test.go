package executor

import (
	"testing"

	"gbaselite/storageengine"
)

// T085: with autocommit disabled a session holds one implicit root transaction, and
// that root is a real entry in the storage engine's transaction registry. The registry
// must show exactly one root across statements, no statement child left behind after a
// statement merges, and an empty registry once the transaction ends -- by COMMIT,
// ROLLBACK or a disconnect.
//
// The registry is reached through the optional storageengine capability and never
// through a physical backend import, which is what keeps the SQL packages free of
// backend dependencies (TestSQLPackagesDoNotImportPhysicalBackends).

func transactionDiagnostics(t *testing.T, e *Engine) storageengine.TransactionDiagnostics {
	t.Helper()
	diagnostics, ok := e.Backend.(storageengine.TransactionDiagnostics)
	if !ok {
		t.Fatal("the MVCC backend does not expose transaction diagnostics")
	}
	if _, ok := e.Backend.(storageengine.Engine); !ok {
		t.Fatal("the diagnostics capability replaced the engine contract")
	}
	return diagnostics
}

// requireActiveRoot asserts exactly one live root and no live child, and returns it.
func requireActiveRoot(t *testing.T, diagnostics storageengine.TransactionDiagnostics) storageengine.TransactionInfo {
	t.Helper()
	roots := 0
	var root storageengine.TransactionInfo
	for _, info := range diagnostics.ActiveTransactions() {
		if info.State != storageengine.TransactionActive {
			continue
		}
		if info.ParentID != "" {
			t.Fatalf("a statement child stayed open: %+v", info)
		}
		roots++
		root = info
	}
	if roots != 1 {
		t.Fatalf("active roots = %d, want 1: %+v", roots, diagnostics.ActiveTransactions())
	}
	if stats := diagnostics.TransactionStats(); stats.ActiveRoot != 1 || stats.ActiveChildren != 0 {
		t.Fatalf("active counts = %+v", stats)
	}
	return root
}

// requireEmptyRegistry asserts no transaction is registered any more.
func requireEmptyRegistry(t *testing.T, diagnostics storageengine.TransactionDiagnostics) {
	t.Helper()
	if transactions := diagnostics.ActiveTransactions(); len(transactions) != 0 {
		t.Fatalf("registry leaked: %+v", transactions)
	}
	if stats := diagnostics.TransactionStats(); stats.ActiveRoot != 0 || stats.ActiveChildren != 0 || stats.HasOldestReadTS {
		t.Fatalf("registry leaked retention: %+v", stats)
	}
}

func TestMVCCAutocommitDisabledImplicitRootRegistryLifecycle(t *testing.T) {
	e, session, run := rangeTestEngine(t)
	diagnostics := transactionDiagnostics(t, e)
	run("CREATE TABLE ac(id INT PRIMARY KEY,v INT)")
	requireEmptyRegistry(t, diagnostics)

	// Autocommit is disabled through the Go API the protocol layer calls for
	// SET autocommit=0: the first statement then opens the implicit root.
	if err := e.SetAutocommit(session, false); err != nil {
		t.Fatal(err)
	}
	run("INSERT INTO ac VALUES(1,10)")
	first := requireActiveRoot(t, diagnostics)
	if first.HasCommitTS || first.CommitTS != 0 {
		t.Fatalf("an uncommitted root reported a commit sequence: %+v", first)
	}
	if first.StartTS != first.ReadTS || first.StartTS == 0 {
		t.Fatalf("implicit root timestamps = %+v", first)
	}
	if first.Generation == 0 && first.StartedAt.IsZero() {
		t.Fatalf("implicit root has no registration metadata: %+v", first)
	}

	// The same root spans every statement of the session, and each statement child is
	// merged into it instead of remaining registered.
	run("INSERT INTO ac VALUES(2,20)")
	run("UPDATE ac SET v=v+1 WHERE id=1")
	second := requireActiveRoot(t, diagnostics)
	if second.ID != first.ID {
		t.Fatalf("implicit root changed %s -> %s across statements", first.ID, second.ID)
	}
	if second.StartTS != first.StartTS {
		t.Fatalf("implicit root snapshot moved %d -> %d", first.StartTS, second.StartTS)
	}
	if second.Writes < first.Writes || second.Writes < 2 {
		t.Fatalf("implicit root write set did not accumulate: %+v -> %+v", first, second)
	}

	// COMMIT ends the root durably and clears the registry.
	run("COMMIT")
	requireEmptyRegistry(t, diagnostics)
	committed := diagnostics.TransactionStats()
	if committed.Committed < 1 {
		t.Fatalf("COMMIT was not counted: %+v", committed)
	}

	// A later statement opens a new root, and ROLLBACK clears it again.
	run("INSERT INTO ac VALUES(3,30)")
	rolledBack := requireActiveRoot(t, diagnostics)
	if rolledBack.ID == first.ID {
		t.Fatalf("a rolled back transaction ID was reused: %s", rolledBack.ID)
	}
	run("ROLLBACK")
	requireEmptyRegistry(t, diagnostics)
	if stats := diagnostics.TransactionStats(); stats.Aborted != committed.Aborted+1 {
		t.Fatalf("ROLLBACK was not counted: %+v -> %+v", committed, stats)
	}

	// A disconnect with an open implicit transaction rolls it back and releases the
	// registry, so the discarded row can never become visible.
	run("INSERT INTO ac VALUES(4,40)")
	if pending := requireActiveRoot(t, diagnostics); pending.Writes == 0 {
		t.Fatalf("disconnect fixture wrote nothing: %+v", pending)
	}
	e.CloseSession(session)
	requireEmptyRegistry(t, diagnostics)

	reader := &Session{CurrentDatabase: "test"}
	result, err := e.Execute(reader, "SELECT id FROM ac ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	// Only the committed rows survive: id=3 was rolled back and id=4 was discarded by
	// the disconnect, so a third row here would be a leaked write.
	if got := len(result.Rows); got != 2 {
		t.Fatalf("rows after disconnect rollback = %d, want 2: %#v", got, result.Rows)
	}
	if first, second := result.Rows[0][0], result.Rows[1][0]; first != int64(1) || second != int64(2) {
		t.Fatalf("surviving rows = %#v, want id 1 and 2", result.Rows)
	}
}
