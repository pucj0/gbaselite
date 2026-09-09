package server

import (
	"gbaselite/executor"
	"gbaselite/storage"
	"testing"
)

func TestDecimalPreparedBindingIsExactAndSafe(t *testing.T) {
	engine, err := openTestEngine(t, t.TempDir(), "root", "password")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	for _, test := range []struct{ left, right, want string }{{"9007199254740993.01", "0.01", "9007199254740993.02"}, {"2", "3", "5"}} {
		bound, err := bindPreparedQuery("SELECT ? + ?", []any{storage.Decimal(test.left), storage.Decimal(test.right)})
		if err != nil {
			t.Fatal(err)
		}
		result, err := engine.Execute(&executor.Session{}, bound)
		if err != nil {
			t.Fatal(err)
		}
		if result.Rows[0][0] != storage.Decimal(test.want) {
			t.Fatalf("bound %s result %#v", bound, result.Rows)
		}
	}
	if got := sqlLiteral(storage.Decimal("0); DROP TABLE t;--")); got != "'0); DROP TABLE t;--'" {
		t.Fatalf("invalid decimal escaped incorrectly %s", got)
	}
}
