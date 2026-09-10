package executor

import (
	"context"
	"gbaselite/parser"
	"reflect"
	"testing"
)

func TestMVCCUniqueLookupAndOverlay(t *testing.T) {
	e, session, run := rangeTestEngine(t)
	run("CREATE TABLE u(id INT PRIMARY KEY,a BIGINT,b INT,UNIQUE KEY ab(a,b))")
	run("INSERT INTO u VALUES(1,10,20),(2,10,21),(3,NULL,20)")
	compare := func(where string) {
		t.Helper()
		a := run("SELECT id FROM u WHERE " + where + " ORDER BY id")
		b := run("SELECT id FROM u WHERE (" + where + ") OR 0=1 ORDER BY id")
		if !reflect.DeepEqual(a.Rows, b.Rows) {
			t.Fatal(where, a.Rows, b.Rows)
		}
	}
	for _, w := range []string{"a=10 AND b=20", "20=b AND 10=a", "a=10 AND b=99", "a=10", "a=NULL AND b=20", "a=9007199254740992 AND b=20", "a=10 AND b=20 AND id=2"} {
		compare(w)
	}
	tx, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	table, schema, _, err := loadVersionedTable(tx, session, "u")
	if err != nil {
		t.Fatal(err)
	}
	expr, _ := parser.ParseExpression("a=10 AND b=20")
	if _, _, ok := sqlUniqueAccess(expr, table, schema); !ok {
		t.Fatal("unique path not selected")
	}
	run("BEGIN")
	run("UPDATE u SET b=22 WHERE a=10 AND b=20")
	compare("a=10 AND b=20")
	compare("a=10 AND b=22")
	run("DELETE FROM u WHERE a=10 AND b=21")
	compare("a=10 AND b=21")
	run("ROLLBACK")
	compare("a=10 AND b=20")
}
