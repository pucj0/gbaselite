package executor

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gbaselite/storage"
)

// parityStep is one scripted statement in a legacy/MVCC parity run. Rows compares
// the full result set, InsertID the reported auto-increment value, Fail requires
// both engines to reject the statement, and SkipAffect ignores the OK-packet
// affected-rows difference that DDL statements have between the two runtimes.
type parityStep struct {
	Query      string
	Rows       bool
	InsertID   bool
	Fail       bool
	SkipAffect bool
}

type parityOutcome struct {
	Failed   bool
	Affected uint64
	InsertID uint64
	Rows     string
}

func runParityScript(t *testing.T, execute func(string) (*Result, error), steps []parityStep) []parityOutcome {
	t.Helper()
	outcomes := make([]parityOutcome, 0, len(steps))
	for _, step := range steps {
		result, err := execute(step.Query)
		if err != nil {
			if !step.Fail {
				t.Fatalf("%s: %v", step.Query, err)
			}
			outcomes = append(outcomes, parityOutcome{Failed: true})
			continue
		}
		if step.Fail {
			t.Fatalf("%s: expected an error", step.Query)
		}
		outcome := parityOutcome{}
		if result != nil {
			if !step.SkipAffect {
				outcome.Affected = result.AffectedRows
			}
			if step.InsertID {
				outcome.InsertID = result.LastInsertID
			}
			if step.Rows {
				outcome.Rows = fmt.Sprint(result.Rows)
			}
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// insertSelectScript is the shared legacy/MVCC INSERT SELECT fixture.
var insertSelectScript = []parityStep{
	{Query: "CREATE DATABASE is_parity", SkipAffect: true},
	{Query: "USE is_parity", SkipAffect: true},
	{Query: "CREATE TABLE src(id INT PRIMARY KEY,v INT,note VARCHAR(20))", SkipAffect: true},
	{Query: "CREATE TABLE dst(id INT PRIMARY KEY,v INT,note VARCHAR(20),flag INT DEFAULT 7)", SkipAffect: true},
	{Query: "CREATE TABLE dst2(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE dst3(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE auto_t(id INT AUTO_INCREMENT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "CREATE TABLE dst_ck(id INT PRIMARY KEY,v INT,CHECK(v>=0))", SkipAffect: true},
	{Query: "CREATE TABLE parent(id INT PRIMARY KEY)", SkipAffect: true},
	{Query: "CREATE TABLE child(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES parent(id))", SkipAffect: true},
	{Query: "CREATE TABLE src_txt(id INT PRIMARY KEY,v VARCHAR(20))", SkipAffect: true},
	{Query: "CREATE TABLE dst_int(id INT PRIMARY KEY,v INT)", SkipAffect: true},
	{Query: "INSERT INTO src VALUES(1,10,'a'),(2,20,'b'),(3,30,NULL)"},
	{Query: "INSERT INTO parent VALUES(1)"},
	{Query: "INSERT INTO src_txt VALUES(1,'abc')"},

	// Column mapping plus defaults for omitted columns.
	{Query: "INSERT INTO dst(id,v,note) SELECT id,v,note FROM src ORDER BY id"},
	{Query: "SELECT id,v,note,flag FROM dst ORDER BY id", Rows: true},
	// Reordered column list.
	{Query: "INSERT INTO dst2(v,id) SELECT v,id FROM src ORDER BY id"},
	{Query: "SELECT id,v FROM dst2 ORDER BY id", Rows: true},
	// Empty source inserts nothing and stays successful.
	{Query: "INSERT INTO dst3 SELECT id,v FROM src WHERE 1=0"},
	{Query: "SELECT COUNT(*) FROM dst3", Rows: true},
	// UNION ALL source.
	{Query: "INSERT INTO dst3(id,v) SELECT id,v FROM src WHERE id=1 UNION ALL SELECT id,v FROM src WHERE id=2"},
	{Query: "SELECT id,v FROM dst3 ORDER BY id", Rows: true},
	// Auto-increment generation and LastInsertID.
	{Query: "INSERT INTO auto_t(v) SELECT v FROM src ORDER BY id", InsertID: true},
	{Query: "SELECT id,v FROM auto_t ORDER BY id", Rows: true},
	// Self source: only rows from the statement snapshot are copied once.
	{Query: "INSERT INTO auto_t(v) SELECT v FROM auto_t ORDER BY id"},
	{Query: "SELECT COUNT(*) FROM auto_t", Rows: true},
	// Explicit auto-increment values and the following generated value.
	{Query: "INSERT INTO auto_t(id,v) SELECT 100,10", InsertID: true},
	{Query: "INSERT INTO auto_t(v) VALUES(55)", InsertID: true},
	{Query: "SELECT id,v FROM auto_t WHERE id>=100 ORDER BY id", Rows: true},

	// Failures must leave the destination untouched.
	{Query: "INSERT INTO dst2 SELECT id FROM src", Fail: true},
	{Query: "INSERT INTO dst SELECT id,v FROM src", Fail: true},
	{Query: "SELECT COUNT(*) FROM dst", Rows: true},
	{Query: "INSERT INTO dst_ck SELECT id,-1 FROM src", Fail: true},
	{Query: "SELECT COUNT(*) FROM dst_ck", Rows: true},
	{Query: "INSERT INTO child SELECT id,99 FROM src", Fail: true},
	{Query: "SELECT COUNT(*) FROM child", Rows: true},
	{Query: "INSERT INTO dst_int SELECT id,v FROM src_txt", Fail: true},
	{Query: "SELECT COUNT(*) FROM dst_int", Rows: true},
}

func TestMVCCInsertSelectMatchesLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runParityScript(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, insertSelectScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runParityScript(t, func(q string) (*Result, error) { return e.Execute(session, q) }, insertSelectScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range insertSelectScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCInsertSelectSelfSourceSnapshot(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO t VALUES(1,10),(2,20)")
	inserted := run("INSERT INTO t SELECT id+10,v FROM t")
	if inserted.AffectedRows != 2 {
		t.Fatalf("affected rows=%d, want the two snapshot rows only", inserted.AffectedRows)
	}
	rows := run("SELECT id,v FROM t ORDER BY id")
	if got := fmt.Sprint(rows.Rows); got != "[[1 10] [2 20] [11 10] [12 20]]" {
		t.Fatalf("self-source re-read its own output: %s", got)
	}
}

func TestMVCCInsertSelectRollsBackOnFailure(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT UNIQUE,CHECK(v>=0))")
	run("CREATE TABLE parent(id INT PRIMARY KEY)")
	run("CREATE TABLE child(id INT PRIMARY KEY,pid INT,CONSTRAINT fk FOREIGN KEY(pid) REFERENCES parent(id))")
	run("CREATE TABLE src_txt(id INT PRIMARY KEY,v VARCHAR(20))")
	run("CREATE TABLE dst_int(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,1),(2,2),(3,3)")
	run("INSERT INTO parent VALUES(1)")
	run("INSERT INTO src_txt VALUES(1,'abc')")
	run("INSERT INTO dst VALUES(2,2)")
	cases := []struct {
		query string
		want  error
	}{
		{"INSERT INTO dst SELECT id,v FROM src", storage.ErrDuplicateKey},
		{"INSERT INTO dst SELECT id+100,-1 FROM src", storage.ErrCheckConstraint},
		{"INSERT INTO child SELECT id,99 FROM src", storage.ErrForeignKey},
		{"INSERT INTO dst_int SELECT id,v FROM src_txt", storage.ErrTypeMismatch},
	}
	for _, c := range cases {
		if _, err := e.Execute(s, c.query); !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v, want %v", c.query, err, c.want)
		}
	}
	if got := fmt.Sprint(run("SELECT id,v FROM dst ORDER BY id").Rows); got != "[[2 2]]" {
		t.Fatalf("failed INSERT SELECT kept partial rows: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM child").Rows); got != "[[0]]" {
		t.Fatalf("failed INSERT SELECT kept child rows: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst_int").Rows); got != "[[0]]" {
		t.Fatalf("failed INSERT SELECT kept converted rows: %s", got)
	}
}

func TestMVCCInsertSelectColumnCountMismatch(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,10)")
	if _, err := e.Execute(s, "INSERT INTO dst SELECT id FROM src"); !errors.Is(err, storage.ErrColumnCount) {
		t.Fatalf("column count error = %v", err)
	}
	if _, err := e.Execute(s, "INSERT INTO dst(id) SELECT id,v FROM src"); !errors.Is(err, storage.ErrColumnCount) {
		t.Fatalf("column count error = %v", err)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[0]]" {
		t.Fatalf("mismatched INSERT SELECT inserted rows: %s", got)
	}
}

func TestMVCCInsertSelectColumnMappingAndDefaults(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,a INT DEFAULT 7,b INT,c INT DEFAULT 9)")
	run("INSERT INTO src VALUES(1,10),(2,20)")
	inserted := run("INSERT INTO dst(b,id) SELECT v,id FROM src ORDER BY id")
	if inserted.AffectedRows != 2 {
		t.Fatalf("affected rows=%d", inserted.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT id,a,b,c FROM dst ORDER BY id").Rows); got != "[[1 7 10 9] [2 7 20 9]]" {
		t.Fatalf("column mapping/defaults = %s", got)
	}
}

func TestMVCCInsertSelectAutoIncrementAndLastInsertID(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT AUTO_INCREMENT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,10),(2,20),(3,30)")
	generated := run("INSERT INTO dst(v) SELECT v FROM src ORDER BY id")
	if generated.AffectedRows != 3 || generated.LastInsertID != 1 {
		t.Fatalf("generated insert = affected %d last id %d", generated.AffectedRows, generated.LastInsertID)
	}
	if got := fmt.Sprint(run("SELECT id,v FROM dst ORDER BY id").Rows); got != "[[1 10] [2 20] [3 30]]" {
		t.Fatalf("generated ids = %s", got)
	}
	explicit := run("INSERT INTO dst(id,v) SELECT 100,10")
	if explicit.AffectedRows != 1 || explicit.LastInsertID != 0 {
		t.Fatalf("explicit insert = affected %d last id %d", explicit.AffectedRows, explicit.LastInsertID)
	}
	run("INSERT INTO dst(v) VALUES(55)")
	if got := fmt.Sprint(run("SELECT id FROM dst WHERE v=55").Rows); got != "[[101]]" {
		t.Fatalf("counter after explicit id = %s", got)
	}
}

func TestMVCCInsertSelectMatchesValuesSemantics(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE viaValues(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE viaSelect(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,10),(2,20),(3,30)")
	// MVCC converts whole-float expression results to integer columns in both
	// paths, unlike the legacy INSERT SELECT reader.
	run("INSERT INTO viaValues VALUES(1001,11),(1002,21),(1003,31)")
	run("INSERT INTO viaSelect SELECT id+1000,v+1 FROM src ORDER BY id")
	values := run("SELECT id,v FROM viaValues ORDER BY id")
	selects := run("SELECT id,v FROM viaSelect ORDER BY id")
	if !reflect.DeepEqual(values.Rows, selects.Rows) {
		t.Fatalf("INSERT VALUES %v differs from INSERT SELECT %v", values.Rows, selects.Rows)
	}
	if _, err := e.Execute(s, "INSERT INTO viaSelect SELECT id,v FROM src WHERE 1=0"); err != nil {
		t.Fatal(err)
	}
}

func TestMVCCInsertSelectUsesStatementSnapshot(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE dst_other(id INT PRIMARY KEY,v INT)")
	run("INSERT INTO src VALUES(1,10),(2,20)")
	a := &Session{CurrentDatabase: "test"}
	runA := func(query string) *Result {
		t.Helper()
		result, err := e.Execute(a, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return result
	}
	runA("BEGIN")
	if affected := runA("INSERT INTO dst SELECT id,v FROM src ORDER BY id").AffectedRows; affected != 2 {
		t.Fatalf("transaction insert affected=%d", affected)
	}
	// Read-your-own-write: the same transaction sees its inserted rows, and a
	// second INSERT SELECT scanning the destination uses them as its snapshot.
	if got := fmt.Sprint(runA("SELECT COUNT(*) FROM dst").Rows); got != "[[2]]" {
		t.Fatalf("transaction did not read its own write: %s", got)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[0]]" {
		t.Fatalf("uncommitted rows leaked to another session: %s", got)
	}
	// Concurrent committed source rows stay outside the transaction snapshot.
	run("INSERT INTO src VALUES(3,30)")
	if affected := runA("INSERT INTO dst_other SELECT id,v FROM src ORDER BY id").AffectedRows; affected != 2 {
		t.Fatalf("transaction saw source rows outside its snapshot: affected=%d", affected)
	}
	runA("ROLLBACK")
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM dst").Rows); got != "[[0]]" {
		t.Fatalf("rollback kept inserted rows: %s", got)
	}
	runA("BEGIN")
	runA("INSERT INTO dst SELECT id,v FROM src ORDER BY id")
	runA("COMMIT")
	if got := fmt.Sprint(run("SELECT id,v FROM dst ORDER BY id").Rows); got != "[[1 10] [2 20] [3 30]]" {
		t.Fatalf("committed rows = %s", got)
	}
}

func TestMVCCInsertSelectLastInsertIDRollback(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE src(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE t(id INT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE)")
	run("INSERT INTO src VALUES(1,5),(2,5)")
	s.LastInsertID = 42
	// The first row reserves a generated id; the second row fails on the unique
	// value, so the statement rolls back and must not publish that id.
	if _, err := e.Execute(s, "INSERT INTO t(v) SELECT v FROM src ORDER BY id"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate key error = %v", err)
	}
	if s.LastInsertID != 42 {
		t.Fatalf("rolled-back INSERT SELECT published LastInsertID=%d", s.LastInsertID)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[0]]" {
		t.Fatalf("rolled-back INSERT SELECT kept rows: %s", got)
	}
	if got := fmt.Sprint(run("SELECT LAST_INSERT_ID()").Rows); got != "[[42]]" {
		t.Fatalf("LAST_INSERT_ID() after rollback = %s", got)
	}
	// A successful statement publishes its first generated id, and later
	// failures or explicit-id statements must not move it again.
	run("DELETE FROM src WHERE id=2")
	generated := run("INSERT INTO t(v) SELECT v FROM src ORDER BY id")
	if generated.LastInsertID == 0 || s.LastInsertID != generated.LastInsertID {
		t.Fatalf("successful INSERT SELECT last id = %d session = %d", generated.LastInsertID, s.LastInsertID)
	}
	saved := s.LastInsertID
	run("INSERT INTO src VALUES(3,5)")
	if _, err := e.Execute(s, "INSERT INTO t(v) SELECT v FROM src WHERE id=3"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate key error = %v", err)
	}
	if s.LastInsertID != saved {
		t.Fatalf("failed INSERT SELECT moved LastInsertID to %d, want %d", s.LastInsertID, saved)
	}
	run("INSERT INTO t(id,v) VALUES(50,50)")
	if s.LastInsertID != saved {
		t.Fatalf("explicit id INSERT SELECT moved LastInsertID to %d, want %d", s.LastInsertID, saved)
	}
}

func TestMVCCInsertValuesLastInsertIDRollback(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE)")
	s.LastInsertID = 42
	if _, err := e.Execute(s, "INSERT INTO t(v) VALUES(5),(5)"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate key error = %v", err)
	}
	if s.LastInsertID != 42 {
		t.Fatalf("rolled-back INSERT VALUES published LastInsertID=%d", s.LastInsertID)
	}
	if got := fmt.Sprint(run("SELECT COUNT(*) FROM t").Rows); got != "[[0]]" {
		t.Fatalf("rolled-back INSERT VALUES kept rows: %s", got)
	}
	inserted := run("INSERT INTO t(v) VALUES(7),(8)")
	if inserted.LastInsertID == 0 || s.LastInsertID != inserted.LastInsertID {
		t.Fatalf("successful INSERT VALUES last id = %d session = %d", inserted.LastInsertID, s.LastInsertID)
	}
}
