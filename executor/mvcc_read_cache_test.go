package executor

import (
	"fmt"
	"testing"
)

func TestMVCCReadCacheFollowsVisibleCatalog(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE cached(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO cached VALUES(1,10)")
	run("SELECT v FROM cached WHERE id=1")
	first := s.mvccReadCache
	if first == nil {
		t.Fatal("cache not populated")
	}
	run("SELECT v FROM cached WHERE id=1")
	if first != s.mvccReadCache {
		t.Fatal("unchanged directory decoded again")
	}
	run("BEGIN")
	run("SELECT v FROM cached WHERE id=1")
	other := &Session{CurrentDatabase: "test"}
	if _, err := e.Execute(other, "ALTER TABLE cached ADD COLUMN n INT DEFAULT 7"); err != nil {
		t.Fatal(err)
	}
	if r := run("SELECT * FROM cached WHERE id=1"); len(r.Columns) != 2 || s.mvccReadCache != first {
		t.Fatal("old snapshot lost", r)
	}
	run("ROLLBACK")
	if r := run("SELECT * FROM cached WHERE id=1"); len(r.Columns) != 3 || fmt.Sprint(r.Rows) != "[[1 10 7]]" {
		t.Fatal(r)
	}
	run("BEGIN")
	run("ALTER TABLE cached ADD COLUMN own INT DEFAULT 8")
	if r := run("SELECT * FROM cached WHERE id=1"); len(r.Columns) != 4 {
		t.Fatal("own DDL invisible", r)
	}
	run("ROLLBACK")
	if r := run("SELECT * FROM cached WHERE id=1"); len(r.Columns) != 3 {
		t.Fatal("rolled back directory cached", r)
	}
	run("SELECT c.v FROM cached c WHERE c.id=1")
	if r := run("SELECT v FROM cached WHERE id=1"); fmt.Sprint(r.Rows) != "[[10]]" {
		t.Fatal("alias modified cache", r)
	}
	run("UPDATE cached SET v=20 WHERE id=1")
	if r := run("SELECT v FROM cached WHERE id=1"); fmt.Sprint(r.Rows) != "[[20]]" {
		t.Fatal("cached row data", r)
	}
	e.ResetConnection(s)
	if s.mvccReadCache != nil {
		t.Fatal("connection reset retained cache")
	}
}

func BenchmarkMVCCReadTableMetadata(b *testing.B) {
	// The existing fixture uses testing.T, so construct an isolated engine here.
	e, err := OpenWithOptions(b.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	s := &Session{}
	for _, q := range []string{"CREATE DATABASE test", "USE test", "CREATE TABLE t(id INT PRIMARY KEY,v INT,payload VARCHAR(128))", "BEGIN"} {
		if _, err = e.Execute(s, q); err != nil {
			b.Fatal(err)
		}
	}
	defer e.CloseSession(s)
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprint(cached), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, _, err := loadVersionedTableInternal(s.mvccTransaction, s, "t", cached); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
