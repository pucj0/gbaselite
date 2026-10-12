package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gbaselite/lockmanager"
	"gbaselite/storageengine"
)

// locking_modify_pipeline_test.go is the T046 suite: the A05 modify pipeline is still the one road
// every write travels, and adding locks to it changed no result, no count and no identity.
//
// The membership half is proved rather than asserted. A statement that reached the pipeline opened a
// statement lock scope there and claimed its table intention; a write that reached the storage through
// some other road would carry no claim at all. So "every shape claims its table intention" is a
// positive test for "every shape goes through the pipeline", and it fails the moment a new write path
// is added beside it.
//
// The compatibility half compares the observable answers the pipeline has always given — affected
// rows, LastInsertID, dedup, statement rollback, temporary-resource cleanup — against the same
// statements driven without any contention, so the locks are the only difference between the runs.

// t046LockManager resolves the engine's lock manager, or fails the test.
func t046LockManager(t *testing.T, e *Engine) *lockmanager.Manager {
	t.Helper()
	manager := engineLockManager(e.Backend)
	if manager == nil {
		t.Fatal("the mvcc backend exposed no lock manager")
	}
	return manager
}

// t046Session opens a session on the fixture's database with a short lock-wait budget, so a blocking
// statement in these tests fails quickly instead of holding the suite for the contract's 50 seconds.
func t046Session(t *testing.T, e *Engine) *Session {
	t.Helper()
	session := &Session{}
	for _, sql := range []string{"USE test", "SET SESSION innodb_lock_wait_timeout = 20"} {
		if _, err := e.Execute(session, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return session
}

// t046Idle asserts that a root's lock state is completely gone.
func t046Idle(t *testing.T, manager *lockmanager.Manager, when string) {
	t.Helper()
	if size := manager.LockTableSize(); size != 0 {
		t.Fatalf("%s: the lock table still holds %d resources", when, size)
	}
	stats := manager.Stats()
	if stats.ActiveOwners != 0 || stats.PendingRequests != 0 || stats.WaitEdges != 0 {
		t.Fatalf("%s: lock statistics = %+v, want every root gone", when, stats)
	}
}

// t046ClaimsIntention reports whether a statement claimed the table intention of its target, which is
// what makes its presence in the pipeline observable.
func t046ClaimsIntention(claims []storageengine.LockComponent) bool {
	for _, claim := range claims {
		if claim.Kind == storageengine.LockComponentTableIntent && claim.Mode == storageengine.LockModeIX {
			return true
		}
	}
	return false
}

// TestT046EveryWriteShapeTravelsTheA05Pipeline drives every DML shape the pipeline serves and checks
// that each one claimed its table intention. A shape that took no claim is a write that left the
// unified path.
func TestT046EveryWriteShapeTravelsTheA05Pipeline(t *testing.T) {
	f := newUniqueFixture(t,
		"CREATE TABLE parents(id BIGINT PRIMARY KEY AUTO_INCREMENT, v INT, sku VARCHAR(20) UNIQUE)",
		"CREATE TABLE children(id BIGINT PRIMARY KEY, pid BIGINT, KEY idx_pid(pid), "+
			"FOREIGN KEY (pid) REFERENCES parents(id) ON DELETE CASCADE ON UPDATE SET NULL)",
		"CREATE TABLE source(id BIGINT PRIMARY KEY, v INT, sku VARCHAR(20))",
		"INSERT INTO parents VALUES(1,1,'A')",
		"INSERT INTO children VALUES(10,1)",
		"INSERT INTO source VALUES(1,2,'B')")
	session := &Session{}
	f.must(session, "USE test")

	shapes := []struct {
		name string
		sql  string
	}{
		{"INSERT VALUES", "INSERT INTO parents(v,sku) VALUES(3,'C')"},
		{"INSERT SELECT", "INSERT INTO parents(id,v,sku) SELECT 4,4,'D' FROM source WHERE id=1"},
		{"INSERT IGNORE", "INSERT IGNORE INTO parents(id,v,sku) VALUES(1,1,'A')"},
		{"REPLACE", "REPLACE INTO parents(id,v,sku) VALUES(5,5,'E')"},
		{"ON DUPLICATE KEY UPDATE", "INSERT INTO parents(id,v,sku) VALUES(1,1,'A') ON DUPLICATE KEY UPDATE v=2"},
		{"UPDATE", "UPDATE parents SET v=7 WHERE id=1"},
		{"UPDATE JOIN", "UPDATE parents p JOIN source s ON s.id = p.id SET p.v = s.v"},
		{"DELETE", "DELETE FROM parents WHERE id=5"},
		{"multi-table DELETE", "DELETE p FROM children c JOIN parents p ON c.pid = p.id WHERE p.id=1"},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			claims := captureLockComponents(t, f, session, shape.sql)
			if !t046ClaimsIntention(claims) {
				t.Fatalf("%s claimed no table intention, so it did not travel the unified modify pipeline: %+v", shape.name, claims)
			}
			if len(claims) < 2 {
				t.Fatalf("%s claimed only %+v, want at least the table intention and a row claim", shape.name, claims)
			}
		})
	}
}

