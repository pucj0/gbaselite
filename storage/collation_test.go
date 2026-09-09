package storage

import (
	"errors"
	"testing"
)

func TestExplicitCollationUniqueAndIndex(t *testing.T) {
	table, err := NewTable("items", []Column{{Name: "name", Type: TypeVarchar, Length: 20, Collation: "utf8mb4_general_ci"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.AddIndex("u", []string{"name"}, true); err != nil {
		t.Fatal(err)
	}
	value := func(s string) Row { v, _ := NewValue(TypeVarchar, s); return Row{v} }
	if err := table.Insert(value("Alice")); err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(value("ALICE")); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("case duplicate: %v", err)
	}
	key, _ := NewValue(TypeVarchar, "alice")
	if _, found, _ := table.LookupUnique("name", key); !found {
		t.Fatal("unique lookup differs from comparison")
	}
	rows, err := table.ScanIndex(IndexScan{Name: "u", EqualPrefix: []Value{key}}, nil, 0, -1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("range lookup: %v %v", rows, err)
	}
}
func TestCollationAlterRejectsExistingDuplicates(t *testing.T) {
	table, _ := NewTable("items", []Column{{Name: "name", Type: TypeVarchar, Length: 20}})
	table.AddIndex("u", []string{"name"}, true)
	for _, s := range []string{"a", "A"} {
		v, _ := NewValue(TypeVarchar, s)
		if err := table.Insert(Row{v}); err != nil {
			t.Fatal(err)
		}
	}
	if err := table.AlterColumn("name", Column{Name: "name", Type: TypeVarchar, Length: 20, Collation: "utf8mb4_general_ci"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("unsafe collation conversion: %v", err)
	}
	if table.ColumnsView()[0].Collation != "" || table.RowCount() != 2 {
		t.Fatal("failed migration changed table")
	}
}
