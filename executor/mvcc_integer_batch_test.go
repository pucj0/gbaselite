package executor

import (
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
	"reflect"
	"strings"
	"testing"
)

func TestMVCCIntegerBatchMatchesScalar(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE ib(id BIGINT PRIMARY KEY,a BIGINT,b BIGINT,note VARCHAR(20),d DECIMAL(20,2),j JSON)")
	var rows []string
	for i := 0; i < 513; i++ {
		a := fmt.Sprint(i - 200)
		if i%7 == 0 {
			a = "NULL"
		}
		rows = append(rows, fmt.Sprintf("(%d,%s,%d,'text',1.25,'{\"x\":1}')", i, a, i*3))
	}
	run("INSERT INTO ib VALUES" + strings.Join(rows, ","))
	pairs := [][2]string{
		{"SELECT id,a,b FROM ib WHERE a>=0 AND 600>b LIMIT 11 OFFSET 3", "SELECT COALESCE(id,NULL),COALESCE(a,NULL),COALESCE(b,NULL) FROM ib WHERE a>=0 AND 600>b LIMIT 11 OFFSET 3"},
		{"SELECT id,a FROM ib WHERE a<=60 ORDER BY id DESC LIMIT 15 OFFSET 4", "SELECT COALESCE(id,NULL),COALESCE(a,NULL) FROM ib WHERE a<=60 ORDER BY id DESC LIMIT 15 OFFSET 4"},
		{"SELECT COUNT(*),COUNT(a),SUM(a),AVG(a),MIN(a),MAX(a) FROM ib WHERE b>=300", "SELECT COUNT(*),COUNT(COALESCE(a,NULL)),SUM(COALESCE(a,NULL)),AVG(COALESCE(a,NULL)),MIN(COALESCE(a,NULL)),MAX(COALESCE(a,NULL)) FROM ib WHERE b>=300"},
		{"SELECT COUNT(*),SUM(a) FROM ib WHERE a>99999", "SELECT COUNT(*),SUM(COALESCE(a,NULL)) FROM ib WHERE a>99999"},
	}
	for _, pair := range pairs {
		a, b := run(pair[0]), run(pair[1])
		if !reflect.DeepEqual(a.Rows, b.Rows) {
			t.Fatal(pair, a.Rows, b.Rows)
		}
	}
	run("BEGIN")
	run("UPDATE ib SET a=10000 WHERE id=2")
	run("DELETE FROM ib WHERE id=3")
	run("INSERT INTO ib VALUES(999,10000,1,'new',NULL,NULL)")
	a, b := run("SELECT id,a FROM ib WHERE a=10000"), run("SELECT COALESCE(id,NULL),COALESCE(a,NULL) FROM ib WHERE a=10000")
	if !reflect.DeepEqual(a.Rows, b.Rows) {
		t.Fatal(a.Rows, b.Rows)
	}
	run("ROLLBACK")
	run("CREATE TABLE edge(id BIGINT PRIMARY KEY,v BIGINT)")
	run("INSERT INTO edge VALUES(1,9223372036854775807),(2,1),(3,-1),(4,NULL)")
	for _, arg := range []string{"SUM", "AVG", "MIN", "MAX"} {
		a, b := run("SELECT "+arg+"(v) FROM edge"), run("SELECT "+arg+"(COALESCE(v,NULL)) FROM edge")
		if !reflect.DeepEqual(a.Rows, b.Rows) {
			t.Fatal(arg, a.Rows, b.Rows)
		}
	}
	_, fastErr := e.Execute(s, "SELECT SUM(v) FROM edge WHERE id<3")
	_, slowErr := e.Execute(s, "SELECT SUM(COALESCE(v,NULL)) FROM edge WHERE id<3")
	if fastErr == nil || slowErr == nil {
		t.Fatal("missing overflow", fastErr, slowErr)
	}
	// Non-integer results retain their existing exact representation.
	a, b = run("SELECT SUM(d),MIN(j) FROM ib"), run("SELECT SUM(COALESCE(d,NULL)),MIN(COALESCE(j,NULL)) FROM ib")
	if !reflect.DeepEqual(a.Rows, b.Rows) {
		t.Fatal(a.Rows, b.Rows)
	}
}