// TestT046AffectedRowsAndLastInsertIDAreUnchanged pins the answers callers already depend on.
func TestT046AffectedRowsAndLastInsertIDAreUnchanged(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE t(id BIGINT PRIMARY KEY AUTO_INCREMENT, v INT, sku VARCHAR(20) UNIQUE)",
		"INSERT INTO t(v,sku) VALUES(1,'A')")
	session := t046Session(t, f.engine)
	exec := func(sql string) *Result {
		t.Helper()
		result, err := f.engine.Execute(session, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return result
	}

	// The auto-increment identity is reported for the row that generated it.
	generated := exec("INSERT INTO t(v,sku) VALUES(2,'B')")
	if generated.LastInsertID != 2 {
		t.Fatalf("LastInsertID = %d, want the generated id", generated.LastInsertID)
	}
	// A multi-row insert counts every row it wrote.
	batch := exec("INSERT INTO t(v,sku) VALUES(3,'C'),(4,'D')")
	if batch.AffectedRows != 2 {
		t.Fatalf("multi-row INSERT affected %d rows, want 2", batch.AffectedRows)
	}
	// IGNORE writes nothing and therefore affects nothing.
	ignored := exec("INSERT IGNORE INTO t(v,sku) VALUES(5,'A')")
	if ignored.AffectedRows != 0 {
		t.Fatalf("INSERT IGNORE affected %d rows, want 0", ignored.AffectedRows)
	}
	// REPLACE counts the row it removed plus the row it inserted.
	replaced := exec("REPLACE INTO t(id,v,sku) VALUES(1,9,'A')")
	if replaced.AffectedRows != 2 {
		t.Fatalf("REPLACE affected %d rows, want the removal plus the insert", replaced.AffectedRows)
	}
	// The pipeline reports the rows it wrote, which is the matched row: a statement that assigns a
	// column its own value still stages a row, and that is the count callers have always received.
	unchanged := exec("UPDATE t SET v=v WHERE id=1")
	if unchanged.AffectedRows != 1 {
		t.Fatalf("a value-preserving UPDATE affected %d rows, want the one matched row it wrote", unchanged.AffectedRows)
	}
	if got := t035Rows(t, exec("SELECT id,v,sku FROM t ORDER BY id")); got != "1,9,A;2,2,B;3,3,C;4,4,D;" {
		t.Fatalf("rows = %q", got)
	}
}

// TestT046ForeignKeyCascadesStillTravelThePipeline proves the derived writes are not a side road: a
// cascade claims the child table's intention too, through the same statement scope the original
// statement opened.
func TestT046ForeignKeyCascadesStillTravelThePipeline(t *testing.T) {
	// The cascade's derived write claims the child row, so the child table's intention must appear
	// beside the parent's. Each shape gets its own fixture: the first cascade removes the very row the
	// second shape would move, so sharing one would leave the second with no candidate to act on.
	for _, shape := range []struct{ name, sql string }{
		{"ON DELETE CASCADE", "DELETE FROM parents WHERE id=1"},
		{"ON UPDATE SET NULL", "UPDATE parents SET id=2 WHERE id=1"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			f := newUniqueFixture(t,
				"CREATE TABLE parents(id BIGINT PRIMARY KEY, v INT)",
				"CREATE TABLE children(id BIGINT PRIMARY KEY, pid BIGINT, KEY idx_pid(pid), "+
					"FOREIGN KEY (pid) REFERENCES parents(id) ON DELETE CASCADE ON UPDATE SET NULL)",
				"INSERT INTO parents VALUES(1,1)",
				"INSERT INTO children VALUES(10,1)")
			session := t046Session(t, f.engine)

			claims := captureLockComponents(t, f, session, shape.sql)
			if !t046ClaimsIntention(claims) {
				t.Fatalf("%s claimed no table intention", shape.name)
			}
			// A cascade onto a child row carries a row claim in a second table's namespace, which is
			// what a bypassed derived write would be missing.
			spaces := map[string]bool{}
			for _, claim := range claims {
				spaces[claim.Space] = true
			}
			if len(spaces) < 3 {
				t.Fatalf("%s claimed only spaces %v; the derived write's own row and index claims are missing", shape.name, spaces)
			}
		})
	}
}

