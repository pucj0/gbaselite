package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// lock_identity_test.go is T026: one identity per real index resource.
//
// The property under test is that every write path that touches a real index resource computes the
// *same* lock identity for it, because the lock manager compares bytes and nothing else. The SQL
// primary row, the unique-index reservation and the non-unique secondary entry are three different
// resources with three different encodings, and the executor must not invent a fourth:
//
//   - the primary row is `sqllayout.Rows(table.ID)` plus the encoded primary key;
//   - a unique value is `sqllayout.UniqueIndex(table.ID, index)` plus `storage.IndexValueKey`,
//     which is the key the write path stores the entry under — the owning primary key is the
//     entry's *value*, so the reservation carries **no** owner suffix. This is the trap the task
//     calls out: an identity that appended the owner would give two rows with the same unique value
//     and different primary keys two different resources, and they would never meet;
//   - a non-unique secondary entry is `sqllayout.SecondaryIndex(table.ID, index)` plus the entry
//     key `secondaryEntry` builds, which *does* carry the owner suffix because two rows may legally
//     share the value.
//
// Every expectation below is computed by calling the write path's own encoder — `sqlPrimaryKey`,
// `storage.IndexValueKey`, `secondaryEntry`, `sqllayout.*` — rather than by re-deriving the format
// in the test, so a change to the encoding moves the lock and the assertion together and a change
// to only one of them fails.
//
// The tests capture components through the production seam `dmlLockRequestHook`, which every DML
// lock request passes through, so they assert on what a real statement locked rather than on what a
// helper would have produced.

// t026Capture records every neutral lock request a DML statement issues through
// dmlLockRequestHook, and optionally replaces its neutral wait decision.
//
// It is the T026 counterpart of locking_dml_wake_test.go's dmlWaitBarrier: the barrier proves that
// a waiter is queued *before* the holder releases (the decision runs under the manager's critical
// section), and the capture proves *which bytes* the waiter queued on. Neither is a timing
// argument, so no assertion here depends on how long anything took.
type t026Capture struct {
	mu       sync.Mutex
	requests [][]storageengine.LockComponent
	// shouldWait, when non-nil, becomes the neutral wait decision of every captured request. A
	// test sets it before arm. A decision that returns false turns a conflict into an immediate
	// ErrConflict, which is how the "these values must not contend" cases stay bounded instead of
	// hanging on a lock they were never supposed to take.
	shouldWait func(context.Context) bool
	// signal, when non-nil, is closed by the wait decision of the first conflicting request,
	// exactly like newDMLWaitBarrier.ready.
	signal chan struct{}
	once   sync.Once
}

func t026NewCapture() *t026Capture { return &t026Capture{} }

// t026ArmConflict makes the next conflicting request report itself and still wait.
func (c *t026Capture) t026ArmConflict() {
	c.signal = make(chan struct{})
	c.shouldWait = func(context.Context) bool {
		c.once.Do(func() { close(c.signal) })
		return true
	}
}

// t026ArmRefuse makes a conflicting request fail immediately instead of waiting, so a test that
// expects no conflict at all cannot hang on one.
func (c *t026Capture) t026ArmRefuse() {
	c.shouldWait = func(context.Context) bool { return false }
}

func (c *t026Capture) arm(t *testing.T) {
	t.Helper()
	shouldWait := c.shouldWait
	previous := dmlLockRequestHook
	dmlLockRequestHook = func(request storageengine.LockRequest) storageengine.LockRequest {
		c.mu.Lock()
		c.requests = append(c.requests, append([]storageengine.LockComponent(nil), request.Components...))
		c.mu.Unlock()
		if shouldWait == nil {
			return request
		}
		return storageengine.LockRequest{Components: request.Components, ShouldWait: shouldWait}
	}
	t.Cleanup(func() { dmlLockRequestHook = previous })
}

// waitForConflict blocks until a request reported a conflict with another owner.
func (c *t026Capture) waitForConflict(t *testing.T, what string) {
	t.Helper()
	select {
	case <-c.signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached a lock conflict; the two statements did not meet on one resource", what)
	}
}

func (c *t026Capture) reset() {
	c.mu.Lock()
	c.requests = nil
	c.mu.Unlock()
}

func (c *t026Capture) bundles() [][]storageengine.LockComponent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]storageengine.LockComponent(nil), c.requests...)
}

// single returns the one request a statement issued, and fails when it issued none or several. A
// single-row INSERT, UPDATE or DELETE must build its identity once per row; a statement that issued
// no request locked nothing, and one that issued several for a single row is a different defect.
func (c *t026Capture) single(t *testing.T, what string) []storageengine.LockComponent {
	t.Helper()
	bundles := c.bundles()
	if len(bundles) != 1 {
		t.Fatalf("%s issued %d lock requests, want exactly 1: %s", what, len(bundles), t026DescribeBundles(bundles))
	}
	return bundles[0]
}

// t026LoadTable reads the committed catalog definition of a table, which is what the lock path
// itself uses to derive an identity.
func t026LoadTable(t *testing.T, e *Engine, session *Session, name string) versionedTable {
	t.Helper()
	tx, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	table, _, _, err := loadVersionedTable(tx, session, name)
	if err != nil {
		t.Fatalf("load table %s: %v", name, err)
	}
	return table
}

// t026Row builds the row the executor holds for one statement, using the catalog's own column
// types. The encoders below read exactly these values, so building them any other way would let the
// test disagree with the write path about the value it is encoding.
func t026Row(t *testing.T, table versionedTable, values ...any) storage.Row {
	t.Helper()
	if len(values) != len(table.Definition.Columns) {
		t.Fatalf("row has %d values for %d columns", len(values), len(table.Definition.Columns))
	}
	row := make(storage.Row, len(values))
	for i, value := range values {
		built, err := storage.NewValue(table.Definition.Columns[i].Type, value)
		if err != nil {
			t.Fatalf("column %s value %v: %v", table.Definition.Columns[i].Name, value, err)
		}
		row[i] = built
	}
	return row
}

