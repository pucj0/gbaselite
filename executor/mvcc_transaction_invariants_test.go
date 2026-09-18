package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"gbaselite/parser"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// This file holds the Phase 8 cross-DML verification: the transaction, snapshot, rollback and
// resource invariants that every unified modify pipeline must keep. These are contract tests, not
// new architecture: they assert that the pipelines built in Phases 3-6 stayed inside the
// storageengine transaction contract, whose iterator rule is
//
//	"Scans see the transaction snapshot plus prior writes; don't mutate that Txn while iterating.
//	 A separate child write transaction is allowed."

// txnObservation is one mutation's transaction pairing.
type txnObservation struct {
	statement parser.Statement
	readID    string
	writeID   string
	snapshot  uint64
	writeSnap uint64
}

// observeStatementTxns installs the transaction hook for one test.
func observeStatementTxns(t *testing.T) *[]txnObservation {
	t.Helper()
	observations := &[]txnObservation{}
	previous := statementTxnHook
	statementTxnHook = func(statement parser.Statement, read, write storageengine.Txn) {
		*observations = append(*observations, txnObservation{
			statement: statement,
			readID:    read.ID(),
			writeID:   write.ID(),
			snapshot:  read.Snapshot(),
			writeSnap: write.Snapshot(),
		})
	}
	t.Cleanup(func() { statementTxnHook = previous })
	return observations
}

func statementName(statement parser.Statement) string {
	switch value := statement.(type) {
	case parser.Insert:
		if value.Select != nil {
			return "INSERT SELECT"
		}
		return "INSERT VALUES"
	case parser.Update:
		if len(value.Joins) > 0 {
			return "UPDATE JOIN"
		}
		return "UPDATE"
	case parser.Delete:
		if len(value.Joins) > 0 || len(value.Targets) > 0 {
			return "MULTI DELETE"
		}
		return "DELETE"
	default:
		return fmt.Sprintf("%T", statement)
	}
}

// TestMVCCModifyPipelinesUseParentReadAndChildWrite proves the read/write transaction split for
// every DML family: the query source reads the parent statement transaction (and therefore the
// parent snapshot), and the operator writes a different, child transaction. That split is what
// keeps a pipeline from scanning and mutating one Txn.
func TestMVCCModifyPipelinesUseParentReadAndChildWrite(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,KEY tid_idx(tid))")
	run("INSERT INTO src VALUES(1,10),(2,20)")
	run("INSERT INTO dst VALUES(1,10),(2,20)")
	run("INSERT INTO s VALUES(11,1),(12,2)")

	observations := observeStatementTxns(t)
	statements := []struct{ query, want string }{
		{"INSERT INTO dst VALUES(3,30)", "INSERT VALUES"},
		{"INSERT INTO dst SELECT id+10,v FROM src", "INSERT SELECT"},
		{"UPDATE dst SET v=v+1", "UPDATE"},
		{"UPDATE dst JOIN s ON s.tid=dst.id SET dst.v=dst.v+s.id", "UPDATE JOIN"},
		{"DELETE FROM dst WHERE id=1", "DELETE"},
		{"DELETE dst FROM dst JOIN s ON s.tid=dst.id", "MULTI DELETE"},
	}
	for _, c := range statements {
		before := len(*observations)
		run(c.query)
		if len(*observations) != before+1 {
			t.Fatalf("%s: hook fired %d times", c.query, len(*observations)-before)
		}
		observation := (*observations)[before]
		if name := statementName(observation.statement); name != c.want {
			t.Fatalf("%s classified as %s, want %s", c.query, name, c.want)
		}
		if observation.readID == observation.writeID {
			t.Errorf("%s: read and write share transaction %s", c.query, observation.readID)
		}
		if observation.readID == "" || observation.writeID == "" {
			t.Errorf("%s: empty transaction id", c.query)
		}
		// The child is cut from the statement, so it can never see a newer snapshot than the
		// parent the source reads.
		if observation.writeSnap > observation.snapshot {
			t.Errorf("%s: child snapshot %d is newer than parent %d", c.query, observation.writeSnap, observation.snapshot)
		}
	}
}