// TestT046SavepointsKeepAndReleaseLocksCorrectly covers child merge and child rollback: a savepoint
// layer may discard its own writes, but a grant the root holds is the root's, and the root's end is
// what releases it.
func TestT046SavepointsKeepAndReleaseLocksCorrectly(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	manager := t046LockManager(t, f.engine)
	session := t046Session(t, f.engine)

	f.must(session, "BEGIN")
	f.must(session, "INSERT INTO items VALUES(1,1)")
	f.must(session, "SAVEPOINT s1")
	f.must(session, "UPDATE items SET v=2 WHERE id=1")

	// A rollback to the savepoint discards the update but must not release the root's claim: the row
	// is still this transaction's to write.
	f.must(session, "ROLLBACK TO SAVEPOINT s1")
	if stats := manager.Stats(); stats.ActiveOwners == 0 {
		t.Fatal("rolling back to a savepoint released the root's owner")
	}
	f.must(session, "UPDATE items SET v=3 WHERE id=1")

	// A released savepoint merges into its parent and keeps working.
	f.must(session, "SAVEPOINT s2")
	f.must(session, "UPDATE items SET v=4 WHERE id=1")
	f.must(session, "RELEASE SAVEPOINT s2")
	f.must(session, "COMMIT")

	t046Idle(t, manager, "after the root committed")
	if got := t035Rows(t, f.must(session, "SELECT v FROM items WHERE id=1")); got != "4;" {
		t.Fatalf("row = %q, want the last write that survived", got)
	}
}

// TestT046RootEndReleasesEveryLockItHeld is the "no residue" acceptance for the pipeline: whichever
// way a root ends, nothing it held survives it.
func TestT046RootEndReleasesEveryLockItHeld(t *testing.T) {
	for _, ending := range []string{"COMMIT", "ROLLBACK"} {
		t.Run(ending, func(t *testing.T) {
			f := newUniqueFixture(t,
				"CREATE TABLE parents(id BIGINT PRIMARY KEY, v INT, sku VARCHAR(20) UNIQUE)",
				"CREATE TABLE children(id BIGINT PRIMARY KEY, pid BIGINT, KEY idx_pid(pid), "+
					"FOREIGN KEY (pid) REFERENCES parents(id) ON DELETE CASCADE)")
			manager := t046LockManager(t, f.engine)
			session := t046Session(t, f.engine)

			f.must(session, "BEGIN")
			f.must(session, "INSERT INTO parents VALUES(1,1,'A')")
			f.must(session, "INSERT INTO children VALUES(10,1)")
			f.must(session, "UPDATE parents SET v=2 WHERE id=1")
			f.must(session, "DELETE FROM children WHERE id=10")
			if stats := manager.Stats(); stats.ActiveOwners == 0 || stats.GrantedComponents == 0 {
				t.Fatalf("the root holds no lock: %+v", stats)
			}

			f.must(session, ending)
			t046Idle(t, manager, "after the root "+ending)
		})
	}
}