// t026Index finds one catalog index by name.
func t026Index(t *testing.T, table versionedTable, name string) storage.Index {
	t.Helper()
	for _, index := range table.Definition.Indexes {
		if strings.EqualFold(index.Name, name) {
			return index
		}
	}
	t.Fatalf("table %s has no index %q", table.CatalogName, name)
	return storage.Index{}
}

// t026PrimaryIndex finds the primary key definition, which is the identity `primaryKeyBytes`
// resolves.
func t026PrimaryIndex(t *testing.T, table versionedTable) storage.Index {
	t.Helper()
	for _, index := range table.Definition.Indexes {
		if index.Primary {
			return index
		}
	}
	t.Fatalf("table %s has no primary key", table.CatalogName)
	return storage.Index{}
}

// t026Pick returns the single *equality* component of one kind, space and index label. The space
// plus the kind plus the label is the identity of the resource in the component set, so a pick that
// found nothing (a lock was never taken) and a pick that found two (the same resource was locked
// twice as two identities) are both failures.
//
// Insertion intentions are excluded from the count and checked separately as a pair: an insertion
// claim is deliberately two components on one key — the intention (what a covering Gap blocks) and
// the record lock (what decides same-key contention) — so counting them together would read a
// correct pairing as a duplicated identity. `t026RequirePairedIntentions` asserts the pairing, which
// is strictly stronger than ignoring the intention would be.
func t026Pick(t *testing.T, what string, components []storageengine.LockComponent, kind storageengine.LockComponentKind, space, indexID string) storageengine.LockComponent {
	t.Helper()
	var found []storageengine.LockComponent
	for _, component := range components {
		if component.Kind == kind && component.Space == space && component.IndexID == indexID &&
			component.Mode != storageengine.LockModeInsertIntention {
			found = append(found, component)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: found %d %s components in space %q for index %q, want exactly 1:%s",
			what, len(found), kind, space, indexID, t026Describe(components))
	}
	t026RequirePairedIntentions(t, what, components)
	return found[0]
}

// t026RequirePairedIntentions asserts that every insert intention in a component set is accompanied
// by the record lock of the same key in the same namespace.
//
// An intention without its record lock is refused by the lock manager, and a record lock without its
// intention would let an insertion walk through another transaction's interval. Checking the pair
// here means a statement that dropped one half fails on its own captured components rather than at
// the lock manager, which is where the reason is still visible.
func t026RequirePairedIntentions(t *testing.T, what string, components []storageengine.LockComponent) {
	t.Helper()
	records := make(map[string]bool, len(components))
	for _, component := range components {
		if component.Kind == storageengine.LockComponentRecord && component.Mode == storageengine.LockModeX {
			records[component.Space+"\x00"+string(component.Key)] = true
		}
	}
	for _, component := range components {
		if component.Kind != storageengine.LockComponentRecord || component.Mode != storageengine.LockModeInsertIntention {
			continue
		}
		if !records[component.Space+"\x00"+string(component.Key)] {
			t.Fatalf("%s: insert intention %s has no record lock of the same key in its bundle:%s",
				what, t026DescribeComponent(component), t026Describe(components))
		}
	}
}

// t026RequireNoComponent asserts that a resource was not locked at all. It is how "NULL reserves
// nothing" is stated: the absence has to be exact, because an identity with an empty key would
// still queue behind something.
func t026RequireNoComponent(t *testing.T, what string, components []storageengine.LockComponent, kind storageengine.LockComponentKind, space string) {
	t.Helper()
	for _, component := range components {
		if component.Kind == kind && component.Space == space {
			t.Fatalf("%s: a %s component exists in space %q, want none:%s", what, kind, space, t026Describe(components))
		}
	}
}

// t026RequireComponent asserts mode, kind, space, labels and the exact key bytes.
func t026RequireComponent(t *testing.T, what string, got, want storageengine.LockComponent) {
	t.Helper()
	if got.Mode != want.Mode || got.Kind != want.Kind || got.Space != want.Space ||
		got.TableID != want.TableID || got.IndexID != want.IndexID || !bytes.Equal(got.Key, want.Key) {
		t.Fatalf("%s identity mismatch:\n  got  %s\n  want %s", what, t026DescribeComponent(got), t026DescribeComponent(want))
	}
}

// t026DescribeComponent renders one component with its key in hex, so a byte-level mismatch is
// readable.
func t026DescribeComponent(component storageengine.LockComponent) string {
	return fmt.Sprintf("%s/%s space=%q table=%q index=%q key=%x", component.Mode, component.Kind,
		component.Space, component.TableID, component.IndexID, component.Key)
}

func t026Describe(components []storageengine.LockComponent) string {
	var out strings.Builder
	for i, component := range components {
		fmt.Fprintf(&out, "\n  [%d] %s", i, t026DescribeComponent(component))
	}
	return out.String()
}

func t026DescribeBundles(bundles [][]storageengine.LockComponent) string {
	var out strings.Builder
	for i, bundle := range bundles {
		fmt.Fprintf(&out, "\n request[%d]:%s", i, t026Describe(bundle))
	}
	return out.String()
}

// t026WantIntent is the table intention component the lock path builds: an IX intention on the
// table's row namespace, which is the identity the DDL and the raw client also use.
func t026WantIntent(table versionedTable) storageengine.LockComponent {
	return storageengine.LockComponent{
		Mode:    storageengine.LockModeIX,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   sqllayout.Rows(table.ID),
		TableID: table.ID,
	}
}

