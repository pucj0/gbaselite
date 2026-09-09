package catalog

import (
	"encoding/gob"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectUserCatalogReportsSafeAggregateMetadata(t *testing.T) {
	directory := t.TempDir()
	users, err := OpenUsers(directory, "private_admin", "private-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.CreateAccount("private_reader", "%", "reader-password", false); err != nil {
		t.Fatal(err)
	}
	if err := users.GrantPrivileges("private_reader", "%", []string{"SELECT", "SHOW VIEW"}, "private_database", "*", false); err != nil {
		t.Fatal(err)
	}

	inspection, err := InspectUserCatalog(filepath.Join(directory, "users", "users.gob"))
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Accounts != 2 || inspection.Grants != 2 || inspection.Privileges != 3 {
		t.Fatalf("inspection counts = %#v", inspection)
	}
	if inspection.SourceFormatVersion != CurrentUserCatalogFormatVersion || inspection.FormatVersion != CurrentUserCatalogFormatVersion {
		t.Fatalf("inspection formats = %#v", inspection)
	}
	if inspection.Size <= 0 || len(inspection.SHA256) != 64 || inspection.ModifiedAt.IsZero() {
		t.Fatalf("inspection file metadata = %#v", inspection)
	}
	formatted := inspection.Path + inspection.SHA256
	for _, secret := range []string{"private_admin", "private_reader", "private-password", "reader-password", "private_database"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("inspection exposed %q", secret)
		}
	}
}

func TestInspectUserCatalogRejectsInvalidCredentialsAndPrivileges(t *testing.T) {
	for name, users := range map[string]map[string]User{
		"password": {
			"broken@%": {Username: "broken", Host: "%", PasswordHash: []byte("short")},
		},
		"privilege": {
			"broken@%": {Username: "broken", Host: "%", PasswordHash: passwordHash("secret"), Grants: []Grant{{Database: "*", Table: "*", Privileges: []string{"NOT A PRIVILEGE"}}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "users.gob")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := gob.NewEncoder(file).Encode(users); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectUserCatalog(path); err == nil || strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "NOT A PRIVILEGE") {
				t.Fatalf("safe validation error = %v", err)
			}
		})
	}
}
