package executor

import (
	"reflect"
	"testing"
)

func TestMVCCMutationScratchPreservesRowsAndRollback(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE scratch(id BIGINT PRIMARY KEY,a BIGINT,b BIGINT,note VARCHAR(20),u INT UNIQUE)")
	run("INSERT INTO scratch VALUES(1,10,0,'first',11),(2,NULL,0,NULL,12),(3,-4,0,'last',13)")
	run("UPDATE scratch SET a=a+1,b=a")
	want := [][]any{{int64(1), int64(11), int64(11), "first", int64(11)}, {int64(2), nil, nil, nil, int64(12)}, {int64(3), int64(-3), int64(-3), "last", int64(13)}}
	check := func() {
		t.Helper()
		if got := run("SELECT * FROM scratch ORDER BY id"); !reflect.DeepEqual(got.Rows, want) {
			t.Fatalf("got %#v want %#v", got.Rows, want)
		}
	}
	check()
	if _, err := e.Execute(s, "UPDATE scratch SET u=99"); err == nil {
		t.Fatal("unique conflict expected")
	}
	check()
	run("BEGIN")
	run("DELETE FROM scratch WHERE a IS NULL")
	run("UPDATE scratch SET note='changed'")
	run("ROLLBACK")
	check()
	run("DELETE FROM scratch WHERE a IS NULL")
	want = [][]any{want[0], want[2]}
	check()
}