// t026WantRow is the primary-row component the lock path builds for one row: the row namespace plus
// the primary key encoded by `sqlPrimaryKey`.
func t026WantRow(t *testing.T, table versionedTable, row storage.Row) storageengine.LockComponent {
	t.Helper()
	key, ok := sqlPrimaryKey(table, t026PrimaryIndex(t, table), row)
	if !ok {
		t.Fatalf("the primary key of table %s cannot be encoded from %v", table.CatalogName, row)
	}
	return storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   sqllayout.Rows(table.ID),
		TableID: table.ID,
		Key:     key,
	}
}

// t026WantUnique is the unique-value reservation for one row, or false when the value does not
// constrain (a NULL part or an unresolvable column).
//
// The key is `storage.IndexValueKey` itself — the write path's encoder for a unique entry — and the
// owner is deliberately not appended: the owning primary key is stored as the entry's *value*, so
// the reservation is the value and nothing else.
func t026WantUnique(t *testing.T, table versionedTable, indexName string, row storage.Row) (storageengine.LockComponent, bool) {
	t.Helper()
	index := t026Index(t, table, indexName)
	valueKey, constrained := storage.IndexValueKey(index, table.Definition.Columns, row)
	if !constrained {
		return storageengine.LockComponent{}, false
	}
	return storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   sqllayout.UniqueIndex(table.ID, index.Name),
		TableID: table.ID,
		IndexID: index.Name,
		Key:     []byte(valueKey),
	}, true
}

// t026WantSecondary is the physical entry component of a non-unique secondary index: the index's
// own namespace plus the key `secondaryEntry` builds, which is the value parts followed by the
// owner suffix.
func t026WantSecondary(t *testing.T, table versionedTable, indexName string, owner []byte, row storage.Row) storageengine.LockComponent {
	t.Helper()
	index := t026Index(t, table, indexName)
	positions, ok := secondaryColumns(table, index)
	if !ok {
		t.Fatalf("index %s of table %s is not a lockable secondary index", indexName, table.CatalogName)
	}
	entryKey, _, err := secondaryEntry(table, positions, owner, row)
	if err != nil {
		t.Fatalf("encode the secondary entry of %s: %v", indexName, err)
	}
	return storageengine.LockComponent{
		Mode:    storageengine.LockModeX,
		Kind:    storageengine.LockComponentRecord,
		Space:   sqllayout.SecondaryIndex(table.ID, index.Name),
		TableID: table.ID,
		IndexID: index.Name,
		Key:     entryKey,
	}
}

// t026RequireRowBundle asserts that one row's mutation bundle carries exactly the identities the
// write path implies for that row: the table intention, the primary row, one reservation per
// unique index whose value is constrained, and one entry per non-unique secondary index.
//
// Extra components are reported but not failed: the component *set* is owned by the range and
// insert-protection tasks (T024/T025), while T026 owns the identity bytes of the resources listed
// above. Silently ignoring them would hide a locked resource from a reader, so they are logged.
func t026RequireRowBundle(t *testing.T, what string, bundle []storageengine.LockComponent, table versionedTable, row storage.Row) {
	t.Helper()
	expected := map[string]bool{
		t026DescribeComponent(t026WantIntent(table)):      true,
		t026DescribeComponent(t026WantRow(t, table, row)): true,
	}
	t026RequireComponent(t, what+" table intention",
		t026Pick(t, what, bundle, storageengine.LockComponentTableIntent, sqllayout.Rows(table.ID), ""), t026WantIntent(table))
	t026RequireComponent(t, what+" primary row",
		t026Pick(t, what, bundle, storageengine.LockComponentRecord, sqllayout.Rows(table.ID), ""), t026WantRow(t, table, row))

	owner := t026WantRow(t, table, row).Key
	for _, index := range table.Definition.Indexes {
		if index.Primary {
			continue
		}
		if index.Unique {
			want, constrained := t026WantUnique(t, table, index.Name, row)
			if !constrained {
				t026RequireNoComponent(t, what+" unique index "+index.Name+" (unconstrained value)", bundle, storageengine.LockComponentRecord, sqllayout.UniqueIndex(table.ID, index.Name))
				continue
			}
			expected[t026DescribeComponent(want)] = true
			t026RequireComponent(t, what+" unique reservation "+index.Name,
				t026Pick(t, what, bundle, storageengine.LockComponentRecord, want.Space, index.Name), want)
			continue
		}
		want := t026WantSecondary(t, table, index.Name, owner, row)
		expected[t026DescribeComponent(want)] = true
		t026RequireComponent(t, what+" secondary entry "+index.Name,
			t026Pick(t, what, bundle, storageengine.LockComponentRecord, want.Space, index.Name), want)
	}
	for _, component := range bundle {
		if !expected[t026DescribeComponent(component)] {
			t.Logf("%s: the bundle also carries an identity T026 does not own (reported, not failed): %s",
				what, t026DescribeComponent(component))
		}
	}
}

