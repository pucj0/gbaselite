package storage

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
)

func TestIncrementalIndexMatchesFullRebuild(t *testing.T) {
	table, err := NewTable("items", []Column{{Name: "id", Type: TypeInt}, {Name: "group_id", Type: TypeInt}, {Name: "name", Type: TypeVarchar, Length: 20}})
	if err != nil {
		t.Fatal(err)
	}
	if err = table.AddPrimaryKey([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	if err = table.AddIndex("groups", []string{"group_id", "name"}, false); err != nil {
		t.Fatal(err)
	}
	if err = table.AddIndex("names", []string{"name"}, true); err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(91))
	for i := 0; i < 300; i++ {
		name := NullValue(TypeVarchar)
		if i%3 != 0 {
			name = MustValue(TypeVarchar, string(rune(0x400+i)))
		}
		row := NewRow(MustValue(TypeInt, i), MustValue(TypeInt, random.Intn(17)), name)
		if err = table.Insert(row); err != nil {
			t.Fatal(err)
		}
		indexes := make(map[string][]int)
		for key, positions := range table.indexRows {
			indexes[key] = append([]int(nil), positions...)
		}
		unique := make(map[string]map[string]int)
		for key, entries := range table.uniqueRows {
			unique[key] = make(map[string]int)
			for value, position := range entries {
				unique[key][value] = position
			}
		}
		if err = table.rebuildIndexesLocked(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(indexes, table.indexRows) || !reflect.DeepEqual(unique, table.uniqueRows) {
			t.Fatalf("indexes diverged after row %d", i)
		}
		if err = table.Insert(row); !errors.Is(err, ErrDuplicateKey) {
			t.Fatalf("duplicate insert: %v", err)
		}
	}
}

func TestCloneIndexAndSchemaIsolation(t *testing.T) {
	s := NewStore()
	db, _ := s.CreateDatabase("isolation")
	table, _ := db.CreateTable("items", []Column{{Name: "id", Type: TypeInt}, {Name: "name", Type: TypeVarchar, Length: 20}})
	_ = table.AddPrimaryKey([]string{"id"})
	_ = table.AddIndex("names", []string{"name"}, true)
	_ = table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "one")))
	before := s.Snapshot()
	clone, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	db2, _ := clone.Database("isolation")
	table2, _ := db2.Table("items")
	if err = table2.Insert(NewRow(MustValue(TypeInt, 2), MustValue(TypeVarchar, "two"))); err != nil {
		t.Fatal(err)
	}
	if err = table2.RenameIndex("names", "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err = table2.Update(nil, map[string]Value{"name": MustValue(TypeVarchar, "same")}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("expected duplicate update: %v", err)
	}
	if !reflect.DeepEqual(before, s.Snapshot()) {
		t.Fatal("clone mutated source")
	}
	_ = table.Insert(NewRow(MustValue(TypeInt, 3), MustValue(TypeVarchar, "three")))
	if table2.Count(nil) != 2 {
		t.Fatal("source insert leaked into clone")
	}
}

func TestIndexCountMatchesScan(t *testing.T) {
	table, err := NewTable("counts", []Column{{Name: "id", Type: TypeInt}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err = table.Insert(NewRow(MustValue(TypeInt, i))); err != nil {
			t.Fatal(err)
		}
	}
	if err = table.AddPrimaryKey([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	for lower := -1; lower < 55; lower += 4 {
		for upper := -1; upper < 55; upper += 5 {
			scan := IndexScan{Name: "PRIMARY", Lower: &IndexBound{Value: MustValue(TypeInt, lower), Inclusive: true}, Upper: &IndexBound{Value: MustValue(TypeInt, upper), Inclusive: false}}
			for _, predicate := range []Predicate{nil, func(row Row) bool { return row[0].Int64%2 == 0 }} {
				rows, err := table.ScanIndex(scan, predicate, 0, -1)
				if err != nil {
					t.Fatal(err)
				}
				count, err := table.CountIndex(scan, predicate)
				if err != nil || count != len(rows) {
					t.Fatalf("bounds %d..%d: %d != %d (%v)", lower, upper, count, len(rows), err)
				}
			}
		}
	}
	if _, err = table.CountIndex(IndexScan{Name: "missing"}, nil); err == nil {
		t.Fatal("missing index accepted")
	}
}
