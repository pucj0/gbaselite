package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"strings"
	"testing"
)

// This file holds the User Story 2 regression tests for the plain (non-JOIN) UPDATE
// path: access-plan preservation, sequential assignment, old physical identity,
// constraints, ON UPDATE timestamps and statement rollback.

func TestMVCCUpdateSequentialAssignmentOrder(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,a INT,b INT,c INT)")
	run("INSERT INTO t VALUES(1,10,0,0)")
	// Every assignment sees the assignments before it: b reads the *new* a, c reads the
	// new b.
	updated := run("UPDATE t SET a=a+1,b=a,c=b")
	if updated.AffectedRows != 1 {
		t.Fatalf("affected=%d", updated.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT a,b,c FROM t").Rows); got != "[[11 11 11]]" {
		t.Fatalf("sequential assignment = %s", got)
	}
	if got := fmt.Sprint(run("SELECT a FROM t").Rows); got != "[[11]]" {
		t.Fatalf("assignment = %s", got)
	}
	// Assigning the same target column twice is rejected as a binding error rather than
	// silently applying only one of them.
	if _, err := e.Execute(s, "UPDATE t SET a=1,a=2"); err == nil {
		t.Fatal("duplicate assignment target accepted")
	}
	if got := fmt.Sprint(run("SELECT a FROM t").Rows); got != "[[11]]" {
		t.Fatalf("rejected duplicate assignment changed rows: %s", got)
	}
	// A NULL old value stays NULL through arithmetic, matching the legacy engine.
	run("CREATE TABLE n(id INT PRIMARY KEY,a INT,b INT)")
	run("INSERT INTO n VALUES(1,NULL,0)")
	run("UPDATE n SET a=a+1,b=a")
	if got := fmt.Sprint(run("SELECT a,b FROM n").Rows); got != "[[<nil> <nil>]]" {
		t.Fatalf("null propagation = %s", got)
	}
}

func TestMVCCUpdatePrimaryKeyKeepsOldPhysicalIdentity(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT,UNIQUE KEY uq_v(v))")
	run("INSERT INTO t VALUES(1,10),(2,20),(3,30)")

	// Changing the primary key must move each row exactly once, from the old physical
	// key the scan observed.
	if affected := run("UPDATE t SET id=id+100").AffectedRows; affected != 3 {
		t.Fatalf("affected=%d, want one mutation per physical row", affected)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[101 10] [102 20] [103 30]]" {
		t.Fatalf("rows after primary key update = %s", got)
	}
	// The old primary keys must be released, so they are free again.
	run("INSERT INTO t VALUES(1,40)")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[4]]" {
		t.Fatalf("old keys were not released: %s", got)
	}
	// A primary-key update onto an occupied key must fail and leave every row alone.
	if _, err := e.Execute(s, "UPDATE t SET id=101 WHERE id=102"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("primary key collision error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id FROM t ORDER BY id").Rows); got != "[[1] [101] [102] [103]]" {
		t.Fatalf("failed primary key update changed rows: %s", got)
	}
}

