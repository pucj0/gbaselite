package executor

import (
	"errors"
	"testing"

	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// locking_foreign_key_test.go is T039/T044: the derived writes of the referential actions.
//
// CASCADE and SET NULL reach child rows that the statement never named, and REPLACE/ON DUPLICATE KEY
// UPDATE rewrite a row a plain INSERT never touches. Both are writes, so they must claim the child
// row and its index entries through the *statement's* lock scope — the same resources an explicit
// statement claims — instead of writing around the lock manager.
//
// The first test asserts that at the component level, through the production seam every scoped lock
// request passes (`dmlLockRequestHook`): before the derived write took locks, a cascade produced no
// captured request at all for the child row. The remaining tests assert the consequence with real
// sessions: the derived write waits for a lock another session holds, and the final data is what the
// referential action says it must be.

// t039Fixture creates a parent with two children: one whose rows are cascaded and one whose foreign
// key is set to NULL.
func t039Fixture(t *testing.T) *t035Fixture {
	t.Helper()
	return newT035Fixture(t,
		"CREATE TABLE parent(id BIGINT PRIMARY KEY, v INT)",
		"CREATE TABLE cascade_child(id BIGINT PRIMARY KEY, pid BIGINT, note VARCHAR(32), "+
			"UNIQUE KEY uq_note(note), KEY idx_pid(pid), "+
			"FOREIGN KEY (pid) REFERENCES parent(id) ON DELETE CASCADE ON UPDATE CASCADE)",
		"CREATE TABLE null_child(id BIGINT PRIMARY KEY, pid BIGINT, "+
			"KEY idx_pid(pid), FOREIGN KEY (pid) REFERENCES parent(id) ON DELETE SET NULL)",
		"INSERT INTO parent VALUES(1,10)",
		"INSERT INTO cascade_child VALUES(100,1,'first')",
		"INSERT INTO null_child VALUES(200,1)",
	)
}

// TestT039CascadeDeleteClaimsTheChildRowAndItsIndexes proves the derived write claims the child's
// physical identity: the primary row *and* the unique and secondary index entries of the row it
// removes. Those components only exist because the cascade goes through the DML lock path; a raw write
// would have claimed nothing here.
func TestT039CascadeDeleteClaimsTheChildRowAndItsIndexes(t *testing.T) {
	f := t039Fixture(t)
	session := t034Session(t, f, 5)

	child := t026LoadTable(t, f.engine, session, "cascade_child")
	childRow := t026Row(t, child, int64(100), int64(1), "first")
	rowKey, ok := primaryKeyBytes(child, childRow)
	if !ok {
		t.Fatal("the child row has no derivable primary key")
	}
	noteKey, ok := storage.IndexValueKey(child.Definition.Indexes[indexByName(t, child, "uq_note")], child.Definition.Columns, childRow)
	if !ok {
		t.Fatal("the child row has no derivable unique value")
	}
	secondaryIndex := child.Definition.Indexes[indexByName(t, child, "idx_pid")]
	positions, ok := secondaryColumns(child, secondaryIndex)
	if !ok {
		t.Fatal("the secondary index columns cannot be resolved")
	}
	secondaryKey, _, err := secondaryEntry(child, positions, rowKey, childRow)
	if err != nil {
		t.Fatal(err)
	}

	capture := t026NewCapture()
	capture.arm(t)
	f.must(session, "DELETE FROM parent WHERE id=1")

	bundles := capture.bundles()
	// The parent's own delete, and the cascade's write of the child row.
	t039RequireComponent(t, bundles, storageengine.LockComponentRecord, sqllayout.Rows(child.ID), string(rowKey))
	t039RequireComponent(t, bundles, storageengine.LockComponentRecord, sqllayout.UniqueIndex(child.ID, "uq_note"), string(noteKey))
	t039RequireComponent(t, bundles, storageengine.LockComponentRecord, sqllayout.SecondaryIndex(child.ID, "idx_pid"), string(secondaryKey))
	// The cascade really happened, so the claims above belong to a write that occurred.
	if got := t035Rows(t, f.must(session, "SELECT id FROM cascade_child")); got != "" {
		t.Fatalf("the cascaded child row survived: %q", got)
	}
	if got := t035Rows(t, f.must(session, "SELECT id FROM parent")); got != "" {
		t.Fatalf("the parent row survived: %q", got)
	}
}

// TestT039CascadeDeleteWaitsForAConcurrentLockingRead proves the derived write cannot write around a
// lock another session holds: the cascade waits, reports its budget, and completes once the reader
// commits.
func TestT039CascadeDeleteWaitsForAConcurrentLockingRead(t *testing.T) {
	f := t039Fixture(t)
	reader := t034Session(t, f, 5)
	deleter := t034Session(t, f, 1)

	f.must(reader, "BEGIN")
	f.must(reader, "SELECT id FROM cascade_child WHERE id=100 FOR UPDATE")

	// The parent delete triggers a cascaded delete of child 100, which the reader holds.
	if _, err := f.engine.Execute(deleter, "DELETE FROM parent WHERE id=1"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("the cascaded delete returned %v, want ErrLockWaitTimeout", err)
	}
	// Nothing was half-applied: the parent and the child are both still there.
	if got := t035Rows(t, f.must(deleter, "SELECT id FROM cascade_child WHERE id=100")); got != "100;" {
		t.Fatalf("the child row changed while the cascade waited: %q", got)
	}
	if got := t035Rows(t, f.must(deleter, "SELECT id FROM parent WHERE id=1")); got != "1;" {
		t.Fatalf("the parent row was removed by a statement that timed out: %q", got)
	}
	f.must(reader, "COMMIT")
	// With the reader gone the cascade applies.
	f.must(deleter, "DELETE FROM parent WHERE id=1")
	if got := t035Rows(t, f.must(deleter, "SELECT id FROM cascade_child WHERE id=100")); got != "" {
		t.Fatalf("the cascade did not remove the child row after the reader committed: %q", got)
	}
}

// TestT039SetNullDerivedWriteWaitsForAConcurrentLockingRead is the same rule for SET NULL: the derived
// update of the child row is a write and must meet the reader's lock, with the column set to NULL only
// after the reader is gone.
func TestT039SetNullDerivedWriteWaitsForAConcurrentLockingRead(t *testing.T) {
	f := t039Fixture(t)
	reader := t034Session(t, f, 5)
	deleter := t034Session(t, f, 1)

	f.must(reader, "BEGIN")
	f.must(reader, "SELECT id FROM null_child WHERE id=200 FOR UPDATE")
	if _, err := f.engine.Execute(deleter, "DELETE FROM parent WHERE id=1"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("the SET NULL derive returned %v, want ErrLockWaitTimeout", err)
	}
	// The child's foreign key is untouched while the derive cannot proceed.
	if got := t035Rows(t, f.must(deleter, "SELECT pid FROM null_child WHERE id=200")); got != "1;" {
		t.Fatalf("the derived update landed while it was blocked: %q", got)
	}
	f.must(reader, "COMMIT")
	f.must(deleter, "DELETE FROM parent WHERE id=1")
	// The column is NULL, asserted on the value itself rather than through a rendering.
	row := f.must(deleter, "SELECT pid FROM null_child WHERE id=200")
	if len(row.Rows) != 1 || len(row.Rows[0]) != 1 {
		t.Fatalf("the derived row read %#v, want one column of one row", row.Rows)
	}
	if row.Rows[0][0] != nil {
		t.Fatalf("the derive did not set the foreign key to NULL: %#v", row.Rows[0][0])
	}
}

// TestT042ReplaceClaimsTheRowItRemoves proves REPLACE claims the conflicting row before removing it.
// The row it deletes was never named by the statement, so before the duplicate paths took locks the
// removal claimed nothing at all.
func TestT042ReplaceClaimsTheRowItRemoves(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, UNIQUE KEY uq_v(v))",
		"INSERT INTO items VALUES(1,7)")
	session := t034Session(t, f, 5)

	table := t026LoadTable(t, f.engine, session, "items")
	row := t026Row(t, table, int64(1), int64(7))
	rowKey, ok := primaryKeyBytes(table, row)
	if !ok {
		t.Fatal("the row has no derivable primary key")
	}
	valueKey, ok := storage.IndexValueKey(table.Definition.Indexes[indexByName(t, table, "uq_v")], table.Definition.Columns, row)
	if !ok {
		t.Fatal("the row has no derivable unique value")
	}

	capture := t026NewCapture()
	capture.arm(t)
	f.must(session, "REPLACE INTO items VALUES(2,7)")

	bundles := capture.bundles()
	t039RequireComponent(t, bundles, storageengine.LockComponentRecord, sqllayout.Rows(table.ID), string(rowKey))
	t039RequireComponent(t, bundles, storageengine.LockComponentRecord, sqllayout.UniqueIndex(table.ID, "uq_v"), string(valueKey))
	// REPLACE removed the conflicting row and installed the new one.
	if got := t035Rows(t, f.must(session, "SELECT id,v FROM items")); got != "2,7;" {
		t.Fatalf("REPLACE left %q, want only the new row", got)
	}
}

// indexByName returns the position of one index in a table definition.
func indexByName(t *testing.T, table versionedTable, name string) int {
	t.Helper()
	for position, index := range table.Definition.Indexes {
		if index.Name == name {
			return position
		}
	}
	t.Fatalf("table %s has no index %s", table.CatalogName, name)
	return -1
}

// t039RequireComponent asserts that some captured bundle names one record component of a namespace
// with the given key. It is the derived-write counterpart of t026Pick: the point here is that the
// component exists at all, not which bundle carried it.
func t039RequireComponent(t *testing.T, bundles [][]storageengine.LockComponent, kind storageengine.LockComponentKind, space, key string) {
	t.Helper()
	for _, bundle := range bundles {
		for _, component := range bundle {
			if component.Kind == kind && component.Space == space && string(component.Key) == key {
				return
			}
		}
	}
	t.Fatalf("no captured bundle claims %s %s/%q; captured: %s", kind, space, key, t026DescribeBundles(bundles))
}
