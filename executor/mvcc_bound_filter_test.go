package executor

import (
	"gbaselite/parser"
	"gbaselite/storage"
	"reflect"
	"testing"
)

func TestBoundMVCCFilterMatchesEvaluator(t *testing.T) {
	schema, err := storage.NewTransientTable("t", []storage.Column{{Name: "v", Type: storage.TypeBigInt}, {Name: "s", Type: storage.TypeVarchar, Length: 30}})
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	session.InitializeSettings()
	for _, sql := range []string{"v=1", "v>0 AND v<5", "v=NULL", "v<=>NULL", "v=9007199254740992", "s='A'", "v=1 OR v=2", "missing=1"} {
		expr, err := parser.ParseExpression(sql)
		if err != nil {
			t.Fatal(err)
		}
		bound := bindMVCCFilter(expr, schema, session)
		for _, row := range []storage.Row{{{Int64: 1, Type: storage.TypeBigInt}, {Text: "a", Type: storage.TypeVarchar}}, {{Null: true, Type: storage.TypeBigInt}, {Null: true, Type: storage.TypeVarchar}}} {
			a, ae := bound(row)
			b, be := evaluateExprWithContext(expr, schema, row, session, nil)
			if !reflect.DeepEqual(a, b) || (ae == nil) != (be == nil) {
				t.Fatalf("%s: %v/%v != %v/%v", sql, a, ae, b, be)
			}
		}
	}
}
