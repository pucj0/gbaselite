package executor

import (
	"errors"
	"testing"
	"time"

	"gbaselite/storage"
	"gbaselite/storageengine"
)

// locking_modify_test.go is the T037 suite for locked DML targets.
//
// The defect it covers: a statement selects its candidates from its parent snapshot (FR-017), then
// writes them through the statement child. Nothing re-checked the target between the two, so a
// statement that waited on a row the holder was rewriting went on to write over the holder's committed
// version. The row's *bytes* are the wrong question — a row changed and changed back is byte-identical
// while its version, and every index entry that named it, were replaced — so the check is made on the
// version the storage layer recorded (`CurrentObservation.ChangeTS`), taken from a read performed
// *under* the row's claim.
//
// Every test drives real SQL on two sessions and establishes "the waiter is queued before the holder
// ends" with the neutral `ShouldWait` barrier, never with a sleep.
//
// The last group is deliberately a compatibility suite: dedup, first-match, LIMIT, SET evaluation
// order and Halloween protection must all behave exactly as they did before any target was verified.
// Protecting a row is not allowed to change which row a statement computes.

// staleUpdateFixture is one row held by a session that rewrites it while a second session waits.
type staleFixture struct {
	f      *t035Fixture
	holder *Session
	waiter *Session
}

// newStaleFixture opens the shared two-session shape: the holder takes the row and rewrites it, the
// waiter's statement queues behind it, and the holder commits.
func newStaleFixture(t *testing.T, schema string, seed string) *staleFixture {
	t.Helper()
	f := newT035Fixture(t, schema, seed)
	holder := t034Session(t, f, 20)
	waiter := t034Session(t, f, 20)
	f.must(holder, "BEGIN")
	f.must(waiter, "BEGIN")
	return &staleFixture{f: f, holder: holder, waiter: waiter}
}

// concurrently runs one statement on the waiter session and returns a channel carrying its error.
func (s *staleFixture) concurrently(statement string) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := s.f.engine.Execute(s.waiter, statement)
		done <- err
	}()
	return done
}

// TestT037StaleUpdateTargetConflictsEarly is the core case, and it pins requirement 5 as well: the
// statement must not recompute its SET on the newer row. The waiter computes `v + 100` from the
// version it read (v=1), so a rebase would leave 105 and its own value would leave 101. Neither may
// appear: the row must still hold the holder's committed 5.
func TestT037StaleUpdateTargetConflictsEarly(t *testing.T) {
	s := newStaleFixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", "INSERT INTO items VALUES(1,1)")
	// The waiter reads v=1 into its snapshot before it waits.
	s.f.must(s.waiter, "SELECT v FROM items WHERE id=1")
	s.f.must(s.holder, "UPDATE items SET v=5 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := s.concurrently("UPDATE items SET v = v + 100 WHERE id=1")
	barrier.wait(t, "the waiter's UPDATE of the held row")
	s.f.must(s.holder, "COMMIT")

	if err := awaitStatement(t, done, "the woken UPDATE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the woken UPDATE = %v, want the retryable ErrConflict", err)
	}
	s.f.must(s.waiter, "ROLLBACK")
	if got := t035Rows(t, s.f.must(s.holder, "SELECT v FROM items WHERE id=1")); got != "5;" {
		t.Fatalf("row 1 holds %q, want the holder's committed 5: neither 101 (the waiter's own value) nor 105 (a rebase onto the newer row)", got)
	}
}

// TestT037StaleDeleteTargetConflictsEarly is requirement 9: a DELETE claims the row it removes and
// the index entries that named it, and a row the holder replaced is not deleted on the old candidate.
func TestT037StaleDeleteTargetConflictsEarly(t *testing.T) {
	s := newStaleFixture(t,
		"CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, sku VARCHAR(20), UNIQUE KEY uq_sku(sku))",
		"INSERT INTO items VALUES(1,1,'a')")
	s.f.must(s.holder, "UPDATE items SET v=5 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := s.concurrently("DELETE FROM items WHERE id=1")
	barrier.wait(t, "the waiter's DELETE of the held row")
	s.f.must(s.holder, "COMMIT")

	if err := awaitStatement(t, done, "the woken DELETE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the woken DELETE = %v, want the retryable ErrConflict", err)
	}
	s.f.must(s.waiter, "ROLLBACK")
	if got := t035Rows(t, s.f.must(s.holder, "SELECT id FROM items")); got != "1;" {
		t.Fatalf("the rejected DELETE removed the row: %q", got)
	}
	// The unique value the rejected delete would have freed is still row 1's: the claim covered the
	// index entries, not only the row.
	if _, err := s.f.engine.Execute(s.holder, "INSERT INTO items VALUES(2,2,'a')"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("the rejected DELETE freed the unique value: %v", err)
	}
}

