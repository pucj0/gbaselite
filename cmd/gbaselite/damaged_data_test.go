package main

import (
	"bytes"
	"encoding/gob"
	"gbaselite/storage"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDamagedData(t *testing.T) {
	d := t.TempDir()
	if damaged, err := checkDamagedData(d); err != nil || damaged {
		t.Fatal(damaged, err)
	}
	if err := os.MkdirAll(filepath.Join(d, "databases"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "databases/store.gob.tmp"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if damaged, err := checkDamagedData(d); err != nil || !damaged {
		t.Fatal(damaged, err)
	}
	if err := os.MkdirAll(filepath.Join(d, "versioned"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "versioned/mvcc.db"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if damaged, err := checkDamagedData(d); err == nil || damaged {
		t.Fatal("must not classify MVCC", damaged, err)
	}
}

func TestDamageProbePreservesValidAndUnknownData(t *testing.T) {
	d := t.TempDir()
	persistence := storage.NewPersistence(d)
	if err := persistence.Save(storage.NewStore()); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(persistence.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persistence.Path()+".tmp", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if damaged, err := checkDamagedData(d); err != nil || damaged {
		t.Fatal("valid primary must win", damaged, err)
	}
	if err := os.Remove(persistence.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persistence.Path()+".tmp", original, 0600); err != nil {
		t.Fatal(err)
	}
	if damaged, err := checkDamagedData(d); err != nil || damaged {
		t.Fatal("valid recovery candidate", damaged, err)
	}
	var future bytes.Buffer
	if err := gob.NewEncoder(&future).Encode(storage.StoreSnapshot{FormatVersion: 65535}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persistence.Path()+".tmp", future.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if damaged, err := checkDamagedData(d); err == nil || damaged {
		t.Fatal("future format must not be classified", damaged, err)
	}
	after, err := os.ReadFile(persistence.Path() + ".tmp")
	if err != nil || !bytes.Equal(after, future.Bytes()) {
		t.Fatal("probe modified data", err)
	}
}
