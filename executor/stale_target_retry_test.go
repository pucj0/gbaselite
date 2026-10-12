package executor

import (
	"errors"
	"testing"

	"gbaselite/storageengine"
)

// stale_target_retry_test.go is the P1 suite: a statement that finds its locked target replaced by a
// newer committed version must end the *transaction*, not merely the statement.
//
// The defect it was written against is measurably worse than either reading of the contract. Before
// the fix, the statement returned the conflict whose text is literally "row version conflict; retry
// transaction" — and then the transaction stayed committable, so the very write the caller had been
// told to abandon was published by the next COMMIT. Whichever semantics a reader prefers, an error
// that says "retry the transaction" while the transaction still publishes is self-contradictory.
//
// The contract is FR-017/FR-018 ("锁后发现过期目标可早期冲突并要求 root 重试") together with T037's
// acceptance clause "早冲突结束root". The transaction is a unit of work: its other statements were
// computed from the same snapshot that has just been proven too old for a row this transaction must
// write, so publishing them would commit a partial unit. The root is therefore ended — rolled back so
// every lock it held is released immediately, and retained so it cannot be used again — exactly as the
// deadlock victim path already does, while reporting a retryable conflict rather than a deadlock.
//
// The last three tests are the negative half, and they are as important as the positive one: a lock
// wait timeout is a statement-level bound, a user cancellation is the caller's own decision, and a
// deadlock is a different cause with a different lifecycle. None of them may be swept into this path.

// p1Fixture is one table with three rows, plus the two sessions the cases share.
type p1Fixture struct {
	f    *t035Fixture
	must func(*Session, string) *Result
}

func newP1Fixture(t *testing.T) *p1Fixture {
	t.Helper()
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1,10),(2,20),(3,30)")
	return &p1Fixture{f: f, must: f.must}
}

// TestP1StaleTargetConflictEndsTheTransactionNotTheStatement walks the reported sequence and pins
// every observable consequence.
func TestP1StaleTargetConflictEndsTheTransactionNotTheStatement(t *testing.T) {
	p := newP1Fixture(t)
	a := t034Session(t, p.f, 20)
	b := t034Session(t, p.f, 20)

	// B stages a write to a row nobody else touches, so the transaction has real work to lose.
	p.must(b, "BEGIN")
	p.must(b, "UPDATE items SET v=200 WHERE id=2")

	// A holds the row B is about to want.
	p.must(a, "BEGIN")
	p.must(a, "UPDATE items SET v=11 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := p.f.engine.Execute(b, "UPDATE items SET v=111 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "B's UPDATE of id=1")
	select {
	case err := <-done:
		t.Fatalf("B's UPDATE of id=1 did not wait: %v", err)
	default:
	}

	p.must(a, "COMMIT")

	// The statement's candidate describes the version B read, which A has replaced.
	if err := awaitStatement(t, done, "B's UPDATE of id=1"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("B's UPDATE of id=1 = %v, want the retryable ErrConflict", err)
	}

	// The transaction is now unusable in every direction: reading, writing and committing would each
	// be an opportunity to publish the work that was abandoned with the stale candidate.
	if _, err := p.f.engine.Execute(b, "SELECT v FROM items WHERE id=2"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("a read on the abandoned transaction = %v, want the retryable ErrConflict", err)
	}
	if _, err := p.f.engine.Execute(b, "UPDATE items SET v=300 WHERE id=3"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("a write on the abandoned transaction = %v, want the retryable ErrConflict", err)
	}
	if _, err := p.f.engine.Execute(b, "SAVEPOINT later"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("a savepoint on the abandoned transaction = %v, want the retryable ErrConflict", err)
	}
	if _, err := p.f.engine.Execute(b, "COMMIT"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("COMMIT on the abandoned transaction = %v, want the retryable ErrConflict", err)
	}

	// Nothing of B's was published, and A's own commit stands.
	if got := t035Rows(t, p.must(a, "SELECT id,v FROM items ORDER BY id")); got != "1,11;2,20;3,30;" {
		t.Fatalf("rows = %q, want only the holder's write published", got)
	}

	// ROLLBACK is the statement that completes the retry the caller was asked for, and it leaves the
	// session usable for the retry itself.
	p.must(b, "ROLLBACK")
	if got := t035Rows(t, p.must(b, "SELECT v FROM items WHERE id=1")); got != "11;" {
		t.Fatalf("the session was not usable after ROLLBACK: %q", got)
	}
}