// TestT037PostReadTSTombstoneConflictsEarly covers the tombstone case: the key still exists when the
// waiter wakes, but it is occupied by a *different* version. A byte comparison against the row the
// waiter read cannot see that at all, because the waiter's row is gone.
func TestT037PostReadTSTombstoneConflictsEarly(t *testing.T) {
	s := newStaleFixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", "INSERT INTO items VALUES(1,1)")
	s.f.must(s.holder, "SELECT v FROM items WHERE id=1 FOR UPDATE")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := s.concurrently("UPDATE items SET v=99 WHERE id=1")
	barrier.wait(t, "the waiter's UPDATE")

	// The holder removes the row the waiter's candidate describes and installs another one under the
	// same key, inside one transaction so no third session can interleave.
	s.f.must(s.holder, "DELETE FROM items WHERE id=1")
	s.f.must(s.holder, "INSERT INTO items VALUES(1,7)")
	s.f.must(s.holder, "COMMIT")

	if err := awaitStatement(t, done, "the woken UPDATE over a re-inserted key"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the woken UPDATE = %v, want the retryable ErrConflict", err)
	}
	s.f.must(s.waiter, "ROLLBACK")
	if got := t035Rows(t, s.f.must(s.holder, "SELECT v FROM items WHERE id=1")); got != "7;" {
		t.Fatalf("the re-inserted row was overwritten: %q, want 7", got)
	}
}

// TestT037TheCandidateSetStaysTheParentSnapshot is requirements 1 and 2: the parent snapshot decides
// which rows a statement writes, so a row that starts matching the predicate after that snapshot is
// never enumerated — and therefore never written — even though the statement is still running.
func TestT037TheCandidateSetStaysTheParentSnapshot(t *testing.T) {
	s := newStaleFixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", "INSERT INTO items VALUES(1,1)")
	// The holder rewrites the only row the waiter's snapshot will find, so the waiter's candidate is
	// genuinely stale once the holder commits.
	s.f.must(s.holder, "UPDATE items SET v=5 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	// The waiter's snapshot contains row 1 and nothing else, so that is its whole candidate set.
	done := s.concurrently("UPDATE items SET v=99 WHERE v=1")
	barrier.wait(t, "the waiter's UPDATE")

	// A row that matches the predicate appears after the waiter's snapshot.
	s.f.must(s.holder, "INSERT INTO items VALUES(2,1)")
	s.f.must(s.holder, "COMMIT")

	if err := awaitStatement(t, done, "the woken UPDATE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the woken UPDATE = %v, want the retryable ErrConflict", err)
	}
	s.f.must(s.waiter, "ROLLBACK")
	// Row 1 keeps the holder's committed value, and the row that started matching after the snapshot
	// still holds its own: the statement never enumerated it, so it has nothing to say about it.
	if got := t035Rows(t, s.f.must(s.holder, "SELECT id,v FROM items ORDER BY id")); got != "1,5;2,1;" {
		t.Fatalf("rows = %q, want row 1 untouched by the rejected statement and the late row never enumerated", got)
	}
}

// TestT037AConcurrentInsertLeavesAWaitingUpdateAlone is the other half of the same rule: an unrelated
// row committed while the waiter is queued changes nothing the waiter's candidate depends on, so the
// statement must succeed rather than conflict. A conflict here would mean the check was reading the
// table instead of the target.
func TestT037AConcurrentInsertLeavesAWaitingUpdateAlone(t *testing.T) {
	s := newStaleFixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", "INSERT INTO items VALUES(1,1)")
	s.f.must(s.holder, "SELECT v FROM items WHERE id=1 FOR UPDATE")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := s.concurrently("UPDATE items SET v=2 WHERE id=1")
	barrier.wait(t, "the waiter's UPDATE")

	s.f.must(s.holder, "INSERT INTO items VALUES(2,7)")
	s.f.must(s.holder, "COMMIT")

	if err := awaitStatement(t, done, "the woken UPDATE"); err != nil {
		t.Fatalf("the woken UPDATE failed on an unrelated concurrent insert: %v", err)
	}
	s.f.must(s.waiter, "COMMIT")
	if got := t035Rows(t, s.f.must(s.holder, "SELECT id,v FROM items ORDER BY id")); got != "1,2;2,7;" {
		t.Fatalf("rows = %q, want the waiter's update beside the concurrent insert", got)
	}
}

