package executor

import (
	"context"
	"gbaselite/storageengine"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMVCCSecondaryCoveringAndRollback(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE sc(id INT PRIMARY KEY,a INT,b INT,note VARCHAR(20),KEY ab(a,b))")
	run("INSERT INTO sc VALUES(1,10,20,'one'),(2,10,NULL,'two'),(3,11,20,'three'),(4,10,20,'four')")
	compare := func(q, full string) {
		t.Helper()
		a, b := run(q), run(full)
		if !reflect.DeepEqual(a.Rows, b.Rows) {
			t.Fatal(q, a.Rows, b.Rows)
		}
	}
	compare("SELECT id,b FROM sc WHERE a=10 ORDER BY id", "SELECT id,b FROM sc WHERE a=10 OR 0=1 ORDER BY id")
	p := run("EXPLAIN SELECT id,b FROM sc WHERE a=10")
	if p.Rows[0][4] != "ref" || !strings.Contains(p.Rows[0][11].(string), "Using index") {
		t.Fatal(p.Rows)
	}
	p = run("EXPLAIN SELECT note FROM sc WHERE a=10")
	if strings.Contains(p.Rows[0][11].(string), "Using index") {
		t.Fatal("incorrect covering plan")
	}
	run("BEGIN")
	run("UPDATE sc SET a=12 WHERE a=10 AND b=20")
	run("DELETE FROM sc WHERE a=10")
	compare("SELECT id,b FROM sc WHERE a=12 ORDER BY id", "SELECT id,b FROM sc WHERE a=12 OR 0=1 ORDER BY id")
	run("ROLLBACK")
	compare("SELECT COUNT(*),SUM(b) FROM sc WHERE a=10", "SELECT COUNT(*),SUM(b) FROM sc WHERE a=10 OR 0=1")
}
func TestMVCCSecondaryTextCollationFallback(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE st(id INT PRIMARY KEY,note VARCHAR(20),KEY note_idx(note))")
	run("INSERT INTO st VALUES(1,'A'),(2,'a'),(3,NULL)")
	s.CollationConnection = "utf8mb4_bin"
	p := run("EXPLAIN SELECT id,note FROM st WHERE note='A'")
	if p.Rows[0][4] != "ref" {
		t.Fatal(p.Rows)
	}
	a, b := run("SELECT id FROM st WHERE note='A'"), run("SELECT id FROM st WHERE note='A' OR 0=1")
	if !reflect.DeepEqual(a.Rows, b.Rows) {
		t.Fatal(a.Rows, b.Rows)
	}
	s.CollationConnection = "utf8mb4_general_ci"
	p = run("EXPLAIN SELECT id FROM st WHERE note='A'")
	if p.Rows[0][4] != "ALL" {
		t.Fatal("unsafe collation used index")
	}
	_ = e
}

func TestMVCCSecondaryAfterFlatMigration(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE sm(id INT PRIMARY KEY,a INT,b INT,KEY ab(a,b))")
	run("INSERT INTO sm VALUES(1,10,20),(2,10,NULL),(3,11,20)")
	root := t.TempDir()
	if err := e.Backend.(storageengine.Maintenance).Compact(context.Background(), filepath.Join(root, "versioned")); err != nil {
		t.Fatal(err)
	}
	migrated, err := OpenWithOptions(root, "root", "pw", OpenOptions{StorageMode: "mvcc", LocalWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	session := &Session{}
	q := func(sql string) *Result {
		t.Helper()
		r, err := migrated.Execute(session, sql)
		if err != nil {
			t.Fatal(sql, err)
		}
		return r
	}
	q("USE test")
	q("BEGIN")
	before := q("SELECT id,b FROM sm WHERE a=10 ORDER BY id")
	writer := &Session{}
	if _, err = migrated.Execute(writer, "USE test"); err != nil {
		t.Fatal(err)
	}
	if _, err = migrated.Execute(writer, "UPDATE sm SET id=4,a=12 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if after := q("SELECT id,b FROM sm WHERE a=10 ORDER BY id"); !reflect.DeepEqual(before.Rows, after.Rows) {
		t.Fatal("index snapshot changed", before.Rows, after.Rows)
	}
	q("COMMIT")
	indexed := q("SELECT id,b FROM sm WHERE a=12 ORDER BY id")
	scan := q("SELECT id,b FROM sm WHERE a=12 OR 0=1 ORDER BY id")
	if !reflect.DeepEqual(indexed.Rows, scan.Rows) || len(indexed.Rows) != 1 {
		t.Fatal(indexed.Rows, scan.Rows)
	}
}
