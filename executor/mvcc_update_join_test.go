package executor

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gbaselite/storage"
)

// sqlStep is one scripted statement in an engine parity run. Rows asks the
// harness to compare the full result set as well as affected rows.
type sqlStep struct {
	Query string
	Rows  bool
	// SkipAffected excludes statements whose OK-packet affected rows differ between the
	// legacy and MVCC runtimes for unrelated DDL reporting reasons.
	SkipAffected bool
}

type sqlOutcome struct {
	Affected uint64
	Rows     string
}

func runSQLSteps(t *testing.T, execute func(string) (*Result, error), steps []sqlStep) []sqlOutcome {
	t.Helper()
	outcomes := make([]sqlOutcome, 0, len(steps))
	for _, step := range steps {
		result, err := execute(step.Query)
		if err != nil {
			t.Fatalf("%s: %v", step.Query, err)
		}
		outcome := sqlOutcome{}
		if result != nil {
			if !step.SkipAffected {
				outcome.Affected = result.AffectedRows
			}
			if step.Rows {
				outcome.Rows = fmt.Sprint(result.Rows)
			}
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// updateJoinScript is the shared legacy/MVCC UPDATE JOIN fixture. Every step runs
// against both engines; affected rows and (when Rows is set) result rows must
// match exactly, which is the old-to-MVCC parity contract.
var updateJoinScript = []sqlStep{
	{Query: "CREATE DATABASE parity", SkipAffected: true},
	{Query: "USE parity", SkipAffected: true},
	{Query: "CREATE TABLE users(id INT NOT NULL,balance INT NOT NULL,marker INT,label VARCHAR(30),email VARCHAR(40),PRIMARY KEY(id),UNIQUE KEY uq_email(email))", SkipAffected: true},
	{Query: "CREATE TABLE adjustments(id INT NOT NULL,user_id INT NOT NULL,delta INT NOT NULL,PRIMARY KEY(id),KEY user_idx(user_id))", SkipAffected: true},
	{Query: "INSERT INTO users VALUES (1,10,0,'start','one@example.com'),(2,20,0,'second','two@example.com'),(3,30,0,'third','three@example.com')"},
	{Query: "INSERT INTO adjustments VALUES (1,1,3),(2,1,7),(3,2,5)"},
	// A target matched by two source rows updates once, keeps legacy first-match
	// selection, and sees earlier assignments inside the same SET list.
	{Query: "UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.balance=u.balance+a.delta,u.label=CONCAT('balance-',u.balance) WHERE u.id=1"},
	{Query: "SELECT id,balance,label,marker FROM users ORDER BY id", Rows: true},
	{Query: "UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.marker=a.delta"},
	{Query: "SELECT id,marker FROM users ORDER BY id", Rows: true},
	// LEFT JOIN updates every target row and null-extends unmatched sources.
	{Query: "UPDATE users u LEFT JOIN adjustments a ON a.user_id=u.id SET u.marker=a.delta"},
	{Query: "SELECT id,marker FROM users ORDER BY id", Rows: true},
	{Query: "UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.label='limited' LIMIT 1"},
	{Query: "SELECT COUNT(*) FROM users WHERE label='limited'", Rows: true},
}

func TestMVCCUpdateJoinMatchesLegacyEngine(t *testing.T) {
	legacy, err := openLegacy(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacySession := &Session{}
	legacyOutcomes := runSQLSteps(t, func(q string) (*Result, error) { return legacy.Execute(legacySession, q) }, updateJoinScript)

	e, session, _ := rangeTestEngine(t)
	mvccOutcomes := runSQLSteps(t, func(q string) (*Result, error) { return e.Execute(session, q) }, updateJoinScript)

	if !reflect.DeepEqual(legacyOutcomes, mvccOutcomes) {
		for i, step := range updateJoinScript {
			if legacyOutcomes[i] != mvccOutcomes[i] {
				t.Errorf("%s\nlegacy=%+v\nmvcc=%+v", step.Query, legacyOutcomes[i], mvccOutcomes[i])
			}
		}
	}
}

func TestMVCCUpdateJoinDeduplicatesRepeatedTargetMatches(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE a(id INT PRIMARY KEY, v INT)")
	run("CREATE TABLE b(id INT PRIMARY KEY, aid INT, delta INT, KEY aid_idx(aid))")
	run("INSERT INTO a VALUES(1,10)")
	run("INSERT INTO b VALUES(1,1,2),(2,1,3)")
	updated := run("UPDATE a JOIN b ON a.id=b.aid SET a.v=a.v+b.delta")
	if updated.AffectedRows != 1 {
		t.Fatalf("affected rows=%d, want one mutation per target row", updated.AffectedRows)
	}
	rows := run("SELECT id,v FROM a ORDER BY id")
	if got := fmt.Sprint(rows.Rows); got != "[[1 12]]" {
		t.Fatalf("duplicate source match mutated the target repeatedly: %s", got)
	}
}

func TestMVCCUpdateJoinRollsBackOnConstraintFailure(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE users(id INT PRIMARY KEY,email VARCHAR(40) UNIQUE,score INT,CHECK(score>=0))")
	run("CREATE TABLE adjustments(id INT PRIMARY KEY,user_id INT,delta INT,KEY user_idx(user_id))")
	run("INSERT INTO users VALUES(1,'one@x.com',10),(2,'two@x.com',20)")
	run("INSERT INTO adjustments VALUES(1,1,5),(2,2,-100)")

	// Both target rows collapse onto one unique value: whichever row is written
	// first, the second must fail and roll the first back.
	if _, err := e.Execute(s, "UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.email='same@x.com'"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate key error = %v", err)
	}
	if _, err := e.Execute(s, "UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.score=u.score+a.delta"); !errors.Is(err, storage.ErrCheckConstraint) {
		t.Fatalf("CHECK error = %v", err)
	}
	rows := run("SELECT id,email,score FROM users ORDER BY id")
	if got := fmt.Sprint(rows.Rows); got != "[[1 one@x.com 10] [2 two@x.com 20]]" {
		t.Fatalf("failed UPDATE JOIN left partial mutations: %s", got)
	}
}

func TestMVCCUpdateJoinRejectsInvalidAssignments(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE users(id INT PRIMARY KEY,score INT,label VARCHAR(20))")
	run("CREATE TABLE adjustments(id INT PRIMARY KEY,user_id INT,delta INT,KEY user_idx(user_id))")
	run("INSERT INTO users VALUES(1,10,'start')")
	run("INSERT INTO adjustments VALUES(1,1,5)")
	for _, query := range []string{
		"UPDATE users u JOIN adjustments a ON a.user_id=u.id SET a.delta=0",
		"UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.missing=1",
		"UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.score=1,u.score=2",
		"UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.score=missing+1",
		"UPDATE users u JOIN missing m ON m.id=u.id SET u.score=1",
	} {
		if _, err := e.Execute(s, query); err == nil {
			t.Errorf("accepted invalid UPDATE JOIN: %s", query)
		}
	}
	if got := fmt.Sprint(run("SELECT id,score,label FROM users").Rows); got != "[[1 10 start]]" {
		t.Fatalf("invalid UPDATE JOIN mutated rows: %s", got)
	}
}

func TestMVCCUpdateJoinUsesStatementSnapshot(t *testing.T) {
	e, _, run := rangeTestEngine(t)
	run("CREATE TABLE users(id INT PRIMARY KEY,marker INT)")
	run("CREATE TABLE adjustments(id INT PRIMARY KEY,user_id INT,delta INT,KEY user_idx(user_id))")
	run("INSERT INTO users VALUES(1,0),(2,0)")
	run("INSERT INTO adjustments VALUES(1,1,5)")
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
	// Fix the read snapshot before another session commits new source rows.
	runA("SELECT COUNT(*) FROM users")
	run("INSERT INTO adjustments VALUES(2,2,7)")
	if got := fmt.Sprint(runA("UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.marker=u.marker+a.delta").AffectedRows); got != "1" {
		t.Fatalf("statement saw rows outside its snapshot: affected=%s", got)
	}
	if got := fmt.Sprint(runA("SELECT id,marker FROM users ORDER BY id").Rows); got != "[[1 5] [2 0]]" {
		t.Fatalf("transaction snapshot rows = %s", got)
	}
	// Read-your-own-write: the statement must observe earlier writes in its own
	// transaction instead of opening an independent transaction.
	runA("UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.marker=u.marker+a.delta")
	if got := fmt.Sprint(runA("SELECT id,marker FROM users ORDER BY id").Rows); got != "[[1 10] [2 0]]" {
		t.Fatalf("read-your-own-write rows = %s", got)
	}
	if got := fmt.Sprint(run("SELECT id,marker FROM users ORDER BY id").Rows); got != "[[1 0] [2 0]]" {
		t.Fatalf("uncommitted UPDATE JOIN leaked to another session: %s", got)
	}
	if _, err := e.Execute(a, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	committed := run("UPDATE users u JOIN adjustments a ON a.user_id=u.id SET u.marker=u.marker+a.delta")
	if committed.AffectedRows != 2 {
		t.Fatalf("post-commit affected rows=%d", committed.AffectedRows)
	}
	if got := fmt.Sprint(run("SELECT id,marker FROM users ORDER BY id").Rows); got != "[[1 15] [2 7]]" {
		t.Fatalf("post-commit rows = %s", got)
	}
}
