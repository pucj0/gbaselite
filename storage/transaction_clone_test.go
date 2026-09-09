package storage

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestTransactionCloneAllMutableCollections(t *testing.T) {
	s := NewStore()
	db, err := s.CreateDatabase("db")
	if err != nil {
		t.Fatal(err)
	}
	table, err := db.CreateTable("items", []Column{{Name: "id", Type: TypeInt, AutoIncrement: true}, {Name: "value", Type: TypeInt}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{table.AddPrimaryKey([]string{"id"}), table.AddIndex("values", []string{"value"}, true), table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeInt, 5))), db.CreateView("v", "SELECT id FROM items", []string{"id"}, false)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	table.SetNamedConstraints([]ForeignKey{{Name: "fk", Columns: []string{"value"}, RefTable: "items", RefColumns: []string{"id"}}}, []CheckConstraint{{Name: "positive", Expression: "value>0"}})
	table.SetComment("original")
	clone, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Snapshot(), clone.Snapshot()) {
		t.Fatal("clone lost persisted metadata")
	}
	db2, _ := clone.Database("db")
	copied, _ := db2.Table("items")
	if !reflect.DeepEqual(table.indexRows, copied.indexRows) || !reflect.DeepEqual(table.uniqueRows, copied.uniqueRows) {
		t.Fatal("clone lost indexes")
	}
	before := s.Snapshot()
	// Deliberately mutate each private collection to catch aliasing, including
	// fields that SQL mutation paths usually replace instead of editing in place.
	copied.columns[0].Name = "changed"
	copied.columnIndex["id"] = 99
	def := copied.indexes["values"]
	def.Columns[0] = "changed"
	copied.indexes["values"] = def
	copied.foreignKeys[0].Columns[0] = "changed"
	copied.foreignKeys[0].RefColumns[0] = "changed"
	copied.checks[0].Expression = "false"
	copied.autoNext["id"] = 999
	copied.indexRows["values"][0] = 99
	for key := range copied.uniqueRows["values"] {
		copied.uniqueRows["values"][key] = 99
	}
	copied.rows[0] = NewRow(MustValue(TypeInt, 99), MustValue(TypeInt, 99))
	view := db2.views["v"]
	view.Columns[0] = "changed"
	if !reflect.DeepEqual(before, s.Snapshot()) {
		t.Fatal("private clone collections alias source")
	}
	if table.indexRows["values"][0] != 0 {
		t.Fatal("position array aliased")
	}
	for _, pos := range table.uniqueRows["values"] {
		if pos != 0 {
			t.Fatal("unique map aliased")
		}
	}
	// Exercise successful update/delete and schema mutation in both directions.
	clone, err = s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	db2, _ = clone.Database("db")
	copied, _ = db2.Table("items")
	if _, err := copied.Update(nil, map[string]Value{"value": MustValue(TypeInt, 7)}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, s.Snapshot()) {
		t.Fatal("clone update changed source payload")
	}
	cloneBefore := clone.Snapshot()
	if count := table.Delete(nil); count != 1 {
		t.Fatal(count)
	}
	if err := table.RenameIndex("values", "renamed"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cloneBefore, clone.Snapshot()) {
		t.Fatal("source mutation changed clone")
	}
}
func TestTransactionCloneConcurrentTableAccess(t *testing.T) {
	s := NewStore()
	db, _ := s.CreateDatabase("db")
	table, _ := db.CreateTable("items", []Column{{Name: "id", Type: TypeInt}})
	if err := table.AddPrimaryKey([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := table.Insert(NewRow(MustValue(TypeInt, i))); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			copied, err := s.Clone()
			if err != nil {
				errCh <- err
				return
			}
			db2, _ := copied.Database("db")
			t2, _ := db2.Table("items")
			if len(t2.indexRows["primary"]) != t2.Count(nil) {
				errCh <- fmt.Errorf("incoherent copied index")
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
