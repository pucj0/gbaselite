package executor

import (
	"errors"
	"fmt"
	"testing"

	"gbaselite/storage"
)

// This file pins the UPDATE JOIN pipeline that User Story 2 establishes:
//
//	JOIN -> WHERE -> UpdateCandidate -> TargetRowDedup -> LIMIT -> UpdateOperator
//
// The tests here assert the pipeline's observable consequences rather than its shape, so
// they keep meaning after the internals move: one mutation per target, first match wins,
// LIMIT counting deduped targets, and joined values visible to the SET expressions.

func TestMVCCUpdateJoinFirstMatchWinsPerTarget(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT,label VARCHAR(10),KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,10),(2,20)")
	// Target 1 matches three source rows with distinguishable values. The winner must be
	// the first joined row, not the largest or the last one.
	run("INSERT INTO s VALUES(11,1,3,'three'),(12,1,7,'seven'),(13,1,4,'four'),(14,2,5,'five')")

	if affected := run("UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+s.delta").AffectedRows; affected != 2 {
		t.Fatalf("affected=%d, want one mutation per target", affected)
	}
	// 10+3 and 20+5: the first matching source row supplied each delta.
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 13] [2 25]]" {
		t.Fatalf("first-match deltas = %s", got)
	}
	// The same first-match choice must reach a non-additive expression, which cannot be
	// satisfied by applying every match.
	run("UPDATE t JOIN s ON s.tid=t.id SET t.v=s.delta")
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 3] [2 5]]" {
		t.Fatalf("first-match assignment = %s", got)
	}
}

func TestMVCCUpdateJoinLimitCountsDedupedTargets(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT,src VARCHAR(10))")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,label VARCHAR(10),KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,0,'n'),(2,0,'n'),(3,0,'n')")
	// Targets 1 and 2 each match two source rows, target 3 matches one.
	run("INSERT INTO s VALUES(11,1,'a'),(12,1,'b'),(13,2,'c'),(14,2,'d'),(15,3,'e')")

	// LIMIT 2 is satisfied by the first two *distinct targets*, even though reaching them
	// consumed four joined rows. A limit applied before dedup would stop after two joined
	// rows and still touch only target 1, so this distinguishes the two orders.
	if affected := run("UPDATE t JOIN s ON s.tid=t.id SET t.src=s.label LIMIT 2").AffectedRows; affected != 2 {
		t.Fatalf("affected=%d, want two deduped targets", affected)
	}
	if got := fmt.Sprint(run("SELECT id,src FROM t ORDER BY id").Rows); got != "[[1 a] [2 c] [3 n]]" {
		t.Fatalf("limits applied to deduped targets = %s", got)
	}
	// LIMIT 1 leaves the second target alone as well.
	if affected := run("UPDATE t JOIN s ON s.tid=t.id SET t.src='z' LIMIT 1").AffectedRows; affected != 1 {
		t.Fatalf("limit 1 affected=%d", affected)
	}
	if got := fmt.Sprint(run("SELECT id,src FROM t ORDER BY id").Rows); got != "[[1 z] [2 c] [3 n]]" {
		t.Fatalf("limit 1 touched the wrong target: %s", got)
	}
}

func TestMVCCUpdateJoinSetSeesJoinedSourceAndSequentialAssignments(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT,note VARCHAR(30))")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT,label VARCHAR(10),KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,10,'old')")
	run("INSERT INTO s VALUES(11,1,7,'seven')")

	// An assignment may read the joined source, and a later assignment sees what an
	// earlier one wrote to the target.
	run("UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+s.delta,t.note=CONCAT(s.label,'-',t.v)")
	if got := fmt.Sprint(run("SELECT v,note FROM t").Rows); got != "[[17 seven-17]]" {
		t.Fatalf("joined sequential assignment = %s", got)
	}
	// A qualified reference to the target keeps working alongside the source reference.
	run("UPDATE t JOIN s ON s.tid=t.id SET t.v=s.delta+t.v")
	if got := fmt.Sprint(run("SELECT v FROM t").Rows); got != "[[24]]" {
		t.Fatalf("qualified target reference = %s", got)
	}
}

