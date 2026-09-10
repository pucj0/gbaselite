package executor

import (
	"gbaselite/storage"
	"reflect"
	"testing"
)

func TestMVCCReusedDecodeClearsNullAndSkippedValues(t *testing.T) {
	table, row := compactTestRow()
	encoded, err := encodeSQLRow(table, row)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := decodeSQLRowInto(table, encoded, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := &dst[0]
	for i := range row {
		row[i] = storage.NullValue(row[i].Type)
	}
	encoded, err = encodeSQLRow(table, row)
	if err != nil {
		t.Fatal(err)
	}
	dst, err = decodeSQLRowInto(table, encoded, nil, dst)
	if err != nil {
		t.Fatal(err)
	}
	if &dst[0] != first || !reflect.DeepEqual(dst, row) {
		t.Fatal("stale fields or buffer not reused", dst)
	}
	table, row = compactTestRow()
	encoded, _ = encodeSQLRow(table, row)
	dst, err = decodeSQLRowInto(table, encoded, make([]bool, len(row)), dst)
	if err != nil {
		t.Fatal(err)
	}
	if dst[5].Text != "" || dst[6].Text != "" || dst[7].Text != "" {
		t.Fatal("skipped fields retained text")
	}
	for i := 0; i < len(encoded); i++ {
		if _, err = decodeSQLRowInto(table, encoded[:i], nil, dst); err == nil {
			t.Fatalf("accepted truncation %d", i)
		}
	}
}

func TestMVCCAggregateScratchPreservesValues(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE reuse(id BIGINT PRIMARY KEY,v BIGINT,note VARCHAR(20))")
	run("INSERT INTO reuse VALUES(1,10,'z'),(2,NULL,NULL),(3,-4,'a'),(4,0,'m')")
	cases := []struct {
		q    string
		want []any
	}{
		{"SELECT COUNT(*),COUNT(v),SUM(v),MIN(note),MAX(note) FROM reuse", []any{int64(4), int64(3), int64(6), "a", "z"}},
		{"SELECT COUNT(*),SUM(v),MIN(note) FROM reuse WHERE id=2", []any{int64(1), nil, nil}},
		{"SELECT COUNT(*),SUM(v),MIN(note) FROM reuse WHERE id>100", []any{int64(0), nil, nil}},
		{"SELECT SUM(v+1),COUNT(note) FROM reuse WHERE id>=3", []any{int64(-2), int64(2)}},
	}
	for _, c := range cases {
		got := run(c.q)
		if len(got.Rows) != 1 || !reflect.DeepEqual(got.Rows[0], c.want) {
			t.Fatalf("%s: got %#v want %#v", c.q, got.Rows, c.want)
		}
	}
	// Projection results retain independent rows; only aggregates reuse scratch.
	got := run("SELECT id,note FROM reuse ORDER BY id")
	if got.Rows[0][1] != "z" || got.Rows[1][1] != nil || got.Rows[2][1] != "a" {
		t.Fatal(got.Rows)
	}
	run("BEGIN")
	run("UPDATE reuse SET v=3 WHERE id=1")
	run("DELETE FROM reuse WHERE id=3")
	run("INSERT INTO reuse VALUES(5,7,'b')")
	got = run("SELECT COUNT(*),SUM(v),MIN(note),MAX(note) FROM reuse")
	if !reflect.DeepEqual(got.Rows, [][]any{{int64(4), int64(10), "b", "z"}}) {
		t.Fatal(got.Rows)
	}
	run("ROLLBACK")
	got = run("SELECT SUM(v) FROM reuse")
	if !reflect.DeepEqual(got.Rows, [][]any{{int64(6)}}) {
		t.Fatal(got.Rows)
	}
}
