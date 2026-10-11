package mvccadapter

import (
	"context"
	"testing"
	"time"

	"gbaselite/lockmanager"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// locking_identity_test.go is T026's raw-storage half: a raw neutral write and the identity a real
// SQL statement locked must be one resource, including for an *index* resource and not only for a
// row.
//
// The bytes are not restated here. They are derived with the same encoders the executor's write
// path uses, and the SQL side of the correspondence is pinned by
// `executor/lock_identity_test.go` (TestT026DMLStatementLockComponentsMatchTheWritePath), which
// captures a real INSERT/UPDATE/DELETE and asserts byte-for-byte that the component it locked is
// exactly this Space plus this Key:
//
//   - a primary row: `sqllayout.Rows(tableID)` + `sqlPrimaryKey`, which for a single signed-integer
//     primary key under the current key encoding is `sqllayout.SignedInteger`;
//   - a unique value reservation: `sqllayout.UniqueIndex(tableID, index)` +
//     `storage.IndexValueKey`, the very encoder the write path stores the unique entry under.
//
// T019 already proved the row-record half of that correspondence (`executor/raw_lock_test.go`,
// `mvcc/raw_locking_test.go`). What is added here is the index half and the adapter-level statement
// of "the raw write blocks on the SQL identity": the SQL side is simulated by the *neutral* bundle
// a SQL statement issues, which is what the executor really acquires, and the conflict is observed
// in the adapter's own lock manager rather than inferred from a delay.
//
// This file deliberately does not test raw insert-intention behaviour: the raw path takes an
// exact-key record X only, and insert intention is another task's round.

// t026OpenEngine opens one MVCC-backed adapter engine with its lock manager. The concrete type is
// returned because every assertion here reads the engine's own lock manager, which the neutral
// surface deliberately does not expose.
func t026OpenEngine(t *testing.T) *engine {
	t.Helper()
	opened, err := Open(t.TempDir(), storageengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := opened.(*engine)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// t026Begin starts one root transaction and ends it at cleanup. A transaction that already ended is
// ended again harmlessly, so cleanup does not depend on the path the test took.
func t026Begin(t *testing.T, e *engine) *txn {
	t.Helper()
	tx, err := e.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx.(*txn)
}

// t026SQLRowIdentity is the Space/Key of one SQL primary-key row, derived with the write path's own
// encoders:
//
//	Space = sqllayout.Rows(tableID)        (storageengine.BindTable builds it exactly this way)
//	Key   = sqllayout.SignedInteger(id)    (executor.sqlPrimaryKey under the integer key encoding)
func t026SQLRowIdentity(tableID string, id int64) (string, []byte) {
	return sqllayout.Rows(tableID), sqllayout.SignedInteger(id)
}

// t026SQLUniqueIdentity is the Space/Key of the unique-value reservation SQL takes for the value
// `value` of the VARCHAR column `sku`:
//
//	Space = sqllayout.UniqueIndex(tableID, indexName)
//	Key   = []byte(storage.IndexValueKey(index, columns, row))
//
// `storage.IndexValueKey` is the writer's own encoder, so the NULL, composite and collation
// semantics travel with it; re-implementing any of that here would let this test agree with a
// format the SQL layer never uses.
func t026SQLUniqueIdentity(t *testing.T, tableID, indexName, value string) (string, []byte) {
	t.Helper()
	index := storage.Index{Name: indexName, Columns: []string{"sku"}, Unique: true}
	columns := []storage.Column{{Name: "sku", Type: storage.TypeVarchar, SQLType: "varchar(20)"}}
	built, err := storage.NewValue(storage.TypeVarchar, value)
	if err != nil {
		t.Fatalf("build the value %q: %v", value, err)
	}
	key, constrained := storage.IndexValueKey(index, columns, storage.Row{built})
	if !constrained {
		t.Fatalf("the unique value %q was reported as unconstrained", value)
	}
	return sqllayout.UniqueIndex(tableID, indexName), []byte(key)
}

// t026SQLComponents is the neutral bundle a SQL statement holds for one resource: the table
// intention on the row namespace (which every SQL DML bundle starts with) plus the record. The
// labels are metadata and do not take part in record identity, which is exactly why a raw write
// with no labels at all meets it.
func t026SQLComponents(rowSpace, space string, key []byte, tableID, indexID string) []storageengine.LockComponent {
	return []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent, Space: rowSpace, TableID: tableID},
		{Mode: storageengine.LockModeX, Kind: storageengine.LockComponentRecord, Space: space, TableID: tableID, IndexID: indexID, Key: key},
	}
}

// t026RecordRef is the same identity as a lock-manager reference, so a test can ask the manager
// directly whether one owner holds it. The labels are absent on purpose: the manager's record
// identity is (generation, space, key).
func t026RecordRef(space string, key []byte) lockmanager.ResourceRef {
	return lockmanager.ResourceRef{Identity: lockmanager.Identity{
		Kind: lockmanager.KindRecord, Mode: lockmanager.ModeX, Space: space, Key: key,
	}}
}

// t026AcquireStatement opens a statement scope and acquires one bundle through the neutral
// capability, failing at the caller's error text when the acquisition is refused.
func t026AcquireStatement(t *testing.T, ctx context.Context, tx storageengine.Txn, components []storageengine.LockComponent) storageengine.LockGrant {
	t.Helper()
	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		t.Fatalf("locking capability: %v", err)
	}
	scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{Components: components})
	if err != nil {
		t.Fatalf("acquire %+v: %v", components, err)
	}
	return grant
}

