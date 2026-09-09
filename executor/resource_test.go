package executor

import (
	"gbaselite/parser"
	"gbaselite/storage"
	"testing"
)

func TestIndexOrderSkipsEqualityConstrainedColumns(t *testing.T) {
	index := storage.Index{Columns: []string{"tenant", "score", "id"}}
	for _, test := range []struct {
		order               []parser.Order
		ordered, descending bool
	}{
		{[]parser.Order{{Column: "tenant"}}, true, false},
		{[]parser.Order{{Column: "tenant", Desc: true}, {Column: "score", Desc: true}}, true, true},
		{[]parser.Order{{Column: "score", Desc: true}, {Column: "tenant"}, {Column: "id", Desc: true}}, true, true},
		{[]parser.Order{{Column: "score"}, {Column: "id", Desc: true}}, false, false},
		{[]parser.Order{{Column: "id"}}, false, false},
	} {
		ordered, descending := indexSatisfiesOrder(index, 1, test.order)
		if ordered != test.ordered || descending != test.descending {
			t.Fatalf("order %+v: got %v/%v", test.order, ordered, descending)
		}
	}
	engine, err := Open(t.TempDir(), "root", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session := &Session{}
	for _, q := range []string{"CREATE DATABASE ordered", "USE ordered", "CREATE TABLE items(tenant INT,score INT,id INT,KEY sorted(tenant,score,id))", "INSERT INTO items VALUES(1,7,1),(1,9,2),(2,100,3),(1,8,4)"} {
		if _, err := engine.Execute(session, q); err != nil {
			t.Fatal(err)
		}
	}
	result, err := engine.Execute(session, "SELECT id FROM items WHERE tenant=1 ORDER BY tenant,score DESC LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 || result.Rows[0][0] != int64(2) || result.Rows[1][0] != int64(4) {
		t.Fatalf("ordered result %+v", result.Rows)
	}
}