// TestT026DMLStatementLockComponentsMatchTheWritePath drives the real INSERT, multi-row INSERT,
// UPDATE (key unchanged and key changed) and DELETE statements of this package's DML pipeline and
// compares every captured component byte-for-byte with the encoder the write path uses for that
// same resource.
//
// This is points 1 and 2 of T026: the raw storage write, the SQL statements, the primary key and
// both index kinds must all name one resource per real index, and the only way to show that is to
// read the identity off a real statement instead of restating it here.
func TestT026DMLStatementLockComponentsMatchTheWritePath(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE t(id BIGINT PRIMARY KEY, a INT, sku VARCHAR(20) UNIQUE, KEY sec_a(a))")
	table := t026LoadTable(t, f.engine, f.setup, "t")
	capture := t026NewCapture()
	capture.arm(t)
	writer := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(writer)
	run := func(sql string) {
		t.Helper()
		f.must(writer, sql)
	}

	// One INSERT takes one bundle per row: the table intention, the primary row, the unique
	// reservation of 'A' and the secondary entry for a=7 with the owner suffix.
	capture.reset()
	run("INSERT INTO t VALUES(1,7,'A')")
	t026RequireRowBundle(t, "the INSERT", capture.single(t, "the INSERT"), table, t026Row(t, table, int64(1), int64(7), "A"))

	// A multi-row INSERT is several row mutations, so every row builds its own identity: two rows
	// with different primary keys and different values must produce two different bundles, each
	// byte-for-byte what the write path installs for that row.
	capture.reset()
	run("INSERT INTO t VALUES(2,8,'B'),(3,9,'C')")
	bundles := capture.bundles()
	if len(bundles) != 2 {
		t.Fatalf("the multi-row INSERT issued %d lock requests, want 2 (one per row): %s", len(bundles), t026DescribeBundles(bundles))
	}
	t026RequireRowBundle(t, "the multi-row INSERT row 2", bundles[0], table, t026Row(t, table, int64(2), int64(8), "B"))
	t026RequireRowBundle(t, "the multi-row INSERT row 3", bundles[1], table, t026Row(t, table, int64(3), int64(9), "C"))

	// An UPDATE that leaves the primary key alone locks the target row and then the replacement
	// row, both under the same primary identity: the row component is the same bytes twice, while
	// the index entries move because the values moved.
	capture.reset()
	run("UPDATE t SET a=11 WHERE id=1")
	bundles = capture.bundles()
	if len(bundles) != 2 {
		t.Fatalf("the UPDATE issued %d lock requests, want 2 (target and replacement): %s", len(bundles), t026DescribeBundles(bundles))
	}
	t026RequireRowBundle(t, "the UPDATE target", bundles[0], table, t026Row(t, table, int64(1), int64(7), "A"))
	t026RequireRowBundle(t, "the UPDATE replacement", bundles[1], table, t026Row(t, table, int64(1), int64(11), "A"))
	if !bytes.Equal(t026Pick(t, "the UPDATE target", bundles[0], storageengine.LockComponentRecord, sqllayout.Rows(table.ID), "").Key,
		t026Pick(t, "the UPDATE replacement", bundles[1], storageengine.LockComponentRecord, sqllayout.Rows(table.ID), "").Key) {
		t.Fatal("a key-preserving UPDATE locked two different primary identities for one row")
	}

	// An UPDATE that moves the primary key locks the old identity and the new one: the row keys
	// differ, the unique reservation of the unchanged value 'B' is the same bytes for both rows
	// (that is the value-only identity), and the secondary entries differ by their owner suffix.
	capture.reset()
	run("UPDATE t SET id=4 WHERE id=2")
	bundles = capture.bundles()
	if len(bundles) != 2 {
		t.Fatalf("the moving UPDATE issued %d lock requests, want 2: %s", len(bundles), t026DescribeBundles(bundles))
	}
	t026RequireRowBundle(t, "the moving UPDATE target", bundles[0], table, t026Row(t, table, int64(2), int64(8), "B"))
	t026RequireRowBundle(t, "the moving UPDATE replacement", bundles[1], table, t026Row(t, table, int64(4), int64(8), "B"))
	oldRow := t026Pick(t, "the moving UPDATE target", bundles[0], storageengine.LockComponentRecord, sqllayout.Rows(table.ID), "")
	newRow := t026Pick(t, "the moving UPDATE replacement", bundles[1], storageengine.LockComponentRecord, sqllayout.Rows(table.ID), "")
	if bytes.Equal(oldRow.Key, newRow.Key) {
		t.Fatal("a primary-key UPDATE locked one identity for two different primary keys")
	}
	oldReservation := t026Pick(t, "the moving UPDATE target", bundles[0], storageengine.LockComponentRecord, sqllayout.UniqueIndex(table.ID, "sku"), "sku")
	newReservation := t026Pick(t, "the moving UPDATE replacement", bundles[1], storageengine.LockComponentRecord, sqllayout.UniqueIndex(table.ID, "sku"), "sku")
	if !bytes.Equal(oldReservation.Key, newReservation.Key) {
		t.Fatalf("a primary-key move re-reserved the same unique value 'B' under two identities: %x vs %x", oldReservation.Key, newReservation.Key)
	}

	// DELETE locks the row it removes, with the unique reservation and secondary entry of the row
	// as it was read: the removal has to meet a concurrent writer on the same resources.
	capture.reset()
	run("DELETE FROM t WHERE id=3")
	t026RequireRowBundle(t, "the DELETE", capture.single(t, "the DELETE"), table, t026Row(t, table, int64(3), int64(9), "C"))
}

