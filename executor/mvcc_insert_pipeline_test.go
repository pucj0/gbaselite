package executor

import (
	"errors"
	"fmt"
	"testing"

	"gbaselite/storage"
)

// This file holds the User Story 1 regression tests: it proves that both INSERT
// sources reach one operator, and pins the auto-increment and rollback semantics the
// unified pipeline must preserve.

// insertPipelineTrace records the candidates the unified INSERT path wrote.
type insertPipelineTrace struct {
	targets map[string]int
	rows    int
	values  map[int]uint64
}

// traceInsertPipeline installs the operator instrumentation for one test and
// restores the previous hook afterwards.
func traceInsertPipeline(t *testing.T) *insertPipelineTrace {
	t.Helper()
	trace := &insertPipelineTrace{targets: map[string]int{}, values: map[int]uint64{}}
	previous := insertApplyHook
	insertApplyHook = func(candidate InsertCandidate, engine string) {
		trace.targets[engine]++
		trace.values[trace.rows] = candidate.Ordinal
		trace.rows++
	}
	t.Cleanup(func() { insertApplyHook = previous })
	return trace
}

func TestMVCCInsertValuesAndSelectShareOneOperator(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,10),(2,20)")

	trace := traceInsertPipeline(t)

	// INSERT VALUES: three literal rows, walked in statement order.
	if got := run("INSERT INTO dst VALUES(1,10),(2,20),(3,30)").AffectedRows; got != 3 {
		t.Fatalf("INSERT VALUES affected=%d", got)
	}
	if trace.targets["test.dst"] != 3 || trace.rows != 3 {
		t.Fatalf("INSERT VALUES wrote %d rows to %v, want 3 into test.dst", trace.rows, trace.targets)
	}
	if trace.values[0] != 0 || trace.values[1] != 1 || trace.values[2] != 2 {
		t.Fatalf("INSERT VALUES ordinals = %v, want 0,1,2", trace.values)
	}

	// INSERT SELECT: the same destination, a completely different source.
	if got := run("INSERT INTO dst SELECT id+10,v FROM src ORDER BY id").AffectedRows; got != 2 {
		t.Fatalf("INSERT SELECT affected=%d", got)
	}
	if trace.targets["test.dst"] != 5 {
		t.Fatalf("destination totals = %v, want both sources", trace.targets)
	}
	// INSERT SELECT walks its source from the beginning of the statement.
	if trace.values[3] != 0 || trace.values[4] != 1 {
		t.Fatalf("INSERT SELECT ordinals = %d,%d want 0,1", trace.values[3], trace.values[4])
	}

	// A statement the operator rejects changes nothing: the destination keeps what it
	// had, and no generated id is published.
	if _, err := e.Execute(s, "INSERT INTO dst VALUES(1,1)"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[5]]" {
		t.Fatalf("destination rows = %s", got)
	}
	if s.LastInsertID != 0 {
		t.Fatalf("failed INSERT published LastInsertID=%d", s.LastInsertID)
	}
}

func TestMVCCInsertSetUsesTheSameOperator(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")

	trace := traceInsertPipeline(t)
	// INSERT SET is rewritten into the single-row VALUES form, so it goes through the
	// same operator with ordinal 0.
	if got := run("INSERT INTO dst SET id=7,v=9").AffectedRows; got != 1 {
		t.Fatalf("INSERT SET affected=%d", got)
	}
	if trace.rows != 1 || trace.values[0] != 0 || trace.targets["test.dst"] != 1 {
		t.Fatalf("INSERT SET trace = %+v", trace)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM dst").Rows); got != "[[7 9]]" {
		t.Fatalf("rows = %s", got)
	}
}

func TestMVCCInsertSelfSourceSnapshotThroughOperator(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,10),(2,20)")
	// The SELECT reads the parent statement snapshot, so the newly inserted rows are
	// invisible to the same statement's source and cannot be copied again.
	inserted := run("INSERT INTO t SELECT id+10,v FROM t")
	if inserted.AffectedRows != 2 {
		t.Fatalf("affected rows=%d, want the two snapshot rows only", inserted.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 10] [2 20] [11 10] [12 20]]" {
		t.Fatalf("self-source re-read its own output: %s", got)
	}
	// A second pass sees exactly the rows committed by the first statement.
	run("INSERT INTO t SELECT id+100,v FROM t")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[8]]" {
		t.Fatalf("second self-source pass = %s", got)
	}
}

