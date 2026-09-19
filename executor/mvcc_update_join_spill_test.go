package executor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"gbaselite/storage"
)

// joinStrings builds a VALUES list from already-formatted tuples.
func joinStrings(parts []string, separator string) string { return strings.Join(parts, separator) }

// This file pins the UPDATE JOIN pipeline's spill behaviour and the single-pass property.
//
// UPDATE JOIN builds a complete candidate — old identity, the target's own old row, and the whole
// joined evaluation row — while the joined row is in hand, and dedups those candidates through a
// spillable sorter. So a target matched many times costs one sorter entry, a winner whose candidate
// spilled is decoded back from a run file, and the join is never replayed to recover a payload.

// joinBuilds counts how many times the UPDATE JOIN pipeline binds its join for one statement.
func joinBuilds(t *testing.T) *int {
	t.Helper()
	previous := updateJoinBuildHook
	count := new(int)
	updateJoinBuildHook = func(*updateJoinPlan) { *count++ }
	t.Cleanup(func() { updateJoinBuildHook = previous })
	return count
}

// TestMVCCUpdateJoinRunsTheJoinOncePerStatement verifies the pipeline binds and runs the join once:
// there is no second pass that re-reads the join to materialise winners, which is what the previous
// ordinal-replay design needed.
func TestMVCCUpdateJoinRunsTheJoinOncePerStatement(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT)")
	run("INSERT INTO t VALUES(1,10),(2,20)")
	run("INSERT INTO s VALUES(1,1,5),(2,1,7),(3,2,9)")

	builds := joinBuilds(t)
	updated, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=v+s.delta")
	if err != nil {
		t.Fatal(err)
	}
	if updated.AffectedRows != 2 {
		t.Fatalf("affected rows = %d, want one per distinct target", updated.AffectedRows)
	}
	if *builds != 1 {
		t.Fatalf("UPDATE JOIN bound its pipeline %d times, want exactly 1 (no join replay)", *builds)
	}
}

// TestMVCCUpdateJoinLargeDistinctTargetsWithSmallSortMemory forces the dedup sorter to spill while
// every target is distinct, so the winner set is as large as the input and each winner's candidate
// has to survive the round trip.
func TestMVCCUpdateJoinLargeDistinctTargetsWithSmallSortMemory(t *testing.T) {
	directory := t.TempDir()
	e, s, run := rangeTestEngine(t)
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT,label VARCHAR(32))")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT)")

	const rows = 300
	var targetValues, sourceValues []string
	for i := 1; i <= rows; i++ {
		targetValues = append(targetValues, fmt.Sprintf("(%d,%d,'seed')", i, i*10))
		sourceValues = append(sourceValues, fmt.Sprintf("(%d,%d,%d)", i, i, i))
	}
	run("INSERT INTO t VALUES " + joinStrings(targetValues, ","))
	run("INSERT INTO s VALUES " + joinStrings(sourceValues, ","))

	updated, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+s.delta, t.label='touched'")
	if err != nil {
		t.Fatal(err)
	}
	if updated.AffectedRows != rows {
		t.Fatalf("affected rows = %d, want %d distinct targets", updated.AffectedRows, rows)
	}
	// Every candidate went through the sorter, so both the arithmetic and the joined-source label
	// prove the payload survived: v is target+source arithmetic, label is a source-independent value.
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE v=id*10+id AND label='touched'").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("rows updated from their own joined candidate = %s, want [[%d]]", got, rows)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}

// TestMVCCUpdateJoinRepeatedMatchesKeepFirstCandidate verifies first-match-wins when a target is
// matched by several source rows and the candidate payload carries the joined source columns: the
// assignment must read the *first* matching source row.
func TestMVCCUpdateJoinRepeatedMatchesKeepFirstCandidate(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT)")
	run("INSERT INTO t VALUES(1,0)")
	// Three matches for the same target, in source order.
	run("INSERT INTO s VALUES(1,1,100),(2,1,200),(3,1,300)")

	updated, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+s.delta")
	if err != nil {
		t.Fatal(err)
	}
	if updated.AffectedRows != 1 {
		t.Fatalf("affected rows = %d, want 1", updated.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT v FROM t").Rows); got != "[[100]]" {
		t.Fatalf("v = %s, want the first matching source row's delta", got)
	}
}

