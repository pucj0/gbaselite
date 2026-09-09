package parser

import (
	"reflect"
	"strings"
	"testing"
)

func TestSavepointSyntax(t *testing.T) {
	for q, want := range map[string]Statement{
		"SAVEPOINT a": Savepoint{"a"}, "SAVEPOINT `保存 点`": Savepoint{"保存 点"},
		"ROLLBACK TO a": RollbackTo{"a"}, "ROLLBACK WORK TO SAVEPOINT a;": RollbackTo{"a"},
		"RELEASE SAVEPOINT a": ReleaseSavepoint{"a"}, "ROLLBACK WORK": Rollback{},
	} {
		got, err := Parse(q)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %#v %v", q, got, err)
		}
	}
	for _, q := range []string{"SAVEPOINT", "SAVEPOINT ''", "SAVEPOINT ``", "SAVEPOINT a.b", "SAVEPOINT a extra", "RELEASE a", "ROLLBACK TO", "ROLLBACK TO SAVEPOINT", "SAVEPOINT " + strings.Repeat("x", 65)} {
		if _, err := Parse(q); err == nil {
			t.Fatalf("invalid query accepted: %s", q)
		}
	}
}
