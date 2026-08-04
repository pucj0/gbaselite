package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastLogEntrySinceReturnsOnlyNewStartupDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gbaselite.log")
	if err := os.WriteFile(path, []byte("old failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	offset := fileSize(path)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("new detail one\nnew detail two\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if detail := lastLogEntrySince(path, offset); detail != "new detail two" {
		t.Fatalf("startup detail = %q", detail)
	}
	if detail := lastLogEntrySince(path, fileSize(path)); detail != "" {
		t.Fatalf("stale startup detail = %q", detail)
	}
}

func TestBackgroundStartupErrorIncludesFreshLogDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gbaselite.log")
	if err := os.WriteFile(path, []byte("startup failed: initial administrator password is required\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := backgroundStartupError(42, "exited during startup", path, 0)
	if !strings.Contains(err.Error(), "initial administrator password is required") || strings.Contains(err.Error(), "check ") {
		t.Fatalf("startup error = %v", err)
	}
}
