package config

import (
	"os"
	"path/filepath"
	"testing"
)

// locking_test.go is the T057 suite for lock budget configuration, YAML, ENV, defaults, and boundary validation.

func TestLockBudgetDefaultsAndYAML(t *testing.T) {
	// 1. Defaults
	def := Default()
	if def.Resources.LockMemoryMB != 64 {
		t.Fatalf("default LockMemoryMB = %d, want 64", def.Resources.LockMemoryMB)
	}
	if def.Resources.LockMaxRequestsPerTxn != 262144 {
		t.Fatalf("default LockMaxRequestsPerTxn = %d, want 262144", def.Resources.LockMaxRequestsPerTxn)
	}
	if def.Resources.LockMaxWaiters != 4096 {
		t.Fatalf("default LockMaxWaiters = %d, want 4096", def.Resources.LockMaxWaiters)
	}
	if def.Resources.LockDeadlockWorkLimit != 100000 {
		t.Fatalf("default LockDeadlockWorkLimit = %d, want 100000", def.Resources.LockDeadlockWorkLimit)
	}

	// 2. YAML parsing
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
resources:
  lock_memory_mb: 128
  lock_max_requests_per_txn: 500000
  lock_max_waiters: 8192
  lock_deadlock_work_limit: 200000
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("failed to load YAML: %v", err)
	}
	if cfg.Resources.LockMemoryMB != 128 || cfg.Resources.LockMaxRequestsPerTxn != 500000 ||
		cfg.Resources.LockMaxWaiters != 8192 || cfg.Resources.LockDeadlockWorkLimit != 200000 {
		t.Fatalf("unexpected YAML resources values: %+v", cfg.Resources)
	}
}

func TestLockBudgetENVOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
resources:
  lock_memory_mb: 128
  lock_max_requests_per_txn: 500000
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DB_LOCK_MEMORY_MB", "256")
	t.Setenv("DB_LOCK_MAX_REQUESTS_PER_TXN", "1000000")
	t.Setenv("DB_LOCK_MAX_WAITERS", "16384")
	t.Setenv("DB_LOCK_DEADLOCK_WORK_LIMIT", "300000")

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("failed to load config with ENV: %v", err)
	}

	if cfg.Resources.LockMemoryMB != 256 {
		t.Fatalf("ENV override failed for LockMemoryMB: got %d, want 256", cfg.Resources.LockMemoryMB)
	}
	if cfg.Resources.LockMaxRequestsPerTxn != 1000000 {
		t.Fatalf("ENV override failed for LockMaxRequestsPerTxn: got %d, want 1000000", cfg.Resources.LockMaxRequestsPerTxn)
	}
	if cfg.Resources.LockMaxWaiters != 16384 {
		t.Fatalf("ENV override failed for LockMaxWaiters: got %d, want 16384", cfg.Resources.LockMaxWaiters)
	}
	if cfg.Resources.LockDeadlockWorkLimit != 300000 {
		t.Fatalf("ENV override failed for LockDeadlockWorkLimit: got %d, want 300000", cfg.Resources.LockDeadlockWorkLimit)
	}
}

func TestLockBudgetBoundaryValidation(t *testing.T) {
	dir := t.TempDir()

	// 1. Negative values are rejected
	yamlNegative := `
resources:
  lock_memory_mb: -10
`
	p1 := filepath.Join(dir, "neg.yaml")
	_ = os.WriteFile(p1, []byte(yamlNegative), 0644)
	if _, err := Load(p1); err == nil {
		t.Fatal("expected error for negative lock_memory_mb, got nil")
	}

	// 2. Overflow (> 1048576 MB) is rejected
	yamlOverflow := `
resources:
  lock_memory_mb: 2000000
`
	p2 := filepath.Join(dir, "overflow.yaml")
	_ = os.WriteFile(p2, []byte(yamlOverflow), 0644)
	if _, err := Load(p2); err == nil {
		t.Fatal("expected error for overflow lock_memory_mb, got nil")
	}

	// 3. 0 values take package defaults per contract
	yamlZero := `
resources:
  lock_memory_mb: 0
  lock_max_requests_per_txn: 0
  lock_max_waiters: 0
  lock_deadlock_work_limit: 0
`
	p3 := filepath.Join(dir, "zero.yaml")
	_ = os.WriteFile(p3, []byte(yamlZero), 0644)
	cfg, err := Load(p3)
	if err != nil {
		t.Fatalf("unexpected error for 0 options: %v", err)
	}
	if cfg.Resources.LockMemoryMB != 64 || cfg.Resources.LockMaxRequestsPerTxn != 262144 ||
		cfg.Resources.LockMaxWaiters != 4096 || cfg.Resources.LockDeadlockWorkLimit != 100000 {
		t.Fatalf("zero option did not take defaults: %+v", cfg.Resources)
	}
}
