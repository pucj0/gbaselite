package storage

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestStoreDatabaseLifecycle(t *testing.T) {
	store := NewStore()
	database, err := store.CreateDatabase("test")
	if err != nil {
		t.Fatal(err)
	}
	if database.Name() != "test" {
		t.Fatalf("database name = %q", database.Name())
	}
	if _, err := store.CreateDatabase("TEST"); !errors.Is(err, ErrDatabaseExists) {
		t.Fatalf("duplicate error = %v, want ErrDatabaseExists", err)
	}
	if got, err := store.Database("TeSt"); err != nil || got != database {
		t.Fatalf("case-insensitive lookup failed: database=%p error=%v", got, err)
	}
	if err := store.DropDatabase("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Database("test"); !errors.Is(err, ErrDatabaseNotFound) {
		t.Fatalf("lookup error = %v, want ErrDatabaseNotFound", err)
	}
}

func TestConcurrentAutoIncrementAndUniqueLookup(t *testing.T) {
	table, err := NewTable("items", []Column{{Name: "id", Type: TypeInt, MetadataVersion: 1, Nullable: false, AutoIncrement: true}, {Name: "name", Type: TypeVarchar, Length: 32}})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.AddPrimaryKey([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	const workers = 128
	var wg sync.WaitGroup
	errorsFound := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := table.NextAutoIncrement("id")
			if err == nil {
				err = table.Insert(NewRow(MustValue(TypeInt, id), MustValue(TypeVarchar, fmt.Sprintf("item-%d", id))))
			}
			if err != nil {
				errorsFound <- err
			}
		}()
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if table.RowCount() != workers {
		t.Fatalf("row count = %d", table.RowCount())
	}
	row, found, indexed := table.LookupUnique("id", MustValue(TypeInt, workers))
	if !indexed || !found || row[1].Text != fmt.Sprintf("item-%d", workers) {
		t.Fatalf("unique lookup = %#v, found=%v indexed=%v", row, found, indexed)
	}
}

func TestNumericDatabaseName(t *testing.T) {
	store := NewStore()
	if _, err := store.CreateDatabase("11"); err != nil {
		t.Fatalf("CreateDatabase numeric name: %v", err)
	}
}

func TestQuotedStyleDatabaseNameWithHyphen(t *testing.T) {
	store := NewStore()
	if _, err := store.CreateDatabase("yuanma-auth"); err != nil {
		t.Fatalf("CreateDatabase hyphenated name: %v", err)
	}
	if _, err := store.Database("YUANMA-AUTH"); err != nil {
		t.Fatalf("Database hyphenated name lookup: %v", err)
	}
}

func TestTableCRUD(t *testing.T) {
	database := mustDatabase(t)
	table := mustUsersTable(t, database)

	rows := []Row{
		NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "张三"), MustValue(TypeInt, 20)),
		NewRow(MustValue(TypeInt, 2), MustValue(TypeVarchar, "李四"), MustValue(TypeInt, 25)),
	}
	for _, row := range rows {
		if err := database.Insert("USER", row); err != nil {
			t.Fatal(err)
		}
	}

	selected, err := database.Select("user", func(row Row) bool { return row[2].Int64 >= 21 })
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0][1].Text != "李四" {
		t.Fatalf("unexpected selected rows: %#v", selected)
	}

	updated, err := database.Update("user", func(row Row) bool { return row[0].Int64 == 1 }, map[string]Value{
		"AGE": MustValue(TypeInt, 21),
	})
	if err != nil || updated != 1 {
		t.Fatalf("Update() = (%d, %v), want (1, nil)", updated, err)
	}

	deleted, err := database.Delete("user", func(row Row) bool { return row[0].Int64 == 2 })
	if err != nil || deleted != 1 {
		t.Fatalf("Delete() = (%d, %v), want (1, nil)", deleted, err)
	}
	remaining := table.Select(nil)
	if len(remaining) != 1 || remaining[0][2].Int64 != 21 {
		t.Fatalf("unexpected remaining rows: %#v", remaining)
	}
}

func TestUniqueIndexValidation(t *testing.T) {
	database := mustDatabase(t)
	table := mustUsersTable(t, database)
	if err := table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "Alice"), MustValue(TypeInt, 20))); err != nil {
		t.Fatal(err)
	}
	if err := table.AddIndex("users_id", []string{"id"}, true); err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "Bob"), MustValue(TypeInt, 30))); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate insert error = %v", err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 2), MustValue(TypeVarchar, "Bob"), MustValue(TypeInt, 30))); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Update(func(row Row) bool { return row[0].Int64 == 2 }, map[string]Value{"id": MustValue(TypeInt, 1)}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate update error = %v", err)
	}
	rows := table.Select(nil)
	if len(rows) != 2 || rows[1][0].Int64 != 2 {
		t.Fatalf("failed update changed rows: %#v", rows)
	}
	indexes := table.Indexes()
	if len(indexes) != 1 || indexes[0].Name != "users_id" || !indexes[0].Unique {
		t.Fatalf("indexes = %#v", indexes)
	}
	if err := table.DropIndex("USERS_ID"); err != nil {
		t.Fatal(err)
	}
}

