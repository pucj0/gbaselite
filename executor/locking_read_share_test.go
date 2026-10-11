package executor

import (
	"errors"
	"testing"

	"gbaselite/storageengine"
)

// locking_read_share_test.go is the FOR SHARE half of the locking-read surface (P1) and the
// absent-key insertion-position half (P2a).
//
// Both were found by review rather than by the first test round, so the tests here are written to fail
// against the previous implementation: a shared range read declared a shared table intention while the
// bundle check demanded an exclusive one for its interval, and an absent point key fell back to
// protecting everything at or after the key instead of the gap around it.

// t034ShareFixture seeds the table the shared-range tests use.
func t034ShareFixture(t *testing.T, seed ...string) *t035Fixture {
	t.Helper()
	return newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", seed...)
}

// TestP1ForShareRangeReadIsAccepted is the direct reproduction of P1: a shared range read has to be
// admitted at all. It declares IS for its table intention (as the contract requires of a shared read)
// and needs the bundle check to accept a shared intention for its interval.
func TestP1ForShareRangeReadIsAccepted(t *testing.T) {
	f := t034ShareFixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)", "INSERT INTO items VALUES(30,1)")
	reader := t034Session(t, f, 5)

	f.must(reader, "BEGIN")
	for _, query := range []string{
		"SELECT id FROM items WHERE id BETWEEN 10 AND 20 FOR SHARE",
		"SELECT id FROM items FOR SHARE",
		"SELECT id FROM items WHERE id BETWEEN 10 AND 20 LOCK IN SHARE MODE",
	} {
		result, err := f.engine.Execute(reader, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if len(result.Rows) == 0 {
			t.Fatalf("%s returned no row", query)
		}
	}
	// An absent key under FOR SHARE takes the gap around it, which is the same bundle shape with a
	// shared intention and has to be admitted for the same reason.
	if got := t035Rows(t, f.must(reader, "SELECT id FROM items WHERE id=42 FOR SHARE")); got != "" {
		t.Fatalf("a shared read of an absent key returned %q, want no row", got)
	}
	f.must(reader, "ROLLBACK")
}

// TestP1TwoForShareRangeReadsCoexist proves the two shared range reads do not exclude each other: a
// gap taken for a shared read is compatible with another shared read's gap.
func TestP1TwoForShareRangeReadsCoexist(t *testing.T) {
	f := t034ShareFixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)", "INSERT INTO items VALUES(30,1)")
	first := t034Session(t, f, 5)
	second := t034Session(t, f, 5)

	f.must(first, "BEGIN")
	f.must(first, "SELECT id FROM items WHERE id BETWEEN 10 AND 20 FOR SHARE")
	// The second shared range read must complete rather than wait for the first.
	f.must(second, "BEGIN")
	if got := t035Rows(t, f.must(second, "SELECT id FROM items WHERE id BETWEEN 10 AND 20 FOR SHARE")); got != "10;20;" {
		t.Fatalf("the second shared range read returned %q, want both rows", got)
	}
	f.must(first, "ROLLBACK")
	f.must(second, "ROLLBACK")
}

// TestP1ForShareRangeReadConflictsWithWriters proves the shared range read excludes the writers it must
// exclude while admitting the readers it must admit.
func TestP1ForShareRangeReadConflictsWithWriters(t *testing.T) {
	f := t034ShareFixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(20,1)", "INSERT INTO items VALUES(30,1)")
	reader := t034Session(t, f, 5)
	inserter := t034Session(t, f, 1)
	updater := t034Session(t, f, 1)

	f.must(reader, "BEGIN")
	f.must(reader, "SELECT id FROM items WHERE id BETWEEN 10 AND 20 FOR SHARE")

	// An insert into the protected interval waits, and so does a write to a row inside it.
	if _, err := f.engine.Execute(inserter, "INSERT INTO items VALUES(15,1)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("an insert inside a shared range read returned %v, want ErrLockWaitTimeout", err)
	}
	if _, err := f.engine.Execute(updater, "UPDATE items SET v=2 WHERE id=20"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("a write inside a shared range read returned %v, want ErrLockWaitTimeout", err)
	}
	// A write outside the interval is not affected.
	f.must(updater, "UPDATE items SET v=2 WHERE id=30")
	f.must(reader, "ROLLBACK")
}

