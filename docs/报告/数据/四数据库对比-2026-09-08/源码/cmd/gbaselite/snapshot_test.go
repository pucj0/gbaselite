package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gbaselite/storage"
)

func TestInspectSnapshotsReportsSafeComparison(t *testing.T) {
	directory := t.TempDir()
	store := storage.NewStore()
	database, err := store.CreateDatabase("secret_database_name")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.CreateTable("secret_table_name", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "value", Type: storage.TypeVarchar, Length: 32}})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, 1), storage.MustValue(storage.TypeVarchar, "secret row value"))); err != nil {
		t.Fatal(err)
	}
	persistence := storage.NewPersistence(directory)
	if err := persistence.Save(store); err != nil {
		t.Fatal(err)
	}
	compared := filepath.Join(directory, "candidate.gob")
	source, err := os.Open(persistence.Path())
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.OpenFile(compared, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		source.Close()
		destination.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		destination.Close()
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := inspectSnapshots([]string{"--file", persistence.Path(), "--compare", compared}, &output); err != nil {
		t.Fatal(err)
	}
	report := output.String()
	for _, marker := range []string{"GBaseLite snapshot inspection", "Primary SHA-256:", "Compared SHA-256:", "databases=1 tables=1 indexes=0 views=0 rows=1", "Identical SHA-256: true"} {
		if !strings.Contains(report, marker) {
			t.Fatalf("snapshot report is missing %q:\n%s", marker, report)
		}
	}
	for _, secret := range []string{"secret_database_name", "secret_table_name", "secret row value"} {
		if strings.Contains(report, secret) {
			t.Fatalf("snapshot report exposed %q:\n%s", secret, report)
		}
	}
}

func TestInspectSnapshotsRequiresFile(t *testing.T) {
	if err := inspectSnapshots(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "--file is required") {
		t.Fatalf("inspection error = %v", err)
	}
}
