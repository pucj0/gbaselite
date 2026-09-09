package executor

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"gbaselite/parser"
	"gbaselite/storage"
)

func indexedJoinFixture(t testing.TB, rightRows int) (*storage.Store, *Session, *storage.Table, *storage.Table) {
	t.Helper()
	columns := []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "value", Type: storage.TypeVarchar, Length: 32}}
	right, err := storage.NewTable("right_items", columns)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rightRows; i++ {
		if err = right.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, i), storage.MustValue(storage.TypeVarchar, "right-value"))); err != nil {
			t.Fatal(err)
		}
	}
	if err = right.AddPrimaryKey([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	left, err := storage.NewTransientTable("left_items", []storage.Column{{Name: "l.id", Type: storage.TypeInt}, {Name: "l.value", Type: storage.TypeVarchar, Length: 32}})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{3, 1, 1, rightRows + 1} {
		if err = left.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, i), storage.MustValue(storage.TypeVarchar, "left-value"))); err != nil {
			t.Fatal(err)
		}
	}
	if err = left.Insert(storage.NewRow(storage.NullValue(storage.TypeInt), storage.MustValue(storage.TypeVarchar, "null-key"))); err != nil {
		t.Fatal(err)
	}
	return storage.NewStore(), &Session{}, left, right
}

func TestIndexedJoinMatchesGeneralInnerAndLeft(t *testing.T) {
	for _, kind := range []string{"INNER", "LEFT"} {
		t.Run(kind, func(t *testing.T) {
			store, session, left, right := indexedJoinFixture(t, 100)
			on, err := parser.ParseExpression("l.id = r.id")
			if err != nil {
				t.Fatal(err)
			}
			join := parser.Join{Type: kind, On: on}
			fast, used, err := tryIndexedJoinRelations(store, session, left, right, "r", join)
			if err != nil {
				t.Fatal(err)
			}
			if !used {
				t.Fatal("eligible unique JOIN not used")
			}
			qualified, err := qualifyRelation(right, "r")
			if err != nil {
				t.Fatal(err)
			}
			general, err := joinRelations(store, session, left, qualified, join)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fast.Select(nil), general.Select(nil)) {
				t.Fatalf("changed rows: %v vs %v", fast.Select(nil), general.Select(nil))
			}
		})
	}
}

func TestIndexedJoinFallbackAndResourceLimits(t *testing.T) {
	store, session, left, right := indexedJoinFixture(t, 10)
	on, _ := parser.ParseExpression("l.id = r.id")
	for _, join := range []parser.Join{{Type: "RIGHT", On: on}, {Type: "INNER", On: parser.BinaryExpr{Operator: "AND", Left: on, Right: on}}} {
		if _, used, err := tryIndexedJoinRelations(store, session, left, right, "r", join); err != nil || used {
			t.Fatalf("invalid plan accepted: %v %v", used, err)
		}
	}
	session.query = newQueryControl(nil, QueryOptions{ResultMemoryBytes: 100})
	if _, used, err := tryIndexedJoinRelations(store, session, left, right, "r", parser.Join{Type: "INNER", On: on}); !used || !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("budget bypass: %v %v", used, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session.query = newQueryControl(ctx, QueryOptions{})
	if _, used, err := tryIndexedJoinRelations(store, session, left, right, "r", parser.Join{Type: "INNER", On: on}); !used || !errors.Is(err, ErrQueryCanceled) {
		t.Fatalf("cancellation bypass: %v %v", used, err)
	}
}

func TestIndexedJoinDoesNotUseIncompatibleTextIndex(t *testing.T) {
	session := &Session{}
	a := storage.Column{Name: "a", Type: storage.TypeVarchar, Length: 20}
	b := storage.Column{Name: "b", Type: storage.TypeVarchar, Length: 20}
	if indexedJoinTypesCompatible(a, b, session) {
		t.Fatal("session case-insensitive comparison cannot use binary index")
	}
	session.CollationConnection = "utf8mb4_bin"
	if !indexedJoinTypesCompatible(a, b, session) {
		t.Fatal("binary comparison should be compatible")
	}
	a.Collation = "utf8mb4_general_ci"
	if indexedJoinTypesCompatible(a, b, session) {
		t.Fatal("explicit case-insensitive left column cannot use binary index")
	}
}

func BenchmarkIndexedJoinSelective(b *testing.B) {
	store, session, left, right := indexedJoinFixture(b, 10000)
	on, _ := parser.ParseExpression("l.id = r.id")
	join := parser.Join{Type: "LEFT", On: on}
	b.Run("general", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			qualified, err := qualifyRelation(right, "r")
			if err != nil {
				b.Fatal(err)
			}
			result, err := joinRelations(store, session, left, qualified, join)
			if err != nil {
				b.Fatal(err)
			}
			if result.RowCount() != 5 {
				b.Fatal("rows")
			}
		}
	})
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			result, used, err := tryIndexedJoinRelations(store, session, left, right, "r", join)
			if err != nil || !used {
				b.Fatal(err)
			}
			if result.RowCount() != 5 {
				b.Fatal("rows")
			}
		}
	})
}
