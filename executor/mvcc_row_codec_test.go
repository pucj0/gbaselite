package executor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"gbaselite/parser"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func compactTestRow() (versionedTable, storage.Row) {
	values := storage.Row{
		storage.MustValue(storage.TypeInt, int64(-123)),
		storage.MustValue(storage.TypeBigInt, int64(math.MinInt64)),
		storage.MustValue(storage.TypeFloat, 1.25),
		storage.MustValue(storage.TypeDouble, math.Copysign(0, -1)),
		storage.MustValue(storage.TypeBoolean, true),
		storage.MustValue(storage.TypeVarchar, "汉字\x00text"),
		storage.MustValue(storage.TypeText, "{\"a\":[1,null,\"b\"]}"),
		storage.MustValue(storage.TypeDecimal, "1234567890123456789.0123456789"),
		storage.MustValue(storage.TypeDate, "2026-09-08"),
		storage.MustValue(storage.TypeDateTime, "2026-09-08 12:34:56.123456"),
		storage.NullValue(storage.TypeText),
	}
	table := versionedTable{RowEncoding: sqlCompactRowEncoding}
	for i, v := range values {
		table.Definition.Columns = append(table.Definition.Columns, storage.Column{Name: string(rune('a' + i)), Type: v.Type})
	}
	return table, values
}
func TestCompactMVCCRowRoundTripAndMalformed(t *testing.T) {
	table, row := compactTestRow()
	encoded, err := encodeSQLRow(table, row)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeSQLRow(table, encoded)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range row {
		if v.Type == storage.TypeDate || v.Type == storage.TypeDateTime {
			if !v.Date.Equal(decoded[i].Date) {
				t.Fatalf("date %+v", decoded[i])
			}
			decoded[i].Date = v.Date
		}
	}
	if !reflect.DeepEqual(row, decoded) {
		t.Fatalf("roundtrip %#v %#v", row, decoded)
	}
	legacy := table
	legacy.RowEncoding = 0
	old, err := encodeSQLRow(legacy, row)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= len(old) {
		t.Fatalf("compact=%d gob=%d", len(encoded), len(old))
	}
	if _, err = decodeSQLRow(legacy, old); err != nil {
		t.Fatal(err)
	}
	if _, err = decodeSQLRow(table, old); err == nil {
		t.Fatal("mixed format accepted")
	}
	for i := 0; i < len(encoded); i++ {
		if _, err = decodeSQLRow(table, encoded[:i]); err == nil {
			t.Fatalf("truncated length %d accepted", i)
		}
	}
	for _, b := range [][]byte{append(bytes.Clone(encoded), 0), {'G', 'B', 'R', 2, 0}, append(bytes.Clone(sqlRowMagic), 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255)} {
		if _, err = decodeSQLRow(table, b); err == nil {
			t.Fatal("malformed row accepted")
		}
	}
	table.RowEncoding = 99
	if _, err = decodeSQLRow(table, encoded); err == nil {
		t.Fatal("unknown format")
	}
	table, row = compactTestRow()
	row[5].Text = strings.Repeat("x", storageengine.MaxValueBytes)
	if _, err = encodeSQLRow(table, row); !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatal(err)
	}
}
func TestCompactMVCCProjectionKeepsDependencies(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE docs(id INT PRIMARY KEY,body JSON,note TEXT)")
	run("INSERT INTO docs VALUES(1,JSON_OBJECT('n',3),'c'),(2,JSON_OBJECT('n',7),'b'),(3,NULL,'a')")
	queries := []string{
		"SELECT JSON_EXTRACT(body,'$.n') FROM docs WHERE id=2",
		"SELECT COUNT(body),COUNT(*),SUM(id) FROM docs",
		"SELECT id FROM docs WHERE note='b' ORDER BY id",
		"SELECT id FROM docs ORDER BY note",
		"SELECT note AS id FROM docs ORDER BY id",
		"SELECT CASE WHEN id=1 THEN note ELSE 'z' END FROM docs ORDER BY id",
		"SELECT * FROM docs ORDER BY id",
	}
	for _, q := range queries {
		r := run(q)
		if len(r.Rows) == 0 {
			t.Fatal(q)
		}
	}
	table, row := compactTestRow()
	encoded, _ := encodeSQLRow(table, row)
	mask := make([]bool, len(row))
	mask[5] = true
	projected, err := decodeSQLRowProjected(table, encoded, mask)
	if err != nil {
		t.Fatal(err)
	}
	if projected[5].Text != row[5].Text || projected[6].Text != "" || !projected[10].Null {
		t.Fatal(projected)
	}
	schema, _ := storage.NewTransientTable("items", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "payload", Type: storage.TypeText}})
	where, _ := parser.ParseExpression("id >= 1")
	plan := parser.Select{Items: []parser.SelectItem{{Expression: "id"}}, Where: where, OrderBy: []parser.Order{{Column: "id"}}}
	if mask = sqlProjectionMask(plan, schema); !reflect.DeepEqual(mask, []bool{true, false}) {
		t.Fatal(mask)
	}
	plan.Items = []parser.SelectItem{{Expression: "COUNT(*)"}}
	plan.OrderBy = nil
	if mask = sqlProjectionMask(plan, schema); !reflect.DeepEqual(mask, []bool{true, false}) {
		t.Fatal(mask)
	}
	plan.Items = []parser.SelectItem{{Expression: "payload", Alias: "k"}}
	plan.OrderBy = []parser.Order{{Column: "k"}}
	if mask = sqlProjectionMask(plan, schema); !reflect.DeepEqual(mask, []bool{true, true}) {
		t.Fatal(mask)
	}
}
func FuzzCompactMVCCRowDecode(f *testing.F) {
	table, row := compactTestRow()
	encoded, _ := encodeSQLRow(table, row)
	f.Add(encoded)
	f.Add([]byte{})
	f.Add(append(bytes.Clone(sqlRowMagic), binary.MaxVarintLen64))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > storageengine.MaxValueBytes+1 {
			return
		}
		_, _ = decodeSQLRow(table, b)
	})
}
func TestCompactMVCCDatesUseSameBinarySemanticsAsGob(t *testing.T) {
	table := versionedTable{RowEncoding: 1, Definition: storage.TableSnapshot{Columns: []storage.Column{{Name: "d", Type: storage.TypeDateTime}}}}
	row := storage.Row{{Type: storage.TypeDateTime, Date: time.Date(2001, 2, 3, 4, 5, 6, 123456789, time.FixedZone("test", 9*3600))}}
	b, err := encodeSQLRow(table, row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSQLRow(table, b)
	if err != nil || !got[0].Date.Equal(row[0].Date) {
		t.Fatal(got, err)
	}
}