func TestMVCCInsertAutoIncrementAcrossValuesAndSelect(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT AUTO_INCREMENT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,10),(2,20),(3,30)")

	// Generated ids through INSERT VALUES.
	generated := run("INSERT INTO dst(v) VALUES(10),(20)")
	if generated.AffectedRows != 2 || generated.LastInsertID != 1 {
		t.Fatalf("VALUES generated = affected %d last id %d", generated.AffectedRows, generated.LastInsertID)
	}
	// The counter continues across statement kinds: SELECT takes the next ids.
	selected := run("INSERT INTO dst(v) SELECT v FROM src ORDER BY id")
	if selected.AffectedRows != 3 || selected.LastInsertID != 3 {
		t.Fatalf("SELECT generated = affected %d last id %d", selected.AffectedRows, selected.LastInsertID)
	}
	// An explicit id raises the counter floor, and both paths continue after it.
	explicit := run("INSERT INTO dst(id,v) VALUES(100,10)")
	if explicit.AffectedRows != 1 || explicit.LastInsertID != 0 {
		t.Fatalf("explicit id = affected %d last id %d", explicit.AffectedRows, explicit.LastInsertID)
	}
	if got := run("INSERT INTO dst(v) VALUES(55)").LastInsertID; got != 101 {
		t.Fatalf("VALUES after explicit id = %d, want 101", got)
	}
	if got := run("INSERT INTO dst(v) SELECT 66").LastInsertID; got != 102 {
		t.Fatalf("SELECT after explicit id = %d, want 102", got)
	}
	if got := fmt.Sprint(run("SELECT id FROM dst ORDER BY id").Rows); got != "[[1] [2] [3] [4] [5] [100] [101] [102]]" {
		t.Fatalf("generated ids = %s", got)
	}
}

func TestMVCCInsertAutoIncrementGapAfterRollbackIsPreserved(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE)")
	run("INSERT INTO src VALUES(1,5),(2,6)")
	run("INSERT INTO dst(v) VALUES(6)")

	// The first source row reserves an id and inserts; the second violates UNIQUE, so
	// the whole statement rolls back and the reserved id is not reused: a gap is
	// allowed and is the documented MVCC auto-increment behaviour.
	if _, err := e.Execute(s, "INSERT INTO dst(v) SELECT v FROM src ORDER BY id"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate key error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[1]]" {
		t.Fatalf("rolled-back INSERT SELECT kept rows: %s", got)
	}
	next := run("INSERT INTO dst(v) VALUES(7)")
	if next.LastInsertID <= 2 {
		t.Fatalf("reservation was reused after rollback: %d", next.LastInsertID)
	}
	if s.LastInsertID != next.LastInsertID {
		t.Fatalf("session last id = %d, want %d", s.LastInsertID, next.LastInsertID)
	}

	// The same holds for the VALUES path, which reserves the whole statement at once.
	run("CREATE TABLE t(id INT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE)")
	run("INSERT INTO t(v) VALUES(7)")
	reserved := run("INSERT INTO t(v) VALUES(8)")
	if _, err := e.Execute(s, "INSERT INTO t(v) VALUES(9),(9)"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate key error = %v", err)
	}
	if got := run("INSERT INTO t(v) VALUES(11)").LastInsertID; got <= reserved.LastInsertID {
		t.Fatalf("VALUES reservation reused after rollback: %d", got)
	}
}

func TestMVCCInsertStatementRollbackThroughOperator(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT UNIQUE,CHECK(v>=0))")
	run("CREATE TABLE parent(id INT PRIMARY KEY)")
	run("CREATE TABLE child(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES parent(id))")
	run("CREATE TABLE src_txt(id INT PRIMARY KEY,v VARCHAR(20))")
	run("CREATE TABLE dst_int(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,1),(2,2),(3,3)")
	run("INSERT INTO parent VALUES(1)")
	run("INSERT INTO src_txt VALUES(1,'abc')")
	run("INSERT INTO dst VALUES(2,2)")

	// One row per failure mode succeeds before the failing row, so each statement
	// must roll every earlier write back through the statement child transaction.
	cases := []struct {
		query string
		want  error
	}{
		{"INSERT INTO dst SELECT id,v FROM src", storage.ErrDuplicateKey},
		{"INSERT INTO dst SELECT id+100,-1 FROM src", storage.ErrCheckConstraint},
		{"INSERT INTO child SELECT id,99 FROM src", storage.ErrForeignKey},
		{"INSERT INTO dst_int SELECT id,v FROM src_txt", storage.ErrTypeMismatch},
		{"INSERT INTO dst VALUES(50,-1)", storage.ErrCheckConstraint},
		{"INSERT INTO child VALUES(50,99)", storage.ErrForeignKey},
	}
	for _, c := range cases {
		if _, err := e.Execute(s, c.query); !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v, want %v", c.query, err, c.want)
		}
	}
	if got := fmt.Sprint(run("SELECT id,v FROM dst ORDER BY id").Rows); got != "[[2 2]]" {
		t.Fatalf("failed INSERT kept partial rows: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM child").Rows); got != "[[0]]" {
		t.Fatalf("failed INSERT kept child rows: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst_int").Rows); got != "[[0]]" {
		t.Fatalf("failed INSERT kept converted rows: %s", got)
	}
	// A successful statement after the failures still commits normally.
	run("INSERT INTO dst VALUES(60,60)")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[2]]" {
		t.Fatalf("destination after recovery = %s", got)
	}
}

func TestMVCCInsertValueCountMismatchFailsBeforeWrite(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")
	for _, query := range []string{
		"INSERT INTO dst VALUES(1)",
		"INSERT INTO dst VALUES(1,2,3)",
	} {
		if _, err := e.Execute(s, query); err == nil {
			t.Errorf("accepted %s", query)
		}
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[0]]" {
		t.Fatalf("mismatched VALUES inserted rows: %s", got)
	}
}
