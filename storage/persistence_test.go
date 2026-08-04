package storage

import (
	"encoding/gob"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersistenceRoundTrip(t *testing.T) {
	directory := t.TempDir()
	store := NewStore()
	database, err := store.CreateDatabase("test")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.CreateTable("users", userColumns())
	if err != nil {
		t.Fatal(err)
	}
	table.SetComment("application users")
	_, createdAt, updatedAt := table.Metadata()
	if createdAt.IsZero() || updatedAt.IsZero() {
		t.Fatalf("missing table timestamps: created=%v updated=%v", createdAt, updatedAt)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 1), MustValue(TypeVarchar, "Alice"), MustValue(TypeInt, 20))); err != nil {
		t.Fatal(err)
	}
	if table.DataLength() == 0 {
		t.Fatal("missing table data length")
	}
	if err := table.AddIndex("users_id", []string{"id"}, true); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateView("adult_users", "SELECT id,name FROM users WHERE age >= 18", []string{"id", "name"}, false); err != nil {
		t.Fatal(err)
	}
	persistence := NewPersistence(directory)
	if err := persistence.Save(store); err != nil {
		t.Fatal(err)
	}
	assertSnapshotFormatVersion(t, persistence.Path(), CurrentSnapshotFormatVersion)
	loaded, err := persistence.Load()
	if err != nil {
		t.Fatal(err)
	}
	loadedDatabase, err := loaded.Database("test")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := loadedDatabase.Select("users", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0][1].Text != "Alice" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
	loadedTable, err := loadedDatabase.Table("users")
	if err != nil {
		t.Fatal(err)
	}
	indexes := loadedTable.Indexes()
	if len(indexes) != 1 || indexes[0].Name != "users_id" || !indexes[0].Unique {
		t.Fatalf("unexpected persisted indexes: %#v", indexes)
	}
	comment, loadedCreatedAt, loadedUpdatedAt := loadedTable.Metadata()
	if comment != "application users" || !loadedCreatedAt.Equal(createdAt) || loadedUpdatedAt.Before(updatedAt) || time.Since(loadedUpdatedAt) > time.Minute {
		t.Fatalf("unexpected persisted metadata: %q %v %v", comment, loadedCreatedAt, loadedUpdatedAt)
	}
	view, err := loadedDatabase.View("adult_users")
	if err != nil || view.Definition != "SELECT id,name FROM users WHERE age >= 18" || len(view.Columns) != 2 {
		t.Fatalf("unexpected persisted view: %#v, %v", view, err)
	}
}

func TestPersistenceLoadsLegacyUnversionedSnapshotThroughMigrationRegistry(t *testing.T) {
	directory := t.TempDir()
	store := NewStore()
	database, err := store.CreateDatabase("legacy")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.CreateTable("items", []Column{{Name: "id", Type: TypeInt}})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 7))); err != nil {
		t.Fatal(err)
	}
	persistence := NewPersistence(directory)
	if err := os.MkdirAll(filepath.Dir(persistence.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(persistence.Path(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	legacy := struct{ Databases []DatabaseSnapshot }{Databases: store.persistenceSnapshot().Databases}
	if err := gob.NewEncoder(file).Encode(legacy); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := persistence.Load()
	if err != nil {
		t.Fatalf("legacy snapshot load = %v", err)
	}
	loadedDatabase, err := loaded.Database("legacy")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := loadedDatabase.Select("items", nil)
	if err != nil || len(rows) != 1 || rows[0][0].Int64 != 7 {
		t.Fatalf("legacy rows = %#v, %v", rows, err)
	}
	inspection, err := InspectSnapshot(persistence.Path())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.SourceFormatVersion != LegacySnapshotFormatVersion || inspection.FormatVersion != CurrentSnapshotFormatVersion {
		t.Fatalf("legacy inspection formats = %#v", inspection)
	}
}

func TestPersistenceRejectsNewerSnapshotWithoutChangingIt(t *testing.T) {
	directory := t.TempDir()
	persistence := NewPersistence(directory)
	if err := os.MkdirAll(filepath.Dir(persistence.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(persistence.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(file).Encode(StoreSnapshot{FormatVersion: CurrentSnapshotFormatVersion + 1}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(persistence.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistence.Load(); err == nil || !strings.Contains(err.Error(), "newer than the supported version") {
		t.Fatalf("newer snapshot load error = %v", err)
	}
	after, err := os.ReadFile(persistence.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("newer snapshot was modified after fail-closed rejection")
	}
}

func assertSnapshotFormatVersion(t *testing.T, path string, want uint16) {
	t.Helper()
	inspection, err := InspectSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.SourceFormatVersion != want || inspection.FormatVersion != want {
		t.Fatalf("snapshot formats = %#v, want %d", inspection, want)
	}
}

func TestPersistenceReplaceFailurePreservesPreviousSnapshot(t *testing.T) {
	directory := t.TempDir()
	store := NewStore()
	database, err := store.CreateDatabase("test")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.CreateTable("items", []Column{{Name: "id", Type: TypeInt}})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 1))); err != nil {
		t.Fatal(err)
	}
	persistence := NewPersistence(directory)
	if err := persistence.Save(store); err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(NewRow(MustValue(TypeInt, 2))); err != nil {
		t.Fatal(err)
	}
	persistence.replaceFile = func(string, string) error { return errors.New("injected replace failure") }
	if err := persistence.Save(store); err == nil || !strings.Contains(err.Error(), "without deleting the previous snapshot") {
		t.Fatalf("save error = %v", err)
	}
	if _, err := os.Stat(persistence.path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("graceful failure left temporary snapshot: %v", err)
	}

	loaded, err := NewPersistence(directory).Load()
	if err != nil {
		t.Fatal(err)
	}
	loadedDatabase, err := loaded.Database("test")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := loadedDatabase.Select("items", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0][0].Int64 != 1 {
		t.Fatalf("failed replace changed durable rows: %#v", rows)
	}
}

func TestPersistenceRefusesSilentEmptyStoreWhenRecoveryCandidateExists(t *testing.T) {
	directory := t.TempDir()
	persistence := NewPersistence(directory)
	if err := os.MkdirAll(filepath.Dir(persistence.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persistence.path+".tmp", []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := persistence.Load(); err == nil || !strings.Contains(err.Error(), "recovery candidate") || !strings.Contains(err.Error(), "do not delete or overwrite") {
		t.Fatalf("load error = %v", err)
	}
	if _, err := os.Stat(persistence.path); !os.IsNotExist(err) {
		t.Fatalf("load created a replacement store: %v", err)
	}
}

func TestPersistenceCorruptionIncludesRecoveryGuidance(t *testing.T) {
	directory := t.TempDir()
	persistence := NewPersistence(directory)
	if err := os.MkdirAll(filepath.Dir(persistence.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persistence.path, []byte("not a gob snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := persistence.Load(); err == nil || !strings.Contains(err.Error(), persistence.path) || !strings.Contains(err.Error(), "known-good backup") {
		t.Fatalf("load error = %v", err)
	}
}