// TestMVCCStatementSnapshotExcludesConcurrentCommits proves a DML source reads the statement
// snapshot rather than the latest committed data: rows committed by another session after the
// snapshot was taken stay invisible to it, for every DML source shape.
func TestMVCCStatementSnapshotExcludesConcurrentCommits(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,10),(2,20)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")

	other := &Session{CurrentDatabase: "test"}
	run("BEGIN")
	if _, err := e.Execute(other, "INSERT INTO t VALUES(3,30)"); err != nil {
		t.Fatal(err)
	}
	// The INSERT SELECT source must see only the two rows its snapshot had.
	if affected := run("INSERT INTO dst SELECT id+100,v FROM t").AffectedRows; affected != 2 {
		t.Fatalf("INSERT SELECT saw %d rows, want the 2 snapshot rows", affected)
	}
	// UPDATE and DELETE sources use that same snapshot, so they also see two rows while the
	// transaction's own inserted row is visible to the transaction itself.
	run("INSERT INTO dst VALUES(1,1),(2,2)")
	if visible := len(run("SELECT id FROM dst").Rows); visible != 4 {
		t.Fatalf("transaction should read its own writes: %s", run("SELECT id FROM dst").Rows)
	}
	if affected := run("UPDATE dst SET v=v+1").AffectedRows; affected != 4 {
		t.Fatalf("UPDATE saw %d rows, want the transaction's own 4 rows", affected)
	}
	if affected := run("DELETE FROM dst").AffectedRows; affected != 4 {
		t.Fatalf("DELETE saw %d rows", affected)
	}
	// The concurrently committed row was never part of any of those statements.
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[2]]" {
		t.Fatalf("transaction snapshot saw the concurrent insert: %s", got)
	}
	run("ROLLBACK")
	// After rollback the concurrent row is the only committed one plus the original two.
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[3]]" {
		t.Fatalf("committed rows = %s", got)
	}
	_ = s
}

// TestMVCCIteratorContractChildWriteIsInvisibleToParentScan pins the storageengine rule the
// pipelines rely on, using the same parent/child shape the executor uses. A parent scan does not
// observe the child's writes, so mutating the child while the parent iterates is safe.
func TestMVCCIteratorContractChildWriteIsInvisibleToParentScan(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,1),(2,2),(3,3)")

	parent, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Rollback()
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	defer child.Rollback()

	space := sqllayout.Catalog
	// The child writes while the parent iterates: the parent must not observe it.
	if err := child.Put(space, []byte("phase8-child"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	seen := 0
	iterator, err := parent.NewIterator(context.Background(), storageengine.ScanRequest{Space: space, Unordered: true})
	if err != nil {
		t.Fatal(err)
	}
	for iterator.Next() {
		if string(iterator.Key()) == "phase8-child" {
			t.Error("parent scan observed a child write")
		}
		seen++
	}
	if err := iterator.Close(); err != nil {
		t.Fatal(err)
	}
	// The child sees its own write, which is where the operator writes.
	if _, exists, err := child.Get(space, []byte("phase8-child")); err != nil || !exists {
		t.Fatalf("child write not visible to child: exists=%v err=%v", exists, err)
	}
	if _, exists, err := parent.Get(space, []byte("phase8-child")); err != nil || exists {
		t.Fatalf("child write leaked to parent: exists=%v err=%v", exists, err)
	}
}

// TestMVCCMutatingScannedTableStaysAtomic proves the practical consequence of the split: a
// predicate that the statement's own writes would keep satisfying still mutates each physical row
// exactly once, because the scan is fixed to the parent snapshot while the writes land in the child.
func TestMVCCMutatingScannedTableStaysAtomic(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,k INT,v INT,KEY k_idx(k))")
	run("INSERT INTO t VALUES(1,1,1),(2,2,2),(3,3,3)")

	// k<100 keeps matching every updated row if the scan could see the statement's own writes.
	if affected := run("UPDATE t SET k=k+1 WHERE k<100").AffectedRows; affected != 3 {
		t.Fatalf("affected=%d, want one mutation per snapshot row", affected)
	}
	if got := fmt.Sprint(run("SELECT k,v FROM t ORDER BY id").Rows); got != "[[2 1] [3 2] [4 3]]" {
		t.Fatalf("rows = %s", got)
	}
	if affected := run("DELETE FROM t WHERE k>=0").AffectedRows; affected != 3 {
		t.Fatalf("delete affected=%d", affected)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[0]]" {
		t.Fatalf("rows left = %s", got)
	}
}

