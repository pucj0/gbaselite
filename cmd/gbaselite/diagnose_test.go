package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiagnoseReportsRuntimePathsWithoutSecrets(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "data")
	logPath := filepath.Join(root, "logs")
	for _, directory := range []string{filepath.Join(dataPath, "databases"), filepath.Join(dataPath, "users"), logPath} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataPath, "databases", "store.gob"), []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataPath, "users", "users.gob"), []byte("catalog"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainLogPath := filepath.Join(logPath, "gbaselite.log")
	auditPath := filepath.Join(logPath, "audit.jsonl")
	binlogPath := filepath.Join(dataPath, "binlog.jsonl")
	for path, content := range map[string]string{
		mainLogPath: "main",
		filepath.Join(logPath, "gbaselite-20260731-120000.000000000Z.log"): "rotated",
		filepath.Join(logPath, "gbaselite-manual.log"):                     "not a managed rotation",
		auditPath:  "audit",
		binlogPath: "binlog",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	configPath := filepath.Join(root, "config.yaml")
	contents := fmt.Sprintf("server:\n  host: 127.0.0.1\n  port: %d\nstorage:\n  path: '%s'\nauth:\n  username: root\n  password: 'do-not-print-this'\nlog:\n  path: '%s'\naudit:\n  enabled: true\n  path: '%s'\nbinlog:\n  enabled: true\n  path: '%s'\n", port, filepath.ToSlash(dataPath), filepath.ToSlash(logPath), filepath.ToSlash(auditPath), filepath.ToSlash(binlogPath))
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := diagnose(configPath, time.Second, &output); err != nil {
		t.Fatal(err)
	}
	report := output.String()
	for _, expected := range []string{"GBaseLite diagnostic", "TCP listener:", "reachable", "Data volume: total=", "available=", "Snapshot:", "User catalog:", "Log volume: total=", "Main log: " + mainLogPath + " (available, 4 bytes", "rotated=1 files, 7 bytes", "max_size=20 MiB, retention=7 days", "TLS: disabled", "Audit: enabled", auditPath, "available, 5 bytes", "Binlog: enabled", binlogPath, "available, 6 bytes"} {
		if !strings.Contains(report, expected) {
			t.Fatalf("diagnostic report is missing %q:\n%s", expected, report)
		}
	}
	if strings.Contains(report, "do-not-print-this") || strings.Contains(report, "username") || strings.Contains(report, "password") {
		t.Fatalf("diagnostic report contains credentials:\n%s", report)
	}
}

func TestDiagnoseFailsClosedForRecoveryCandidate(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "data")
	logPath := filepath.Join(root, "logs")
	if err := os.MkdirAll(filepath.Join(dataPath, "databases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataPath, "users"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(logPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataPath, "databases", "store.gob.tmp"), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	contents := fmt.Sprintf("server:\n  host: 127.0.0.1\n  port: 1\nstorage:\n  path: '%s'\nlog:\n  path: '%s'\n", filepath.ToSlash(dataPath), filepath.ToSlash(logPath))
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := diagnose(configPath, 10*time.Millisecond, &output); err == nil {
		t.Fatal("diagnose accepted a recovery candidate and unreachable listener")
	}
	report := output.String()
	if !strings.Contains(report, "recovery candidate exists") || !strings.Contains(report, "unreachable") {
		t.Fatalf("diagnostic failure report =\n%s", report)
	}
}