// TestMVCCUpdateJoinLimitCountsDistinctTargetsWithSpill verifies LIMIT is applied after dedup even
// when the dedup ledger spilled: a target matched by many source rows consumes one unit.
func TestMVCCUpdateJoinLimitCountsDistinctTargetsWithSpill(t *testing.T) {
	directory := t.TempDir()
	e, s, run := rangeTestEngine(t)
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT)")
	const rows = 200
	var targets, sources []string
	for i := 1; i <= rows; i++ {
		targets = append(targets, fmt.Sprintf("(%d,0)", i))
		// Each target matched three times.
		for m := 0; m < 3; m++ {
			sources = append(sources, fmt.Sprintf("(%d,%d)", (i-1)*3+m+1, i))
		}
	}
	run("INSERT INTO t VALUES " + joinStrings(targets, ","))
	run("INSERT INTO s VALUES " + joinStrings(sources, ","))

	updated, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=1 LIMIT 10")
	if err != nil {
		t.Fatal(err)
	}
	if updated.AffectedRows != 10 {
		t.Fatalf("affected rows = %d, want LIMIT to count 10 distinct targets", updated.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE v=1").Rows); got != "[[10]]" {
		t.Fatalf("updated rows = %s, want [[10]]", got)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}

// TestMVCCUpdateJoinSpillReleasesResourcesOnCancellation verifies a cancelled UPDATE JOIN that had
// already spilled leaves no temporary run behind.
func TestMVCCUpdateJoinSpillReleasesResourcesOnCancellation(t *testing.T) {
	directory := t.TempDir()
	e, s, run := rangeTestEngine(t)
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT)")
	var targets, sources []string
	for i := 1; i <= 200; i++ {
		targets = append(targets, fmt.Sprintf("(%d,0)", i))
		sources = append(sources, fmt.Sprintf("(%d,%d)", i, i))
	}
	run("INSERT INTO t VALUES " + joinStrings(targets, ","))
	run("INSERT INTO s VALUES " + joinStrings(sources, ","))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := &Session{CurrentDatabase: s.CurrentDatabase, Context: ctx}
	if _, err := e.Execute(cancelled, "UPDATE t JOIN s ON s.tid=t.id SET t.v=1"); err == nil {
		t.Fatal("a cancelled UPDATE JOIN succeeded")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled UPDATE JOIN leaked temporary files: %v", entries)
	}
	// Nothing was written by the cancelled statement.
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE v<>0").Rows); got != "[[0]]" {
		t.Fatalf("cancelled statement wrote rows: %s", got)
	}
}

// TestMVCCUpdateJoinCandidateCarriesJoinedSourceValues is the payload-fidelity check: an assignment
// that reads only joined-source columns must still see them after the candidate went through dedup.
func TestMVCCUpdateJoinCandidateCarriesJoinedSourceValues(t *testing.T) {
	directory := t.TempDir()
	e, s, run := rangeTestEngine(t)
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT,note VARCHAR(32))")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT,tag VARCHAR(32))")
	const rows = 150
	var targets, sources []string
	for i := 1; i <= rows; i++ {
		targets = append(targets, fmt.Sprintf("(%d,0,'original')", i))
		sources = append(sources, fmt.Sprintf("(%d,%d,%d,'from-source-%d')", i, i, i*3, i))
	}
	run("INSERT INTO t VALUES " + joinStrings(targets, ","))
	run("INSERT INTO s VALUES " + joinStrings(sources, ","))

	if _, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=s.delta, t.note=s.tag"); err != nil {
		t.Fatal(err)
	}
	// Every row must carry both joined-source values, which only survive if the candidate kept its
	// whole evaluation row across the spill.
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE v=id*3 AND note=CONCAT('from-source-',id)").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("rows carrying joined source values = %s, want [[%d]]", got, rows)
	}
	// The target's own old row also survived, so an assignment can read it.
	if _, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=v+1"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE v=id*3+1").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("rows whose old value was read = %s, want [[%d]]", got, rows)
	}
}

