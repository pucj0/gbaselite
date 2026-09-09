package parser

import "testing"

func TestInsertJSONValueExpressions(t *testing.T) {
	statement, err := Parse(`INSERT INTO docs VALUES(1,JSON_OBJECT('a',JSON_ARRAY(2,NULL))),(2,'{}'),(3,JSON_SET('{}','$.n',1+2)) ON DUPLICATE KEY UPDATE body=VALUES(body)`)
	if err != nil {
		t.Fatal(err)
	}
	insert := statement.(Insert)
	if len(insert.Values) != 3 || len(insert.ValueExpressions) != 2 || len(insert.OnDuplicate) != 1 {
		t.Fatalf("bad insert AST: %#v", insert)
	}
	for _, key := range [][2]int{{0, 1}, {2, 1}} {
		if _, ok := insert.ValueExpressions[key].(FunctionExpr); !ok {
			t.Fatalf("missing function at %v", key)
		}
	}
	if insert.Values[1][1].Kind != LiteralString {
		t.Fatal("ordinary literal changed")
	}
	statement, err = Parse(`INSERT INTO docs VALUES(-1,'{}'),(2,NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	if statement.(Insert).ValueExpressions != nil {
		t.Fatal("literal insert allocated expression map")
	}
	for _, q := range []string{`INSERT INTO docs VALUES(1,JSON_OBJECT('a',))`, `INSERT INTO docs VALUES(1,JSON_ARRAY(1)`} {
		if _, err := Parse(q); err == nil {
			t.Fatalf("invalid syntax accepted: %s", q)
		}
	}
}