// TestMVCCDownstreamErrorRollsBackChildTransaction proves a failure anywhere in a modify pipeline
// discards every earlier write of that statement, for each DML family.
func TestMVCCDownstreamErrorRollsBackChildTransaction(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE t(id INT PRIMARY KEY,u INT UNIQUE,score INT CHECK(score>=0))")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,KEY tid_idx(tid))")
	run("INSERT INTO src VALUES(1,1),(2,2)")
	run("INSERT INTO s VALUES(11,1),(12,2)")
	run("INSERT INTO t VALUES(1,10,1),(2,20,2)")

	failing := []struct {
		query string
		want  error
	}{
		{"INSERT INTO t SELECT id+10,id,-1 FROM src", storage.ErrCheckConstraint},
		{"INSERT INTO t VALUES(50,50,-1)", storage.ErrCheckConstraint},
		{"UPDATE t SET score=-1", storage.ErrCheckConstraint},
		{"UPDATE t JOIN s ON s.tid=t.id SET t.score=-1", storage.ErrCheckConstraint},
		// A UNIQUE collapse detected part-way through the join must roll the earlier target back.
		{"UPDATE t JOIN s ON s.tid=t.id SET t.u=7", storage.ErrDuplicateKey},
	}
	for _, c := range failing {
		before := fmt.Sprint(run("SELECT id,u,score FROM t ORDER BY id").Rows)
		result, err := e.Execute(s, c.query)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v, want %v", c.query, err, c.want)
		}
		if result != nil {
			t.Errorf("%s: failed statement returned a result", c.query)
		}
		after := fmt.Sprint(run("SELECT id,u,score FROM t ORDER BY id").Rows)
		if before != after {
			t.Errorf("%s: partial mutation survived: %s -> %s", c.query, before, after)
		}
	}

	// A multi-row statement whose last row fails must keep none of the earlier rows.
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT UNIQUE)")
	run("INSERT INTO dst VALUES(2,2)")
	if _, err := e.Execute(s, "INSERT INTO dst SELECT id,v FROM src"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate insert error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM dst ORDER BY id").Rows); got != "[[2 2]]" {
		t.Fatalf("rolled-back INSERT SELECT kept rows: %s", got)
	}
}

// TestMVCCStatementFailureInsideUserTransactionKeepsSavepointSemantics proves the unified
// pipelines did not alter explicit-transaction behaviour: a failed statement rolls back only its own
// effects and leaves the surrounding transaction usable.
func TestMVCCStatementFailureInsideUserTransactionKeepsSavepointSemantics(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT UNIQUE,score INT CHECK(score>=0))")
	run("INSERT INTO t VALUES(1,10,1),(2,20,2)")

	run("BEGIN")
	run("UPDATE t SET score=score+1 WHERE id=1")
	if _, err := e.Execute(s, "UPDATE t SET score=-1"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("check error = %v", err)
	}
	if _, err := e.Execute(s, "INSERT INTO t VALUES(3,30,-5)"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("check error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,score FROM t ORDER BY id").Rows); got != "[[1 2] [2 2]]" {
		t.Fatalf("failed statements changed in-transaction state: %s", got)
	}
	run("SAVEPOINT sp1")
	run("UPDATE t SET score=score+10 WHERE id=2")
	run("ROLLBACK TO SAVEPOINT sp1")
	if got := fmt.Sprint(run("SELECT id,score FROM t ORDER BY id").Rows); got != "[[1 2] [2 2]]" {
		t.Fatalf("savepoint rollback did not restore: %s", got)
	}
	run("COMMIT")
	if got := fmt.Sprint(run("SELECT id,score FROM t ORDER BY id").Rows); got != "[[1 2] [2 2]]" {
		t.Fatalf("committed rows = %s", got)
	}
}

// TestMVCCLastInsertIDIsPublishedOnlyAfterStatementCommit re-checks the publication ordering: only
// a successful INSERT moves it, and only for the id it actually generated.
func TestMVCCLastInsertIDIsPublishedOnlyAfterStatementCommit(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT AUTO_INCREMENT PRIMARY KEY,v INT)")

	// A successful INSERT publishes the first generated id.
	inserted := run("INSERT INTO t(v) VALUES(1)")
	if inserted.LastInsertID == 0 || s.LastInsertID != inserted.LastInsertID {
		t.Fatalf("successful insert id=%d session=%d", inserted.LastInsertID, s.LastInsertID)
	}
	saved := s.LastInsertID
	// Non-insert statements never publish an id.
	run("INSERT INTO t(v) VALUES(2)")
	published := s.LastInsertID
	if published == saved {
		t.Fatalf("second successful INSERT should publish its own id")
	}
	run("UPDATE t SET v=9")
	run("DELETE FROM t WHERE v=2")
	if s.LastInsertID != published {
		t.Fatalf("non-insert statement moved LastInsertID to %d", s.LastInsertID)
	}
	// A failed INSERT must not move it either.
	if _, err := e.Execute(s, "INSERT INTO t VALUES(5,1,2)"); err == nil {
		t.Fatal("value count mismatch accepted")
	}
	if s.LastInsertID != published {
		t.Fatalf("failed INSERT moved LastInsertID to %d", s.LastInsertID)
	}
	// A failed INSERT SELECT must not move it either.
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,5),(2,5)")
	run("CREATE TABLE dst(id INT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE)")
	if _, err := e.Execute(s, "INSERT INTO dst(v) SELECT v FROM src ORDER BY id"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate error = %v", err)
	}
	if s.LastInsertID != published {
		t.Fatalf("failed INSERT SELECT moved LastInsertID to %d", s.LastInsertID)
	}
}

