package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestClaimPIDFileReclaimsCurrentProcessPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gbaselite.pid")
	currentPID := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(path, []byte(" \n"+currentPID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := claimPIDFile(path); err != nil {
		t.Fatalf("claim stale current-process PID file: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != currentPID {
		t.Fatalf("PID file = %q, want %q", contents, currentPID)
	}
}

func TestClaimPIDFileRejectsOtherLiveProcess(t *testing.T) {
	parentPID := os.Getppid()
	if parentPID <= 0 || !processIsAlive(parentPID) {
		t.Skip("parent process is not available for a live PID check")
	}
	path := filepath.Join(t.TempDir(), "gbaselite.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(parentPID)), 0o600); err != nil {
		t.Fatal(err)
	}

	err := claimPIDFile(path)
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(parentPID)) {
		t.Fatalf("claim live PID file error = %v", err)
	}
}

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