// TestP1TheRetriedTransactionSucceeds proves the refusal is a retry instruction and not a dead end:
// after ROLLBACK the same work, re-read on a fresh snapshot, commits.
func TestP1TheRetriedTransactionSucceeds(t *testing.T) {
	p := newP1Fixture(t)
	a := t034Session(t, p.f, 20)
	b := t034Session(t, p.f, 20)

	p.must(b, "BEGIN")
	p.must(b, "UPDATE items SET v=200 WHERE id=2")
	p.must(a, "BEGIN")
	p.must(a, "UPDATE items SET v=11 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := p.f.engine.Execute(b, "UPDATE items SET v=111 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "B's UPDATE of id=1")
	p.must(a, "COMMIT")
	if err := awaitStatement(t, done, "B's UPDATE of id=1"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("B's UPDATE of id=1 = %v, want the retryable ErrConflict", err)
	}
	if _, err := p.f.engine.Execute(b, "COMMIT"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("COMMIT = %v, want the retryable ErrConflict", err)
	}
	// The abandoned work is discarded, not silently kept for the retry: the retry re-reads.
	p.must(b, "ROLLBACK")
	if got := t035Rows(t, p.must(b, "SELECT v FROM items WHERE id=2")); got != "20;" {
		t.Fatalf("the abandoned write was kept: id=2 holds %q", got)
	}

	// The retry, on a snapshot that now includes A's commit.
	p.must(b, "BEGIN")
	p.must(b, "UPDATE items SET v=200 WHERE id=2")
	p.must(b, "UPDATE items SET v=111 WHERE id=1")
	p.must(b, "COMMIT")
	if got := t035Rows(t, p.must(b, "SELECT id,v FROM items ORDER BY id")); got != "1,111;2,200;3,30;" {
		t.Fatalf("rows after the retry = %q", got)
	}
}

