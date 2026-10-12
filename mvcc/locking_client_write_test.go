package mvcc

import (
	"context"
	"testing"
	"time"

	"gbaselite/lockmanager"
)

// locking_client_write_test.go is the T045 suite for the raw client's write entry points.
//
// The raw client is a second way into the same storage, so the question this file answers is not
// "does the raw path lock" — that is `raw_locking_test.go` — but "is the raw path's *client contract*
// the same one the SQL path has": one resource per physical row, a wait that ends with the root that
// caused it, no residue once that root is gone, and a replay path that is deliberately outside user
// lock waits because it is not a user writing.
//
// One limit is stated rather than tested away: `Put` and `Delete` carry no context, so a raw wait
// cannot be cancelled by one. It is bounded by the manager (the session wait budget) and released by
// the manager's lifecycle. Inventing a context inside `AcquireRawWriteLock` would silently drop a
// caller deadline it was never given, so the absence is the design, and the SQL side — which does
// carry a statement context — is where context cancellation is tested (`executor/locking_modify_test.go`
// and `executor/locking_read_lifecycle_test.go`).

// sRef is the shared record lock a SQL `SELECT ... FOR SHARE` takes on one key.
func sRef(space string, key []byte) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind:  lockmanager.KindRecord,
		Mode:  lockmanager.ModeS,
		Space: space,
		Key:   key,
	}}
}

// TestT045ARawDeleteWaitsForAnotherRootsSharedRead is the raw client meeting a reader.
//
// A shared read is compatible with another shared read and incompatible with a write, so the first
// half of the assertion is that two readers never serialise and the second is that the raw writer is
// the one that waits.
func TestT045ARawDeleteWaitsForAnotherRootsSharedRead(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	seed := f.begin()
	if err := seed.Put(rawSpace, rawKey, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Two roots take the shared record lock of the same key: neither may block the other.
	reader := f.begin()
	if _, op, _, err := reader.AcquireLock(ctx, reader.lockScopeID, []lockmanager.ResourceRef{sRef(rawSpace, rawKey)}, nil); err != nil {
		t.Fatalf("the first shared read: %v", err)
	} else if op != lockmanager.LockOpGranted {
		t.Fatalf("the first shared read op = %s, want GRANTED", op)
	}
	second := f.begin()
	if _, op, _, err := second.AcquireLock(ctx, second.lockScopeID, []lockmanager.ResourceRef{sRef(rawSpace, rawKey)}, nil); err != nil {
		t.Fatalf("the second shared read: %v", err)
	} else if op != lockmanager.LockOpGranted {
		t.Fatalf("two shared reads of one key conflicted: op = %s, want GRANTED", op)
	}
	_ = second.Rollback()

	// The raw delete is an exclusive write of that key, so it must queue behind the reader.
	deleter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- deleter.Delete(rawSpace, rawKey) }()
	f.awaitWait("the raw delete behind a shared read")
	select {
	case err := <-outcome:
		t.Fatalf("the raw delete returned (%v) while a shared read held the key", err)
	default:
	}
	if pending := f.manager.PendingRequests(deleter.ID); pending != 1 {
		t.Fatalf("the deleting root has %d queued requests, want 1", pending)
	}

	if err := reader.Rollback(); err != nil {
		t.Fatalf("reader rollback: %v", err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken raw delete failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the raw delete was never woken by the reader ending")
	}
	if _, err := deleter.Commit(ctx); err != nil {
		t.Fatalf("the woken delete could not commit: %v", err)
	}
	f.requireIdle()
}

// TestT045ARawWriteWaitsForAnotherRootsRecordLockAndIsReleasedByItsRollback is the wait half of the
// client contract seen from the waiter: the wait ends with the root that caused it, and once that root
// is gone nothing of the wait survives in the manager.
func TestT045ARawWriteWaitsForAnotherRootsRecordLockAndIsReleasedByItsRollback(t *testing.T) {
	f := newRawLockFixture(t)
	ctx := context.Background()

	seed := f.begin()
	if err := seed.Put(rawSpace, rawKey, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	holder := f.begin()
	if err := holder.Put(rawSpace, rawKey, []byte("holder")); err != nil {
		t.Fatal(err)
	}
	waiter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- waiter.Put(rawSpace, rawKey, []byte("waiter")) }()
	f.awaitWait("the second raw write of a held key")
	if pending := f.manager.PendingRequests(waiter.ID); pending != 1 {
		t.Fatalf("the waiting root has %d queued requests, want 1", pending)
	}
	if edges := f.manager.WaitEdges(); len(edges) == 0 {
		t.Fatal("a queued request produced no wait-for edge, so the waiter is not attributed to a holder")
	}

	// The holder rolls back rather than committing: the wait must end either way, and a rollback is
	// the case where nothing at all is published.
	if err := holder.Rollback(); err != nil {
		t.Fatalf("holder rollback: %v", err)
	}
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("the woken raw write failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the raw write was never woken by the holder's rollback")
	}
	if _, err := waiter.Commit(ctx); err != nil {
		t.Fatalf("the woken write could not commit: %v", err)
	}
	f.requireIdle()
}