func TestAlterColumnConvertsRowsAndRenamesIndexes(t *testing.T) {
	database := mustDatabase(t)
	table, err := database.CreateTable("events", []Column{{Name: "id", Type: TypeInt}, {Name: "last_time", Type: TypeDate}})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeDate, "2026-07-28"))); err != nil {
		t.Fatal(err)
	}
	if err := table.AddIndex("events_last_time", []string{"last_time"}, false); err != nil {
		t.Fatal(err)
	}
	if err := table.AlterColumn("last_time", Column{Name: "updated_at", Type: TypeDateTime}); err != nil {
		t.Fatal(err)
	}
	columns := table.Columns()
	if columns[1].Name != "updated_at" || columns[1].Type != TypeDateTime {
		t.Fatalf("unexpected altered column: %#v", columns[1])
	}
	rows := table.Select(nil)
	if rows[0][1].Type != TypeDateTime || rows[0][1].String() != "2026-07-28 00:00:00" {
		t.Fatalf("unexpected converted value: %#v", rows[0][1])
	}
	indexes := table.Indexes()
	if len(indexes) != 1 || len(indexes[0].Columns) != 1 || indexes[0].Columns[0] != "updated_at" {
		t.Fatalf("unexpected altered indexes: %#v", indexes)
	}
	if _, ok := table.ColumnIndex("last_time"); ok {
		t.Fatal("old column name still resolves")
	}
}

func TestAlterColumnFailureIsAtomic(t *testing.T) {
	database := mustDatabase(t)
	table, err := database.CreateTable("events", []Column{{Name: "label", Type: TypeVarchar, Length: 32}, {Name: "other", Type: TypeInt}})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeVarchar, "not-a-date"), MustValue(TypeInt, 1))); err != nil {
		t.Fatal(err)
	}
	if err := table.AlterColumn("label", Column{Name: "happened_at", Type: TypeDateTime}); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("alter error = %v, want ErrTypeMismatch", err)
	}
	columns := table.Columns()
	rows := table.Select(nil)
	if columns[0].Name != "label" || columns[0].Type != TypeVarchar || rows[0][0].Text != "not-a-date" {
		t.Fatalf("failed ALTER changed table: columns=%#v rows=%#v", columns, rows)
	}
	if err := table.AlterColumn("missing", Column{Name: "missing", Type: TypeDate}); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("missing column error = %v", err)
	}
	if err := table.AlterColumn("label", Column{Name: "other", Type: TypeVarchar, Length: 32}); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("duplicate column error = %v", err)
	}
}

func TestInsertValidationAndSnapshotIsolation(t *testing.T) {
	database := mustDatabase(t)
	table := mustUsersTable(t, database)

	if err := table.Insert(NewRow(MustValue(TypeInt, 1))); !errors.Is(err, ErrColumnCount) {
		t.Fatalf("short row error = %v, want ErrColumnCount", err)
	}
	if err := table.Insert(NewRow(
		MustValue(TypeInt, 1),
		MustValue(TypeVarchar, "a name that is too long"),
		MustValue(TypeInt, 20),
	)); err == nil {
		t.Fatal("expected VARCHAR length validation error")
	}

	input := NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "Alice"), MustValue(TypeInt, 20))
	if err := table.Insert(input); err != nil {
		t.Fatal(err)
	}
	input[1] = MustValue(TypeVarchar, "Bob")
	snapshot := table.Select(nil)
	if snapshot[0][1].Text != "Alice" {
		t.Fatalf("stored row changed through input slice: %#v", snapshot[0])
	}
	snapshot[0][1] = MustValue(TypeVarchar, "Carol")
	if table.Select(nil)[0][1].Text != "Alice" {
		t.Fatal("stored row changed through result snapshot")
	}
}

