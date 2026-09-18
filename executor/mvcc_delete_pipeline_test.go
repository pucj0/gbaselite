package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
)

// This file holds the User Story 3 regression tests for the plain (single-table) DELETE
// path: the unified DeleteOperator, access-plan preservation, FK actions, statement
// atomicity and the parent-snapshot / child-transaction split.

// deletePipelineTrace records the candidates the single-table DELETE path deleted.
type deletePipelineTrace struct {
	tables map[string]int
	rows   int
	keys   [][]byte
}

func traceDeletePipeline(t *testing.T) *deletePipelineTrace {
	t.Helper()
	trace := &deletePipelineTrace{tables: map[string]int{}}
	previous := deleteApplyHook
	deleteApplyHook = func(candidate DeleteCandidate, engine string) {
		trace.tables[engine]++
		trace.rows++
		trace.keys = append(trace.keys, append([]byte(nil), candidate.Identity.Key...))
	}
	t.Cleanup(func() { deleteApplyHook = previous })
	return trace
}

func TestMVCCDeleteUsesUnifiedDeleteOperator(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,10),(2,20),(3,30)")

	trace := traceDeletePipeline(t)
	if affected := run("DELETE FROM t WHERE v>=20").AffectedRows; affected != 2 {
		t.Fatalf("affected=%d, want the two matching rows", affected)
	}
	// One application per deleted row, through this operator alone.
	if trace.rows != 2 || trace.tables["test.t"] != 2 {
		t.Fatalf("delete operator applications = %d %v", trace.rows, trace.tables)
	}
	// Every candidate carried a usable physical identity.
	for _, key := range trace.keys {
		if len(key) == 0 {
			t.Fatal("delete candidate had no physical identity")
		}
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[1]]" {
		t.Fatalf("rows left = %s", got)
	}

	// A full-table DELETE and a non-matching WHERE both go through the same operator.
	before := trace.rows
	run("DELETE FROM t WHERE v=999")
	if trace.rows != before {
		t.Fatalf("non-matching DELETE reached the operator %d times", trace.rows-before)
	}
	run("DELETE FROM t")
	if trace.rows != before+1 {
		t.Fatalf("full DELETE reached the operator %d times", trace.rows-before)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[0]]" {
		t.Fatalf("table not emptied: %s", got)
	}
}

func TestMVCCDeleteWhereAndLimitSemantics(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,g INT)")
	run("INSERT INTO t VALUES(1,1),(2,1),(3,2)")

	// LIMIT counts matching rows, so the WHERE filter runs before it.
	if affected := run("DELETE FROM t WHERE g=1 LIMIT 1").AffectedRows; affected != 1 {
		t.Fatalf("limit affected=%d", affected)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[2]]" {
		t.Fatalf("limit deleted the wrong number: %s", got)
	}
	if affected := run("DELETE FROM t WHERE g=99 LIMIT 1").AffectedRows; affected != 0 {
		t.Fatalf("filtered-out limit affected=%d", affected)
	}
	if affected := run("DELETE FROM t LIMIT 0").AffectedRows; affected != 0 {
		t.Fatalf("zero limit affected=%d", affected)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[2]]" {
		t.Fatalf("limit 0 deleted rows: %s", got)
	}
}

func TestMVCCDeleteKeepsAccessPlan(t *testing.T) {
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
	indexed := parser.BinaryExpr{Left: parser.Identifier{Name: "k"}, Operator: "=", Right: parser.LiteralExpr{Value: parser.Literal{Kind: parser.LiteralNumber, Text: "2"}}}
	op := mutationScan(context.Background(), tx, definition, schema, s, indexed, -1)
	limit, ok := op.(physical.Limit[mutationRow])
	if !ok {
		t.Fatalf("mutationScan returned %T", op)
	}
	filter := limit.Input.(physical.Filter[mutationRow])
	scan := filter.Input.(physical.Scan[mutationRow])
	if scan.Plan.Attributes["index"] != "k_idx" || scan.Plan.Attributes["access"] == sqlAccessAll {
		t.Fatalf("indexed DELETE lost its access plan: %+v", scan.Plan.Attributes)
	}
	// An unservable predicate still falls back to a plain scan rather than failing.
	unindexed := parser.BinaryExpr{Left: parser.Identifier{Name: "v"}, Operator: ">", Right: parser.LiteralExpr{Value: parser.Literal{Kind: parser.LiteralNumber, Text: "0"}}}
	op = mutationScan(context.Background(), tx, definition, schema, s, unindexed, -1)
	scan = op.(physical.Limit[mutationRow]).Input.(physical.Filter[mutationRow]).Input.(physical.Scan[mutationRow])
	if scan.Plan.Kind != "Scan" {
		t.Fatalf("unindexed DELETE plan kind=%q", scan.Plan.Kind)
	}

	// The indexed DELETE still only removes matching rows.
	if affected := run("DELETE FROM t WHERE k=2").AffectedRows; affected != 1 {
		t.Fatalf("indexed delete affected=%d", affected)
	}
	if got := fmt.Sprint(run("SELECT id FROM t ORDER BY id").Rows); got != "[[1] [3]]" {
		t.Fatalf("indexed delete rows = %s", got)
	}
}

