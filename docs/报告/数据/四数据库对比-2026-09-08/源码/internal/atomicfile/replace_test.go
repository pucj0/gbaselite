package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceOverwritesTargetAtomically(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.tmp")
	target := filepath.Join(directory, "target.dat")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(source, target); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new" {
		t.Fatalf("target contents = %q", contents)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after replace: %v", err)
	}
}

func TestReplaceFailurePreservesTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.dat")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(filepath.Join(directory, "missing.tmp"), target); err == nil {
		t.Fatal("replace unexpectedly succeeded")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "old" {
		t.Fatalf("failed replace changed target to %q", contents)
	}
}
