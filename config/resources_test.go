package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResourceSettingsAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("resources:\n  memory_limit_mb: 256\n  max_procs: 2\nserver:\n  max_prepared_statements: 32\n  max_prepared_memory_kb: 128\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.MemoryLimitMB != 256 || cfg.Resources.MaxProcs != 2 || cfg.Server.MaxPreparedStatements != 32 || cfg.Server.MaxPreparedMemoryKB != 128 {
		t.Fatalf("settings: %+v", cfg)
	}
	t.Setenv("DB_MEMORY_LIMIT_MB", "512")
	t.Setenv("DB_MAX_PROCS", "0")
	cfg, err = Load(path)
	if err != nil || cfg.Resources.MemoryLimitMB != 512 || cfg.Resources.MaxProcs != 0 {
		t.Fatalf("overrides: %+v %v", cfg.Resources, err)
	}
	for _, entry := range []struct{ name, value string }{{"DB_MEMORY_LIMIT_MB", "-1"}, {"DB_MEMORY_LIMIT_MB", "1048577"}, {"DB_MAX_PROCS", "1025"}, {"DB_MAX_PREPARED_STATEMENTS", "0"}, {"DB_MAX_PREPARED_MEMORY_KB", "0"}} {
		t.Run(entry.name+entry.value, func(t *testing.T) {
			t.Setenv(entry.name, entry.value)
			if _, err := Load(path); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
}