// TestT026UniqueReservationIsOneResourceWhateverThePrimaryKey is point 3 of T026 and the trap the
// task names: two rows with the same non-NULL unique value but different primary keys must contend
// on ONE resource.
//
// It is proved end-to-end with two real SQL sessions: the second INSERT must reach a conflict with
// the first owner (the neutral wait decision reports it while the manager holds its critical
// section, which is what makes "it queued behind the first" a fact) and must still be waiting until
// the holder commits. The captured bytes then show why: the unique components are identical, and
// the primary row and secondary-entry components — the only things the two rows differ in — are
// not. The table carries a non-unique index on the same column on purpose, so the test shows the
// owner-suffixed entry encoding beside the value-only reservation for the same logical row.
func TestT026UniqueReservationIsOneResourceWhateverThePrimaryKey(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE, KEY sec_sku(sku))")
	table := t026LoadTable(t, f.engine, f.setup, "u")
	holder := &Session{CurrentDatabase: "test"}
	waiter := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(holder)
	defer f.engine.CloseSession(waiter)

	capture := t026NewCapture()
	capture.t026ArmConflict()
	capture.arm(t)

	f.must(holder, "BEGIN")
	f.must(waiter, "BEGIN")
	f.must(holder, "INSERT INTO u VALUES(1,'A')")

	done := insertInFlight(f.engine, waiter, "INSERT INTO u VALUES(2,'A')")
	capture.waitForConflict(t, "the second INSERT of the same unique value")
	// The conflict is the barrier; the statement must not have returned from it, because the holder
	// has not ended and the reservation has not been released.
	select {
	case err := <-done:
		t.Fatalf("the conflicting INSERT returned before the holder committed: %v", err)
	default:
	}

	bundles := capture.bundles()
	if len(bundles) != 2 {
		t.Fatalf("the two INSERTs issued %d lock requests, want 2: %s", len(bundles), t026DescribeBundles(bundles))
	}
	holderUniqueSpace := sqllayout.UniqueIndex(table.ID, "sku")
	holderReservation := t026Pick(t, "the holder INSERT", bundles[0], storageengine.LockComponentRecord, holderUniqueSpace, "sku")
	waiterReservation := t026Pick(t, "the waiter INSERT", bundles[1], storageengine.LockComponentRecord, holderUniqueSpace, "sku")
	if !bytes.Equal(holderReservation.Key, waiterReservation.Key) {
		t.Fatalf("two rows with the same unique value locked two different reservations: %x vs %x", holderReservation.Key, waiterReservation.Key)
	}

	// Both reservations are exactly the write path's value key, which carries no owner suffix: the
	// owning primary key is the entry's value, not part of the key.
	wantHolder, ok := t026WantUnique(t, table, "sku", t026Row(t, table, int64(1), "A"))
	if !ok {
		t.Fatal("the unique value 'A' was reported as unconstrained")
	}
	wantWaiter, _ := t026WantUnique(t, table, "sku", t026Row(t, table, int64(2), "A"))
	t026RequireComponent(t, "the holder reservation", holderReservation, wantHolder)
	t026RequireComponent(t, "the waiter reservation", waiterReservation, wantWaiter)

	// The primary row identities differ. If they did not, the trap would be hidden rather than
	// avoided: the two rows must differ *only* outside the reservation.
	holderRow := t026Pick(t, "the holder INSERT", bundles[0], storageengine.LockComponentRecord, sqllayout.Rows(table.ID), "")
	waiterRow := t026Pick(t, "the waiter INSERT", bundles[1], storageengine.LockComponentRecord, sqllayout.Rows(table.ID), "")
	if bytes.Equal(holderRow.Key, waiterRow.Key) {
		t.Fatal("two rows with different primary keys locked one primary identity")
	}

	// The non-unique secondary entries of the two rows differ as well, because that encoding
	// carries the owner suffix. The contrast is the whole point: the value-only reservation is one
	// resource for both rows, and the owner-suffixed entry is two resources, one per row.
	holderSecondarySpace := sqllayout.SecondaryIndex(table.ID, "sec_sku")
	holderEntry := t026Pick(t, "the holder INSERT", bundles[0], storageengine.LockComponentRecord, holderSecondarySpace, "sec_sku")
	waiterEntry := t026Pick(t, "the waiter INSERT", bundles[1], storageengine.LockComponentRecord, holderSecondarySpace, "sec_sku")
	if bytes.Equal(holderEntry.Key, waiterEntry.Key) {
		t.Fatalf("two rows with different primary keys locked one secondary entry %x", holderEntry.Key)
	}
	if !bytes.HasSuffix(holderEntry.Key, holderRow.Key) || !bytes.HasSuffix(waiterEntry.Key, waiterRow.Key) {
		t.Fatalf("a secondary entry does not end with its own owner suffix: %x (owner %x) vs %x (owner %x)",
			holderEntry.Key, holderRow.Key, waiterEntry.Key, waiterRow.Key)
	}

	f.must(holder, "COMMIT")
	// Once the holder is gone the waiter is woken and the locked re-check reports the permanent
	// constraint violation, which is the observable proof that the two statements really did meet
	// on the reservation instead of merely taking unrelated locks.
	if err := awaitStatement(t, done, "the conflicting INSERT"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("the woken INSERT = %v, want storage.ErrDuplicateKey", err)
	}
	_, _ = f.engine.Execute(waiter, "ROLLBACK")
}

// TestT026DifferentUniqueValuesDoNotContend is the reverse of point 3: the reservation is per
// value, not per index and not per table.
//
// The wait decision refuses to wait, so a conflict would surface immediately as ErrConflict instead
// of hanging: a bounded negative that cannot be confused with a slow statement. The captured keys
// are then asserted to differ, which is what makes the non-conflict meaningful rather than lucky.
func TestT026DifferentUniqueValuesDoNotContend(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE u(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)")
	table := t026LoadTable(t, f.engine, f.setup, "u")
	holder := &Session{CurrentDatabase: "test"}
	other := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(holder)
	defer f.engine.CloseSession(other)

	capture := t026NewCapture()
	capture.t026ArmRefuse()
	capture.arm(t)

	f.must(holder, "BEGIN")
	f.must(other, "BEGIN")
	f.must(holder, "INSERT INTO u VALUES(1,'A')")
	if err := awaitStatement(t, insertInFlight(f.engine, other, "INSERT INTO u VALUES(2,'B')"), "an unrelated INSERT"); err != nil {
		t.Fatalf("an INSERT of a different unique value met the holder: %v", err)
	}

	bundles := capture.bundles()
	if len(bundles) != 2 {
		t.Fatalf("the two INSERTs issued %d lock requests, want 2: %s", len(bundles), t026DescribeBundles(bundles))
	}
	space := sqllayout.UniqueIndex(table.ID, "sku")
	first := t026Pick(t, "the holder INSERT", bundles[0], storageengine.LockComponentRecord, space, "sku")
	second := t026Pick(t, "the unrelated INSERT", bundles[1], storageengine.LockComponentRecord, space, "sku")
	if bytes.Equal(first.Key, second.Key) {
		t.Fatalf("two different unique values share one reservation key %x", first.Key)
	}
	f.must(holder, "COMMIT")
	f.must(other, "COMMIT")
}