func TestConcurrentInsert(t *testing.T) {
	database := mustDatabase(t)
	table := mustUsersTable(t, database)

	const count = 100
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			row := NewRow(
				MustValue(TypeInt, id),
				MustValue(TypeVarchar, fmt.Sprintf("u%d", id)),
				MustValue(TypeInt, 20),
			)
			if err := table.Insert(row); err != nil {
				t.Errorf("Insert() error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if table.RowCount() != count {
		t.Fatalf("RowCount() = %d, want %d", table.RowCount(), count)
	}
}

func TestProjectAndCount(t *testing.T) {
	database := mustDatabase(t)
	table := mustUsersTable(t, database)
	for i, name := range []string{"Alice", "Bob", "Carol", "Dave"} {
		if err := table.Insert(NewRow(
			MustValue(TypeInt, i+1),
			MustValue(TypeVarchar, name),
			MustValue(TypeInt, 20+i),
		)); err != nil {
			t.Fatal(err)
		}
	}
	predicate := func(row Row) bool { return row[2].Int64 >= 21 }
	if got := table.Count(predicate); got != 3 {
		t.Fatalf("Count() = %d, want 3", got)
	}
	rows := table.Project(predicate, []int{1, 0}, 1, 1)
	if len(rows) != 1 || rows[0][0] != "Carol" || rows[0][1] != int64(3) {
		t.Fatalf("unexpected projection: %#v", rows)
	}
}

func TestTableLifecycle(t *testing.T) {
	database := mustDatabase(t)
	mustUsersTable(t, database)
	if _, err := database.CreateTable("USER", userColumns()); !errors.Is(err, ErrTableExists) {
		t.Fatalf("duplicate error = %v, want ErrTableExists", err)
	}
	if err := database.DropTable("User"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Table("user"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("lookup error = %v, want ErrTableNotFound", err)
	}
}

func TestViewLifecycleAndSharedRelationNamespace(t *testing.T) {
	database := mustDatabase(t)
	if err := database.CreateView("active_users", "SELECT id FROM users", []string{"user_id"}, false); err != nil {
		t.Fatal(err)
	}
	view, err := database.View("ACTIVE_USERS")
	if err != nil || view.Definition != "SELECT id FROM users" || len(view.Columns) != 1 {
		t.Fatalf("unexpected view: %#v, %v", view, err)
	}
	if _, err := database.CreateTable("active_users", userColumns()); !errors.Is(err, ErrTableExists) {
		t.Fatalf("table/view collision error = %v", err)
	}
	if err := database.CreateView("active_users", "SELECT name FROM users", nil, false); !errors.Is(err, ErrViewExists) {
		t.Fatalf("duplicate view error = %v", err)
	}
	if err := database.CreateView("active_users", "SELECT name FROM users", nil, true); err != nil {
		t.Fatal(err)
	}
	if names := database.ListRelations(); len(names) != 1 || names[0] != "active_users" {
		t.Fatalf("relations = %v", names)
	}
	if err := database.DropView("active_users"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.View("active_users"); !errors.Is(err, ErrViewNotFound) {
		t.Fatalf("view lookup error = %v", err)
	}
}

func TestTableDataLengthTracksMutations(t *testing.T) {
	database := mustDatabase(t)
	table, err := database.CreateTable("sizes", []Column{{Name: "id", Type: TypeInt}, {Name: "label", Type: TypeVarchar, Length: 32}})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "abc"))); err != nil {
		t.Fatal(err)
	}
	if got := table.DataLength(); got != 7 {
		t.Fatalf("data length after insert = %d, want 7", got)
	}
	if _, err := table.Update(func(row Row) bool { return row[0].Int64 == 1 }, map[string]Value{"label": MustValue(TypeVarchar, "x")}); err != nil {
		t.Fatal(err)
	}
	if got := table.DataLength(); got != 5 {
		t.Fatalf("data length after update = %d, want 5", got)
	}
	if deleted := table.Delete(func(row Row) bool { return row[0].Int64 == 1 }); deleted != 1 {
		t.Fatalf("deleted rows = %d, want 1", deleted)
	}
	if got := table.DataLength(); got != 0 {
		t.Fatalf("data length after delete = %d, want 0", got)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 2), MustValue(TypeVarchar, "reset"))); err != nil {
		t.Fatal(err)
	}
	table.Truncate()
	if got := table.DataLength(); got != 0 {
		t.Fatalf("data length after truncate = %d, want 0", got)
	}
}

func TestDeleteLimitRemovesOnlyRequestedMatches(t *testing.T) {
	database := mustDatabase(t)
	table := mustUsersTable(t, database)
	for _, row := range []Row{
		NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "first"), MustValue(TypeInt, 20)),
		NewRow(MustValue(TypeInt, 2), MustValue(TypeVarchar, "second"), MustValue(TypeInt, 20)),
		NewRow(MustValue(TypeInt, 3), MustValue(TypeVarchar, "third"), MustValue(TypeInt, 30)),
	} {
		if err := table.Insert(row); err != nil {
			t.Fatal(err)
		}
	}
	if deleted := table.DeleteLimit(func(row Row) bool { return row[2].Int64 == 20 }, 1); deleted != 1 {
		t.Fatalf("DeleteLimit() = %d, want 1", deleted)
	}
	rows := table.Select(nil)
	if len(rows) != 2 || rows[0][0].Int64 != 2 || rows[1][0].Int64 != 3 {
		t.Fatalf("unexpected remaining rows: %#v", rows)
	}
	if deleted := table.DeleteLimit(nil, 0); deleted != 0 || len(table.Select(nil)) != 2 {
		t.Fatalf("zero limit changed rows: deleted=%d rows=%#v", deleted, table.Select(nil))
	}
}

func mustDatabase(t *testing.T) *Database {
	t.Helper()
	database, err := NewDatabase("test")
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func mustUsersTable(t *testing.T, database *Database) *Table {
	t.Helper()
	table, err := database.CreateTable("user", userColumns())
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func userColumns() []Column {
	return []Column{
		{Name: "id", Type: TypeInt},
		{Name: "name", Type: TypeVarchar, Length: 10},
		{Name: "age", Type: TypeInt},
	}
}
