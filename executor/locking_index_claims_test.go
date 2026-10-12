package executor

import (
	"bytes"
	"context"
	"testing"

	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// locking_index_claims_test.go is the T043 suite: every index write goes through the one path that
// exists, and the claim a statement takes names the key that path installs.
//
// The invariant is a negative one — "there is no second index encoder" — so the tests do not restate
// the encoding. They capture the components a real SQL statement asks the lock manager for, resolve
// the row that statement wrote through the engine's own decode path, and then recompute the expected
// key with the writer's own functions. Agreement is therefore evidence that the lock path and the
// write path are the same code, and disagreement is a failure of the invariant rather than of the
// test's copy of it.
//
// If a second encoder were ever introduced, it would have to be introduced *here* as well for these
// tests to keep passing, which is exactly the coupling that keeps the two from drifting apart.

// captureLockComponents runs one statement and returns every lock component it asked the manager for.
func captureLockComponents(t *testing.T, f *uniqueFixture, session *Session, statement string) []storageengine.LockComponent {
	t.Helper()
	var captured []storageengine.LockComponent
	previous := dmlLockRequestHook
	dmlLockRequestHook = func(request storageengine.LockRequest) storageengine.LockRequest {
		captured = append(captured, request.Components...)
		return request
	}
	defer func() { dmlLockRequestHook = previous }()
	if _, err := f.engine.Execute(session, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	if len(captured) == 0 {
		t.Fatalf("%s asked for no lock at all", statement)
	}
	return captured
}

// t043IndexTable is the fixture: one primary key and the three index shapes the argument covers — a
// single-column unique index, a composite unique index, and a non-unique secondary index.
const t043IndexSchema = "CREATE TABLE t(id BIGINT PRIMARY KEY, a INT, b INT, sku VARCHAR(20), " +
	"UNIQUE KEY uq_sku(sku), UNIQUE KEY uq_ab(a,b), KEY idx_b(b))"

// t043State is the captured claims of one statement plus the row it wrote, resolved through the
// engine's own encoder so the test never builds a row by hand.
type t043State struct {
	table  versionedTable
	owner  []byte
	row    storage.Row
	claims []storageengine.LockComponent
}

// t043Resolve captures one statement's claims and resolves the row it left under the primary key.
func t043Resolve(t *testing.T, f *uniqueFixture, session *Session, statement string) t043State {
	t.Helper()
	claims := captureLockComponents(t, f, session, statement)
	read, err := f.engine.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	table, _, _, err := loadVersionedTable(read, session, "t")
	if err != nil {
		t.Fatal(err)
	}
	owner := readComponentKey(t, claims, sqllayout.Rows(table.ID), "")
	encoded, exists, err := read.Table(table.ID).Get(owner)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("%s left no row under the primary key it claimed", statement)
	}
	row, err := decodeSQLRow(table, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return t043State{table: table, owner: owner, row: row, claims: claims}
}

// readComponentKey returns the key of the one *row* claim in a namespace, and fails if it is not there.
//
// The table intention lives in the same space as the row it governs and carries no key, so a match on
// the namespace alone would find the intention first and resolve to nothing.
func readComponentKey(t *testing.T, claims []storageengine.LockComponent, space, indexID string) []byte {
	t.Helper()
	for _, claim := range claims {
		if claim.Space == space && claim.IndexID == indexID && len(claim.Key) > 0 {
			return claim.Key
		}
	}
	t.Fatalf("no keyed claim in space %q index %q; claims: %+v", space, indexID, claims)
	return nil
}

// readClaim returns the one claim in a namespace, and fails if it is not there.
func readClaim(t *testing.T, claims []storageengine.LockComponent, space, indexID string) storageengine.LockComponent {
	t.Helper()
	for _, claim := range claims {
		if claim.Space == space && claim.IndexID == indexID {
			return claim
		}
	}
	t.Fatalf("no claim in space %q index %q; claims: %+v", space, indexID, claims)
	return storageengine.LockComponent{}
}

// TestT043TheIndexLockKeyIsTheKeyTheWriterInstalls is the core invariant for an INSERT: for every
// non-primary index, the component the statement claims carries exactly the key the writer installs —
// the value reservation for a unique index, and the value-plus-owner entry for a secondary one.
func TestT043TheIndexLockKeyIsTheKeyTheWriterInstalls(t *testing.T) {
	f := newUniqueFixture(t, t043IndexSchema)
	session := &Session{}
	f.must(session, "USE test")

	state := t043Resolve(t, f, session, "INSERT INTO t VALUES(1,7,9,'X')")

	checked := 0
	for _, index := range state.table.Definition.Indexes {
		if index.Primary {
			continue
		}
		if index.Unique {
			valueKey, ok := storage.IndexValueKey(index, state.table.Definition.Columns, state.row)
			if !ok {
				t.Fatalf("unique index %s has no value key for the row it just wrote", index.Name)
			}
			claim := readClaim(t, state.claims, sqllayout.UniqueIndex(state.table.ID, index.Name), index.Name)
			if string(claim.Key) != valueKey {
				t.Fatalf("unique index %s is claimed on %q, want the writer's own value key %q", index.Name, claim.Key, valueKey)
			}
			if bytes.HasSuffix(claim.Key, state.owner) {
				t.Fatalf("unique index %s is claimed on %q, which carries the owner suffix: two rows writing the same value would never meet", index.Name, claim.Key)
			}
			checked++
			continue
		}
		positions, ok := secondaryColumns(state.table, index)
		if !ok {
			t.Fatalf("secondary index %s has no resolvable columns, so its claim cannot be checked", index.Name)
		}
		entryKey, _, err := secondaryEntry(state.table, positions, state.owner, state.row)
		if err != nil {
			t.Fatal(err)
		}
		claim := readClaim(t, state.claims, sqllayout.SecondaryIndex(state.table.ID, index.Name), index.Name)
		if !bytes.Equal(claim.Key, entryKey) {
			t.Fatalf("secondary index %s is claimed on %q, want the writer's own entry key %q", index.Name, claim.Key, entryKey)
		}
		if !bytes.HasSuffix(claim.Key, state.owner) {
			t.Fatalf("secondary index %s is claimed on %q, which drops the owner suffix: two rows sharing a value would be two claims for one entry", index.Name, claim.Key)
		}
		checked++
	}
	if checked != 3 {
		t.Fatalf("checked %d indexes, want the schema's three non-primary indexes", checked)
	}
}

// TestT043AnUpdateClaimsTheOldAndNewIndexPositions proves requirement 8 on the index side: an UPDATE
// that moves an indexed value holds the position it removes *and* the position it installs, so neither
// a concurrent reader of the old value nor a concurrent writer of the new one can slip past.
func TestT043AnUpdateClaimsTheOldAndNewIndexPositions(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE t(id BIGINT PRIMARY KEY, sku VARCHAR(20), "+
		"UNIQUE KEY uq_sku(sku), KEY idx_sku(sku))")
	session := &Session{}
	f.must(session, "USE test")

	before := t043Resolve(t, f, session, "INSERT INTO t VALUES(1,'A')")
	after := t043Resolve(t, f, session, "UPDATE t SET sku='B' WHERE id=1")

	for _, index := range after.table.Definition.Indexes {
		if index.Primary {
			continue
		}
		var space string
		var oldKey, newKey []byte
		if index.Unique {
			space = sqllayout.UniqueIndex(after.table.ID, index.Name)
			oldValue, ok := storage.IndexValueKey(index, after.table.Definition.Columns, before.row)
			if !ok {
				t.Fatalf("index %s has no value key for the old row", index.Name)
			}
			newValue, ok := storage.IndexValueKey(index, after.table.Definition.Columns, after.row)
			if !ok {
				t.Fatalf("index %s has no value key for the new row", index.Name)
			}
			oldKey, newKey = []byte(oldValue), []byte(newValue)
		} else {
			space = sqllayout.SecondaryIndex(after.table.ID, index.Name)
			positions, ok := secondaryColumns(after.table, index)
			if !ok {
				t.Fatalf("secondary index %s has no resolvable columns", index.Name)
			}
			var err error
			oldKey, _, err = secondaryEntry(after.table, positions, before.owner, before.row)
			if err != nil {
				t.Fatal(err)
			}
			newKey, _, err = secondaryEntry(after.table, positions, after.owner, after.row)
			if err != nil {
				t.Fatal(err)
			}
		}
		if bytes.Equal(oldKey, newKey) {
			t.Fatalf("index %s has the same key before and after, so this case proves nothing", index.Name)
		}
		// Both positions must be claimed. The removal side carries no insertion intention, so the
		// claim is matched on its key alone.
		claimedOld, claimedNew := false, false
		for _, claim := range after.claims {
			if claim.Space != space || claim.IndexID != index.Name {
				continue
			}
			if bytes.Equal(claim.Key, oldKey) {
				claimedOld = true
			}
			if bytes.Equal(claim.Key, newKey) {
				claimedNew = true
			}
		}
		if !claimedOld {
			t.Fatalf("index %s does not claim the position it removes (%q): a concurrent writer of the old value would not meet this statement", index.Name, oldKey)
		}
		if !claimedNew {
			t.Fatalf("index %s does not claim the position it installs (%q): a concurrent insert of the new value would not meet this statement", index.Name, newKey)
		}
	}
}

// TestT043ADeleteClaimsTheEntriesItRemovesWithoutAnInsertionIntention is the DELETE half. The removal
// must hold the entries it takes away, and must not hold an insertion intention on them: an interval
// that only blocks new keys has no business holding up a removal.
func TestT043ADeleteClaimsTheEntriesItRemovesWithoutAnInsertionIntention(t *testing.T) {
	f := newUniqueFixture(t, "CREATE TABLE t(id BIGINT PRIMARY KEY, sku VARCHAR(20), "+
		"UNIQUE KEY uq_sku(sku), KEY idx_sku(sku))")
	session := &Session{}
	f.must(session, "USE test")

	inserted := t043Resolve(t, f, session, "INSERT INTO t VALUES(1,'A')")
	claims := captureLockComponents(t, f, session, "DELETE FROM t WHERE id=1")

	for _, index := range inserted.table.Definition.Indexes {
		if index.Primary {
			continue
		}
		var space string
		var expected []byte
		if index.Unique {
			space = sqllayout.UniqueIndex(inserted.table.ID, index.Name)
			valueKey, ok := storage.IndexValueKey(index, inserted.table.Definition.Columns, inserted.row)
			if !ok {
				t.Fatalf("unique index %s has no value key for the row", index.Name)
			}
			expected = []byte(valueKey)
		} else {
			space = sqllayout.SecondaryIndex(inserted.table.ID, index.Name)
			positions, ok := secondaryColumns(inserted.table, index)
			if !ok {
				t.Fatalf("secondary index %s has no resolvable columns", index.Name)
			}
			entryKey, _, err := secondaryEntry(inserted.table, positions, inserted.owner, inserted.row)
			if err != nil {
				t.Fatal(err)
			}
			expected = entryKey
		}
		var found, foundIntention bool
		for _, claim := range claims {
			if claim.Space != space || claim.IndexID != index.Name {
				continue
			}
			if !bytes.Equal(claim.Key, expected) {
				continue
			}
			if claim.Mode == storageengine.LockModeInsertIntention {
				foundIntention = true
				continue
			}
			found = true
		}
		if !found {
			t.Fatalf("index %s does not claim the entry it removes (%q)", index.Name, expected)
		}
		if foundIntention {
			t.Fatalf("index %s claims an insertion intention on an entry the DELETE removes", index.Name)
		}
	}
}