// t026AwaitPending blocks until one root has exactly the expected number of queued requests.
//
// A queued request is the observable fact that a waiter reached the holder and has not been
// granted: the manager's own state, not elapsed time, is what establishes the ordering, and the
// polling interval only decides how often that state is read. A request that never queues fails the
// test after the deadline instead of hanging it. This mirrors mvcc/raw_locking_test.go, which
// documents the same rule for the same reason.
func t026AwaitPending(t *testing.T, e *engine, rootID string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if pending := e.locks.PendingRequests(rootID); pending == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("%s: %s has %d queued requests, want %d", what, rootID, pending, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestT026RawWriteWaitsForTheSQLIdentities proves the "one resource" claim in the direction that
// matters most for T026: a raw transaction that writes the exact Space and Key a SQL statement
// protects must wait, rather than staging a write the SQL statement cannot see.
//
// Each case derives the identity with the SQL write path's encoder, has a holder acquire it through
// the neutral bundle a real statement issues, and asserts the raw write is queued on that holder
// (the adapter's own pending-request count for the raw root) and completes once the holder ends.
// The completion after the release is what separates "waited" from "failed".
func TestT026RawWriteWaitsForTheSQLIdentities(t *testing.T) {
	const tableID = "t026-identity"

	cases := []struct {
		name    string
		indexID string
		what    string
	}{
		{name: "primary row record", what: "the raw write to a SQL primary row"},
		{name: "unique value reservation", indexID: "sku", what: "the raw write to a SQL unique reservation"},
	}

	ctx := context.Background()
	rowSpace, rowKey := t026SQLRowIdentity(tableID, 10)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := t026OpenEngine(t)
			space, key := rowSpace, rowKey
			if tc.indexID != "" {
				space, key = t026SQLUniqueIdentity(t, tableID, tc.indexID, "A")
			}

			// The SQL side: one statement's bundle, acquired by this root.
			holder := t026Begin(t, e)
			t026AcquireStatement(t, ctx, holder, t026SQLComponents(rowSpace, space, key, tableID, tc.indexID))
			// Non-vacuity: if the holder had acquired nothing, the raw write below would succeed
			// for a reason that has nothing to do with the identity under test.
			if mode, held := e.locks.HeldMode(holder.ID(), t026RecordRef(space, key)); !held || mode != lockmanager.ModeX {
				t.Fatalf("the SQL-shaped holder does not hold an X on %q/%x: mode %q, held %v", space, key, mode, held)
			}

			// The raw side: the same bytes, no labels, through the ordinary client entry point.
			raw := t026Begin(t, e)
			done := make(chan error, 1)
			go func() { done <- raw.Put(space, key, []byte("raw")) }()

			t026AwaitPending(t, e, raw.ID(), 1, tc.what)
			select {
			case err := <-done:
				t.Fatalf("the raw write completed (%v) while the SQL transaction still held %q/%x", err, space, key)
			default:
			}

			// The holder ending is the only release point.
			if err := holder.Rollback(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the raw write failed after the SQL transaction ended: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("the raw write on %q/%x was never woken by the SQL transaction ending", space, key)
			}
		})
	}
}

