package main

import (
	"gbaselite/catalog"
	"gbaselite/executor"
	"gbaselite/storage"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateLegacyCommandDispatch(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	store := storage.NewStore()
	db, err := store.CreateDatabase("migrated")
	if err != nil {
		t.Fatal(err)
	}
	table, err := db.CreateTableWithIndexes("items", []storage.Column{{Name: "id", Type: storage.TypeInt}}, []string{"id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = table.Insert(storage.Row{storage.MustValue(storage.TypeInt, int64(9))}); err != nil {
		t.Fatal(err)
	}
	if err = storage.NewPersistence(source).Save(store); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.OpenUsers(source, "admin", "fixture-only"); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"migrate-legacy", "--source", source, "--target", target}); err != nil {
		t.Fatal(err)
	}
	engine, err := executor.Open(target, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	result, err := engine.Execute(&executor.Session{CurrentDatabase: "migrated"}, "SELECT id FROM items")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != int64(9) {
		t.Fatalf("migration result %v, error %v", result, err)
	}
}

func TestRetiredOfflineCommandsRejectBeforeFilesystemAccess(t *testing.T) {
	for _, command := range []string{"shell", "import", "export", "backup", "restore"} {
		t.Run(command, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing-config.yaml")
			err := run([]string{command, "--config", path})
			if err == nil || !strings.Contains(err.Error(), "was removed") {
				t.Fatalf("got %v", err)
			}
			if _, err = os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("unexpected filesystem change: %v", err)
			}
		})
	}
}
