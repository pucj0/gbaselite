package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTransactionWriteConfiguration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if c, err := Load(p); err != nil || c.Resources.TransactionWriteMB != 0 {
		t.Fatal(c.Resources, err)
	}
	os.WriteFile(p, []byte("resources:\n  transaction_write_mb: 512\n"), 0600)
	if c, err := Load(p); err != nil || c.Resources.TransactionWriteMB != 512 {
		t.Fatal(c.Resources, err)
	}
	t.Setenv("DB_TRANSACTION_WRITE_MB", "0")
	if c, err := Load(p); err != nil || c.Resources.TransactionWriteMB != 0 {
		t.Fatal(c.Resources, err)
	}
	for _, value := range []string{"-1", "1048577", "invalid"} {
		t.Setenv("DB_TRANSACTION_WRITE_MB", value)
		if _, err := Load(p); err == nil {
			t.Fatal("accepted", value)
		}
	}
	t.Setenv("DB_TRANSACTION_WRITE_MB", "")
	for _, value := range []string{"-1", "1048577", "invalid"} {
		os.WriteFile(p, []byte("resources:\n  transaction_write_mb: "+value+"\n"), 0600)
		if _, err := Load(p); err == nil {
			t.Fatal("accepted yaml", value)
		}
	}
}
