package executor

import (
	"errors"
	"gbaselite/storageengine"
	"testing"
)

func TestMVCCSQLTransactionsAndReopen(t *testing.T) {
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
	s := &Session{}
	run := func(s *Session, sql string) *Result {
		t.Helper()
		r, err := e.Execute(s, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return r
	}
	run(s, "CREATE DATABASE test")
	run(s, "USE test")
	run(s, "CREATE TABLE items(id BIGINT PRIMARY KEY AUTO_INCREMENT, v INT, name VARCHAR(30) UNIQUE, doc JSON)")
	run(s, `INSERT INTO items(id,v,name,doc) VALUES(10,1,'a',JSON_OBJECT('x',1))`)
	r := run(s, `INSERT INTO items(v,name) VALUES(2,'b')`)
	if r.LastInsertID != 11 {
		t.Fatalf("auto id: %d", r.LastInsertID)
	}
	a, b := &Session{CurrentDatabase: "test"}, &Session{CurrentDatabase: "test"}
	defer e.CloseSession(a)
	defer e.CloseSession(b)
	run(a, "BEGIN")
	run(b, "BEGIN")
	run(a, "UPDATE items SET v=3 WHERE id=10")
	run(b, "UPDATE items SET v=4 WHERE id=11")
	run(a, "COMMIT")
	run(b, "COMMIT")
	run(a, "BEGIN")
	run(b, "BEGIN")
	run(a, "UPDATE items SET v=5 WHERE id=10")
	run(b, "UPDATE items SET v=6 WHERE id=10")
	run(a, "COMMIT")
	if _, err = e.Execute(b, "COMMIT"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err = e.Execute(s, `INSERT INTO items(v,name) VALUES(7,'c'),(8,'a')`); err == nil {
		t.Fatal("duplicate accepted")
	}
	if r = run(s, "SELECT COUNT(*) FROM items"); r.Rows[0][0] != int64(2) {
		t.Fatalf("atomic rows: %#v", r.Rows)
	}
	run(s, "DELETE FROM items WHERE id=11")
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e = nil
	e, err = OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	if r = run(s, "SELECT v FROM items WHERE id=10"); len(r.Rows) != 1 || r.Rows[0][0] != int64(5) {
		t.Fatalf("reopen: %#v", r.Rows)
	}
}

func TestMVCCSchemaConflictAndStatementRollback(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &Session{}
	run := func(s *Session, q string) {
		t.Helper()
		if _, err := e.Execute(s, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run(s, "CREATE DATABASE test")
	run(s, "USE test")
	run(s, "CREATE TABLE items(id INT PRIMARY KEY, name VARCHAR(10) UNIQUE)")
	run(s, "INSERT INTO items VALUES(1,'a'),(2,'b')")
	a := &Session{CurrentDatabase: "test"}
	defer e.CloseSession(a)
	run(a, "BEGIN")
	run(a, "UPDATE items SET name='c' WHERE id=1")
	run(s, "TRUNCATE TABLE items")
	if _, err = e.Execute(a, "COMMIT"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("schema conflict %v", err)
	}
	run(a, "BEGIN")
	run(a, "INSERT INTO items VALUES(1,'a')")
	if _, err = e.Execute(a, "INSERT INTO items VALUES(2,'b'),(3,'a')"); err == nil {
		t.Fatal("duplicate accepted")
	}
	run(a, "COMMIT")
	r, err := e.Execute(s, "SELECT COUNT(*) FROM items")
	if err != nil || r.Rows[0][0] != int64(1) {
		t.Fatalf("statement rollback %+v %v", r, err)
	}
	run(s, "DROP DATABASE test")
	if _, err = e.Execute(s, "SELECT * FROM items"); err == nil {
		t.Fatal("dropped table survived")
	}
}

func TestMVCCModeDirectoryGuards(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, "root", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if e.Backend == nil {
		t.Fatal("default engine is not MVCC")
	}
	e.Close()
	e, err = OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	for _, mode := range []string{"snapshot", "paged", "legacy", "unknown"} {
		if _, err = OpenWithOptions(dir, "root", "pw", OpenOptions{StorageMode: mode}); err == nil {
			t.Fatalf("accepted %s", mode)
		}
	}
	legacy := t.TempDir()
	old, err := openLegacy(legacy, "root", "pw")
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	if _, err = Open(legacy, "root", "pw"); err == nil {
		t.Fatal("opened legacy data without migration")
	}
}
