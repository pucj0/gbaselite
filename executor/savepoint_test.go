package executor

import (
	"errors"
	"fmt"
	"gbaselite/storage"
	"math/rand"
	"reflect"
	"testing"
)

func savepointEngine(t *testing.T) (*legacyEngine, *Session, func(string) *Result) {
	t.Helper()
	e, err := openLegacy(t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{}
	t.Cleanup(func() { e.CloseSession(s); e.Close() })
	run := func(q string) *Result {
		t.Helper()
		r, err := e.Execute(s, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	run("CREATE DATABASE sp")
	run("USE sp")
	run("CREATE TABLE items(id INT AUTO_INCREMENT PRIMARY KEY, value INT UNIQUE)")
	return e, s, run
}

func TestLegacySavepointRollbackAndPersistence(t *testing.T) {
	e, s, run := savepointEngine(t)
	run("INSERT INTO items(value) VALUES(10)")
	run("BEGIN")
	run("SAVEPOINT a")
	run("INSERT INTO items(value) VALUES(20)")
	run("SAVEPOINT b")
	run("UPDATE items SET value=99 WHERE id=1")
	run("ROLLBACK WORK TO SAVEPOINT A")
	if _, err := e.Execute(s, "ROLLBACK TO b"); !errors.Is(err, ErrSavepointNotFound) {
		t.Fatalf("later point: %v", err)
	}
	if got := run("SELECT id,value FROM items").Rows; !reflect.DeepEqual(got, [][]any{{int64(1), int64(10)}}) {
		t.Fatal(got)
	}
	run("INSERT INTO items(value) VALUES(30)")
	run("ROLLBACK TO a")
	run("INSERT INTO items(value) VALUES(40)")
	run("RELEASE SAVEPOINT A")
	run("COMMIT")
	if e.legacyState(s).savepoints != nil || e.legacyState(s).binlogStatements != nil {
		t.Fatal("commit retained transaction references")
	}
	got := run("SELECT id,value FROM items ORDER BY id").Rows
	want := [][]any{{int64(1), int64(10)}, {int64(4), int64(40)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v != %v", got, want)
	}

}

func TestLegacySavepointLifecycleAndLimits(t *testing.T) {
	e, s, run := savepointEngine(t)
	run("SAVEPOINT ignored")
	for _, q := range []string{"ROLLBACK TO ignored", "RELEASE SAVEPOINT ignored"} {
		if _, err := e.Execute(s, q); !errors.Is(err, ErrSavepointNotFound) {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run("BEGIN")
	for i := 0; i < maxTransactionSavepoints; i++ {
		run(fmt.Sprintf("SAVEPOINT s%d", i))
	}
	if _, err := e.Execute(s, "SAVEPOINT overflow"); !errors.Is(err, ErrResourceLimit) {
		t.Fatal(err)
	}
	run("SAVEPOINT S0") // replacement is permitted at the limit and moves to the end
	run("RELEASE SAVEPOINT s1")
	if len(e.legacyState(s).savepoints) != 31 {
		t.Fatal(len(e.legacyState(s).savepoints))
	}
	run("ROLLBACK TO s2")
	if len(e.legacyState(s).savepoints) != 1 || e.legacyState(s).savepoints[0].name != "s2" {
		t.Fatal("replacement/release order")
	}
	run("INSERT INTO items(value) VALUES(1)")
	if len(e.legacyState(s).binlogStatements) != 0 {
		t.Fatal("disabled binlog retained SQL")
	}
	backing := e.legacyState(s).savepoints[:cap(e.legacyState(s).savepoints)]
	for _, point := range backing[1:] {
		if point.snapshot.Databases != nil {
			t.Fatal("discarded savepoint retained rows")
		}
	}
	e.CloseSession(s)
	if e.legacyState(s).savepoints != nil || e.legacyState(s).transaction != nil {
		t.Fatal("disconnect retained transaction")
	}
	if len(run("SELECT * FROM items").Rows) != 0 {
		t.Fatal("disconnect committed rows")
	}
	run("BEGIN")
	run("SAVEPOINT end")
	run("ROLLBACK")
	if e.legacyState(s).savepoints != nil {
		t.Fatal("rollback retained savepoints")
	}
}

func TestLegacySavepointDDLAndConstraints(t *testing.T) {
	e, s, run := savepointEngine(t)
	run("CREATE TABLE child(id INT PRIMARY KEY,parent INT,FOREIGN KEY(parent) REFERENCES items(id) ON DELETE CASCADE)")
	run("INSERT INTO items(value) VALUES(1)")
	run("INSERT INTO child VALUES(1,1)")
	run("BEGIN")
	run("SAVEPOINT before_ddl")
	run("CREATE TABLE added(id INT)")
	run("CREATE INDEX secondary ON child(parent)")
	run("ALTER TABLE items ADD COLUMN extra INT DEFAULT 3")
	run("DELETE FROM items WHERE id=1")
	if len(run("SELECT * FROM child").Rows) != 0 {
		t.Fatal("cascade failed")
	}
	run("ROLLBACK TO before_ddl")
	if len(run("SELECT * FROM child").Rows) != 1 {
		t.Fatal("cascade rollback failed")
	}
	if _, err := e.Execute(s, "SELECT * FROM added"); err == nil {
		t.Fatal("created table survived")
	}
	if _, err := e.Execute(s, "SELECT extra FROM items"); err == nil {
		t.Fatal("added column survived")
	}
	if _, err := e.Execute(s, "INSERT INTO child VALUES(2,999)"); err == nil {
		t.Fatal("foreign key lost")
	}
	if _, err := e.Execute(s, "INSERT INTO items(value) VALUES(1)"); err == nil {
		t.Fatal("unique constraint lost")
	}
	for _, q := range []string{"DROP DATABASE sp", "DROP TABLE items", "TRUNCATE items", "RENAME TABLE items TO renamed", "ALTER TABLE items RENAME COLUMN id TO other", "ALTER TABLE items DROP COLUMN value", "ALTER TABLE items MODIFY COLUMN value BIGINT", "ALTER TABLE items ADD COLUMN added INT, DROP COLUMN value"} {
		if _, err := e.Execute(s, q); err == nil {
			t.Fatalf("identity change accepted: %s", q)
		}
	}
	run("ROLLBACK TO before_ddl")
	run("RELEASE SAVEPOINT before_ddl")
	run("ALTER TABLE items RENAME COLUMN value TO renamed")
	run("ROLLBACK")
}

func TestLegacySavepointRandomModel(t *testing.T) {
	_, _, run := savepointEngine(t)
	run("BEGIN")
	type point struct {
		name string
		rows [][]any
	}
	var points []point
	var rows [][]any
	random := rand.New(rand.NewSource(9182))
	nextID := int64(1)
	for step := 0; step < 200; step++ {
		switch random.Intn(5) {
		case 0, 1:
			run(fmt.Sprintf("INSERT INTO items(value) VALUES(%d)", step+100))
			rows = append(rows, []any{nextID, int64(step + 100)})
			nextID++
		case 2:
			name := fmt.Sprintf("p%d", random.Intn(8))
			for i, p := range points {
				if p.name == name {
					points = append(points[:i], points[i+1:]...)
					break
				}
			}
			run("SAVEPOINT " + name)
			points = append(points, point{name, append([][]any(nil), rows...)})
		case 3:
			if len(points) > 0 {
				i := random.Intn(len(points))
				run("ROLLBACK TO " + points[i].name)
				rows = append([][]any(nil), points[i].rows...)
				points = points[:i+1]
			}
		case 4:
			if len(points) > 0 {
				i := random.Intn(len(points))
				run("RELEASE SAVEPOINT " + points[i].name)
				points = append(points[:i], points[i+1:]...)
			}
		}
		got := run("SELECT id,value FROM items ORDER BY id").Rows
		if len(got) != len(rows) || len(rows) > 0 && !reflect.DeepEqual(got, rows) {
			t.Fatalf("step %d: %v != %v", step, got, rows)
		}
	}
	run("COMMIT")
}

func TestLegacySavepointCommitFailureKeepsDurableSnapshot(t *testing.T) {
	path := t.TempDir()
	e, err := openLegacy(path, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{}
	for _, q := range []string{"CREATE DATABASE sp", "USE sp", "CREATE TABLE items(id INT PRIMARY KEY)", "INSERT INTO items VALUES(1)", "BEGIN"} {
		if _, err := e.Execute(s, q); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	e.persistSave = func(*storage.Store) error { calls++; return errors.New("injected commit failure") }
	for _, q := range []string{"SAVEPOINT a", "INSERT INTO items VALUES(2)", "ROLLBACK TO a", "INSERT INTO items VALUES(3)"} {
		if _, err := e.Execute(s, q); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("savepoint unexpectedly persisted")
	}
	if _, err := e.Execute(s, "COMMIT"); !errors.Is(err, ErrPersistenceUnavailable) {
		t.Fatalf("commit: %v", err)
	}
	if e.legacyState(s).savepoints != nil || e.legacyState(s).transaction != nil || e.legacyState(s).transactionGate {
		t.Fatal("failed commit retained state/lock")
	}
	if _, err := e.Execute(s, "SAVEPOINT a"); !errors.Is(err, ErrPersistenceUnavailable) {
		t.Fatalf("failed engine accepted savepoint: %v", err)
	}
	if err := e.Close(); !errors.Is(err, ErrPersistenceUnavailable) {
		t.Fatal(err)
	}
	reopened, err := openLegacy(path, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Execute(&Session{CurrentDatabase: "sp"}, "SELECT id FROM items")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Rows, [][]any{{int64(1)}}) || calls != 1 {
		t.Fatalf("failed commit reached disk: %v, calls %d", got.Rows, calls)
	}
}