func TestMVCCUpdateJoinWhereFiltersBeforeDedup(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT,label VARCHAR(10),KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,10),(2,20)")
	// Target 1's first source row is filtered out by WHERE, so the surviving source row
	// must supply the value.
	run("INSERT INTO s VALUES(11,1,3,'skip'),(12,1,7,'take'),(13,2,5,'take')")

	run("UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v+s.delta WHERE s.label='take'")
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 17] [2 25]]" {
		t.Fatalf("where-before-dedup = %s", got)
	}
	// A target whose every match is filtered out is not mutated at all.
	run("UPDATE t JOIN s ON s.tid=t.id SET t.v=0 WHERE s.delta>100")
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 17] [2 25]]" {
		t.Fatalf("fully filtered target was mutated: %s", got)
	}
}

func TestMVCCUpdateJoinLeftJoinVisitsEveryTargetOnce(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT,marker INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT,KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,10,0),(2,20,0),(3,30,0)")
	// Target 1 matches twice, target 2 once, target 3 not at all.
	run("INSERT INTO s VALUES(11,1,3),(12,1,7),(13,2,5)")

	// A LEFT JOIN updates every target: the matched ones once, the unmatched one with a
	// null-extended source.
	if affected := run("UPDATE t LEFT JOIN s ON s.tid=t.id SET t.marker=s.delta").AffectedRows; affected != 3 {
		t.Fatalf("left join affected=%d, want every target once", affected)
	}
	if got := fmt.Sprint(run("SELECT id,marker FROM t ORDER BY id").Rows); got != "[[1 3] [2 5] [3 <nil>]]" {
		t.Fatalf("left join rows = %s", got)
	}
}

func TestMVCCUpdateJoinConstraintFailureRollsBackWholeStatement(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT UNIQUE,score INT CHECK(score>=0))")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT,KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,10,1),(2,20,2)")
	run("INSERT INTO s VALUES(11,1,1),(12,2,1)")

	// Both targets collapse onto one unique value: the first write succeeds and the second
	// must fail, rolling the first back.
	if _, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=99"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("unique error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 10] [2 20]]" {
		t.Fatalf("failed unique UPDATE JOIN kept partial mutations: %s", got)
	}
	// A CHECK failure on a later target behaves the same way.
	if _, err := e.Execute(s, "UPDATE t JOIN s ON s.tid=t.id SET t.score=-1"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("check error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,score FROM t ORDER BY id").Rows); got != "[[1 1] [2 2]]" {
		t.Fatalf("failed check UPDATE JOIN kept partial mutations: %s", got)
	}
}

func TestMVCCUpdateJoinInvalidAssignmentFailsBeforeMutation(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,delta INT,KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,10)")
	run("INSERT INTO s VALUES(11,1,3)")

	for _, query := range []string{
		"UPDATE t JOIN s ON s.tid=t.id SET s.delta=0",
		"UPDATE t JOIN s ON s.tid=t.id SET t.missing=1",
		"UPDATE t JOIN s ON s.tid=t.id SET t.v=1,t.v=2",
		"UPDATE t JOIN s ON s.tid=t.id SET t.v=missing+1",
		"UPDATE t JOIN missing m ON m.id=t.id SET t.v=1",
	} {
		if _, err := e.Execute(s, query); err == nil {
			t.Errorf("accepted invalid UPDATE JOIN: %s", query)
		}
	}
	if got := fmt.Sprint(run("SELECT id,v FROM t").Rows); got != "[[1 10]]" {
		t.Fatalf("invalid UPDATE JOIN mutated rows: %s", got)
	}
}

func TestMVCCUpdateJoinPrimaryKeyUpdateUsesOldIdentity(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,10),(2,20)")
	run("INSERT INTO s VALUES(11,1),(12,2)")

	// The candidate identity is the old physical row, so the row moves once and the old
	// keys are released.
	if affected := run("UPDATE t JOIN s ON s.tid=t.id SET t.id=t.id+100").AffectedRows; affected != 2 {
		t.Fatalf("primary key UPDATE JOIN affected=%d", affected)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[101 10] [102 20]]" {
		t.Fatalf("primary key UPDATE JOIN rows = %s", got)
	}
	run("INSERT INTO t VALUES(1,0),(2,0)")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[4]]" {
		t.Fatalf("old keys were not released: %s", got)
	}
}