func integerBatchFixture(tb testing.TB) (versionedTable, *storage.Table, parser.Select, []sqlBatchEntry) {
	tb.Helper()
	cols := []storage.Column{{Name: "a", Type: storage.TypeBigInt}, {Name: "b", Type: storage.TypeBigInt}, {Name: "note", Type: storage.TypeVarchar, Length: 128}}
	schema, err := storage.NewTransientTable("bench", cols)
	if err != nil {
		tb.Fatal(err)
	}
	table := versionedTable{RowEncoding: 1, Definition: storage.TableSnapshot{Columns: cols}}
	stmt, err := parser.Parse("SELECT SUM(b) FROM bench WHERE a>=32 AND a<96")
	if err != nil {
		tb.Fatal(err)
	}
	batch := make([]sqlBatchEntry, 128)
	for i := range batch {
		row := storage.Row{storage.MustValue(storage.TypeBigInt, i), storage.MustValue(storage.TypeBigInt, i*3), storage.MustValue(storage.TypeVarchar, strings.Repeat("x", 128))}
		if i%7 == 0 {
			row[1] = storage.NullValue(storage.TypeBigInt)
		}
		v, err := encodeSQLRow(table, row)
		if err != nil {
			tb.Fatal(err)
		}
		batch[i] = sqlBatchEntry{value: v}
	}
	return table, schema, stmt.(parser.Select), batch
}
func TestIntegerBatchRejectsTruncatedRows(t *testing.T) {
	table, schema, stmt, batch := integerBatchFixture(t)
	p := planIntegerBatch(stmt, table, schema, sqlAccessPlan{kind: sqlAccessAll})
	if p == nil {
		t.Fatal("no batch plan")
	}
	v := make([]int64, p.width*128)
	n := make([]bool, len(v))
	original := batch[0].value
	for i := 0; i < len(original); i++ {
		batch[0].value = original[:i]
		if err := decodeIntegerBatch(table, p, batch[:1], 128, v, n); err == nil {
			t.Fatal("accepted truncation", i)
		}
	}
}

var integerBatchSink int64

func BenchmarkMVCCIntegerBatchKernel(b *testing.B) {
	table, schema, stmt, batch := integerBatchFixture(b)
	session := &Session{}
	b.Run("row", func(b *testing.B) {
		filter := bindSQLFilter(stmt.Where, schema, session)
		needed := sqlProjectionMask(stmt, schema)
		var scratch storage.Row
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sum := int64(0)
			for _, entry := range batch {
				var err error
				scratch, err = decodeSQLRowInto(table, entry.value, needed, scratch)
				if err != nil {
					b.Fatal(err)
				}
				ok, err := filter(scratch)
				if err != nil {
					b.Fatal(err)
				}
				if truthy(ok) && !scratch[1].Null {
					sum += scratch[1].Int64
				}
			}
			integerBatchSink = sum
		}
	})
	b.Run("vector", func(b *testing.B) {
		p := planIntegerBatch(stmt, table, schema, sqlAccessPlan{kind: sqlAccessAll})
		v := make([]int64, p.width*128)
		n := make([]bool, len(v))
		sel := make([]uint16, 128)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := decodeIntegerBatch(table, p, batch, 128, v, n); err != nil {
				b.Fatal(err)
			}
			selected := p.selectRows(128, 128, v, n, sel)
			sum := int64(0)
			base := p.positions[1] * 128
			for _, r := range selected {
				if !n[base+int(r)] {
					sum += v[base+int(r)]
				}
			}
			integerBatchSink = sum
		}
	})
}
