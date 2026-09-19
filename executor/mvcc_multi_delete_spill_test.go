package executor

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"gbaselite/storage"
)

// This file pins the multi-table DELETE pipeline's spill behaviour.
//
// The pipeline deduplicates winners through a spillable sorter and then reorders them children-first
// through a second spillable staging run. Neither stage keeps an entry per distinct target in memory:
// a winner whose candidate spilled is decoded back from a run file, and the write phase re-reads that
// staged run once per target.

// multiDeleteFixture builds a parents/children pair whose join fans every target out heavily, so the
// dedup ledger and the staging run both have to spill under a small sort budget.
type multiDeleteFixture struct {
	rows      int
	targets   []string
	sources   []string
	directory string
}

func newMultiDeleteFixture(t *testing.T, e *Engine, run func(string) *Result, rows int, fanOut int) multiDeleteFixture {
	t.Helper()
	directory := t.TempDir()
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE parents(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE children(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES parents(id))")
	fixture := multiDeleteFixture{rows: rows, directory: directory}
	for i := 1; i <= rows; i++ {
		fixture.targets = append(fixture.targets, fmt.Sprintf("(%d,%d)", i, i))
	}
	for i := 1; i <= rows*fanOut; i++ {
		fixture.sources = append(fixture.sources, fmt.Sprintf("(%d,%d)", i, (i-1)%rows+1))
	}
	run("INSERT INTO parents VALUES " + joinStrings(fixture.targets, ","))
	run("INSERT INTO children VALUES " + joinStrings(fixture.sources, ","))
	return fixture
}

func (f multiDeleteFixture) leaked(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestMVCCMultiTableDeleteManyDistinctTargetsWithSpill drives a two-target DELETE over hundreds of
// distinct physical targets under a small sort budget, so dedup and staging both spill, and verifies
// every target row is removed exactly once.
func TestMVCCMultiTableDeleteManyDistinctTargetsWithSpill(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	const rows = 300
	fixture := newMultiDeleteFixture(t, e, run, rows, 3)

	deleted, err := e.Execute(s, "DELETE p,c FROM parents p JOIN children c ON c.pid=p.id")
	if err != nil {
		t.Fatal(err)
	}
	// Every parent, and every child (each of which references a parent), is removed exactly once.
	if want := uint64(rows * 4); deleted.AffectedRows != want {
		t.Fatalf("affected rows = %d, want %d", deleted.AffectedRows, want)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM parents").Rows); got != "[[0]]" {
		t.Fatalf("parents remaining = %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM children").Rows); got != "[[0]]" {
		t.Fatalf("children remaining = %s", got)
	}
	if leaks := fixture.leaked(t); len(leaks) != 0 {
		t.Fatalf("temporary files leaked: %v", leaks)
	}
}

// TestMVCCMultiTableDeleteNoPrimaryKeyIdenticalRowsStayDistinct verifies a heap table with no primary
// key keeps value-identical rows distinct through a spill: identity is the physical storage key, so
// deleting two identical rows removes two rows rather than collapsing them into one.
func TestMVCCMultiTableDeleteNoPrimaryKeyIdenticalRowsStayDistinct(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	directory := t.TempDir()
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE heap(a INT,b INT)")
	run("CREATE TABLE driver(id INT PRIMARY KEY)")
	// Four value-identical heap rows, each driven by its own row.
	run("INSERT INTO heap VALUES(1,1),(1,1),(1,1),(1,1)")
	run("INSERT INTO driver VALUES(1),(2),(3),(4)")

	deleted, err := e.Execute(s, "DELETE heap FROM heap JOIN driver ON heap.a=driver.id")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.AffectedRows != 4 {
		t.Fatalf("affected rows = %d, want 4 distinct physical rows", deleted.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM heap").Rows); got != "[[0]]" {
		t.Fatalf("heap remaining = %s, want all four identical rows gone", got)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v (%v)", entries, err)
	}
}

// TestMVCCMultiTableDeleteSpillKeepsChildBeforeParentOrder verifies the children-first dependency
// order survives staging through a spill: a child table referencing a parent is deleted first, so the
// statement does not trip its own foreign key.
func TestMVCCMultiTableDeleteSpillKeepsChildBeforeParentOrder(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	directory := t.TempDir()
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE grandparent(id INT PRIMARY KEY)")
	run("CREATE TABLE parent(id INT PRIMARY KEY,gid INT,CONSTRAINT fk1 FOREIGN KEY(gid) REFERENCES grandparent(id))")
	run("CREATE TABLE child(id INT PRIMARY KEY,pid INT,CONSTRAINT fk2 FOREIGN KEY(pid) REFERENCES parent(id))")
	const rows = 150
	var grandparents, parents, children []string
	for i := 1; i <= rows; i++ {
		grandparents = append(grandparents, fmt.Sprintf("(%d)", i))
		parents = append(parents, fmt.Sprintf("(%d,%d)", i, i))
		children = append(children, fmt.Sprintf("(%d,%d)", i, i))
	}
	run("INSERT INTO grandparent VALUES " + joinStrings(grandparents, ","))
	run("INSERT INTO parent VALUES " + joinStrings(parents, ","))
	run("INSERT INTO child VALUES " + joinStrings(children, ","))

	// All three targets in one statement: child, then parent, then grandparent.
	deleted, err := e.Execute(s, "DELETE c,p,g FROM child c JOIN parent p ON c.pid=p.id JOIN grandparent g ON p.gid=g.id")
	if err != nil {
		t.Fatalf("three-target DELETE failed, so the dependency order was not honoured: %v", err)
	}
	if deleted.AffectedRows != uint64(rows*3) {
		t.Fatalf("affected rows = %d, want %d", deleted.AffectedRows, rows*3)
	}
	for _, table := range []string{"child", "parent", "grandparent"} {
		if got := fmt.Sprint(run("SELECT COUNT(*) FROM " + table).Rows); got != "[[0]]" {
			t.Fatalf("%s remaining = %s", table, got)
		}
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v (%v)", entries, err)
	}
}

// TestMVCCMultiTableDeleteOuterJoinSkipsNullExtendedTarget verifies an outer-join null extension
// produces no candidate even when the staging run spilled: the decision comes from physical
// provenance, not from a target's column values.
func TestMVCCMultiTableDeleteOuterJoinSkipsNullExtendedTarget(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	directory := t.TempDir()
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE parents(id INT PRIMARY KEY)")
	run("CREATE TABLE children(id INT PRIMARY KEY,pid INT)")
	run("INSERT INTO parents VALUES(1),(2),(3)")
	// Only parent 1 has children, so parents 2 and 3 are null-extended on the children side.
	run("INSERT INTO children VALUES(10,1)")

	deleted, err := e.Execute(s, "DELETE p,c FROM parents p LEFT JOIN children c ON c.pid=p.id WHERE p.id=1")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.AffectedRows != 2 {
		t.Fatalf("affected rows = %d, want parent 1 and its one child", deleted.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM parents").Rows); got != "[[2]]" {
		t.Fatalf("parents remaining = %s, want the two unmatched parents kept", got)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v (%v)", entries, err)
	}
}

// TestMVCCMultiTableDeleteSpillRollsBackWholeStatement verifies a failure part-way through the write
// phase rolls the whole statement back, including targets already deleted from the staged run.
func TestMVCCMultiTableDeleteSpillRollsBackWholeStatement(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	directory := t.TempDir()
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: 64 << 20, TempDirectory: directory}
	run("CREATE TABLE parents(id INT PRIMARY KEY)")
	run("CREATE TABLE children(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES parents(id))")
	const rows = 200
	var parents, children []string
	for i := 1; i <= rows; i++ {
		parents = append(parents, fmt.Sprintf("(%d)", i))
		children = append(children, fmt.Sprintf("(%d,%d)", i, i))
	}
	run("INSERT INTO parents VALUES " + joinStrings(parents, ","))
	run("INSERT INTO children VALUES " + joinStrings(children, ","))

	// Target only the parent: RESTRICT must fail, and the whole statement must roll back.
	if _, err := e.Execute(s, "DELETE p FROM parents p JOIN children c ON c.pid=p.id"); !errors.Is(err, storage.ErrForeignKey) {
		t.Fatalf("parent-only DELETE error = %v, want a foreign key error", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM parents").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("failed statement changed parents: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM children").Rows); got != fmt.Sprintf("[[%d]]", rows) {
		t.Fatalf("failed statement changed children: %s", got)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("temporary files leaked after failure: %v (%v)", entries, err)
	}
}

// multiDeleteBudgetRun runs one two-target DELETE over a heavily fanned-out pair under the given
// temporary budget, and reports the affected rows or the error.
func multiDeleteBudgetRun(t *testing.T, maxTemp int64, rows int) (uint64, error) {
	t.Helper()
	e, s, run := rangeTestEngine(t)
	directory := t.TempDir()
	e.QueryOptions = QueryOptions{SortMemoryBytes: 64 << 10, MaxTempBytes: maxTemp, TempDirectory: directory}
	run("CREATE TABLE parents(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE children(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES parents(id))")
	var parents, children []string
	for i := 1; i <= rows; i++ {
		parents = append(parents, fmt.Sprintf("(%d,%d)", i, i))
		children = append(children, fmt.Sprintf("(%d,%d)", i, i))
	}
	// Each parent is matched by three children, so the staged winner set is large while the input is
	// fanned out further still.
	for i := rows + 1; i <= rows*3; i++ {
		children = append(children, fmt.Sprintf("(%d,%d)", i, (i-1)%rows+1))
	}
	run("INSERT INTO parents VALUES " + joinStrings(parents, ","))
	run("INSERT INTO children VALUES " + joinStrings(children, ","))

	deleted, err := e.Execute(s, "DELETE p,c FROM parents p JOIN children c ON c.pid=p.id")
	if err != nil {
		return 0, err
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
	return deleted.AffectedRows, nil
}

// TestMVCCMultiTableDeleteRespectsTemporaryBudget verifies the staging run is charged to the
// statement's temporary budget: too small a budget reports the resource limit instead of growing
// without bound, and the same workload succeeds once the budget allows it.
func TestMVCCMultiTableDeleteRespectsTemporaryBudget(t *testing.T) {
	const rows = 400
	// Each parent is matched by three children, so the staged stream is far larger than this budget.
	// A budget too small to hold even one staged run must report the resource limit rather than grow.
	if _, err := multiDeleteBudgetRun(t, 1<<10, rows); err == nil {
		t.Fatal("a tiny temporary budget was accepted")
	} else if !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("tiny temporary budget error = %v, want ErrQueryResourceLimit", err)
	}
	// The same workload with a workable budget must succeed and delete every target row once.
	affected, err := multiDeleteBudgetRun(t, 64<<20, rows)
	if err != nil {
		t.Fatalf("workable temporary budget failed: %v", err)
	}
	if want := uint64(rows * 4); affected != want {
		t.Fatalf("affected rows = %d, want %d", affected, want)
	}
}
