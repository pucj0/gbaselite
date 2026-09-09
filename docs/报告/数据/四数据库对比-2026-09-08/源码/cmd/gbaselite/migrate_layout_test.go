package main

import (
	"gbaselite/mvcc"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLayoutCLI(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "versioned")
	s, err := mvcc.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err = runMigrateLayout([]string{"--source", source, "--target", target}); err == nil {
		t.Fatal("opened running store")
	}
	s.Close()
	if err = runMigrateLayout([]string{"--source", source, "--target", target}); err != nil {
		t.Fatal(err)
	}
	if err = runMigrateLayout([]string{"--source", source, "--target", target}); err == nil {
		t.Fatal("existing destination accepted")
	}
	missing := filepath.Join(root, "missing")
	if err = runMigrateLayout([]string{"--source", missing, "--target", filepath.Join(root, "bad")}); err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err = os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created missing source")
	}
}