// TestMVCCUpdateJoinConstraintFailureRollsBackAfterSpill verifies the whole statement still rolls back
// when a constraint fails after the dedup ledger and candidate payload spilled.
func TestMVCCUpdateJoinConstraintFailureRollsBackAfterSpill(t *testing.T) {
	directory := t.TempDir()
	e, s, run := rangeTestEngine(t)
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT,CHECK(v<1000))")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT)")
	const rows = 200
	var targets, sources []string
	for i := 1; i <= rows; i++ {
		targets = append(targets, fmt.Sprintf("(%d,%d)", i, i))
		// The last source row pushes its target over the CHECK bound.
		delta := 1
		if i == rows {
			delta = 5000
		}
		sources = append(sources, fmt.Sprintf("(%d,%d,%d)", i, i, delta))
	}
	run("INSERT INTO t VALUES " + joinStrings(targets, ","))
	run("INSERT INTO s VALUES " + joinStrings(sources, ","))

	if _, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+s.delta"); err == nil {
		t.Fatal("a CHECK violation after a spill was accepted")
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE v=id").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("failed statement changed rows: %s, want all %d unchanged", got, rows)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files leaked after failure: %v", entries)
	}
}

// TestMVCCUpdateJoinPrimaryKeyUpdateKeepsOldIdentityAfterSpill verifies a primary-key UPDATE still
// addresses the row by its old physical identity when the candidate came back from a spill run.
func TestMVCCUpdateJoinPrimaryKeyUpdateKeepsOldIdentityAfterSpill(t *testing.T) {
	directory := t.TempDir()
	e, s, run := rangeTestEngine(t)
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,newid INT)")
	const rows = 120
	var targets, sources []string
	for i := 1; i <= rows; i++ {
		targets = append(targets, fmt.Sprintf("(%d,%d)", i, i))
		sources = append(sources, fmt.Sprintf("(%d,%d,%d)", i, i, i+1000))
	}
	run("INSERT INTO t VALUES " + joinStrings(targets, ","))
	run("INSERT INTO s VALUES " + joinStrings(sources, ","))

	if _, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.id=s.newid"); err != nil {
		t.Fatal(err)
	}
	// The old keys must be gone and the new ones present, one row each: that is only true if the
	// write path used each candidate's old identity rather than the new value.
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("row count = %s, want [[%d]] (no duplicated or lost rows)", got, rows)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE id>1000").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("new keys = %s, want [[%d]]", got, rows)
	}
}

// TestMVCCUpdateJoinSpillKeepsStorageTypes verifies a spilled candidate's values keep their declared
// column types. The write path validates them, so a widened stand-in would be a type error rather
// than a silent change.
func TestMVCCUpdateJoinSpillKeepsStorageTypes(t *testing.T) {
	directory := t.TempDir()
	e, s, run := rangeTestEngine(t)
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE t(id INT PRIMARY KEY,big BIGINT,f DOUBLE,d DECIMAL(10,2),txt VARCHAR(32),b BOOLEAN,dt DATETIME)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT)")
	const rows = 120
	var targets, sources []string
	for i := 1; i <= rows; i++ {
		targets = append(targets, fmt.Sprintf("(%d,%d,1.5,2.25,'text-%d',1,'2024-01-02 03:04:05')", i, i*1000, i))
		sources = append(sources, fmt.Sprintf("(%d,%d)", i, i))
	}
	run("INSERT INTO t VALUES " + joinStrings(targets, ","))
	run("INSERT INTO s VALUES " + joinStrings(sources, ","))

	// Touch every row so each candidate goes through the sorter, then read the typed columns back.
	if _, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.txt='touched', t.big=big+1, t.f=f+0.5, t.d=d+0.25, t.b=NOT b, t.dt='2025-05-06 07:08:09'"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE txt='touched' AND big=id*1000+1 AND f=2 AND d=2.5 AND b=0 AND dt='2025-05-06 07:08:09'").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("typed columns after spill = %s, want [[%d]]", got, rows)
	}
	result := run("SELECT big,f,d,txt,b,dt FROM t WHERE id=1")
	columns := result.Columns
	for i, want := range []storage.DataType{storage.TypeBigInt, storage.TypeDouble, storage.TypeDecimal, storage.TypeVarchar, storage.TypeBoolean, storage.TypeDateTime} {
		if columns[i].Type != want {
			t.Fatalf("column %s type = %s, want %s", columns[i].Name, columns[i].Type, want)
		}
	}
}
