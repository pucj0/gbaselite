package executor

import (
	"context"
	"gbaselite/parser"
	"strings"
	"testing"
)

func TestMVCCExplainMatchesAccessPlan(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE p(id INT PRIMARY KEY,a INT,b INT,UNIQUE KEY ab(a,b),KEY unused(b))")
	run("INSERT INTO p VALUES(1,10,20),(2,10,21)")
	cases := []struct{ query, kind, key, extra string }{
		{"SELECT * FROM p WHERE id=1", "const", "PRIMARY", "Using where"},
		{"SELECT * FROM p WHERE a=10 AND b=20", "const", "ab", "Using where"},
		{"SELECT * FROM p WHERE id>=1 ORDER BY id DESC LIMIT 1", "range", "PRIMARY", "Backward index scan"},
		{"SELECT * FROM p ORDER BY id LIMIT 1", "index", "PRIMARY", "Statistics unavailable"},
		{"SELECT * FROM p WHERE b=20 ORDER BY a", "ref", "unused", "Using filesort"},
	}
	for _, c := range cases {
		r := run("EXPLAIN " + c.query)
		if len(r.Columns) != 12 || len(r.Rows) != 1 {
			t.Fatal(r)
		}
		row := r.Rows[0]
		if row[4] != c.kind || c.key != "" && row[6] != c.key || c.key == "" && row[6] != nil || !strings.Contains(row[11].(string), c.extra) {
			t.Fatal(c.query, row)
		}
		if c.kind == "ALL" && (row[9] != nil || row[10] != nil) {
			t.Fatal("fabricated statistics", row)
		}
		stmt, err := parser.Parse(c.query)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := e.MVCC.Begin(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		table, schema, _, err := loadVersionedTable(tx, s, "p")
		if err != nil {
			t.Fatal(err)
		}
		plan := planMVCCAccess(stmt.(parser.Select), table, schema, s)
		tx.Rollback()
		if plan.index != c.key {
			t.Fatal("EXPLAIN/execution selected different keys")
		}
	}
	for _, q := range []string{"EXPLAIN SELECT missing FROM p", "EXPLAIN SELECT id FROM p WHERE missing=1"} {
		if _, err := e.Execute(s, q); err == nil {
			t.Fatal("unsupported query accepted", q)
		}
	}
}
func TestMVCCExplainDoesNotExecuteAndUsesCatalogSnapshot(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE p(id INT PRIMARY KEY)")
	before, _ := e.MVCC.Head()
	s.LastInsertID = 42
	run("EXPLAIN SELECT LAST_INSERT_ID(99) FROM p")
	after, _ := e.MVCC.Head()
	if before != after || s.LastInsertID != 42 {
		t.Fatal("EXPLAIN changed data/session state")
	}
	run("BEGIN")
	other := &Session{CurrentDatabase: "test"}
	if _, err := e.Execute(other, "DROP TABLE p"); err != nil {
		t.Fatal(err)
	}
	run("EXPLAIN SELECT * FROM p WHERE id=1")
	run("ROLLBACK")
	if _, err := e.Execute(s, "EXPLAIN SELECT * FROM p"); err == nil {
		t.Fatal("new snapshot saw dropped table")
	}
}