// TestT037JoinUpdateTargetIsVerified proves the joined form goes through the same check: without it,
// UPDATE ... JOIN would be the one statement that rewrites a row without protecting it.
func TestT037JoinUpdateTargetIsVerified(t *testing.T) {
	f := newT035Fixture(t,
		"CREATE TABLE target(id BIGINT PRIMARY KEY, v INT)",
		"CREATE TABLE source(id BIGINT PRIMARY KEY, tid BIGINT, addend INT)",
		"INSERT INTO target VALUES(1,1)",
		"INSERT INTO source VALUES(10,1,10)")
	holder := t034Session(t, f, 20)
	waiter := t034Session(t, f, 20)
	f.must(holder, "BEGIN")
	f.must(waiter, "BEGIN")
	f.must(holder, "UPDATE target SET v=5 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(waiter, "UPDATE target t JOIN source s ON s.tid = t.id SET t.v = t.v + s.addend")
		done <- err
	}()
	barrier.wait(t, "the UPDATE JOIN's target lock")
	f.must(holder, "COMMIT")

	if err := awaitStatement(t, done, "the woken UPDATE JOIN"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the woken UPDATE JOIN = %v, want the retryable ErrConflict", err)
	}
	f.must(waiter, "ROLLBACK")
	if got := t035Rows(t, f.must(holder, "SELECT v FROM target WHERE id=1")); got != "5;" {
		t.Fatalf("the target holds %q, want the holder's committed 5", got)
	}
}

// TestT037MultiTableDeleteTargetIsVerified is the multi-table DELETE half of the same rule.
func TestT037MultiTableDeleteTargetIsVerified(t *testing.T) {
	f := newT035Fixture(t,
		"CREATE TABLE parents(id BIGINT PRIMARY KEY, v INT)",
		"CREATE TABLE children(id BIGINT PRIMARY KEY, parent_id BIGINT, KEY idx_parent(parent_id))",
		"INSERT INTO parents VALUES(1,1),(2,1)",
		"INSERT INTO children VALUES(10,1),(20,2)")
	holder := t034Session(t, f, 20)
	waiter := t034Session(t, f, 20)
	f.must(holder, "BEGIN")
	f.must(waiter, "BEGIN")
	f.must(holder, "UPDATE parents SET v=5 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(waiter, "DELETE p FROM children c JOIN parents p ON c.parent_id = p.id WHERE p.id=1")
		done <- err
	}()
	barrier.wait(t, "the multi-table DELETE's target lock")
	f.must(holder, "COMMIT")

	if err := awaitStatement(t, done, "the woken multi-table DELETE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the woken multi-table DELETE = %v, want the retryable ErrConflict", err)
	}
	f.must(waiter, "ROLLBACK")
	if got := t035Rows(t, f.must(holder, "SELECT id,v FROM parents ORDER BY id")); got != "1,5;2,1;" {
		t.Fatalf("parents = %q, want both rows intact", got)
	}
	if got := t035Rows(t, f.must(holder, "SELECT COUNT(*) FROM children")); got != "2;" {
		t.Fatalf("children = %q, want both rows intact", got)
	}
}

// TestT037PrimaryKeyMoveConflictsWhenTheOldRowChanged is requirement 8's first half: moving a key
// still claims the *old* identity, so a holder that replaced that row is met on it.
func TestT037PrimaryKeyMoveConflictsWhenTheOldRowChanged(t *testing.T) {
	s := newStaleFixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", "INSERT INTO items VALUES(1,1)")
	s.f.must(s.holder, "UPDATE items SET v=5 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := s.concurrently("UPDATE items SET id=2 WHERE id=1")
	barrier.wait(t, "the key-moving UPDATE's old identity")
	s.f.must(s.holder, "COMMIT")

	if err := awaitStatement(t, done, "the key-moving UPDATE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the key-moving UPDATE = %v, want the retryable ErrConflict", err)
	}
	s.f.must(s.waiter, "ROLLBACK")
	if got := t035Rows(t, s.f.must(s.holder, "SELECT id,v FROM items ORDER BY id")); got != "1,5;" {
		t.Fatalf("rows = %q, want the rewritten row intact under its old key", got)
	}
}