// TestT026NullUniqueValueReservesNothing is point 4 of T026.
//
// A unique value containing a NULL is not constrained, so it must produce no reservation
// component at all: reserving one would serialise two rows the constraint itself allows to coexist.
// The assertion is on the absence of a record component in the index's own namespace — an empty-key
// reservation would also be a resource, so "no lock was taken" is the only correct answer. Each
// case pairs the NULL with a constrained row on the same table, so a statement that simply locked
// nothing at all would fail the non-vacuity half.
func TestT026NullUniqueValueReservesNothing(t *testing.T) {
	single := newUniqueFixture(t, "CREATE TABLE n(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE)")
	singleTable := t026LoadTable(t, single.engine, single.setup, "n")
	singleCapture := t026NewCapture()
	singleCapture.arm(t)
	writer := &Session{CurrentDatabase: "test"}
	defer single.engine.CloseSession(writer)

	singleCapture.reset()
	single.must(writer, "INSERT INTO n VALUES(1,NULL)")
	bundle := singleCapture.single(t, "the NULL unique INSERT")
	t026RequireNoComponent(t, "the NULL unique INSERT", bundle, storageengine.LockComponentRecord, sqllayout.UniqueIndex(singleTable.ID, "sku"))
	// The row itself is still locked: the absence above is about the reservation, not about the
	// statement.
	t026RequireComponent(t, "the NULL unique INSERT primary row",
		t026Pick(t, "the NULL unique INSERT", bundle, storageengine.LockComponentRecord, sqllayout.Rows(singleTable.ID), ""),
		t026WantRow(t, singleTable, t026Row(t, singleTable, int64(1), nil)))

	singleCapture.reset()
	single.must(writer, "INSERT INTO n VALUES(2,'A')")
	t026RequireComponent(t, "the constrained unique INSERT reservation",
		t026Pick(t, "the constrained unique INSERT", singleCapture.single(t, "the constrained unique INSERT"), storageengine.LockComponentRecord, sqllayout.UniqueIndex(singleTable.ID, "sku"), "sku"),
		mustT026WantUnique(t, singleTable, "sku", t026Row(t, singleTable, int64(2), "A")))

	// A composite unique index is unconstrained when *any* part is NULL, so both NULL positions
	// reserve nothing while the full value does.
	composite := newUniqueFixture(t, "CREATE TABLE cn(id BIGINT PRIMARY KEY, a INT, b INT, UNIQUE KEY uq_ab(a,b))")
	compositeTable := t026LoadTable(t, composite.engine, composite.setup, "cn")
	compositeCapture := t026NewCapture()
	compositeCapture.arm(t)
	defer composite.engine.CloseSession(writer)
	space := sqllayout.UniqueIndex(compositeTable.ID, "uq_ab")

	for _, sql := range []string{
		"INSERT INTO cn VALUES(1,7,NULL)",
		"INSERT INTO cn VALUES(2,NULL,9)",
		"INSERT INTO cn VALUES(3,NULL,NULL)",
	} {
		compositeCapture.reset()
		composite.must(writer, sql)
		t026RequireNoComponent(t, sql, compositeCapture.single(t, sql), storageengine.LockComponentRecord, space)
	}

	compositeCapture.reset()
	composite.must(writer, "INSERT INTO cn VALUES(4,7,9)")
	t026RequireComponent(t, "the fully constrained composite INSERT",
		t026Pick(t, "the fully constrained composite INSERT", compositeCapture.single(t, "the fully constrained composite INSERT"), storageengine.LockComponentRecord, space, "uq_ab"),
		mustT026WantUnique(t, compositeTable, "uq_ab", t026Row(t, compositeTable, int64(4), int64(7), int64(9))))
}

// mustT026WantUnique is t026WantUnique for the cases where the value is known to be constrained.
func mustT026WantUnique(t *testing.T, table versionedTable, indexName string, row storage.Row) storageengine.LockComponent {
	t.Helper()
	want, ok := t026WantUnique(t, table, indexName, row)
	if !ok {
		t.Fatalf("index %s reported the value of %v as unconstrained", indexName, row)
	}
	return want
}

