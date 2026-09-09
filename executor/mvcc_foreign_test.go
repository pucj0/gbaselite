package executor

import (
	"errors"
	"fmt"
	"gbaselite/mvcc"
	"testing"
)

func TestMVCCForeignKeyRaces(t *testing.T) {
	for _, wal := range []bool{false, true} {
		t.Run(fmt.Sprint(wal), func(t *testing.T) {
			e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc", LocalWAL: wal})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			s := &Session{}
			run := func(s *Session, q string) *Result {
				t.Helper()
				r, err := e.Execute(s, q)
				if err != nil {
					t.Fatal(q, err)
				}
				return r
			}
			run(s, "CREATE DATABASE test")
			run(s, "USE test")
			run(s, "CREATE TABLE parent(id INT PRIMARY KEY,v INT)")

			run(s, "CREATE DATABASE other")
			if _, err := e.Execute(s, "CREATE TABLE other.rejected(id INT PRIMARY KEY,pid INT,FOREIGN KEY(pid) REFERENCES test.parent(id))"); err == nil {
				t.Fatal("accepted cross-database FK")
			}
			run(s, "CREATE TABLE other.parent(id INT PRIMARY KEY)")
			run(s, "CREATE TABLE other.child(id INT PRIMARY KEY,pid INT,FOREIGN KEY(pid) REFERENCES parent(id))")
			run(s, "INSERT INTO other.parent VALUES(99)")
			run(s, "INSERT INTO other.child VALUES(1,99)")
			run(s, "CREATE TABLE child(id INT PRIMARY KEY,pid INT,CONSTRAINT fk_parent FOREIGN KEY(pid) REFERENCES parent(id))")
			run(s, "INSERT INTO parent VALUES(1,10),(2,20)")
			run(s, "INSERT INTO child VALUES(1,1),(2,NULL)")
			for _, q := range []string{"INSERT INTO child VALUES(3,99)", "DELETE FROM parent WHERE id=1", "UPDATE parent SET id=3 WHERE id=1", "DROP TABLE parent", "TRUNCATE TABLE parent"} {
				if _, err := e.Execute(s, q); err == nil {
					t.Fatal("accepted FK violation", q)
				}
			}
			a, b := &Session{CurrentDatabase: "test"}, &Session{CurrentDatabase: "test"}
			run(a, "BEGIN")
			run(a, "DELETE FROM parent WHERE id=2")
			run(b, "BEGIN")
			run(b, "INSERT INTO child VALUES(3,2)")
			run(b, "COMMIT")
			if _, err = e.Execute(a, "COMMIT"); !errors.Is(err, mvcc.ErrConflict) {
				t.Fatal("parent deletion missed phantom", err)
			}
			run(s, "INSERT INTO parent VALUES(4,40)")
			run(a, "BEGIN")
			run(a, "INSERT INTO child VALUES(4,4)")
			run(b, "DELETE FROM parent WHERE id=4")
			if _, err = e.Execute(a, "COMMIT"); !errors.Is(err, mvcc.ErrConflict) {
				t.Fatal("child accepted removed parent", err)
			}
			run(s, "ALTER TABLE child DROP FOREIGN KEY fk_parent")
			run(s, "DELETE FROM parent WHERE id=1")
			if _, err = e.Execute(s, "ALTER TABLE child ADD CONSTRAINT again FOREIGN KEY(pid) REFERENCES parent(id)"); err == nil {
				t.Fatal("accepted invalid existing rows")
			}
		})
	}
}
