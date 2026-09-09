package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func assertJSONEqual(t *testing.T, actual any, expected string) {
	t.Helper()
	if actual == nil {
		t.Fatalf("SQL NULL, expected %s", expected)
	}
	parse := func(s string) any {
		t.Helper()
		decoder := json.NewDecoder(strings.NewReader(s))
		decoder.UseNumber()
		var v any
		if err := decoder.Decode(&v); err != nil {
			t.Fatalf("invalid JSON %q: %v", s, err)
		}
		return v
	}
	if !reflect.DeepEqual(parse(fmt.Sprint(actual)), parse(expected)) {
		t.Fatalf("got %s, want %s", actual, expected)
	}
}

func TestLegacyJSONSQLFunctions(t *testing.T) {
	_, _, run := savepointEngine(t)
	for _, tc := range []struct{ sql, want string }{
		{`JSON_OBJECT()`, `{}`}, {`JSON_ARRAY()`, `[]`},
		{`JSON_OBJECT('a',1,'b',NULL,'c',TRUE,'a',2)`, `{"a":2,"b":null,"c":true}`},
		{`JSON_OBJECT(1,'x','中文',JSON_ARRAY(1,JSON_OBJECT('x',FALSE)),'s','{"a":1}')`, `{"1":"x","中文":[1,{"x":false}],"s":"{\"a\":1}"}`},
		{`JSON_ARRAY(JSON_QUOTE('a'),JSON_OBJECT('n',9223372036854775807))`, `["\"a\"",{"n":9223372036854775807}]`},
		{`JSON_EXTRACT('{"big":18446744073709551615}', '$.big')`, `18446744073709551615`},
		{`JSON_EXTRACT('{"a":null}', '$.a')`, `null`},
		{`JSON_EXTRACT('{"a":[1,2]}', '$.a[1]','$.missing')`, `[2]`},
		{`JSON_EXTRACT('{"a":[{"v":1},{"v":2}]}', '$.a[*].v')`, `[1,2]`},
		{`JSON_EXTRACT('{"a":1}', '$.*')`, `[1]`},
		{`JSON_EXTRACT('{"中文":1,"a.b":[2]}', '$.中文', '$."a.b"[0]')`, `[1,2]`},
		{`JSON_EXTRACT('1','$[0]')`, `1`},
		{`JSON_SET('{"a":1,"b":[2,3]}','$.a',10,'$.c','[true,false]')`, `{"a":10,"b":[2,3],"c":"[true,false]"}`},
		{`JSON_INSERT('{"a":1}','$.a',10,'$.b',JSON_ARRAY(1))`, `{"a":1,"b":[1]}`},
		{`JSON_REPLACE('{"a":1}','$.a',NULL,'$.b',2)`, `{"a":null}`},
		{`JSON_REMOVE('[0,1,2,3]','$[0]','$[1]')`, `[1,3]`},
		{`JSON_REMOVE('{"a":1,"b":2}','$.a','$.missing')`, `{"b":2}`},
		{`JSON_SET('{}','$.missing.a',2)`, `{}`},
		{`JSON_SET('{}','$.a',JSON_OBJECT(),'$.a.b',3)`, `{"a":{"b":3}}`},
		{`JSON_SET('[1]','$[18446744073709551615]',2)`, `[1,2]`},
		{`JSON_SET('1','$[4]',2)`, `[1,2]`},
		{`JSON_SET('1','$[0]',2)`, `2`},
		{`JSON_INSERT('1','$[0]',2)`, `1`},
		{`JSON_REPLACE('1','$',JSON_ARRAY(2))`, `[2]`},
		{`JSON_KEYS('{"b":1,"a":2}')`, `["a","b"]`},
	} {
		t.Run(tc.sql, func(t *testing.T) { assertJSONEqual(t, run("SELECT " + tc.sql).Rows[0][0], tc.want) })
	}
	for _, tc := range []struct {
		sql  string
		want any
	}{
		{`JSON_UNQUOTE(JSON_EXTRACT('{"a":"hello"}','$.a'))`, "hello"},
		{`JSON_UNQUOTE('plain')`, "plain"}, {`JSON_UNQUOTE('[1, 2]')`, "[1, 2]"},
		{`JSON_TYPE('null')`, "NULL"}, {`JSON_TYPE('true')`, "BOOLEAN"},
		{`JSON_TYPE('1.5')`, "DOUBLE"}, {`JSON_TYPE('1e2')`, "DOUBLE"},
		{`JSON_TYPE('18446744073709551615')`, "UNSIGNED INTEGER"},
		{`JSON_TYPE(JSON_OBJECT())`, "OBJECT"}, {`JSON_TYPE(JSON_ARRAY())`, "ARRAY"},
		{`JSON_LENGTH('{"a":[1,2]}','$.a')`, int64(2)}, {`JSON_LENGTH('null')`, int64(1)},
		{`JSON_DEPTH('[10,{"a":20}]')`, int64(3)}, {`JSON_DEPTH('[]')`, int64(1)},
		{`JSON_VALID('{')`, int64(0)}, {`JSON_VALID('{}')`, int64(1)},
		{`JSON_CONTAINS_PATH('{"a":null}','all','$.a','$.b')`, int64(0)},
		{`JSON_CONTAINS_PATH('{"a":null}','one','$.a','$.b')`, int64(1)},
		{`JSON_CONTAINS_PATH('{"a":[1]}','all','$.a[*]')`, int64(1)},
		{`JSON_EXTRACT('{"a":1}','$.missing')`, nil}, {`JSON_EXTRACT('{}','$.a','$.b')`, nil},
		{`JSON_LENGTH('{}','$.missing')`, nil}, {`JSON_KEYS('1')`, nil},
		{`JSON_SET('{}',NULL,1)`, nil}, {`JSON_REMOVE(NULL,'$.a')`, nil},
		{`JSON_QUOTE(NULL)`, nil}, {`JSON_UNQUOTE(NULL)`, nil}, {`JSON_VALID(NULL)`, nil},
		{`JSON_EXTRACT('10','$') + 2`, float64(12)},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got := run("SELECT " + tc.sql).Rows[0][0]
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestLegacyJSONFunctionErrorsAndLimits(t *testing.T) {
	e, s, _ := savepointEngine(t)
	for _, tc := range []struct {
		sql  string
		code uint16
	}{
		{`JSON_OBJECT('a')`, 1582}, {`JSON_OBJECT(NULL,1)`, 3158}, {`JSON_EXTRACT('{}')`, 1582},
		{`JSON_SET('{}','$.a')`, 1582}, {`JSON_VALID()`, 1582},
		{`JSON_EXTRACT('{','$')`, 3141}, {`JSON_TYPE('true false')`, 3141},
		{`JSON_EXTRACT('{}','notpath')`, 3143}, {`JSON_EXTRACT('{}','$.')`, 3143},
		{`JSON_EXTRACT('{}','$**.a')`, 3143}, {`JSON_EXTRACT('[]','$[-1]')`, 3143},
		{`JSON_EXTRACT('[]','$[last]')`, 3143}, {`JSON_EXTRACT('[]','$[0 to 1]')`, 3143},
		{`JSON_SET('{}','$.*',1)`, 3149}, {`JSON_REMOVE('[]','$[*]')`, 3149},
		{`JSON_REMOVE('{}','$')`, 3153}, {`JSON_CONTAINS_PATH('{}','bad','$.a')`, 3154},
	} {
		_, err := e.Execute(s, "SELECT "+tc.sql)
		var je *JSONFunctionError
		if !errors.As(err, &je) || je.Code != tc.code {
			t.Errorf("%s: want code %d, got %v", tc.sql, tc.code, err)
		}
	}
	nested := strings.Repeat("[", 100) + "0" + strings.Repeat("]", 100)
	if _, err := evaluateJSONFunction("JSON_EXTRACT", []any{nested, "$"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]any{{"[" + nested + "]", "$"}, {nested, "$." + strings.Repeat("a", 4096)}} {
		if _, err := evaluateJSONFunction("JSON_EXTRACT", args); err == nil {
			t.Fatal("missing depth/path bound")
		}
	}
	if _, err := evaluateJSONFunction("JSON_ARRAY", []any{jsonDocument(nested)}); err == nil {
		t.Fatal("constructor exceeded nesting bound")
	}
	if _, err := evaluateJSONFunction("JSON_OBJECT", []any{string([]byte{255}), 1}); err == nil {
		t.Fatal("accepted invalid UTF-8 key")
	}
}

func TestLegacyJSONColumnsAtomicityAndMaterialization(t *testing.T) {
	e, s, run := savepointEngine(t)
	run(`CREATE TABLE documents(id INT PRIMARY KEY, body JSON, plain TEXT)`)
	run(`INSERT INTO documents VALUES(1,JSON_OBJECT('a',1),'[1]'),(2,JSON_OBJECT('a',2),'bad')`)
	assertJSONEqual(t, run(`SELECT JSON_OBJECT('j',body,'t',plain) FROM documents WHERE id=1`).Rows[0][0], `{"j":{"a":1},"t":"[1]"}`)
	assertJSONEqual(t, run(`SELECT JSON_ARRAY(d.body) FROM (SELECT body FROM documents WHERE id=1) d`).Rows[0][0], `[{"a":1}]`)
	assertJSONEqual(t, run(`SELECT JSON_ARRAY(d.j) FROM (SELECT JSON_OBJECT('a',1) AS j) d`).Rows[0][0], `[{"a":1}]`)
	run(`INSERT INTO items(value) VALUES(1)`)
	assertJSONEqual(t, run(`SELECT JSON_ARRAY((SELECT body FROM documents WHERE id=1)) FROM items`).Rows[0][0], `[{"a":1}]`)
	run(`CREATE VIEW json_view AS SELECT body FROM documents WHERE id=1`)
	assertJSONEqual(t, run(`SELECT JSON_OBJECT('j',body) FROM json_view`).Rows[0][0], `{"j":{"a":1}}`)
	run(`BEGIN`)
	run(`SAVEPOINT before_json`)
	run(`UPDATE documents SET body=JSON_SET(body,'$.nested',JSON_ARRAY(1,2)) WHERE id=1`)
	run(`ROLLBACK TO before_json`)
	run(`COMMIT`)
	assertJSONEqual(t, run(`SELECT body FROM documents WHERE id=1`).Rows[0][0], `{"a":1}`)
	if _, err := e.Execute(s, `UPDATE documents SET body=JSON_EXTRACT(plain,'$')`); err == nil {
		t.Fatal("expected invalid document failure")
	}
	assertJSONEqual(t, run(`SELECT body FROM documents WHERE id=1`).Rows[0][0], `{"a":1}`)
	if got := run(`SELECT id FROM documents WHERE JSON_EXTRACT(body,'$.a') > 1`).Rows; len(got) != 1 || got[0][0] != int64(2) {
		t.Fatal(got)
	}
}

func TestLegacyJSONPersistence(t *testing.T) {
	dir := t.TempDir()
	e, err := openLegacy(dir, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{}
	for _, q := range []string{`CREATE DATABASE jd`, `USE jd`, `CREATE TABLE docs(id INT,body JSON)`, `INSERT INTO docs VALUES(1,JSON_OBJECT('n',9223372036854775807))`, `UPDATE docs SET body=JSON_SET(body,'$.v',JSON_ARRAY(NULL,TRUE))`} {
		if _, err := e.Execute(s, q); err != nil {
			e.Close()
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = openLegacy(dir, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	result, err := e.Execute(&Session{CurrentDatabase: "jd"}, `SELECT JSON_ARRAY(body) FROM docs`)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, result.Rows[0][0], `[{"n":9223372036854775807,"v":[null,true]}]`)
}

func BenchmarkLegacyJSONFunctions(b *testing.B) {
	for _, tc := range []struct {
		name string
		args []any
	}{
		{"JSON_OBJECT", []any{"id", int64(1), "name", "hello", "active", true}},
		{"JSON_EXTRACT", []any{`{"a":[1,2,3],"n":9223372036854775807}`, "$.n"}},
		{"JSON_SET", []any{`{"a":[1,2,3],"n":9223372036854775807}`, "$.a[1]", int64(4)}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := evaluateJSONFunction(tc.name, tc.args); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestLegacyJSONCorrelatedSubqueryKeepsEachRow(t *testing.T) {
	_, _, run := savepointEngine(t)
	run(`INSERT INTO items(value) VALUES(10),(20)`)
	rows := run(`SELECT JSON_OBJECT('value',(SELECT b.value FROM items b WHERE b.id=a.id)) FROM items a ORDER BY a.id`).Rows
	assertJSONEqual(t, rows[0][0], `{"value":10}`)
	assertJSONEqual(t, rows[1][0], `{"value":20}`)
}

func TestLegacyJSONInsertFormsAndAtomicity(t *testing.T) {
	e, s, run := savepointEngine(t)
	run(`CREATE TABLE forms(id INT PRIMARY KEY,body JSON)`)
	run(`INSERT INTO forms SET id=1,body=JSON_OBJECT('a',1)`)
	run(`INSERT INTO forms SELECT 2,JSON_OBJECT('a',2)`)
	run(`REPLACE INTO forms VALUES(2,JSON_ARRAY(3))`)
	run(`INSERT INTO forms VALUES(2,JSON_OBJECT('a',4)) ON DUPLICATE KEY UPDATE body=JSON_OBJECT('new',VALUES(body),'old',body)`)
	assertJSONEqual(t, run(`SELECT body FROM forms WHERE id=2`).Rows[0][0], `{"new":{"a":4},"old":[3]}`)
	if _, err := e.Execute(s, `INSERT INTO forms VALUES(3,JSON_ARRAY(1)),(4,JSON_OBJECT(NULL,1))`); err == nil {
		t.Fatal("expected invalid key")
	}
	if got := run(`SELECT COUNT(*) FROM forms`).Rows[0][0]; got != int64(2) {
		t.Fatalf("partially inserted: %v", got)
	}
	assertJSONEqual(t, run(`WITH j AS (SELECT body FROM forms WHERE id=1) SELECT JSON_ARRAY(body) FROM j`).Rows[0][0], `[{"a":1}]`)
}

func TestLegacyJSONInsertSubqueryAuthorization(t *testing.T) {
	e, _, run := savepointEngine(t)
	run(`CREATE TABLE target(id INT, body JSON)`)
	run(`INSERT INTO items(value) VALUES(10)`)
	run(`CREATE USER 'json_writer'@'%' IDENTIFIED BY 'secret'`)
	run(`GRANT INSERT ON sp.target TO 'json_writer'@'%'`)
	writer := &Session{Username: "json_writer", Host: "%", CurrentDatabase: "sp"}
	q := `INSERT INTO target VALUES(1,JSON_OBJECT('secret',(SELECT value FROM items)))`
	if _, err := e.Execute(writer, q); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("missing read authorization: %v", err)
	}
	run(`GRANT SELECT ON sp.items TO 'json_writer'@'%'`)
	if _, err := e.Execute(writer, q); err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, run(`SELECT body FROM target`).Rows[0][0], `{"secret":10}`)
}

func TestLegacyJSONInsertSelectRollback(t *testing.T) {
	e, s, run := savepointEngine(t)
	run(`CREATE TABLE source_json(id INT, raw TEXT)`)
	run(`CREATE TABLE target_json(id INT PRIMARY KEY, body JSON)`)
	run(`INSERT INTO source_json VALUES(1,'{}'),(2,'bad')`)
	run(`BEGIN`)
	if _, err := e.Execute(s, `INSERT INTO target_json SELECT id,JSON_EXTRACT(raw,'$') FROM source_json`); err == nil {
		t.Fatal("expected invalid JSON")
	}
	if got := run(`SELECT COUNT(*) FROM target_json`).Rows[0][0]; got != int64(0) {
		t.Fatal("partial insert select", got)
	}
	if _, err := e.Execute(s, `INSERT INTO target_json VALUES(1,JSON_OBJECT()),(1,JSON_ARRAY())`); err == nil {
		t.Fatal("expected duplicate key")
	}
	if got := run(`SELECT COUNT(*) FROM target_json`).Rows[0][0]; got != int64(0) {
		t.Fatal("partial expression insert", got)
	}
	run(`COMMIT`)
	run(`CREATE TABLE json_copy AS SELECT JSON_OBJECT('a',1) AS body`)
	assertJSONEqual(t, run(`SELECT JSON_ARRAY(body) FROM json_copy`).Rows[0][0], `[{"a":1}]`)
}
