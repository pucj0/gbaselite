package executor

import (
	"reflect"
	"testing"
)

func TestMVCCBoundAggregateColumnsMatchExpressions(t *testing.T) {
	_, _, run := rangeTestEngine(t)
	run("CREATE TABLE bound(id BIGINT PRIMARY KEY,v BIGINT,d DECIMAL(20,2),note VARCHAR(20))")
	run("INSERT INTO bound VALUES(1,9223372036854775806,1.25,'z'),(2,1,2.75,'a'),(3,NULL,NULL,NULL)")
	pairs := [][2]string{
		{"SELECT COUNT(v),SUM(v),AVG(v),MIN(v),MAX(v) FROM bound", "SELECT COUNT(COALESCE(v,NULL)),SUM(COALESCE(v,NULL)),AVG(COALESCE(v,NULL)),MIN(COALESCE(v,NULL)),MAX(COALESCE(v,NULL)) FROM bound"},
		{"SELECT COUNT(d),SUM(d),AVG(d),MIN(d),MAX(d) FROM bound", "SELECT COUNT(COALESCE(d,NULL)),SUM(COALESCE(d,NULL)),AVG(COALESCE(d,NULL)),MIN(COALESCE(d,NULL)),MAX(COALESCE(d,NULL)) FROM bound"},
		{"SELECT MIN(note),MAX(note),COUNT(note) FROM bound", "SELECT MIN(COALESCE(note,NULL)),MAX(COALESCE(note,NULL)),COUNT(COALESCE(note,NULL)) FROM bound"},
		{"SELECT SUM(b.v),COUNT(b.d) FROM bound AS b WHERE b.id=3", "SELECT SUM(COALESCE(v,NULL)),COUNT(COALESCE(d,NULL)) FROM bound WHERE id=3"},
		{"SELECT SUM(v),COUNT(v) FROM bound WHERE id>10", "SELECT SUM(COALESCE(v,NULL)),COUNT(COALESCE(v,NULL)) FROM bound WHERE id>10"},
	}
	for _, p := range pairs {
		a, b := run(p[0]), run(p[1])
		if !reflect.DeepEqual(a.Rows, b.Rows) {
			t.Fatalf("%s: %#v != %#v", p[0], a.Rows, b.Rows)
		}
	}
	run("BEGIN")
	run("UPDATE bound SET v=4 WHERE id=2")
	run("DELETE FROM bound WHERE id=1")
	a, b := run("SELECT SUM(v),COUNT(v) FROM bound"), run("SELECT SUM(COALESCE(v,NULL)),COUNT(COALESCE(v,NULL)) FROM bound")
	if !reflect.DeepEqual(a.Rows, b.Rows) {
		t.Fatal(a.Rows, b.Rows)
	}
	run("ROLLBACK")
}
