package executor

import (
	"bytes"
	"context"
	"fmt"
	"gbaselite/mvcc"
	"gbaselite/parser"
	"gbaselite/storage"
	"math"
	"reflect"
	"strings"
	"testing"
)

func rangeTestEngine(t *testing.T) (*Engine, *Session, func(string) *Result) {
	t.Helper()
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	s := &Session{}
	run := func(q string) *Result {
		t.Helper()
		r, err := e.Execute(s, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	run("CREATE DATABASE test")
	run("USE test")
	return e, s, run
}
func TestMVCCOrderedIntegerKeys(t *testing.T) {
	numbers := []int64{math.MinInt64, -9007199254740992, -100000, -10, -1, 0, 1, 9, 10, 99, 100, 9007199254740992, math.MaxInt64}
	for i, n := range numbers {
		if len(mvccIntegerKey(n)) != 8 {
			t.Fatal(n)
		}
		if i > 0 && bytes.Compare(mvccIntegerKey(numbers[i-1]), mvccIntegerKey(n)) >= 0 {
			t.Fatal("key order", n)
		}
	}
}
func TestMVCCRangeSQLMatchesFullScan(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE fast(id BIGINT PRIMARY KEY,v INT,payload VARCHAR(20))")
	run("CREATE TABLE slow(id BIGINT PRIMARY KEY,v INT,payload VARCHAR(20))")
	// Encode the old catalog struct without KeyEncoding, as written by old binaries.
	tx, _ := e.MVCC.Begin(context.Background(), nil)
	def, _, k, err := loadVersionedTable(tx, s, "slow")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeVersioned(struct {
		ID         string
		Definition storage.TableSnapshot
	}{def.ID, def.Definition})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Put("catalog", k, encoded); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"fast", "slow"} {
		var values []string
		for _, n := range []int{-101, -100, -10, -9, -1, 0, 1, 2, 9, 10, 11, 20, 99, 100, 101, 999, 1000} {
			values = append(values, fmt.Sprintf("(%d,%d,'row')", n, n%3))
		}
		run("INSERT INTO " + table + " VALUES" + strings.Join(values, ","))
	}
	queries := []string{
		"SELECT id,v FROM %s WHERE id>=9 AND id<101 ORDER BY id",
		"SELECT id,v FROM %s WHERE 9<=id AND 101>id ORDER BY id DESC",
		"SELECT id FROM %s WHERE id BETWEEN -10 AND 10 ORDER BY id",
		"SELECT id FROM %s WHERE id>=-100 AND id<0 AND v=0 ORDER BY id DESC LIMIT 2",
		"SELECT id FROM %s WHERE id>9 AND id>=9 AND id<=10 ORDER BY id",
		"SELECT id FROM %s WHERE id=10 AND id<10 ORDER BY id",
		"SELECT id FROM %s WHERE id>=0 AND id<=0 ORDER BY id DESC",
		"SELECT id FROM %s ORDER BY id DESC LIMIT 3 OFFSET 2",
		"SELECT id FROM %s ORDER BY id LIMIT 0",
		"SELECT id,v FROM %s WHERE id>2000 ORDER BY id",
		"SELECT id FROM %s WHERE id<0 OR id>100 ORDER BY id",
		"SELECT id FROM %s WHERE id>1.5 ORDER BY id",
		"SELECT id FROM %s WHERE id>'10' ORDER BY id",
		"SELECT id FROM %s WHERE id IS NULL ORDER BY id",
		"SELECT id FROM %s WHERE id>=9007199254740992 ORDER BY id",
		"SELECT id FROM %s WHERE id>=-9223372036854775808 ORDER BY id",
		"SELECT id FROM %s WHERE id>=10 ORDER BY v,id DESC",
		"SELECT 100-id AS id FROM %s WHERE id>=0 ORDER BY id LIMIT 3",
		"SELECT 100-id AS id FROM %s a WHERE a.id>=0 ORDER BY a.id LIMIT 3",
		"SELECT v AS id FROM %s WHERE id>=0 ORDER BY id LIMIT 3",
		"SELECT id AS k FROM %s WHERE id>=0 ORDER BY k DESC LIMIT 3",
		"SELECT a.id FROM %s a WHERE a.id>=9 AND a.id<=99 ORDER BY a.id DESC LIMIT 2",
		"SELECT DISTINCT v FROM %s WHERE id>=9 AND id<=999 ORDER BY v",
		"SELECT COUNT(*),SUM(v) FROM %s WHERE id>=9 AND id<101",
	}
	for _, q := range queries {
		a := run(fmt.Sprintf(q, "fast"))
		b := run(fmt.Sprintf(q, "slow"))
		if !reflect.DeepEqual(a.Rows, b.Rows) {
			t.Fatalf("%s\nfast=%v\nslow=%v", q, a.Rows, b.Rows)
		}
	}
	for _, table := range []string{"fast", "slow"} {
		run("UPDATE " + table + " SET id=id+5000 WHERE id>=9 AND id<=20")
		run("DELETE FROM " + table + " WHERE id BETWEEN 99 AND 999")
	}
	if a, b := run("SELECT * FROM fast ORDER BY id"), run("SELECT * FROM slow ORDER BY id"); !reflect.DeepEqual(a.Rows, b.Rows) {
		t.Fatalf("range DML %v %v", a.Rows, b.Rows)
	}
	read, _ := e.MVCC.Begin(context.Background(), nil)
	defer read.Rollback()
	fast, schema, _, _ := loadVersionedTable(read, s, "fast")
	slow, _, _, _ := loadVersionedTable(read, s, "slow")
	expr, _ := parser.ParseExpression("id >= 5009 AND id < 5021")
	plan, ok := mvccPrimaryRange(expr, fast, schema)
	if !ok {
		t.Fatal("no range plan")
	}
	stats := mvcc.ScanStats{}
	plan.Stats = &stats
	count := 0
	err = read.ScanRange(context.Background(), "row/"+fast.ID, plan, func(k, v []byte) error { count++; return nil })
	if err != nil || count != 4 || stats.StoredKeys != 4 {
		t.Fatalf("bounded SQL plan: count %d stats %+v error %v", count, stats, err)
	}
	if _, ok = mvccPrimaryRange(expr, slow, schema); ok {
		t.Fatal("legacy text keys treated as ordered")
	}
	for _, text := range []string{"id > 9007199254740992", "id>0 OR id<10", "id>0 AND missing=1", "id>1.5", "id IS NULL", "id BETWEEN '1' AND '10'"} {
		expr, err := parser.ParseExpression(text)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := mvccPrimaryRange(expr, fast, schema); ok {
			t.Fatalf("unsafe plan for %s", text)
		}
	}
}
func TestMVCCRangeSnapshotOwnWritesAndReopen(t *testing.T) {
	dir := t.TempDir()
	e, err := OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e != nil {
			e.Close()
		}
	}()
	run := func(s *Session, q string) *Result {
		t.Helper()
		r, err := e.Execute(s, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	s := &Session{}
	run(s, "CREATE DATABASE test")
	run(s, "USE test")
	run(s, "CREATE TABLE items(id BIGINT PRIMARY KEY,v INT UNIQUE)")
	run(s, "INSERT INTO items VALUES(9,9),(10,10),(11,11),(12,12)")
	a := &Session{CurrentDatabase: "test"}
	run(a, "BEGIN")
	run(s, "UPDATE items SET v=110 WHERE id=11")
	run(s, "DELETE FROM items WHERE id=12")
	run(a, "UPDATE items SET id=13 WHERE id=10")
	run(a, "DELETE FROM items WHERE id=9")
	run(a, "INSERT INTO items VALUES(14,14)")
	q := "SELECT id,v FROM items WHERE id>=9 AND id<=14 ORDER BY id DESC"
	got := run(a, q).Rows
	want := [][]any{{int64(14), int64(14)}, {int64(13), int64(10)}, {int64(12), int64(12)}, {int64(11), int64(11)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot overlay %v", got)
	}
	if _, err = e.Execute(a, "INSERT INTO items VALUES(15,10)"); err == nil {
		t.Fatal("unique key lost after primary move")
	}
	if _, err = e.Execute(a, "INSERT INTO items VALUES(14,15)"); err == nil {
		t.Fatal("duplicate primary accepted")
	}
	run(a, "COMMIT")
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e = nil
	e, err = OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	want = [][]any{{int64(14), int64(14)}, {int64(13), int64(10)}, {int64(11), int64(110)}}
	if got = run(s, q).Rows; !reflect.DeepEqual(got, want) {
		t.Fatalf("reopen %v", got)
	}
	run(s, "TRUNCATE TABLE items")
	run(s, "INSERT INTO items VALUES(-1,1),(0,2)")
	if got = run(s, "SELECT id FROM items WHERE id>=-1 AND id<=0 ORDER BY id").Rows; !reflect.DeepEqual(got, [][]any{{int64(-1)}, {int64(0)}}) {
		t.Fatal(got)
	}
}
func TestMVCCUnsupportedKeyLayouts(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	for _, q := range []string{"CREATE TABLE txt(id VARCHAR(8) PRIMARY KEY)", "CREATE TABLE pair(a INT,b INT,PRIMARY KEY(a,b))", "CREATE TABLE heap(id INT)"} {
		run(q)
	}
	tx, _ := e.MVCC.Begin(context.Background(), nil)
	defer tx.Rollback()
	for _, name := range []string{"txt", "pair", "heap"} {
		d, _, _, err := loadVersionedTable(tx, s, name)
		if err != nil || d.KeyEncoding != 0 {
			t.Fatalf("%s: %v %+v", name, err, d)
		}
	}
	def := versionedTable{KeyEncoding: 99}
	if validateMVCCKeyEncoding(def) == nil {
		t.Fatal("unknown layout accepted")
	}
}