func TestMVCCDeleteForeignKeyActions(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE casc(id INT PRIMARY KEY)")
	run("CREATE TABLE casc_child(id INT PRIMARY KEY,pid INT,CONSTRAINT fk_c FOREIGN KEY(pid) REFERENCES casc(id) ON DELETE CASCADE)")
	run("INSERT INTO casc VALUES(1),(2)")
	run("INSERT INTO casc_child VALUES(10,1)")

	// ON DELETE CASCADE runs through the shared FK action path.
	run("DELETE FROM casc WHERE id=1")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM casc_child").Rows); got != "[[0]]" {
		t.Fatalf("cascade left child rows: %s", got)
	}

	// RESTRICT refuses and the statement stays atomic.
	run("CREATE TABLE rest(id INT PRIMARY KEY)")
	run("CREATE TABLE rest_child(id INT PRIMARY KEY,pid INT,CONSTRAINT fk_r FOREIGN KEY(pid) REFERENCES rest(id))")
	run("INSERT INTO rest VALUES(1)")
	run("INSERT INTO rest_child VALUES(10,1)")
	if _, err := e.Execute(s, "DELETE FROM rest WHERE id=1"); !errors.Is(err, storage.ErrForeignKey) && !errors.Is(err, storage.ErrForeignKeyReferenced) {
		t.Fatalf("restrict delete error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM rest").Rows); got != "[[1]]" {
		t.Fatalf("failed restrict delete removed the parent: %s", got)
	}

	// ON DELETE SET NULL that violates a CHECK must roll the statement back.
	run("CREATE TABLE setp(id INT PRIMARY KEY)")
	run("CREATE TABLE setc(id INT PRIMARY KEY,pid INT,CONSTRAINT ck_nn CHECK(pid IS NOT NULL),CONSTRAINT fk_s FOREIGN KEY(pid) REFERENCES setp(id) ON DELETE SET NULL)")
	run("INSERT INTO setp VALUES(1)")
	run("INSERT INTO setc VALUES(10,1)")
	if _, err := e.Execute(s, "DELETE FROM setp WHERE id=1"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("set null CHECK error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM setp").Rows); got != "[[1]]" {
		t.Fatalf("failed SET NULL delete removed the parent: %s", got)
	}
	if got := fmt.Sprint(run("SELECT pid FROM setc").Rows); got != "[[1]]" {
		t.Fatalf("failed SET NULL delete changed the child: %s", got)
	}
}

func TestMVCCDeleteStatementRollbackIsAtomic(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE p(id INT PRIMARY KEY,keep INT)")
	run("CREATE TABLE c(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES p(id))")
	// Three parents; the child references only the last one, so deleting all three succeeds
	// for the first two rows and fails on the third.
	run("INSERT INTO p VALUES(1,0),(2,0),(3,0)")
	run("INSERT INTO c VALUES(10,3)")

	if _, err := e.Execute(s, "DELETE FROM p"); err == nil {
		t.Fatal("delete with a referenced parent succeeded")
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM p").Rows); got != "[[3]]" {
		t.Fatalf("partial DELETE survived: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM c").Rows); got != "[[1]]" {
		t.Fatalf("failed DELETE touched the child table: %s", got)
	}

	// Inside a transaction the same failure must not leak, and ROLLBACK restores deletes.
	run("DELETE FROM c")
	run("BEGIN")
	run("DELETE FROM p")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM p").Rows); got != "[[0]]" {
		t.Fatalf("transaction could not read its own deletes: %s", got)
	}
	run("ROLLBACK")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM p").Rows); got != "[[3]]" {
		t.Fatalf("rollback kept deletes: %s", got)
	}
}

func TestMVCCMultiTableDeleteUsesUnifiedDeleteOperator(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE a(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE b(id INT PRIMARY KEY,aid INT,KEY aid_idx(aid))")
	run("INSERT INTO a VALUES(1,10),(2,20)")
	run("INSERT INTO b VALUES(10,1),(11,2)")

	// Since Phase 7 a joined DELETE reaches the same DeleteOperator as a single-table DELETE, and
	// exactly once per row deleted: it no longer owns a separate write path.
	trace := traceDeletePipeline(t)
	if affected := run("DELETE a FROM a JOIN b ON b.aid=a.id").AffectedRows; affected != 2 {
		t.Fatalf("multi-table delete affected=%d", affected)
	}
	if trace.rows != 2 || trace.tables["test.a"] != 2 {
		t.Fatalf("multi-table DELETE reached the operator %d times (%v), want 2 into test.a", trace.rows, trace.tables)
	}
	for _, key := range trace.keys {
		if len(key) == 0 {
			t.Fatal("multi-table delete candidate had no physical identity")
		}
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM a").Rows); got != "[[0]]" {
		t.Fatalf("multi-table delete did not remove rows: %s", got)
	}
}