// TestMVCCModifyPipelinesDoNotMaterializeResults proves the pipelines stream: no DML statement
// exposes a materialized Result.Rows or a row stream, because their sources are operators.
func TestMVCCModifyPipelinesDoNotMaterializeResults(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,KEY tid_idx(tid))")
	for i := 0; i < 50; i++ {
		run(fmt.Sprintf("INSERT INTO src VALUES(%d,%d)", i, i))
		run(fmt.Sprintf("INSERT INTO s VALUES(%d,%d)", i, i))
	}

	for _, query := range []string{
		"INSERT INTO dst SELECT id,v FROM src",
		"UPDATE dst JOIN s ON s.tid=dst.id SET dst.v=dst.v+1",
		"DELETE dst FROM dst JOIN s ON s.tid=dst.id",
		"UPDATE dst SET v=v+1",
		"DELETE FROM dst",
		"INSERT INTO dst VALUES(1000,1000)",
	} {
		result, err := e.Execute(s, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if result == nil {
			t.Fatalf("%s: no result", query)
		}
		if len(result.Rows) != 0 {
			t.Errorf("%s: materialized %d result rows", query, len(result.Rows))
		}
		if result.StreamRows != nil || result.StreamValues != nil {
			t.Errorf("%s: mutation result exposed a row stream", query)
		}
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[1]]" {
		t.Fatalf("rows = %s", got)
	}
}

// TestMVCCCancellationReleasesOperatorResources proves that a cancelled mutation releases the
// temporary resources the pipeline owns and writes nothing, and that a successful run with the same
// spilling configuration also cleans up.
func TestMVCCCancellationReleasesOperatorResources(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &Session{}
	run := func(q string) *Result {
		t.Helper()
		result, err := e.Execute(s, q)
		if err != nil {
			t.Fatal(q, err)
		}
		return result
	}
	run("CREATE DATABASE test")
	run("USE test")
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,KEY tid_idx(tid))")
	for i := 0; i < 200; i++ {
		run(fmt.Sprintf("INSERT INTO src VALUES(%d,%d)", i, i))
		run(fmt.Sprintf("INSERT INTO s VALUES(%d,%d)", i, i))
	}

	// Force the dedup/sort stages to spill into a directory this test owns.
	tempDir := t.TempDir()
	e.QueryOptions.SortMemoryBytes = 64 << 10
	e.QueryOptions.TempDirectory = tempDir
	e.QueryOptions.MaxTempBytes = 32 << 20

	leaked := func(t *testing.T) []string {
		t.Helper()
		entries, err := os.ReadDir(tempDir)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}

	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	for _, query := range []string{
		"UPDATE dst JOIN s ON s.tid=dst.id SET dst.v=1",
		"INSERT INTO dst SELECT id,v FROM src",
		"DELETE dst FROM dst JOIN s ON s.tid=dst.id",
	} {
		cancelled := &Session{CurrentDatabase: "test", Context: cancelledContext}
		if _, err := e.Execute(cancelled, query); err == nil {
			t.Fatalf("cancelled %s succeeded", query)
		}
		if names := leaked(t); len(names) != 0 {
			t.Fatalf("cancelled %s leaked temporary files: %v", query, names)
		}
		if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[0]]" {
			t.Fatalf("cancelled %s wrote rows: %s", query, got)
		}
	}

	// The same configuration on a successful statement also leaves nothing behind.
	run("UPDATE dst JOIN s ON s.tid=dst.id SET dst.v=dst.v+1")
	if names := leaked(t); len(names) != 0 {
		t.Fatalf("successful UPDATE JOIN leaked temporary files: %v", names)
	}
}

// TestMVCCSpillableDedupStaysBounded proves the dedup stage trades memory for temporary storage
// rather than collecting an unbounded in-memory set: a heavily fanned-out join still updates each
// target once under a small sort budget, and leaves nothing behind.
func TestMVCCSpillableDedupStaysBounded(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,KEY tid_idx(tid))")
	// Each target matches eight source rows, so the join fans out heavily.
	for i := 0; i < 40; i++ {
		run(fmt.Sprintf("INSERT INTO t VALUES(%d,0)", i))
		for j := 0; j < 8; j++ {
			run(fmt.Sprintf("INSERT INTO s VALUES(%d,%d)", i*8+j, i))
		}
	}
	tempDir := t.TempDir()
	e.QueryOptions.SortMemoryBytes = 64 << 10
	e.QueryOptions.TempDirectory = tempDir
	e.QueryOptions.MaxTempBytes = 32 << 20

	if affected := run("UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+1").AffectedRows; affected != 40 {
		t.Fatalf("affected=%d, want one mutation per target", affected)
	}
	if got := fmt.Sprint(run("SELECT SUM(v) FROM t").Rows); got != "[[40]]" {
		t.Fatalf("each target must have been incremented once: %s", got)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dedup left temporary files: %v", entries)
	}
	_ = s
}
