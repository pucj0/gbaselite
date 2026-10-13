package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// locking_settings_test.go is the T057 suite for session lock wait timeout syntax/bounds validation,
// query deadline overrides, and engine-level lock budget passing.

func TestT057SessionLockWaitTimeoutBoundsAndSyntax(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)")
	s := &Session{}
	f.must(s, "USE test")

	// 1. Valid range 1..86400
	for _, valid := range []int64{1, 50, 86400} {
		f.must(s, fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", valid))
		if got, want := s.LockWaitTimeout(), time.Duration(valid)*time.Second; got != want {
			t.Fatalf("expected s.LockWaitTimeout() to be %s, got %s", want, got)
		}
	}

	// 2. SET GLOBAL is rejected
	_, err := f.engine.Execute(s, "SET GLOBAL innodb_lock_wait_timeout = 50")
	if err == nil || !strings.Contains(err.Error(), "GLOBAL") {
		t.Fatalf("expected error rejecting SET GLOBAL, got: %v", err)
	}

	// 3. Negative, 0, decimals, and out-of-range (> 86400) values are rejected
	invalids := []string{"-1", "0", "1.5", "86401", "abc", "10,20"}
	for _, inv := range invalids {
		_, err := f.engine.Execute(s, "SET SESSION innodb_lock_wait_timeout = "+inv)
		if err == nil {
			t.Fatalf("expected error for invalid timeout value %q, got nil", inv)
		}
	}
}

func TestT057QueryContextDeadlineOverridesSessionBudget(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1, 10)")

	sA := t034Session(t, f, 50) // Session A has 50s session budget
	sB := t034Session(t, f, 50) // Session B has 50s session budget

	// Session A holds lock on row 1
	f.must(sA, "BEGIN")
	f.must(sA, "UPDATE items SET v = 11 WHERE id = 1")

	// Session B executes query with a short 200ms query context deadline!
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	sB.Context = ctx
	start := time.Now()
	_, err := f.engine.Execute(sB, "UPDATE items SET v = 99 WHERE id = 1")
	elapsed := time.Since(start)

	// Must be cancelled by context deadline within ~500ms, NOT waiting 50s session budget!
	if elapsed > 2*time.Second {
		t.Fatalf("query was not interrupted by context deadline: elapsed=%v", elapsed)
	}
	if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrQueryTimeout) && !errors.Is(err, storageengine.ErrLockWaitTimeout)) {
		t.Fatalf("expected deadline/timeout error, got: %v", err)
	}

	sB.Context = nil
	f.must(sA, "ROLLBACK")
	f.engine.CloseSession(sA)
	f.engine.CloseSession(sB)
}

func TestT057EngineCustomLockBudgetsPassToLockManager(t *testing.T) {
	dir := t.TempDir()
	opts := OpenOptions{
		StorageMode:            "mvcc",
		LockMemoryBytes:        128 << 20,
		LockMaxRequestsPerRoot: 500000,
		LockMaxWaiters:         8192,
		LockGraphWorkLimit:     200000,
	}

	e, err := OpenWithOptions(dir, "root", "pw", opts)
	if err != nil {
		t.Fatalf("OpenWithOptions with custom lock budgets failed: %v", err)
	}
	defer e.Close()

	lm := engineLockManager(e.Backend)
	if lm == nil {
		t.Fatal("no lock manager on backend")
	}

	ledger := lm.Ledger()
	if ledger == nil {
		t.Fatal("no ledger on lock manager")
	}

	// Verify custom limits passed down
	limits := ledger.Limits()
	if limits.MemoryBytes != 128<<20 {
		t.Fatalf("expected MemoryBytes %d, got %d", 128<<20, limits.MemoryBytes)
	}
	if limits.RootRequests != 500000 {
		t.Fatalf("expected RootRequests 500000, got %d", limits.RootRequests)
	}
	if limits.Waiters != 8192 {
		t.Fatalf("expected Waiters 8192, got %d", limits.Waiters)
	}
	if limits.GraphWork != 200000 {
		t.Fatalf("expected GraphWork 200000, got %d", limits.GraphWork)
	}
}
