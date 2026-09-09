package storage

import (
	"fmt"
	"testing"
)

func benchmarkIndexedStore(b *testing.B) (*Store, *Table) {
	return benchmarkIndexedStoreRows(b, 10000)
}

func benchmarkIndexedStoreRows(b *testing.B, count int) (*Store, *Table) {
	b.Helper()
	s := NewStore()
	db, err := s.CreateDatabase("bench")
	if err != nil {
		b.Fatal(err)
	}
	table, err := db.CreateTable("items", []Column{{Name: "id", Type: TypeInt}, {Name: "value", Type: TypeVarchar, Length: 32}})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if err := table.Insert(NewRow(MustValue(TypeInt, i), MustValue(TypeVarchar, "payload"))); err != nil {
			b.Fatal(err)
		}
	}
	if err := table.AddPrimaryKey([]string{"id"}); err != nil {
		b.Fatal(err)
	}
	if err := table.AddIndex("value_idx", []string{"value"}, false); err != nil {
		b.Fatal(err)
	}
	return s, table
}

func BenchmarkIndexedInsert10000(b *testing.B) {
	_, table := benchmarkIndexedStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := table.Insert(NewRow(MustValue(TypeInt, 10000+i), MustValue(TypeVarchar, "payload"))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreClone10000(b *testing.B) {
	s, _ := benchmarkIndexedStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Clone(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkForeignKeyInsert10000(b *testing.B) {
	s, _ := benchmarkIndexedStore(b)
	db, _ := s.Database("bench")
	child, err := db.CreateTable("child", []Column{{Name: "parent_id", Type: TypeInt}})
	if err != nil {
		b.Fatal(err)
	}
	if err := db.AddForeignKey(child.Name(), ForeignKey{Name: "fk", Columns: []string{"parent_id"}, RefTable: "items", RefColumns: []string{"id"}}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Insert("child", NewRow(MustValue(TypeInt, 9999))); err != nil {
			b.Fatal(fmt.Errorf("insert: %w", err))
		}
	}
}

func BenchmarkStoreClone100000(b *testing.B) {
	s, _ := benchmarkIndexedStoreRows(b, 100000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Clone(); err != nil {
			b.Fatal(err)
		}
	}
}