// TestT037PrimaryKeyMoveReportsDuplicateWhenTheNewKeyIsTaken is requirement 8's second half. The old
// row here is untouched, so the statement is not stale — what stops it is the *new* identity, which
// the holder took while the waiter was queued. That is the permanent duplicate, not a retryable
// conflict, and a statement that only claimed the old key would have overwritten the holder's row.
func TestT037PrimaryKeyMoveReportsDuplicateWhenTheNewKeyIsTaken(t *testing.T) {
	s := newStaleFixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", "INSERT INTO items VALUES(1,1)")
	s.f.must(s.holder, "SELECT v FROM items WHERE id=1 FOR UPDATE")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := s.concurrently("UPDATE items SET id=2 WHERE id=1")
	barrier.wait(t, "the key-moving UPDATE's old identity")

	// The holder installs the key the waiter is moving to, without touching the old row.
	s.f.must(s.holder, "INSERT INTO items VALUES(2,9)")
	s.f.must(s.holder, "COMMIT")

	if err := awaitStatement(t, done, "the key-moving UPDATE"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("the key-moving UPDATE = %v, want storage.ErrDuplicateKey", err)
	}
	s.f.must(s.waiter, "ROLLBACK")
	if got := t035Rows(t, s.f.must(s.holder, "SELECT id,v FROM items ORDER BY id")); got != "1,1;2,9;" {
		t.Fatalf("rows = %q, want both rows intact: the move must not overwrite the key it moved onto", got)
	}
}

// TestT037AFailedStatementLeavesNoWriteBehind is requirement 10. Row 1 is written before row 2
// conflicts, and the failed statement's child transaction must take that write with it: neither the
// waiter's own transaction nor the committed state may show it.
func TestT037AFailedStatementLeavesNoWriteBehind(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1,1),(2,1)")
	holder := t034Session(t, f, 20)
	waiter := t034Session(t, f, 20)
	f.must(holder, "BEGIN")
	f.must(waiter, "BEGIN")
	// Row 1 is free for the waiter; row 2 is held and about to be rewritten.
	f.must(holder, "UPDATE items SET v=5 WHERE id=2")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(waiter, "UPDATE items SET v=99 WHERE id IN (1,2)")
		done <- err
	}()
	barrier.wait(t, "the multi-row UPDATE")
	f.must(holder, "COMMIT")

	if err := awaitStatement(t, done, "the multi-row UPDATE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the multi-row UPDATE = %v, want the retryable ErrConflict", err)
	}
	// Nothing the statement staged leaked into its own transaction.
	if got := t035Rows(t, f.must(waiter, "SELECT v FROM items WHERE id=1")); got != "1;" {
		t.Fatalf("the failed statement leaked its write into the transaction: %q", got)
	}
	f.must(waiter, "ROLLBACK")
	if got := t035Rows(t, f.must(holder, "SELECT id,v FROM items ORDER BY id")); got != "1,1;2,5;" {
		t.Fatalf("rows = %q, want row 1 untouched by the rolled-back statement", got)
	}
}

// TestT037UpdateJoinKeepsFirstMatchDedupAndLimit is the compatibility half of requirement 6. Target 1
// is matched by two source rows: the *first* match supplies the assignment, dedup makes the target
// count once, and LIMIT counts distinct targets rather than joined rows. Verifying a target must not
// disturb any of that.
func TestT037UpdateJoinKeepsFirstMatchDedupAndLimit(t *testing.T) {
	f := newT035Fixture(t,
		"CREATE TABLE target(id BIGINT PRIMARY KEY, v INT)",
		"CREATE TABLE source(id BIGINT PRIMARY KEY, tid BIGINT, addend INT)",
		"INSERT INTO target VALUES(1,0),(2,0)",
		"INSERT INTO source VALUES(10,1,10),(11,1,20),(12,2,30)")
	session := t034Session(t, f, 20)

	f.must(session, "BEGIN")
	f.must(session, "UPDATE target t JOIN source s ON s.tid = t.id SET t.v = t.v + s.addend LIMIT 1")
	f.must(session, "COMMIT")

	if got := t035Rows(t, f.must(session, "SELECT id,v FROM target ORDER BY id")); got != "1,10;2,0;" {
		t.Fatalf("rows = %q, want the first match's +10 on target 1 and target 2 untouched by the limit", got)
	}
}