// TestT026CollationEquivalentUniqueValuesShareOneKey is point 5 of T026.
//
// With a case-insensitive collation 'A' and 'a' are the same unique value, so they must be the same
// lock identity. Deriving the expected key with `storage.IndexValueKey` on a row whose text is
// exactly what SQL stores shows that the folding lives in the write path's encoder and the lock
// reused it, rather than the lock lowercasing anything of its own.
func TestT026CollationEquivalentUniqueValuesShareOneKey(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE ci(id BIGINT PRIMARY KEY, sku VARCHAR(20) COLLATE utf8mb4_general_ci UNIQUE)")
	table := t026LoadTable(t, f.engine, f.setup, "ci")
	holder := &Session{CurrentDatabase: "test"}
	waiter := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(holder)
	defer f.engine.CloseSession(waiter)

	capture := t026NewCapture()
	capture.t026ArmConflict()
	capture.arm(t)

	f.must(holder, "BEGIN")
	f.must(waiter, "BEGIN")
	f.must(holder, "INSERT INTO ci VALUES(1,'A')")
	done := insertInFlight(f.engine, waiter, "INSERT INTO ci VALUES(2,'a')")
	capture.waitForConflict(t, "the collation-equivalent INSERT")
	select {
	case err := <-done:
		t.Fatalf("the collation-equivalent INSERT returned before the holder committed: %v", err)
	default:
	}

	bundles := capture.bundles()
	if len(bundles) != 2 {
		t.Fatalf("the two INSERTs issued %d lock requests, want 2: %s", len(bundles), t026DescribeBundles(bundles))
	}
	space := sqllayout.UniqueIndex(table.ID, "sku")
	upper := t026Pick(t, "the 'A' INSERT", bundles[0], storageengine.LockComponentRecord, space, "sku")
	lower := t026Pick(t, "the 'a' INSERT", bundles[1], storageengine.LockComponentRecord, space, "sku")
	if !bytes.Equal(upper.Key, lower.Key) {
		t.Fatalf("collation-equivalent values locked two identities: %x vs %x", upper.Key, lower.Key)
	}
	// Both are the encoder's output for the *stored* text, which is the folded value for a _ci
	// collation. If the lock had encoded the raw text, this comparison would fail.
	t026RequireComponent(t, "the 'A' reservation", upper, mustT026WantUnique(t, table, "sku", t026Row(t, table, int64(1), "A")))
	t026RequireComponent(t, "the 'a' reservation", lower, mustT026WantUnique(t, table, "sku", t026Row(t, table, int64(2), "a")))
	// The folded key is the entry key the write path installs, so the reservation and the physical
	// unique entry are one resource here.
	if !bytes.Equal(upper.Key, mustT026WantUnique(t, table, "sku", t026Row(t, table, int64(1), "A")).Key) {
		t.Fatal("the reservation and the unique entry key diverged")
	}

	f.must(holder, "COMMIT")
	if err := awaitStatement(t, done, "the collation-equivalent INSERT"); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("the woken INSERT = %v, want storage.ErrDuplicateKey", err)
	}
	_, _ = f.engine.Execute(waiter, "ROLLBACK")
}

// TestT026CompositeIndexLocksTheWholeValue is point 6 of T026.
//
// A composite unique index reserves its whole value as one key, and a composite secondary entry
// includes the owner suffix. The prefix case is the interesting one: a row matching only the first
// column of the composite must not be given the reservation of a full value, so the captured key is
// compared against the key the same encoder produces for the first column alone.
func TestT026CompositeIndexLocksTheWholeValue(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE c(id BIGINT PRIMARY KEY, a INT, b INT, UNIQUE KEY uq_ab(a,b), KEY sec_ab(a,b))")
	table := t026LoadTable(t, f.engine, f.setup, "c")
	capture := t026NewCapture()
	capture.arm(t)
	writer := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(writer)

	capture.reset()
	f.must(writer, "INSERT INTO c VALUES(1,7,9)")
	bundle := capture.single(t, "the composite INSERT")
	row := t026Row(t, table, int64(1), int64(7), int64(9))
	t026RequireRowBundle(t, "the composite INSERT", bundle, table, row)

	uniqueSpace := sqllayout.UniqueIndex(table.ID, "uq_ab")
	compositeKey := t026Pick(t, "the composite INSERT", bundle, storageengine.LockComponentRecord, uniqueSpace, "uq_ab")
	secondarySpace := sqllayout.SecondaryIndex(table.ID, "sec_ab")
	secondaryKey := t026Pick(t, "the composite INSERT", bundle, storageengine.LockComponentRecord, secondarySpace, "sec_ab")
	owner := t026WantRow(t, table, row).Key

	// The secondary entry ends with the owner suffix; the reservation must not, because the owner of
	// a unique value is the entry's value rather than part of its key.
	if !bytes.HasSuffix(secondaryKey.Key, owner) {
		t.Fatalf("the composite secondary entry %x does not end with its owner suffix %x", secondaryKey.Key, owner)
	}
	if bytes.HasSuffix(compositeKey.Key, owner) {
		t.Fatalf("the composite reservation %x carries an owner suffix %x", compositeKey.Key, owner)
	}
	if bytes.Equal(compositeKey.Key, secondaryKey.Key) {
		t.Fatalf("the composite reservation and the composite secondary entry share one identity %x", compositeKey.Key)
	}

	// A prefix-only equality value: the same encoder applied to a definition holding just the first
	// column. The whole-value reservation must not be that key, or a row matching the prefix would
	// reserve what only a full composite value constrains.
	prefix := t026Index(t, table, "uq_ab")
	prefix.Columns = prefix.Columns[:1]
	prefixKey, ok := storage.IndexValueKey(prefix, table.Definition.Columns, row)
	if !ok {
		t.Fatal("the prefix encoding of a constrained value reported unconstrained")
	}
	t.Logf("T026 prefix report: the first column encodes to %x, the composite reservation is %x", []byte(prefixKey), compositeKey.Key)
	if bytes.Equal(compositeKey.Key, []byte(prefixKey)) {
		t.Fatalf("the composite reservation equals the prefix-only encoding %x", prefixKey)
	}

	// Two rows sharing the prefix produce two different reservations, which is what "the whole
	// composite value is the identity" means in practice.
	capture.reset()
	f.must(writer, "INSERT INTO c VALUES(2,7,10)")
	other := t026Pick(t, "the second composite INSERT", capture.single(t, "the second composite INSERT"), storageengine.LockComponentRecord, uniqueSpace, "uq_ab")
	if bytes.Equal(other.Key, compositeKey.Key) {
		t.Fatalf("two rows sharing the first column of a composite unique index share one reservation %x", other.Key)
	}
}

