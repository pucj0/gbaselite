package executor

import (
	"errors"
	"fmt"
	"gbaselite/storageengine"
	"testing"
)

func TestMVCCAlterAtomicSnapshotAndPhantom(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE alt(id INT PRIMARY KEY AUTO_INCREMENT,v INT)")
	run("INSERT INTO alt VALUES(1,10),(100,20)")
	run("DELETE FROM alt WHERE id=100")
	old := &Session{CurrentDatabase: "test"}
	if _, err := e.Execute(old, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	run("ALTER TABLE alt ADD COLUMN n VARCHAR(20) DEFAULT 'filled'")
	if r := run("SELECT * FROM alt"); fmt.Sprint(r.Rows) != "[[1 10 filled]]" {
		t.Fatal(r.Rows)
	}
	r, err := e.Execute(old, "SELECT * FROM alt")
	if err != nil || len(r.Columns) != 2 {
		t.Fatal("schema snapshot changed", r, err)
	}
	e.CloseSession(old)
	run("CREATE INDEX v_idx ON alt(v)")
	if r := run("EXPLAIN SELECT id,v FROM alt WHERE v=10"); r.Rows[0][4] != "ref" {
		t.Fatal(r.Rows)
	}
	run("INSERT INTO alt(v) VALUES(30)")
	if r := run("SELECT id FROM alt WHERE v=30"); fmt.Sprint(r.Rows) != "[[101]]" {
		t.Fatal("counter reused", r.Rows)
	}
	run("ALTER TABLE alt RENAME COLUMN n TO note")
	run("ALTER TABLE alt MODIFY COLUMN v BIGINT")
	run("ALTER TABLE alt DROP COLUMN note")
	run("BEGIN")
	run("ALTER TABLE alt ADD COLUMN pending INT DEFAULT 1")
	other := &Session{CurrentDatabase: "test"}
	if _, err = e.Execute(other, "INSERT INTO alt(v) VALUES(40)"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Execute(s, "COMMIT"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatal("DDL missed phantom", err)
	}
	if r := run("SELECT * FROM alt"); len(r.Columns) != 2 || len(r.Rows) != 3 {
		t.Fatal(r)
	}
	run("BEGIN")
	run("ALTER TABLE alt ADD COLUMN rolled INT")
	run("ROLLBACK")
	if r := run("SELECT * FROM alt"); len(r.Columns) != 2 {
		t.Fatal("DDL rollback", r.Columns)
	}
	run("INSERT INTO alt(v) VALUES(40)")
	if _, err = e.Execute(s, "CREATE UNIQUE INDEX unique_v ON alt(v)"); err == nil {
		t.Fatal("duplicate index accepted")
	}
	if r := run("SELECT COUNT(*) FROM alt"); fmt.Sprint(r.Rows) != "[[4]]" {
		t.Fatal(r.Rows)
	}
}
func TestMVCCCheckValidation(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE checked(id INT PRIMARY KEY,v INT,CONSTRAINT positive CHECK(v>=0))")
	run("INSERT INTO checked VALUES(1,1),(2,NULL)")
	for _, q := range []string{"INSERT INTO checked VALUES(3,-1)", "UPDATE checked SET v=-1 WHERE id=1", "ALTER TABLE checked ADD CONSTRAINT large CHECK(v>10)"} {
		if _, err := e.Execute(s, q); err == nil {
			t.Fatal("accepted violation", q)
		}
	}
	if r := run("SELECT v FROM checked WHERE id=1"); fmt.Sprint(r.Rows) != "[[1]]" {
		t.Fatal(r.Rows)
	}
	run("ALTER TABLE checked DROP CHECK positive")
	run("UPDATE checked SET v=-1 WHERE id=1")
}
