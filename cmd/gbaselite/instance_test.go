package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gbaselite/catalog"
	"gbaselite/storage"
)

func TestInspectInstanceCopyValidatesDataIndexesUsersAndGrants(t *testing.T) {
	directory := t.TempDir()
	store := storage.NewStore()
	db, err := store.CreateDatabase("private_database")
	if err != nil {
		t.Fatal(err)
	}
	table, err := db.CreateTableWithIndexes("private_table", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "label", Type: storage.TypeVarchar, Length: 32}}, []string{"id"}, []storage.Index{{Name: "private_index", Columns: []string{"label"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = table.Insert(storage.Row{storage.MustValue(storage.TypeInt, int64(1)), storage.MustValue(storage.TypeVarchar, "private row value")}); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateView("private_view", "SELECT id,label FROM private_table", nil, false); err != nil {
		t.Fatal(err)
	}
	if err = storage.NewPersistence(directory).Save(store); err != nil {
		t.Fatal(err)
	}
	users, err := catalog.OpenUsers(directory, "private_admin", "private-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = users.CreateAccount("private_reader", "%", "private-reader-password", false); err != nil {
		t.Fatal(err)
	}
	if err = users.GrantPrivileges("private_reader", "%", []string{"SELECT"}, "private_database", "*", false); err != nil {
		t.Fatal(err)
	}

	storePath := filepath.Join(directory, "databases", "store.gob")
	userPath := filepath.Join(directory, "users", "users.gob")
	storeBefore, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	usersBefore, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := inspectInstanceCopy([]string{"--directory", directory}, &output); err != nil {
		t.Fatal(err)
	}
	report := output.String()
	for _, marker := range []string{
		"GBaseLite stopped instance-copy inspection",
		"databases=1 tables=1 indexes=2 views=1 rows=1",
		"accounts=2 grants=2 privileges=2",
		"Database snapshot SHA-256:",
		"User catalog SHA-256:",
	} {
		if !strings.Contains(report, marker) {
			t.Fatalf("instance report is missing %q:\n%s", marker, report)
		}
	}
	for _, secret := range []string{
		"private_admin",
		"private-admin-password",
		"private_database",
		"private_table",
		"private_index",
		"private_view",
		"private_reader",
		"private-reader-password",
		"private row value",
	} {
		if strings.Contains(report, secret) {
			t.Fatalf("instance report exposed %q:\n%s", secret, report)
		}
	}
	storeAfter, _ := os.ReadFile(storePath)
	usersAfter, _ := os.ReadFile(userPath)
	if !bytes.Equal(storeBefore, storeAfter) || !bytes.Equal(usersBefore, usersAfter) {
		t.Fatal("instance inspection modified a persistent file")
	}
}

func TestInspectInstanceCopyRejectsRecoveryCandidate(t *testing.T) {
	directory := t.TempDir()
	if err := storage.NewPersistence(directory).Save(storage.NewStore()); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(directory, "databases", "store.gob.tmp")
	if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inspectInstanceCopy([]string{"--directory", directory}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), candidate) || !strings.Contains(err.Error(), "preserve the copy") {
		t.Fatalf("candidate inspection error = %v", err)
	}
}

func TestInspectInstanceCopyRequiresDirectory(t *testing.T) {
	if err := inspectInstanceCopy(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--directory is required") {
		t.Fatalf("inspection error = %v", err)
	}
}

func TestInspectInstanceRejectsPagedDirectoryWithStaleGob(t *testing.T) {
	for _, marker := range []string{"store.pages", "store.checkpoint", "store.wal"} {
		t.Run(marker, func(t *testing.T) {
			directory := t.TempDir()
			if err := storage.NewPersistence(directory).Save(storage.NewStore()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "databases", marker), []byte("paged marker"), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err := inspectInstanceCopy([]string{"--directory", directory}, &output)
			if err == nil || !strings.Contains(err.Error(), "paged instance inspection") || output.Len() != 0 {
				t.Fatalf("inspection output=%s error=%v", output.String(), err)
			}
		})
	}
}