// TestT037SetEvaluationOrderIsUnchanged pins requirement 6's SET half: earlier assignments in the same
// statement are visible to later ones, and the statement's own row is the one they evaluate against.
func TestT037SetEvaluationOrderIsUnchanged(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, a INT, b INT)",
		"INSERT INTO items VALUES(1,1,0)")
	session := t034Session(t, f, 20)

	result := f.must(session, "UPDATE items SET a = a + 1, b = a WHERE id=1")
	if result.AffectedRows != 1 {
		t.Fatalf("affected rows = %d, want 1", result.AffectedRows)
	}
	// b reads the *new* a, which is the evaluation order the pipeline has always had.
	if got := t035Rows(t, f.must(session, "SELECT a,b FROM items WHERE id=1")); got != "2,2;" {
		t.Fatalf("row = %q, want b to see the new a", got)
	}
}

// TestT037SelfReferencingUpdateKeepsHalloweenProtection is requirement 7. The predicate reads the
// column the statement writes, over an index on that column, so a scan that visited its own output
// would keep matching rows it had already updated. Exactly the two original matches may be affected.
func TestT037SelfReferencingUpdateKeepsHalloweenProtection(t *testing.T) {
	f := newT035Fixture(t,
		"CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, KEY idx_v(v))",
		"INSERT INTO items VALUES(1,1),(2,1),(3,2)")
	session := t034Session(t, f, 20)

	result := f.must(session, "UPDATE items SET v=2 WHERE v=1")
	if result.AffectedRows != 2 {
		t.Fatalf("affected rows = %d, want exactly the two rows that matched before the statement ran", result.AffectedRows)
	}
	if got := t035Rows(t, f.must(session, "SELECT id,v FROM items ORDER BY id")); got != "1,2;2,2;3,2;" {
		t.Fatalf("rows = %q, want no row visited twice", got)
	}
}

// TestT037UpdateLimitBoundsTheCandidateSet covers the LIMIT shape alongside locking.
func TestT037UpdateLimitBoundsTheCandidateSet(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1,1),(2,1),(3,1)")
	session := t034Session(t, f, 20)

	result := f.must(session, "UPDATE items SET v=2 WHERE v=1 LIMIT 2")
	if result.AffectedRows != 2 {
		t.Fatalf("affected rows = %d, want the limit", result.AffectedRows)
	}
	if got := t035Rows(t, f.must(session, "SELECT COUNT(*) FROM items WHERE v=2")); got != "2;" {
		t.Fatalf("rows = %q, want exactly two updated", got)
	}
}

// TestT037TheRejectedStatementDoesNotHoldTheRowForever proves the failed statement's claim is released
// with the statement scope rather than being kept until the session ends: another writer can take the
// row once the waiter's transaction is rolled back, and the claim does not wait out a timeout.
func TestT037TheRejectedStatementDoesNotHoldTheRowForever(t *testing.T) {
	s := newStaleFixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)", "INSERT INTO items VALUES(1,1)")
	s.f.must(s.holder, "UPDATE items SET v=5 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := s.concurrently("UPDATE items SET v=99 WHERE id=1")
	barrier.wait(t, "the waiter's UPDATE")
	s.f.must(s.holder, "COMMIT")
	if err := awaitStatement(t, done, "the woken UPDATE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the woken UPDATE = %v, want the retryable ErrConflict", err)
	}
	s.f.must(s.waiter, "ROLLBACK")

	// A fresh writer takes the row immediately: the rejected statement left no claim behind.
	third := t034Session(t, s.f, 2)
	started := time.Now()
	s.f.must(third, "UPDATE items SET v=6 WHERE id=1")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the row was still held %v after the rejected statement ended", elapsed)
	}
	if got := t035Rows(t, s.f.must(third, "SELECT v FROM items WHERE id=1")); got != "6;" {
		t.Fatalf("row 1 holds %q, want the fresh writer's value", got)
	}
}