func TestMVCCUpdateKeepsAccessPlan(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,k INT,v INT,KEY k_idx(k))")
	run("INSERT INTO t VALUES(1,1,10),(2,2,20),(3,3,30)")

	tx, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	definition, schema, _, err := loadVersionedTable(tx, s, "t")
	if err != nil {
		t.Fatal(err)
	}
	scanPlan := func(t *testing.T, where parser.Expr) *physical.PlanNode {
		t.Helper()
		op := mutationScan(context.Background(), tx, definition, schema, s, where, -1)
		limit, ok := op.(physical.Limit[mutationRow])
		if !ok {
			t.Fatalf("mutationScan returned %T, want a Limit stage", op)
		}
		filter, ok := limit.Input.(physical.Filter[mutationRow])
		if !ok {
			t.Fatalf("limit input is %T, want a Filter stage", limit.Input)
		}
		scan, ok := filter.Input.(physical.Scan[mutationRow])
		if !ok {
			t.Fatalf("filter input is %T, want a Scan stage", filter.Input)
		}
		if scan.Plan == nil {
			t.Fatal("scan has no plan node")
		}
		return scan.Plan
	}

	// An indexed equality predicate still plans an index probe instead of a full scan:
	// the plan names the index and does not degrade to sqlAccessAll.
	indexed := scanPlan(t, parser.BinaryExpr{Left: parser.Identifier{Name: "k"}, Operator: "=", Right: parser.LiteralExpr{Value: parser.Literal{Kind: parser.LiteralNumber, Text: "1"}}})
	if indexed.Attributes["index"] != "k_idx" {
		t.Fatalf("indexed access did not choose k_idx: access=%q index=%q", indexed.Attributes["access"], indexed.Attributes["index"])
	}
	if indexed.Attributes["access"] == sqlAccessAll {
		t.Fatalf("indexed access degraded to a full scan: %q", indexed.Attributes["access"])
	}
	if indexed.Kind == "Scan" {
		t.Fatalf("indexed plan kind=%q, want an index scan", indexed.Kind)
	}
	// A predicate the planner cannot serve still falls back to a plan scan.
	unindexed := scanPlan(t, parser.BinaryExpr{Left: parser.Identifier{Name: "v"}, Operator: ">", Right: parser.LiteralExpr{Value: parser.Literal{Kind: parser.LiteralNumber, Text: "0"}}})
	if unindexed.Kind != "Scan" || unindexed.Attributes["access"] != sqlAccessAll {
		t.Fatalf("unindexed plan kind=%q access=%q", unindexed.Kind, unindexed.Attributes["access"])
	}

	// The indexed UPDATE still only touches matching rows.
	if affected := run("UPDATE t SET v=v+1 WHERE k=2").AffectedRows; affected != 1 {
		t.Fatalf("indexed update affected=%d", affected)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM t ORDER BY id").Rows); got != "[[1 10] [2 21] [3 30]]" {
		t.Fatalf("indexed update rows = %s", got)
	}
}

func TestMVCCUpdateWhereAndLimit(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,g INT,v INT)")
	run("INSERT INTO t VALUES(1,1,0),(2,1,0),(3,2,0)")

	// LIMIT counts matching rows, so the WHERE filter runs before it.
	if affected := run("UPDATE t SET v=9 WHERE g=1 LIMIT 1").AffectedRows; affected != 1 {
		t.Fatalf("limit affected=%d", affected)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t WHERE v=9").Rows); got != "[[1]]" {
		t.Fatalf("limit touched %s rows", got)
	}
	if affected := run("UPDATE t SET v=99 WHERE g=5 LIMIT 1").AffectedRows; affected != 0 {
		t.Fatalf("filtered-out limit affected=%d", affected)
	}
	if affected := run("UPDATE t SET v=7 LIMIT 0").AffectedRows; affected != 0 {
		t.Fatalf("zero limit affected=%d", affected)
	}
}

func TestMVCCUpdateConstraintsAndOnUpdateTimestamp(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,u INT UNIQUE,score INT CHECK(score>=0),stamp DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,touch DATETIME DEFAULT CURRENT_TIMESTAMP)")
	run("INSERT INTO t(id,u,score) VALUES(1,10,5),(2,20,5)")

	// CHECK failure rolls the whole statement back.
	if _, err := e.Execute(s, "UPDATE t SET score=-1"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("check error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,score FROM t ORDER BY id").Rows); got != "[[1 5] [2 5]]" {
		t.Fatalf("failed check update changed rows: %s", got)
	}
	// UNIQUE collapse must be detected: two rows cannot share one unique value.
	if _, err := e.Execute(s, "UPDATE t SET u=10 WHERE id=2"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("unique error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,u FROM t ORDER BY id").Rows); got != "[[1 10] [2 20]]" {
		t.Fatalf("failed unique update changed rows: %s", got)
	}
	// An indexed-column update keeps the index consistent in both directions.
	run("UPDATE t SET u=30 WHERE id=1")
	run("INSERT INTO t(id,u,score) VALUES(3,10,5)")
	if got := fmt.Sprint(run("SELECT id,u FROM t ORDER BY id").Rows); got != "[[1 30] [2 20] [3 10]]" {
		t.Fatalf("index after update = %s", got)
	}

	// ON UPDATE CURRENT_TIMESTAMP moves only the ON UPDATE column, and only when the
	// statement actually assigns something.
	before := fmt.Sprint(run("SELECT stamp,touch FROM t WHERE id=1").Rows)
	run("UPDATE t SET score=score")
	after := fmt.Sprint(run("SELECT stamp,touch FROM t WHERE id=1").Rows)
	if before == after {
		t.Fatalf("ON UPDATE column did not move: %s", after)
	}
	// Assigning the ON UPDATE column explicitly wins over the automatic value. The
	// stored DATETIME renders through the engine's chosen session time zone.
	run("UPDATE t SET stamp='2001-02-03 04:05:06' WHERE id=1")
	if got := fmt.Sprint(run("SELECT stamp FROM t WHERE id=1").Rows); !strings.Contains(got, "2001-02-03 04:05:06") {
		t.Fatalf("explicit ON UPDATE assignment = %s", got)
	}
}