// TestP2AbsentKeyGapDoesNotBlockDistantInserts is the direct reproduction of P2: with 10 and 100 in the
// table, the gap taken for an absent 42 covers the neighbourhood of 42 — not everything above it.
func TestP2AbsentKeyGapDoesNotBlockDistantInserts(t *testing.T) {
	f := t034ShareFixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(100,1)")
	holder := t034Session(t, f, 5)
	blocked := t034Session(t, f, 1)
	distant := t034Session(t, f, 5)

	f.must(holder, "BEGIN")
	if got := t035Rows(t, f.must(holder, "SELECT id FROM items WHERE id=42 FOR UPDATE")); got != "" {
		t.Fatalf("the locking read of an absent key returned %q, want no row", got)
	}

	// The insertion position itself is protected.
	if _, err := f.engine.Execute(blocked, "INSERT INTO items VALUES(42,1)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("an insert at the protected position returned %v, want ErrLockWaitTimeout", err)
	}
	// So is the gap around it: both neighbours of the absent key bound it, so a key between them is
	// inside the same gap (this is also what a next-key gap lock protects).
	if _, err := f.engine.Execute(blocked, "INSERT INTO items VALUES(50,1)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("an insert inside the protected gap returned %v, want ErrLockWaitTimeout", err)
	}
	// But a key beyond the gap's upper bound is not: the protection of 42 must not reach it.
	f.must(distant, "BEGIN")
	f.must(distant, "INSERT INTO items VALUES(150,1)")
	f.must(distant, "COMMIT")

	f.must(holder, "COMMIT")
	f.must(blocked, "INSERT INTO items VALUES(42,1)")
	if got := t035Rows(t, f.must(distant, "SELECT id FROM items WHERE id IN (42,150) ORDER BY id")); got != "42;150;" {
		t.Fatalf("the committed rows read %q, want 42 and 150", got)
	}
}

// TestP2AbsentKeyGapBoundaries covers the edges of the same rule: below the minimum, above the maximum,
// between two neighbours, and in an empty table.
func TestP2AbsentKeyGapBoundaries(t *testing.T) {
	t.Run("below the minimum key", func(t *testing.T) {
		f := t034ShareFixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(100,1)")
		holder := t034Session(t, f, 5)
		blocked := t034Session(t, f, 1)
		distant := t034Session(t, f, 5)

		f.must(holder, "BEGIN")
		f.must(holder, "SELECT id FROM items WHERE id=5 FOR UPDATE")
		// The gap is (-inf, 10), so 7 is inside it and 150 is not.
		if _, err := f.engine.Execute(blocked, "INSERT INTO items VALUES(7,1)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("an insert below the minimum inside the gap returned %v, want ErrLockWaitTimeout", err)
		}
		f.must(distant, "BEGIN")
		f.must(distant, "INSERT INTO items VALUES(150,1)")
		f.must(distant, "COMMIT")
		f.must(holder, "ROLLBACK")
	})

	t.Run("above the maximum key", func(t *testing.T) {
		f := t034ShareFixture(t, "INSERT INTO items VALUES(10,1)", "INSERT INTO items VALUES(100,1)")
		holder := t034Session(t, f, 5)
		blocked := t034Session(t, f, 1)

		f.must(holder, "BEGIN")
		f.must(holder, "SELECT id FROM items WHERE id=500 FOR UPDATE")
		// The gap is (100, +inf): any key above the maximum is inside it, which is the conservative and
		// contract-consistent shape (the insertion position is only bounded above by the end of the index).
		if _, err := f.engine.Execute(blocked, "INSERT INTO items VALUES(600,1)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("an insert above the maximum returned %v, want ErrLockWaitTimeout", err)
		}
		f.must(holder, "ROLLBACK")
	})

	t.Run("empty table", func(t *testing.T) {
		f := t034ShareFixture(t)
		holder := t034Session(t, f, 5)
		blocked := t034Session(t, f, 1)

		f.must(holder, "BEGIN")
		if got := t035Rows(t, f.must(holder, "SELECT id FROM items WHERE id=42 FOR UPDATE")); got != "" {
			t.Fatalf("the absent-key read of an empty table returned %q", got)
		}
		// With no neighbouring key the gap is the whole index, so every insertion position is inside it.
		if _, err := f.engine.Execute(blocked, "INSERT INTO items VALUES(42,1)"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("an insert into an empty table's gap returned %v, want ErrLockWaitTimeout", err)
		}
		f.must(holder, "ROLLBACK")
		// After the rollback the gap is gone and the insert succeeds.
		f.must(blocked, "INSERT INTO items VALUES(42,1)")
	})
}
