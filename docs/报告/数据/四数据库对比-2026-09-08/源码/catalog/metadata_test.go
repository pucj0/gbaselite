package catalog

import (
	"encoding/gob"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountPrivilegesPersistAndLegacyUsersMigrate(t *testing.T) {
	dataDir := t.TempDir()
	userDir := filepath.Join(dataDir, "users")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(userDir, "users.gob"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]User{"root": {Username: "root", PasswordHash: passwordHash("root-secret")}}
	if err := gob.NewEncoder(file).Encode(legacy); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	users, err := OpenUsers(dataDir, "root", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if !users.VerifyPassword("root", "root-secret") {
		t.Fatal("legacy root password was not preserved")
	}
	if !users.Allowed("root", "%", "CREATE USER", "*", "*") || !users.CanGrant("root", "%", "SELECT", "app", "records") {
		t.Fatal("default account did not receive administrator privileges")
	}
	if created, err := users.CreateAccount("reader", "localhost", "secret", false); err != nil || !created {
		t.Fatalf("create account = %v, %v", created, err)
	}
	if err := users.GrantPrivileges("reader", "localhost", []string{"SELECT", "UPDATE"}, "app", "records", true); err != nil {
		t.Fatal(err)
	}
	if !users.Allowed("reader", "localhost", "SELECT", "app", "records") || users.Allowed("reader", "localhost", "INSERT", "app", "records") {
		t.Fatal("object privileges were not enforced")
	}
	if !users.CanGrant("reader", "localhost", "UPDATE", "app", "records") {
		t.Fatal("grant option was not persisted")
	}

	reopened, err := OpenUsers(dataDir, "root", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Allowed("reader", "localhost", "SELECT", "app", "records") {
		t.Fatal("grant was lost after reopening users catalog")
	}
	if err := reopened.RevokePrivileges("reader", "localhost", []string{"UPDATE"}, "app", "records", false); err != nil {
		t.Fatal(err)
	}
	if reopened.Allowed("reader", "localhost", "UPDATE", "app", "records") {
		t.Fatal("revoked privilege remains active")
	}
	if err := reopened.RenameAccount("reader", "localhost", "reporter", "%"); err != nil {
		t.Fatal(err)
	}
	if _, exists := reopened.Account("reporter", "%"); !exists {
		t.Fatal("renamed account not found")
	}
	if dropped, err := reopened.DropAccount("reporter", "%", false); err != nil || !dropped {
		t.Fatalf("drop account = %v, %v", dropped, err)
	}
}

func TestUserCatalogRejectsNewerFormatWithoutReplacingIt(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "users", "users.gob")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	newer := userCatalogSnapshot{FormatVersion: CurrentUserCatalogFormatVersion + 1, Users: map[string]User{}}
	if err := gob.NewEncoder(file).Encode(newer); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUsers(dataDir, "root", "bootstrap-secret"); err == nil || !strings.Contains(err.Error(), "newer than the supported version") {
		t.Fatalf("newer user catalog error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("newer user catalog was modified after fail-closed rejection")
	}
}

func TestBootstrapPasswordIsRequiredOnlyForNewUserCatalog(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := OpenUsers(dataDir, "root", ""); err == nil {
		t.Fatal("empty bootstrap password initialized a new user catalog")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "users", "users.gob")); !os.IsNotExist(err) {
		t.Fatalf("failed bootstrap left a user catalog: %v", err)
	}

	users, err := OpenUsers(dataDir, "root", "initial-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !users.VerifyPassword("root", "initial-secret") {
		t.Fatal("initial administrator password was not persisted")
	}
	reopened, err := OpenUsers(dataDir, "root", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.VerifyPassword("root", "initial-secret") {
		t.Fatal("blank bootstrap config changed the persisted password")
	}
}

func TestExistingCatalogDoesNotRecreateDroppedBootstrapAccount(t *testing.T) {
	dataDir := t.TempDir()
	users, err := OpenUsers(dataDir, "root", "initial-secret")
	if err != nil {
		t.Fatal(err)
	}
	if dropped, err := users.DropAccount("root", "%", false); err != nil || !dropped {
		t.Fatalf("drop bootstrap account = %v, %v", dropped, err)
	}

	reopened, err := OpenUsers(dataDir, "root", "initial-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := reopened.Account("root", "%"); exists {
		t.Fatal("existing catalog recreated the dropped bootstrap account")
	}
}

func TestUserCatalogReplaceFailurePreservesPreviousCatalog(t *testing.T) {
	dataDir := t.TempDir()
	users, err := OpenUsers(dataDir, "root", "old-secret")
	if err != nil {
		t.Fatal(err)
	}
	users.replaceFile = func(string, string) error { return errors.New("injected replace failure") }
	if err := users.AlterPassword("root", "new-secret"); err == nil || !strings.Contains(err.Error(), "without deleting the previous catalog") {
		t.Fatalf("alter password error = %v", err)
	}
	if !users.VerifyPassword("root", "old-secret") || users.VerifyPassword("root", "new-secret") {
		t.Fatal("failed replace left the in-memory password changed")
	}
	if _, err := os.Stat(users.path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("graceful failure left temporary catalog: %v", err)
	}

	reopened, err := OpenUsers(dataDir, "root", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.VerifyPassword("root", "old-secret") || reopened.VerifyPassword("root", "new-secret") {
		t.Fatal("failed replace changed the durable password")
	}
}

func TestUserCatalogRefusesBootstrapWhenRecoveryCandidateExists(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "users", "users.gob")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUsers(dataDir, "root", "bootstrap-secret"); err == nil || !strings.Contains(err.Error(), "recovery candidate") || !strings.Contains(err.Error(), "do not initialize") {
		t.Fatalf("open users error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bootstrap replaced the recovery candidate: %v", err)
	}
}

func TestUserCatalogCorruptionIncludesRecoveryGuidance(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "users", "users.gob")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a gob catalog"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUsers(dataDir, "root", "bootstrap-secret"); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "known-good backup") {
		t.Fatalf("open users error = %v", err)
	}
}
