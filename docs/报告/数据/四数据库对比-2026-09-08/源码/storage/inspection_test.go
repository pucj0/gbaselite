package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectSnapshotReportsStructuralMetadata(t *testing.T) {
	directory := t.TempDir()
	store := NewStore()
	database, err := store.CreateDatabase("private_database_name")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.CreateTableWithIndexes(
		"private_table_name",
		[]Column{{Name: "id", Type: TypeInt}, {Name: "secret", Type: TypeVarchar, Length: 32}},
		[]string{"id"},
		[]Index{{Name: "private_index_name", Columns: []string{"secret"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "private row value"))); err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 2), MustValue(TypeVarchar, "another private value"))); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateView("private_view_name", "SELECT id FROM private_table_name", []string{"id"}, false); err != nil {
		t.Fatal(err)
	}
	persistence := NewPersistence(directory)
	if err := persistence.Save(store); err != nil {
		t.Fatal(err)
	}

	inspection, err := InspectSnapshot(persistence.Path())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Databases != 1 || inspection.Tables != 1 || inspection.Indexes != 2 || inspection.Views != 1 || inspection.Rows != 2 {
		t.Fatalf("inspection counts = %#v", inspection)
	}
	if inspection.SourceFormatVersion != CurrentSnapshotFormatVersion || inspection.FormatVersion != CurrentSnapshotFormatVersion {
		t.Fatalf("inspection formats = %#v", inspection)
	}
	if inspection.Size <= 0 || len(inspection.SHA256) != 64 || inspection.ModifiedAt.IsZero() {
		t.Fatalf("inspection file metadata = %#v", inspection)
	}
	if inspection.Path != persistence.Path() {
		t.Fatalf("inspection path = %q, want %q", inspection.Path, persistence.Path())
	}
	formatted := inspection.Path + inspection.SHA256
	for _, secret := range []string{"private_database_name", "private_table_name", "private_index_name", "private_view_name", "private row value"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("inspection exposed %q", secret)
		}
	}
}

func TestInspectSnapshotRejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.gob")
	if err := os.WriteFile(path, []byte("not a gob snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectSnapshot(path); err == nil || !strings.Contains(err.Error(), "decode snapshot") || !strings.Contains(err.Error(), path) {
		t.Fatalf("inspection error = %v", err)
	}
}