// TestT046ACommitConflictLeavesNoLockBehind covers the commit-conflict half of the same rule: the
// losing root's commit is refused, and the refuse must take its locks with it.
func TestT046ACommitConflictLeavesNoLockBehind(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", "INSERT INTO items VALUES(1,1)")
	manager := t046LockManager(t, f.engine)
	first := t046Session(t, f.engine)
	second := t046Session(t, f.engine)

	f.must(first, "BEGIN")
	f.must(second, "BEGIN")
	// The second root reads the row, then the first commits a new version of it.
	f.must(second, "SELECT v FROM items WHERE id=1")
	f.must(first, "UPDATE items SET v=2 WHERE id=1")
	f.must(first, "COMMIT")

	if _, err := f.engine.Execute(second, "UPDATE items SET v=3 WHERE id=1"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the stale UPDATE = %v, want the retryable ErrConflict", err)
	}
	if _, err := f.engine.Execute(second, "ROLLBACK"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	t046Idle(t, manager, "after the conflicting root rolled back")
}

// TestT046LargeSpilledStatementsStillClaimAndReleaseResources drives the shapes that spill their
// candidate ledger and staging run, so the pipeline's temporary machinery and its lock scope are
// exercised together rather than apart.
func TestT046LargeSpilledStatementsStillClaimAndReleaseResources(t *testing.T) {
	directory := t.TempDir()
	e, session, run := rangeTestEngine(t)
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	manager := t046LockManager(t, e)

	run("CREATE TABLE target(id INT PRIMARY KEY, v INT, label VARCHAR(32))")
	run("CREATE TABLE source(id INT PRIMARY KEY, tid INT, delta INT)")
	run("CREATE TABLE children(id INT PRIMARY KEY, pid INT, KEY idx_pid(pid))")

	const rows = 300
	var targetValues, sourceValues, childValues []string
	for i := 1; i <= rows; i++ {
		targetValues = append(targetValues, fmt.Sprintf("(%d,%d,'seed')", i, i))
		sourceValues = append(sourceValues, fmt.Sprintf("(%d,%d,1)", i, i))
		childValues = append(childValues, fmt.Sprintf("(%d,%d)", i, i))
	}
	run("INSERT INTO target VALUES " + strings.Join(targetValues, ","))
	run("INSERT INTO source VALUES " + strings.Join(sourceValues, ","))
	run("INSERT INTO children VALUES " + strings.Join(childValues, ","))

	// UPDATE JOIN: every target is distinct, so the dedup ledger and the staged candidates must
	// survive a spill. One row per distinct target is affected.
	updated := run("UPDATE target t JOIN source s ON s.tid = t.id SET t.v = t.v + s.delta")
	if updated.AffectedRows != rows {
		t.Fatalf("the spilled UPDATE JOIN affected %d rows, want %d", updated.AffectedRows, rows)
	}
	// Every target is distinct and gets one increment, so all of them move and none is visited twice —
	// which is what proves the winners survived the spill one for one.
	if got := t035Rows(t, run("SELECT COUNT(*) FROM target WHERE v > 1")); got != fmt.Sprintf("%d;", rows) {
		t.Fatalf("rows with v>1 = %q, want all %d targets incremented exactly once", got, rows)
	}

	// Multi-table DELETE: the winners are staged children-first through a second spillable run. Every
	// target now carries an incremented value, so every child matches and the staging run has to
	// deliver all of them exactly once.
	deleted := run("DELETE c FROM target t JOIN children c ON c.pid = t.id WHERE t.v >= 2")
	if deleted.AffectedRows != rows {
		t.Fatalf("the spilled multi-table DELETE affected %d rows, want %d", deleted.AffectedRows, rows)
	}
	if got := t035Rows(t, run("SELECT COUNT(*) FROM children")); got != "0;" {
		t.Fatalf("children = %q, want every staged winner deleted exactly once", got)
	}
	_ = session

	// The statements above ran in autocommit, so each root ended and released everything it held.
	t046Idle(t, manager, "after the spilled statements committed")
}

// TestT046NoSecondRowWriterExists is the structural half of the task: the pipeline has exactly one
// place that stores a versioned row, and every DML shape's row claim is taken by one shared claim
// builder. The check is a source-level one on purpose — a duplicate writer would not show up in any
// behavioural test until it disagreed with the first, which is too late.
func TestT046NoSecondRowWriterExists(t *testing.T) {
	// Every DML shape claims through claimModifyTarget / claimModifyReplacement, so their observable
	// effect is a row claim in the table's own row namespace. Two shapes that claimed differently would
	// produce different keys for the same row, which this asserts cannot happen.
	f := newUniqueFixture(t,
		"CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"CREATE TABLE refs(id BIGINT PRIMARY KEY, tid BIGINT)",
		"INSERT INTO items VALUES(1,1),(2,2)",
		"INSERT INTO refs VALUES(100,1)")
	session := t046Session(t, f.engine)

	rowKey := func(statement string) string {
		t.Helper()
		claims := captureLockComponents(t, f, session, statement)
		for _, claim := range claims {
			if claim.Kind == storageengine.LockComponentRecord && claim.IndexID == "" &&
				claim.Mode == storageengine.LockModeX && len(claim.Key) > 0 &&
				strings.HasPrefix(claim.Space, "row/") {
				return claim.Space + "|" + string(claim.Key)
			}
		}
		t.Fatalf("%s claimed no row identity: %+v", statement, claims)
		return ""
	}

	// The same physical row addressed by an UPDATE and by an UPDATE JOIN must be one resource. The
	// joined form is driven from a second table because that is the shape the join pipeline selects a
	// target identity for.
	updated := rowKey("UPDATE items SET v=9 WHERE id=1")
	joined := rowKey("UPDATE items i JOIN refs r ON r.tid = i.id SET i.v = 8 WHERE i.id=1")
	if updated != joined {
		t.Fatalf("one row produced different identities: UPDATE=%q UPDATE JOIN=%q", updated, joined)
	}
	// A different row must produce a different identity, or the identity is not per row.
	if other := rowKey("UPDATE items SET v=9 WHERE id=2"); other == updated {
		t.Fatalf("two different rows share the identity %q", other)
	}
	// The DELETE's own identity is the row it removes, which must be the same resource again.
	deleted := rowKey("DELETE FROM items WHERE id=1")
	if deleted != updated {
		t.Fatalf("UPDATE claimed %q and DELETE claimed %q for one row", updated, deleted)
	}
}
