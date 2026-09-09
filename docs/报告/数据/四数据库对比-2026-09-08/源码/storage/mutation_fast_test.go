package storage

import (
	"math/rand"
	"reflect"
	"testing"
)

func TestIncrementalMutationsMatchGeneralAtomicPath(t *testing.T) {
	s := NewStore()
	db, _ := s.CreateDatabase("db")
	table, err := db.CreateTable("items", []Column{{Name: "id", Type: TypeInt, AutoIncrement: true}, {Name: "u", Type: TypeInt, Nullable: true}, {Name: "v", Type: TypeInt}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{table.AddPrimaryKey([]string{"id"}), table.AddIndex("u", []string{"u"}, true), table.AddIndex("v", []string{"v", "id"}, false)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		if err := table.Insert(NewRow(MustValue(TypeInt, i+1), MustValue(TypeInt, i), MustValue(TypeInt, i%7))); err != nil {
			t.Fatal(err)
		}
	}
	reference, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	db2, _ := reference.Database("db")
	other, _ := db2.Table("items")
	random := rand.New(rand.NewSource(48923))
	for step := 0; step < 300; step++ {
		snapshot := table.Snapshot()
		if len(snapshot.Rows) < 2 {
			break
		}
		a, b := random.Intn(len(snapshot.Rows)), random.Intn(len(snapshot.Rows))
		mutation := RowMutation{Table: "items", Replacements: map[int]Row{}}
		switch step % 6 {
		case 0:
			row := cloneRow(snapshot.Rows[a])
			row[2] = MustValue(TypeInt, random.Intn(9))
			mutation.Replacements[a] = row
		case 1:
			row := cloneRow(snapshot.Rows[a])
			row[1] = snapshot.Rows[b][1]
			mutation.Replacements[a] = row
		case 2:
			left, right := cloneRow(snapshot.Rows[a]), cloneRow(snapshot.Rows[b])
			left[1], right[1] = right[1], left[1]
			mutation.Replacements[a] = left
			mutation.Replacements[b] = right
		case 3:
			row := cloneRow(snapshot.Rows[a])
			row[1] = NullValue(TypeInt)
			mutation.Replacements[a] = row
		case 4:
			mutation.Delete = []int{a}
		case 5:
			mutation.Delete = []int{a, a}
		}
		count, gotErr := db.ApplyRowMutations([]RowMutation{mutation})
		expected, wantErr := db2.applyRowMutationsGeneralLocked([]RowMutation{mutation})
		if (gotErr == nil) != (wantErr == nil) || count != expected {
			t.Fatalf("step %d: %d/%v != %d/%v", step, count, gotErr, expected, wantErr)
		}
		if gotErr != nil && !reflect.DeepEqual(snapshot, table.Snapshot()) {
			t.Fatalf("failed mutation changed state at %d", step)
		}
		left, right := table.Snapshot(), other.Snapshot()
		if !reflect.DeepEqual(left.Rows, right.Rows) || !reflect.DeepEqual(left.AutoIncrementNext, right.AutoIncrementNext) || !reflect.DeepEqual(table.uniqueRows, other.uniqueRows) || !reflect.DeepEqual(table.indexRows, other.indexRows) || table.dataLength != other.dataLength {
			t.Fatalf("state diverged at %d", step)
		}
	}
}
func TestIndependentMutationKeepsPublishedRowsImmutable(t *testing.T) {
	s := NewStore()
	db, _ := s.CreateDatabase("db")
	table, _ := db.CreateTable("items", []Column{{Name: "id", Type: TypeInt}})
	_ = table.Insert(NewRow(MustValue(TypeInt, 1)))
	_ = table.Insert(NewRow(MustValue(TypeInt, 2)))
	rows := table.rowSnapshot()
	if _, err := db.ApplyRowMutations([]RowMutation{{Table: "items", Replacements: map[int]Row{0: NewRow(MustValue(TypeInt, 9))}, Delete: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0][0].Int64 != 1 || rows[1][0].Int64 != 2 {
		t.Fatal("mutation changed a published row snapshot")
	}
}

func TestIncrementalDeleteAllAndForeignKeyRouting(t *testing.T) {
	s := NewStore()
	db, _ := s.CreateDatabase("db")
	parent, _ := db.CreateTable("parent", []Column{{Name: "id", Type: TypeInt}, {Name: "v", Type: TypeInt}})
	child, _ := db.CreateTable("child", []Column{{Name: "id", Type: TypeInt}, {Name: "parent", Type: TypeInt}})
	if err := parent.AddPrimaryKey([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	if err := child.AddPrimaryKey([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{parent.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeInt, 1))), parent.Insert(NewRow(MustValue(TypeInt, 2), MustValue(TypeInt, 2))), child.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeInt, 1)))} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddForeignKey("child", ForeignKey{Name: "fk", Columns: []string{"parent"}, RefTable: "parent", RefColumns: []string{"id"}, OnUpdate: "CASCADE", OnDelete: "CASCADE"}); err != nil {
		t.Fatal(err)
	}
	if _, handled, err := db.tryIndependentMutation([]RowMutation{{Table: "parent", Replacements: map[int]Row{0: NewRow(MustValue(TypeInt, 1), MustValue(TypeInt, 9))}}}); !handled || err != nil {
		t.Fatalf("non-key update did not use fast path: %v %v", handled, err)
	}
	change := []RowMutation{{Table: "parent", Replacements: map[int]Row{0: NewRow(MustValue(TypeInt, 3), MustValue(TypeInt, 9))}}}
	if _, handled, err := db.tryIndependentMutation(change); handled || err != nil {
		t.Fatalf("cascade used fast path: %v %v", handled, err)
	}
	if _, err := db.ApplyRowMutations(change); err != nil {
		t.Fatal(err)
	}
	if child.Select(nil)[0][1].Int64 != 3 {
		t.Fatal("cascade update lost")
	}
	if _, err := db.ApplyRowMutations([]RowMutation{{Table: "child", Delete: []int{0}}}); err != nil {
		t.Fatal(err)
	}
	if child.Count(nil) != 0 || len(child.indexRows["primary"]) != 0 || len(child.uniqueRows["primary"]) != 0 {
		t.Fatal("delete all left stale indexes")
	}
	if err := db.Insert("child", NewRow(MustValue(TypeInt, 1), MustValue(TypeInt, 3))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyRowMutations([]RowMutation{{Table: "parent", Delete: []int{0, 1}}}); err != nil {
		t.Fatal(err)
	}
	if parent.Count(nil) != 0 || child.Count(nil) != 0 {
		t.Fatal("cascade delete lost")
	}
}