// TestT026SQLDerivedAcquisitionWaitsForARawWrite is the reverse direction: once a raw writer holds
// the bytes, a SQL-shaped acquisition of the same identity must queue rather than be granted.
//
// It is asserted for both identities T026 introduces, and the queued request is read from the
// manager, so "the SQL side waited" is an observation and not an inference from the raw holder's
// activity. Together with the test above this is the "raw and SQL converge on one resource" half of
// T026 in both directions.
func TestT026SQLDerivedAcquisitionWaitsForARawWrite(t *testing.T) {
	const tableID = "t026-identity"
	ctx := context.Background()
	rowSpace, rowKey := t026SQLRowIdentity(tableID, 10)

	for _, tc := range []struct {
		name    string
		indexID string
	}{
		{name: "primary row record", indexID: ""},
		{name: "unique value reservation", indexID: "sku"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := t026OpenEngine(t)
			space, key := rowSpace, rowKey
			if tc.indexID != "" {
				space, key = t026SQLUniqueIdentity(t, tableID, tc.indexID, "A")
			}

			raw := t026Begin(t, e)
			if err := raw.Put(space, key, []byte("raw-holder")); err != nil {
				t.Fatalf("the raw write: %v", err)
			}

			acquirer := t026Begin(t, e)
			locking, err := storageengine.LockingTxnOf(acquirer)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := locking.BeginLockScope(ctx, storageengine.LockScopeStatement)
			if err != nil {
				t.Fatal(err)
			}
			// The neutral wait decision is consulted under the manager's critical section and before
			// the request joins the queue, exactly like newDMLWaitBarrier in the executor tests.
			queued := make(chan struct{}, 1)
			type outcome struct {
				grant storageengine.LockGrant
				err   error
			}
			done := make(chan outcome, 1)
			go func() {
				grant, acquireErr := locking.Acquire(ctx, scope, storageengine.LockRequest{
					Components: t026SQLComponents(rowSpace, space, key, tableID, tc.indexID),
					ShouldWait: func(context.Context) bool {
						select {
						case queued <- struct{}{}:
						default:
						}
						return true
					},
				})
				done <- outcome{grant: grant, err: acquireErr}
			}()

			select {
			case <-queued:
			case <-time.After(10 * time.Second):
				t.Fatalf("the SQL-shaped acquisition of %q/%x never reached a conflict with the raw holder", space, key)
			}
			t026AwaitPending(t, e, acquirer.ID(), 1, "the SQL-shaped acquisition behind a raw write")
			select {
			case out := <-done:
				t.Fatalf("the SQL-shaped acquisition resolved while the raw holder was open: grant=%+v err=%v", out.grant, out.err)
			default:
			}

			if err := raw.Rollback(); err != nil {
				t.Fatal(err)
			}
			select {
			case out := <-done:
				if out.err != nil {
					t.Fatalf("the woken SQL-shaped acquisition failed: %v", out.err)
				}
				if out.grant.ID == 0 || out.grant.OwnerID != acquirer.ID() {
					t.Fatalf("the woken grant is not a capability of the acquirer: %+v", out.grant)
				}
				// Non-vacuity of the release: the grant really covers the identity a raw writer had
				// held, read back from the manager rather than from the bundle that asked for it.
				if mode, held := e.locks.HeldMode(acquirer.ID(), t026RecordRef(space, key)); !held || mode != lockmanager.ModeX {
					t.Fatalf("the woken acquirer does not hold %q/%x: mode %q, held %v", space, key, mode, held)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s was never granted after the raw holder ended", tc.name)
			}
		})
	}
}
