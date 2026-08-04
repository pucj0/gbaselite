package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gbaselite/executor"
)

func TestInspectInstanceCopyValidatesDataIndexesUsersAndGrants(t *testing.T) {
	directory := t.TempDir()
	engine, err := executor.Open(directory, "private_admin", "private-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	session := &executor.Session{Username: "private_admin", Host: "%"}
	for _, query := range []string{
		"CREATE DATABASE private_database",
		"CREATE TABLE private_database.private_table(id INT NOT NULL,label VARCHAR(32),PRIMARY KEY(id),KEY private_index(label))",
		"INSERT INTO private_database.private_table VALUES(1,'private row value')",
		"CREATE VIEW private_database.private_view AS SELECT id,label FROM private_database.private_table",
		"CREATE USER 'private_reader'@'%' IDENTIFIED BY 'private-reader-password'",
		"GRANT SELECT ON private_database.* TO 'private_reader'@'%'",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if err := engine.Close(); err != nil {
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
	engine, err := executor.Open(directory, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
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