// TestP1AutocommitStaleTargetLeavesTheSessionUsable is the boundary that keeps the fix honest.
//
// In autocommit the statement's own root is the unit that failed, so there is no session transaction
// to end. Marking the session anyway would refuse the caller's *next*, unrelated statement — turning a
// one-statement conflict into a broken connection. The same wait-then-wake sequence is used, minus the
// BEGIN, so the only difference between this case and the explicit one is which root failed.
func TestP1AutocommitStaleTargetLeavesTheSessionUsable(t *testing.T) {
	p := newP1Fixture(t)
	a := t034Session(t, p.f, 20)
	b := t034Session(t, p.f, 20)

	p.must(a, "BEGIN")
	p.must(a, "UPDATE items SET v=11 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	// No BEGIN: this statement owns its root, and that root is what must be discarded.
	go func() {
		_, err := p.f.engine.Execute(b, "UPDATE items SET v=111 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "the autocommit UPDATE of id=1")
	p.must(a, "COMMIT")

	if err := awaitStatement(t, done, "the autocommit UPDATE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the autocommit UPDATE = %v, want the retryable ErrConflict", err)
	}
	// The session is untouched: the next statement is a fresh root and sees A's commit.
	if got := t035Rows(t, p.must(b, "SELECT v FROM items WHERE id=1")); got != "11;" {
		t.Fatalf("the session was left unusable after an autocommit conflict: %q", got)
	}
	p.must(b, "UPDATE items SET v=12 WHERE id=1")
	if got := t035Rows(t, p.must(b, "SELECT v FROM items WHERE id=1")); got != "12;" {
		t.Fatalf("the autocommit write did not take: %q", got)
	}
}

// TestP1AStaleTargetInsideASavepointEndsTheWholeTransaction covers the savepoint layer.
//
// A savepoint is a layer *inside* the transaction, and the conflict is a statement about the
// transaction's snapshot, which every layer shares. So the whole root ends — all layers discarded —
// and neither ROLLBACK TO nor COMMIT can be used to keep the part of the work that came before the
// savepoint.
func TestP1AStaleTargetInsideASavepointEndsTheWholeTransaction(t *testing.T) {
	p := newP1Fixture(t)
	a := t034Session(t, p.f, 20)
	b := t034Session(t, p.f, 20)

	// Work staged before the savepoint, which must not survive.
	p.must(b, "BEGIN")
	p.must(b, "UPDATE items SET v=200 WHERE id=2")
	p.must(b, "SAVEPOINT s1")

	p.must(a, "BEGIN")
	p.must(a, "UPDATE items SET v=11 WHERE id=1")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := p.f.engine.Execute(b, "UPDATE items SET v=111 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "B's UPDATE of id=1")
	p.must(a, "COMMIT")
	if err := awaitStatement(t, done, "B's UPDATE of id=1"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("B's UPDATE of id=1 = %v, want the retryable ErrConflict", err)
	}

	// Every layer is gone, so the savepoint cannot be used to salvage the earlier write.
	if _, err := p.f.engine.Execute(b, "ROLLBACK TO SAVEPOINT s1"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("ROLLBACK TO SAVEPOINT = %v, want the retryable ErrConflict", err)
	}
	if _, err := p.f.engine.Execute(b, "RELEASE SAVEPOINT s1"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("RELEASE SAVEPOINT = %v, want the retryable ErrConflict", err)
	}
	if _, err := p.f.engine.Execute(b, "COMMIT"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("COMMIT = %v, want the retryable ErrConflict", err)
	}
	p.must(b, "ROLLBACK")
	if got := t035Rows(t, p.must(a, "SELECT id,v FROM items ORDER BY id")); got != "1,11;2,20;3,30;" {
		t.Fatalf("rows = %q, want the pre-savepoint write discarded too", got)
	}
}

// TestP1ALockWaitTimeoutStillOnlyFailsTheStatement is the first negative case.
//
// A lock wait that runs out its bound says nothing about the transaction's snapshot: the row it wanted
// may be released a moment later with the version the snapshot already read. Treating it as a reason to
// end the root would turn a transient wait into a lost unit of work, so the transaction must stay
// usable and its earlier write must still be committable.
func TestP1ALockWaitTimeoutStillOnlyFailsTheStatement(t *testing.T) {
	p := newP1Fixture(t)
	holder := t034Session(t, p.f, 20)
	waiter := t034Session(t, p.f, 1) // a one-second wait budget

	p.must(holder, "BEGIN")
	p.must(holder, "UPDATE items SET v=11 WHERE id=1")

	p.must(waiter, "BEGIN")
	p.must(waiter, "UPDATE items SET v=200 WHERE id=2")

	// The holder never ends, so this statement runs out its own bound.
	if _, err := p.f.engine.Execute(waiter, "UPDATE items SET v=111 WHERE id=1"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("the bounded wait = %v, want ErrLockWaitTimeout", err)
	}
	// The failure is the statement's. Nothing else about the transaction changed: it still reads, and
	// the read shows its own staged value (read-your-writes), which is the evidence that the write it
	// made before the wait is intact.
	if got := t035Rows(t, p.must(waiter, "SELECT v FROM items WHERE id=2")); got != "200;" {
		t.Fatalf("the transaction lost its own staged write after a lock wait timeout: %q", got)
	}
	p.must(waiter, "COMMIT")
	// The holder is inside its own transaction, so its snapshot predates the waiter's commit: the
	// published value has to be read by a session that has no older snapshot.
	p.must(holder, "ROLLBACK")
	observer := t034Session(t, p.f, 20)
	if got := t035Rows(t, p.must(observer, "SELECT v FROM items WHERE id=2")); got != "200;" {
		t.Fatalf("the write staged before the timeout was lost: id=2 = %q", got)
	}
}

// TestP1TheStaleTargetRefusalIsNotADeadlock is the second negative case, and the one the round asks
// about most directly: the two causes must not be conflated.
//
// Both are retryable and both map to the MySQL deadlock code, so the distinction is only visible at the
// error level — which is exactly where a caller decides what to do, and where diagnostics report why a
// transaction ended. The victim flow itself (victim selection, the victim's own error, its refusal and
// its ROLLBACK) is covered by executor/locking_deadlock_test.go and is unchanged by this fix; what is
// added here is that a stale target does not masquerade as one.
func TestP1TheStaleTargetRefusalIsNotADeadlock(t *testing.T) {
	p := newP1Fixture(t)
	a := t034Session(t, p.f, 20)
	b := t034Session(t, p.f, 20)

	p.must(a, "BEGIN")
	p.must(a, "UPDATE items SET v=11 WHERE id=1")
	p.must(b, "BEGIN")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := p.f.engine.Execute(b, "UPDATE items SET v=111 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "B's UPDATE of id=1")
	p.must(a, "COMMIT")

	statementErr := <-done
	commitErr := func() error { _, err := p.f.engine.Execute(b, "COMMIT"); return err }()
	for name, err := range map[string]error{"statement": statementErr, "commit": commitErr} {
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("the %s refusal = %v, want the retryable ErrConflict", name, err)
		}
		if errors.Is(err, storageengine.ErrDeadlock) {
			t.Fatalf("the %s refusal reported a deadlock: %v — a stale target is not a chosen victim", name, err)
		}
		// The message a client sees must stay the conflict's own text.
		if err.Error() != storageengine.ErrConflict.Error() {
			t.Fatalf("the %s refusal message = %q, want the conflict's existing text %q", name, err.Error(), storageengine.ErrConflict.Error())
		}
	}
	p.must(b, "ROLLBACK")
}

// TestP1RepeatedOperationsAreRefusedWithoutLosingConflict verifies that once a transaction is marked
// with a stale target conflict, subsequent operations (SELECT, INSERT, UPDATE, DELETE, SAVEPOINT,
// COMMIT) are repeatedly refused and the retryConflict marker is not cleared prematurely.
func TestP1RepeatedOperationsAreRefusedWithoutLosingConflict(t *testing.T) {
	p := newP1Fixture(t)
	a := t034Session(t, p.f, 20)
	b := t034Session(t, p.f, 20)

	p.must(a, "BEGIN")
	p.must(a, "UPDATE items SET v=11 WHERE id=1")

	p.must(b, "BEGIN")
	p.must(b, "UPDATE items SET v=200 WHERE id=2")

	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := p.f.engine.Execute(b, "UPDATE items SET v=111 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "B's UPDATE of id=1")
	p.must(a, "COMMIT")

	if err := awaitStatement(t, done, "B's UPDATE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}

	// Repeated operations must all be refused without losing the conflict marker:
	stmts := []string{
		"SELECT * FROM items WHERE id=2",
		"INSERT INTO items VALUES(4, 40)",
		"UPDATE items SET v=99 WHERE id=2",
		"DELETE FROM items WHERE id=2",
		"SAVEPOINT sp1",
		"ROLLBACK TO SAVEPOINT sp1",
		"RELEASE SAVEPOINT sp1",
		"COMMIT",
	}

	for _, sql := range stmts {
		if _, err := p.f.engine.Execute(b, sql); !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("statement %q was not refused with ErrConflict, got %v", sql, err)
		}
	}

	// Session's retryConflict marker is preserved until ROLLBACK
	p.must(b, "ROLLBACK")

	// After ROLLBACK, the session is clean and operations succeed
	res := p.must(b, "SELECT id, v FROM items ORDER BY id")
	if got := t035Rows(t, res); got != "1,11;2,20;3,30;" {
		t.Fatalf("unexpected data after rollback: %q", got)
	}
}

// TestP1BeginResetsAbortedSessionState verifies that when a transaction has been aborted by a stale
// target conflict, a subsequent BEGIN resets the aborted session and starts a fresh, usable transaction.
func TestP1BeginResetsAbortedSessionState(t *testing.T) {
	p := newP1Fixture(t)
	a := t034Session(t, p.f, 20)
	b := t034Session(t, p.f, 20)

	p.must(a, "BEGIN")
	p.must(a, "UPDATE items SET v=11 WHERE id=1")

	p.must(b, "BEGIN")
	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	done := make(chan error, 1)
	go func() {
		_, err := p.f.engine.Execute(b, "UPDATE items SET v=111 WHERE id=1")
		done <- err
	}()
	barrier.wait(t, "B's UPDATE")
	p.must(a, "COMMIT")

	if err := awaitStatement(t, done, "B's UPDATE"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}

	// Directly call BEGIN without ROLLBACK:
	p.must(b, "BEGIN")

	// Fresh transaction should be fully usable and succeed
	p.must(b, "UPDATE items SET v=222 WHERE id=2")
	p.must(b, "COMMIT")

	res := p.must(a, "SELECT id, v FROM items WHERE id=2")
	if got := t035Rows(t, res); got != "2,222;" {
		t.Fatalf("expected id=2 to be 222 after new transaction commit, got %q", got)
	}
}