// TestT045ReplayApplyNeverWaitsForAUserLock is the boundary the round asks about explicitly.
//
// `Store.Apply` is how an already-decided command reaches the store: it replays a decision that has
// already been made, so it is not a user writing and must never queue behind a user's lock. The
// assertion is made on the manager's own clock rather than on elapsed time: if the apply waited, the
// manager would arm a wait deadline, and the armed clock is the one dependency that reports exactly
// that moment.
func TestT045ReplayApplyNeverWaitsForAUserLock(t *testing.T) {
	f := newRawLockFixture(t)

	// A user root holds the record of the key the replay touches and does not end.
	holder := f.begin()
	if err := holder.Put(rawSpace, rawKey, []byte("holder")); err != nil {
		t.Fatal(err)
	}
	// Drain anything the holder's own acquisition armed, so the assertion below can only see a wait
	// the apply itself starts.
	for {
		select {
		case <-f.clock.armed:
			continue
		default:
		}
		break
	}

	replay := Command{Kind: "stage", ID: "replay-root", Ops: []Op{
		{Space: rawSpace, Key: rawKey, Value: []byte("replayed")},
	}}
	done := make(chan error, 1)
	go func() {
		_, err := f.store.Apply(1, replay)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the replay apply failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the replay apply did not finish while a user root held the key: it must not join user lock waits")
	}
	select {
	case <-f.clock.armed:
		t.Fatal("the replay apply armed a lock wait: a replayed decision must not queue behind a user lock")
	default:
	}

	// The replayed op is there, so the apply really did the work rather than skipping it.
	if _, err := f.store.Apply(2, Command{Kind: "commit", ID: "replay-root"}); err != nil {
		t.Fatalf("committing the replayed stage: %v", err)
	}
	fresh := f.begin()
	value, found, err := fresh.Get(rawSpace, rawKey)
	if err != nil {
		t.Fatal(err)
	}
	if !found || string(value) != "replayed" {
		t.Fatalf("the replayed key holds %q (found=%v), want the replayed value", value, found)
	}
	_ = fresh.Rollback()

	if err := holder.Rollback(); err != nil {
		t.Fatalf("holder rollback: %v", err)
	}
	f.requireIdle()
}

// TestT045ARawWriteRefusesToStageAfterItsRootEnded proves the fail-closed half: a write that waited
// TestT045ARawWriteRefusesToStageAfterItsRootRolledBackWhileQueued proves the fail-closed half: a
// write that was still queued when its own root ended is refused, and nothing is staged.
//
// The refusal arrives through the manager's cancel path — the queued request is withdrawn when the
// owner is released — so the error it surfaces is the manager's own conflict rather than ErrClosed.
// The invariant this test defends is the one that matters and does not depend on which of the two is
// reported: the write is never staged on the strength of a grant that outlived its transaction. The
// buffered-op check is what proves it, because a non-nil error alone would not — the op could have
// been buffered first and refused afterwards.
func TestT045ARawWriteRefusesToStageAfterItsRootRolledBackWhileQueued(t *testing.T) {
	f := newRawLockFixture(t)

	holder := f.begin()
	if err := holder.Put(rawSpace, rawKey, []byte("holder")); err != nil {
		t.Fatalf("the holder write: %v", err)
	}
	waiter := f.begin()
	outcome := make(chan error, 1)
	go func() { outcome <- waiter.Put(rawSpace, rawKey, []byte("waiter")) }()
	f.awaitWait("the raw write behind the holder")

	// The waiting root ends while its write is still queued.
	if err := waiter.Rollback(); err != nil {
		t.Fatalf("waiter rollback: %v", err)
	}
	select {
	case err := <-outcome:
		if err == nil {
			t.Fatal("the write reported success after its own root had ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued write was never decided after its root ended")
	}
	if len(waiter.buffered) != 0 || waiter.stage != nil {
		t.Fatalf("a refused write left %d buffered ops (stage open: %v)", len(waiter.buffered), waiter.stage != nil)
	}
	// The holder's own write was never published either, so the key is still absent for a new reader.
	fresh := f.begin()
	if _, found, err := fresh.Get(rawSpace, rawKey); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("a refused write became visible to a later reader")
	}
	_ = fresh.Rollback()
	_ = holder.Rollback()
	f.requireIdle()
}
