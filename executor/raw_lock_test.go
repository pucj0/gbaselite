package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/storageengine"
)

// raw_lock_test.go is the SQL/raw half of T019: the two write entry points must exclude each other
// on one resource, at the level of a real engine with real sessions.
//
// The component the SQL statement locks is captured from the statement itself instead of being
// recomputed here. That is deliberate: the test must not restate the key encoding, or it would only
// prove that the test and the executor agree. Capturing the component means the raw transaction
// writes the byte-identical namespace and key the SQL statement protected, so a bypass would show
// up as the statement proceeding while the raw writer holds it.
//
// The SQL side is observed with the neutral `ShouldWait` barrier the DML lock tests already use,
// which is signalled while the conflict is decided — i.e. before the request can be released — so
// "the statement is queued behind the raw writer" is a fact and not a delay. The raw side has no
// such neutral hook (its wait is inside the storage layer), so that direction asserts the absence of
// completion and then the completion after the holder ends; its deterministic proof, with the wait
// observed through an injected clock, lives in mvcc/raw_locking_test.go.

// rawLockRowComponent captures the primary-key record component one SQL UPDATE of the row locks.
func rawLockRowComponent(t *testing.T, e *Engine, session *Session, update string) storageengine.LockComponent {
	t.Helper()
	var captured []storageengine.LockComponent
	previous := dmlLockRequestHook
	dmlLockRequestHook = func(request storageengine.LockRequest) storageengine.LockRequest {
		captured = append(captured, request.Components...)
		return request
	}
	defer func() { dmlLockRequestHook = previous }()

	if _, err := e.Execute(session, update); err != nil {
		t.Fatalf("%s: %v", update, err)
	}
	for _, component := range captured {
		if component.Kind == storageengine.LockComponentRecord && component.IndexID == "" && len(component.Key) > 0 {
			return component
		}
	}
	t.Fatalf("the statement locked no primary-key record: %+v", captured)
	return storageengine.LockComponent{}
}

// TestB02SQLUpdateWaitsForARawRecordLock proves a raw writer's protection is not bypassed by SQL.
//
// A raw transaction holds the exact record the statement would lock. The statement must therefore
// queue behind it, which the barrier observes, and it must finish only once the raw holder ends.
func TestB02SQLUpdateWaitsForARawRecordLock(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	setup := &Session{}
	for _, sql := range []string{
		"CREATE DATABASE test",
		"USE test",
		"CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(10,1)",
	} {
		if _, err := e.Execute(setup, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// Capture the component from a successful statement, so the raw write addresses exactly what SQL
	// protects rather than a key this test recomputed.
	probe := &Session{CurrentDatabase: "test"}
	component := rawLockRowComponent(t, e, probe, "UPDATE items SET v=2 WHERE id=10")

	// The raw writer takes the record lock of that component and stays open.
	rawTx, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := rawTx.Put(component.Space, component.Key, []byte("raw-holder")); err != nil {
		t.Fatalf("the raw write: %v", err)
	}

	updater := &Session{CurrentDatabase: "test"}
	barrier := newDMLWaitBarrier()
	barrier.arm(t)
	outcome := make(chan error, 1)
	go func() {
		_, err := e.Execute(updater, "UPDATE items SET v=3 WHERE id=10")
		outcome <- err
	}()
	barrier.wait(t, "the SQL UPDATE behind a raw record lock")

	// The statement is queued; ending the raw holder is the only thing that may release it.
	if err := rawTx.Rollback(); err != nil {
		t.Fatalf("raw rollback: %v", err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the SQL UPDATE failed after the raw holder ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the SQL UPDATE was never woken by the raw holder ending")
	}
}

// TestB02RawWriteWaitsForASQLRecordLock proves the other direction: SQL's record X is not bypassed
// by a raw write.
//
// The window before the commit only asserts that the raw write has not completed while the holder is
// open; what makes the case meaningful is that it completes once the holder commits, which is the
// release path and not a timeout.
func TestB02RawWriteWaitsForASQLRecordLock(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "pw", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	setup := &Session{}
	for _, sql := range []string{
		"CREATE DATABASE test",
		"USE test",
		"CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(10,1)",
	} {
		if _, err := e.Execute(setup, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	probe := &Session{CurrentDatabase: "test"}
	component := rawLockRowComponent(t, e, probe, "UPDATE items SET v=2 WHERE id=10")

	// The SQL session holds the row X and does not commit yet.
	holder := &Session{CurrentDatabase: "test"}
	if _, err := e.Execute(holder, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(holder, "UPDATE items SET v=4 WHERE id=10"); err != nil {
		t.Fatal(err)
	}

	rawTx, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	outcome := make(chan error, 1)
	go func() { outcome <- rawTx.Put(component.Space, component.Key, []byte("raw-waiter")) }()

	select {
	case err := <-outcome:
		t.Fatalf("the raw write returned (%v) while the SQL holder was still open", err)
	case <-time.After(250 * time.Millisecond):
	}

	if _, err := e.Execute(holder, "COMMIT"); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the raw write failed after the holder committed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the raw write was never woken by the SQL commit")
	}
	// The write is staged but the transaction began before the commit, so publishing it must meet
	// the ordinary optimistic conflict: the lock orders the writers, it does not replace the check.
	if _, err := rawTx.Commit(context.Background()); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the raw transaction committed over a newer version: err = %v, want ErrConflict", err)
	}
}