func TestMVCCUpdateForeignKeyActionsAndRollback(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE parent(id INT PRIMARY KEY,label VARCHAR(20))")
	run("CREATE TABLE child(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES parent(id) ON UPDATE CASCADE)")
	run("INSERT INTO parent VALUES(1,'one'),(2,'two')")
	run("INSERT INTO child VALUES(10,1),(11,2)")

	// ON UPDATE CASCADE must still run through the shared FK action path.
	run("UPDATE parent SET id=id+100")
	if got := fmt.Sprint(run("SELECT id,pid FROM child ORDER BY id").Rows); got != "[[10 101] [11 102]]" {
		t.Fatalf("cascade did not follow: %s", got)
	}
	// A RESTRICT-style failure later in the statement rolls every earlier row back.
	run("CREATE TABLE rparent(id INT PRIMARY KEY)")
	run("CREATE TABLE rchild(id INT PRIMARY KEY,pid INT,CONSTRAINT fk2 FOREIGN KEY(pid) REFERENCES rparent(id))")
	run("INSERT INTO rparent VALUES(1),(2)")
	run("INSERT INTO rchild VALUES(1,2)")
	_, err := e.Execute(s, "UPDATE rparent SET id=id+100")
	if err == nil {
		t.Fatal("restrict update succeeded")
	}
	if got := fmt.Sprint(run("SELECT id FROM rparent ORDER BY id").Rows); got != "[[1] [2]]" {
		t.Fatalf("failed FK update changed rows: %s", got)
	}
	if got := fmt.Sprint(run("SELECT id,pid FROM rchild").Rows); got != "[[1 2]]" {
		t.Fatalf("failed FK update changed child rows: %s", got)
	}
}

func TestMVCCUpdateRejectsInvalidAssignmentBeforeWriting(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE other(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,10)")

	for _, query := range []string{
		"UPDATE t SET missing=1",
		"UPDATE t SET v=1,v=2",
		"UPDATE t SET other.v=1",
		"UPDATE t SET v=missing+1",
	} {
		if _, err := e.Execute(s, query); err == nil {
			t.Errorf("accepted %s", query)
		}
	}
	if got := fmt.Sprint(run("SELECT id,v FROM t").Rows); got != "[[1 10]]" {
		t.Fatalf("invalid assignment mutated rows: %s", got)
	}
}

func TestMVCCUpdateStatementRollbackIsAtomic(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,u INT UNIQUE,score INT CHECK(score>=0))")
	run("INSERT INTO t VALUES(1,10,1),(2,20,2),(3,30,3)")

	// The first rows succeed and the last fails: every mutation must be gone.
	if _, err := e.Execute(s, "UPDATE t SET score=-1 WHERE id=3"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("check error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT id,score FROM t ORDER BY id").Rows); got != "[[1 1] [2 2] [3 3]]" {
		t.Fatalf("partial update survived: %s", got)
	}
	// Inside an explicit transaction the same failure must not leak to other sessions.
	run("BEGIN")
	if _, err := e.Execute(s, "UPDATE t SET score=score+10"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(run("SELECT SUM(score) FROM t").Rows); got != "[[36]]" {
		t.Fatalf("transaction did not read its own update: %s", got)
	}
	run("ROLLBACK")
	if got := fmt.Sprint(run("SELECT SUM(score) FROM t").Rows); got != "[[6]]" {
		t.Fatalf("rollback kept updates: %s", got)
	}
}
