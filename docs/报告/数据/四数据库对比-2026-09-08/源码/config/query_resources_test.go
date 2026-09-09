package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQueryStorageResourceSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("resources:\n  query_timeout_ms: 250\n  sort_memory_mb: 8\n  query_result_memory_mb: 32\n  query_temp_mb: 512\n  query_temp_path: ./sort-temp\n  optimistic_transactions: true\nstorage:\n  mode: paged\n  page_cache_mb: 4\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.QueryTimeout != 250*time.Millisecond || cfg.Resources.SortMemoryMB != 8 || cfg.Resources.QueryResultMemoryMB != 32 || cfg.Resources.QueryTempMB != 512 || cfg.Resources.QueryTempPath != "./sort-temp" || !cfg.Resources.OptimisticTransactions || cfg.Storage.Mode != "paged" || cfg.Storage.PageCacheMB != 4 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	t.Setenv("DB_QUERY_TIMEOUT_MS", "1000")
	t.Setenv("DB_SORT_MEMORY_MB", "2")
	t.Setenv("DB_QUERY_RESULT_MEMORY_MB", "6")
	t.Setenv("DB_QUERY_TEMP_MB", "16")
	t.Setenv("DB_QUERY_TEMP_PATH", "env-temp")
	t.Setenv("DB_STORAGE_MODE", "snapshot")
	t.Setenv("DB_PAGE_CACHE_MB", "0")
	t.Setenv("DB_OPTIMISTIC_TRANSACTIONS", "false")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.QueryTimeout != time.Second || cfg.Resources.SortMemoryMB != 2 || cfg.Resources.QueryResultMemoryMB != 6 || cfg.Resources.QueryTempMB != 16 || cfg.Resources.QueryTempPath != "env-temp" || cfg.Resources.OptimisticTransactions || cfg.Storage.Mode != "snapshot" || cfg.Storage.PageCacheMB != 0 {
		t.Fatalf("environment ignored: %+v", cfg)
	}
}

func TestRejectQueryStorageResourceSettings(t *testing.T) {
	for _, data := range []string{"resources:\n  query_timeout_ms: -1\n", "resources:\n  query_timeout_ms: 86400001\n", "resources:\n  sort_memory_mb: 1048577\n", "resources:\n  query_result_memory_mb: -1\n", "resources:\n  query_temp_mb: 1048577\n", "resources:\n  optimistic_transactions: invalid\n", "storage:\n  mode: invalid\n", "storage:\n  page_cache_mb: -1\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}

func TestColdReadConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  path: /app/data\n  mode: paged\n  page_cache_mb: 3\n  cold_reads: true\n  cold_materialize_mb: 7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Mode != "paged" || cfg.Storage.PageCacheMB != 3 || !cfg.Storage.ColdRead || cfg.Storage.ColdMaterializeMB != 7 {
		t.Fatalf("configuration reset during path normalization: %+v", cfg.Storage)
	}
	t.Setenv("DB_COLD_READS", "false")
	t.Setenv("DB_COLD_MATERIALIZE_MB", "12")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.ColdRead || cfg.Storage.ColdMaterializeMB != 12 {
		t.Fatalf("environment ignored: %+v", cfg.Storage)
	}
}

func TestInvalidColdReadConfiguration(t *testing.T) {
	for _, data := range []string{"storage:\n  cold_reads: true\n", "storage:\n  mode: paged\n  cold_materialize_mb: 0\n", "storage:\n  mode: paged\n  cold_materialize_mb: -1\n", "storage:\n  mode: paged\n  cold_materialize_mb: 1048577\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}
