package main

import (
	"gbaselite/executor"
	"os"
	"path/filepath"
	"testing"
)

func TestMVCCDefaultRejectsLegacySavepoints(t *testing.T) {
	e, err := openTestEngine(t, t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	s := &executor.Session{}
	if _, err = e.Execute(s, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	defer e.CloseSession(s)
	if _, err = e.Execute(s, "SAVEPOINT p"); err == nil {
		t.Fatal("legacy savepoint accepted")
	}
	if !s.InTransaction() {
		t.Fatal("rejected statement lost transaction")
	}
	if _, err = e.Execute(s, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(cfg, []byte("binlog:\n  enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = runReplayBinlog([]string{"--config", cfg}); err == nil {
		t.Fatal("runtime legacy replay accepted")
	}
}