// TestT026ReservationIsNotTheEntryIdentity is point 8 of T026.
//
// The unique reservation and the secondary entry are different resources and must never be
// conflated: the reservation names the logical value (the unique entry's key, whose owner is the
// entry's value), while the secondary entry names one physical row's entry (value plus owner). One
// table carries both indexes so the comparison is about the same logical row, and the reservation
// is shown to be a strict *value* identity while the secondary key carries the owner suffix.
func TestT026ReservationIsNotTheEntryIdentity(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE e(id BIGINT PRIMARY KEY, sku VARCHAR(20) UNIQUE, KEY sec_sku(sku))")
	table := t026LoadTable(t, f.engine, f.setup, "e")
	capture := t026NewCapture()
	capture.arm(t)
	writer := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(writer)

	capture.reset()
	f.must(writer, "INSERT INTO e VALUES(1,'A')")
	bundle := capture.single(t, "the INSERT")
	row := t026Row(t, table, int64(1), "A")
	t026RequireRowBundle(t, "the INSERT", bundle, table, row)

	reservation := t026Pick(t, "the INSERT", bundle, storageengine.LockComponentRecord, sqllayout.UniqueIndex(table.ID, "sku"), "sku")
	entry := t026Pick(t, "the INSERT", bundle, storageengine.LockComponentRecord, sqllayout.SecondaryIndex(table.ID, "sec_sku"), "sec_sku")

	// The reservation *is* the write path's unique entry key: for a unique index the owner is
	// stored as the entry's value, so the key is the value alone. This is why the reservation may
	// reuse the index namespace without pretending the physical entry is something else.
	if !bytes.Equal(reservation.Key, mustT026WantUnique(t, table, "sku", row).Key) {
		t.Fatalf("the reservation %x is not the unique entry key the write path installs", reservation.Key)
	}
	if bytes.HasSuffix(reservation.Key, t026WantRow(t, table, row).Key) {
		t.Fatalf("the reservation %x names the owning primary key, so two rows with the same value would not meet", reservation.Key)
	}
	// The secondary entry of the same row is a different resource: it cannot equal the reservation,
	// and it must name the owner, because two rows may legally share a secondary value.
	if bytes.Equal(reservation.Key, entry.Key) {
		t.Fatalf("the reservation and the secondary entry of one row share identity %x", reservation.Key)
	}
	if !bytes.HasSuffix(entry.Key, t026WantRow(t, table, row).Key) {
		t.Fatalf("the secondary entry %x does not name its owner %x", entry.Key, t026WantRow(t, table, row).Key)
	}
	if reservation.Space == entry.Space {
		t.Fatalf("the reservation and the secondary entry share the namespace %q", reservation.Space)
	}
	// And they belong to different iterations of the index list, which is what makes them separate
	// lock components rather than two spellings of one.
	if reservation.IndexID == entry.IndexID {
		t.Fatalf("the reservation and the secondary entry name the same index %q", reservation.IndexID)
	}
}

// TestT026RawWriteMeetsTheCapturedSQLIndexIdentity is point 7 of T026 stated end-to-end inside the
// SQL package: the component is read off a real statement (never recomputed here), and a raw
// neutral transaction then writes those exact bytes while the SQL transaction is still open.
//
// Both index identities T026 introduces are exercised — the unique-value reservation and the
// non-unique secondary entry — because T019's raw coverage in this package and in mvcc only proved
// the primary row record. The raw write must not complete while the SQL transaction holds the
// resource, and it must complete once that transaction ends; the completion after the release is
// what separates "waited" from "failed", and the bounded negative check cannot be confused with a
// statement that simply was never blocked.
//
// The wait itself is asserted deterministically in the adapter package
// (storageengine/mvccadapter/locking_identity_test.go), where the engine's own pending-request
// count is reachable for these same bytes; this test adds the genuinely SQL-captured component.
func TestT026RawWriteMeetsTheCapturedSQLIndexIdentity(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE r(id BIGINT PRIMARY KEY, a INT, sku VARCHAR(20) UNIQUE, KEY sec_a(a))")
	table := t026LoadTable(t, f.engine, f.setup, "r")
	capture := t026NewCapture()
	capture.arm(t)
	holder := &Session{CurrentDatabase: "test"}
	defer f.engine.CloseSession(holder)

	for _, indexName := range []string{"sku", "sec_a"} {
		indexName := indexName
		capture.reset()
		if _, err := f.engine.Execute(holder, "BEGIN"); err != nil {
			t.Fatal(err)
		}
		f.must(holder, "INSERT INTO r VALUES(1,7,'A')")
		bundle := capture.single(t, "the holder INSERT for index "+indexName)
		component := t026Pick(t, "the holder INSERT for index "+indexName, bundle, storageengine.LockComponentRecord,
			t026ComponentSpace(t, table, indexName), indexName)

		raw, err := f.engine.Backend.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- raw.Put(component.Space, component.Key, []byte("raw")) }()

		// The bounded negative: the raw write must not have completed while the SQL transaction
		// still holds the resource IT ACTUALLY LOCKED. A raw write to different bytes would return
		// here immediately, which is the bypass this test exists to catch.
		select {
		case err := <-done:
			t.Fatalf("the raw write to %q/%x returned (%v) while the SQL transaction still held it", component.Space, component.Key, err)
		case <-time.After(300 * time.Millisecond):
		}

		if _, err := f.engine.Execute(holder, "ROLLBACK"); err != nil {
			t.Fatalf("the SQL holder rollback: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the raw write to %q/%x failed after the SQL holder ended: %v", component.Space, component.Key, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the raw write to %q/%x was never woken by the SQL holder ending", component.Space, component.Key)
		}
		if err := raw.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}

// t026ComponentSpace resolves the namespace a locked component of one index uses, by looking the
// index up in the catalog rather than by naming a prefix here: the test must not be able to agree
// with a namespace the write path does not use.
func t026ComponentSpace(t *testing.T, table versionedTable, indexName string) string {
	t.Helper()
	for _, index := range table.Definition.Indexes {
		if !strings.EqualFold(index.Name, indexName) {
			continue
		}
		if index.Unique {
			return sqllayout.UniqueIndex(table.ID, index.Name)
		}
		return sqllayout.SecondaryIndex(table.ID, index.Name)
	}
	t.Fatalf("table %s has no index %q", table.CatalogName, indexName)
	return ""
}
