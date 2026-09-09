package storage

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestSnapshotSaveRefusesColdReferencesWithoutOverwritingDestination(t *testing.T) {
	store, _ := pagedTestStore(t, 128)
	paged := NewPagedPersistence(t.TempDir(), 4096)
	if err := paged.Save(store); err != nil {
		t.Fatal(err)
	}
	cold, err := paged.LoadCold()
	if err != nil {
		t.Fatal(err)
	}
	destination := NewPersistence(t.TempDir())
	previous, _ := pagedTestStore(t, 1)
	if err := destination.Save(previous); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(destination.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.Save(cold); err == nil || !strings.Contains(err.Error(), "unloaded cold tables") {
		t.Fatalf("cold save error=%v", err)
	}
	after, err := os.ReadFile(destination.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused cold save replaced destination")
	}
	if _, err := os.Stat(destination.Path() + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("cold save left recovery file: %v", err)
	}
	emptyDestination := NewPersistence(t.TempDir())
	if err := emptyDestination.Save(cold); err == nil {
		t.Fatal("cold save created empty snapshot")
	}
	if _, err := os.Stat(emptyDestination.Path()); !os.IsNotExist(err) {
		t.Fatalf("cold save created destination: %v", err)
	}
	if err := cold.Materialize(16 << 20); err != nil {
		t.Fatal(err)
	}
	if err := destination.Save(cold); err != nil {
		t.Fatal(err)
	}
	loaded, err := destination.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, _ := loaded.Database("paged")
	table, _ := db.Table("items")
	if table.RowCount() != 128 {
		t.Fatalf("explicit conversion lost rows: %d", table.RowCount())
	}
}
