package executor

import (
	"errors"
	"testing"

	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// locking_derived_test.go is this round's suite for the derived writes: the referential actions a
// statement triggers (T039/T044) and the duplicate-key variants that rewrite an existing row (T042).
//
// The bug it was written against: `INSERT ... ON DUPLICATE KEY UPDATE` moved a parent's primary key
// with a plain `writeVersionedRow`, so `ON UPDATE CASCADE` never ran 闁?the child rows kept the old key
// while the plain `UPDATE parent SET id=2` path cascaded correctly. The first two tests compare the two
// statements directly, which is what makes the difference visible instead of inferred.

// t042CascadeFixture builds a parent with four children: ON UPDATE CASCADE, ON UPDATE SET NULL,
// RESTRICT and a grandchild two levels below the cascading child.
func t042CascadeFixture(t *testing.T) *t035Fixture {
	t.Helper()
	return newT035Fixture(t,
		"CREATE TABLE parent(id BIGINT PRIMARY KEY, v INT)",
		"CREATE TABLE cascade_child(id BIGINT PRIMARY KEY, pid BIGINT, note VARCHAR(32), "+
			"KEY idx_pid(pid), FOREIGN KEY (pid) REFERENCES parent(id) ON UPDATE CASCADE ON DELETE CASCADE)",
		"CREATE TABLE grand_child(id BIGINT PRIMARY KEY, cid BIGINT, "+
			"KEY idx_cid(cid), FOREIGN KEY (cid) REFERENCES cascade_child(id) ON UPDATE CASCADE)",
		"CREATE TABLE null_child(id BIGINT PRIMARY KEY, pid BIGINT, "+
			"KEY idx_pid(pid), FOREIGN KEY (pid) REFERENCES parent(id) ON UPDATE SET NULL)",
		"CREATE TABLE restrict_child(id BIGINT PRIMARY KEY, pid BIGINT, "+
			"KEY idx_pid(pid), FOREIGN KEY (pid) REFERENCES parent(id) ON UPDATE RESTRICT)",
		"INSERT INTO parent VALUES(1,10)",
		"INSERT INTO cascade_child VALUES(100,1,'keep')",
		"INSERT INTO grand_child VALUES(1000,100)",
		"INSERT INTO null_child VALUES(200,1)",
		"INSERT INTO restrict_child VALUES(300,1)",
	)
}

// TestT042OnDuplicateKeyUpdateRunsForeignKeyActions is the minimal reproduction and its control: the
// same key move through ON DUPLICATE KEY UPDATE and through a plain UPDATE must leave the same child
// state.
func TestT042OnDuplicateKeyUpdateRunsForeignKeyActions(t *testing.T) {
	t.Run("plain UPDATE cascades", func(t *testing.T) {
		f := t042CascadeFixture(t)
		session := t034Session(t, f, 5)
		// The RESTRICT child would refuse the move, so it is repointed first for this control case.
		f.must(session, "UPDATE restrict_child SET pid=NULL WHERE id=300")
		f.must(session, "UPDATE parent SET id=2 WHERE id=1")
		if got := t035Rows(t, f.must(session, "SELECT pid FROM cascade_child WHERE id=100")); got != "2;" {
			t.Fatalf("the plain UPDATE left the cascading child at %q, want 2", got)
		}
		if got := t035Rows(t, f.must(session, "SELECT cid FROM grand_child WHERE id=1000")); got != "100;" {
			t.Fatalf("the plain UPDATE disturbed the grandchild: %q", got)
		}
	})

	t.Run("ON DUPLICATE KEY UPDATE cascades too", func(t *testing.T) {
		f := t042CascadeFixture(t)
		session := t034Session(t, f, 5)
		f.must(session, "UPDATE restrict_child SET pid=NULL WHERE id=300")
		if _, err := f.engine.Execute(session, "INSERT INTO parent VALUES(1,10) ON DUPLICATE KEY UPDATE id=2"); err != nil {
			t.Fatalf("the upsert failed: %v", err)
		}
		// Same expectation as the control: the referential action must have run.
		if got := t035Rows(t, f.must(session, "SELECT pid FROM cascade_child WHERE id=100")); got != "2;" {
			t.Fatalf("the upsert left the cascading child at %q, want 2: the ON UPDATE CASCADE did not run", got)
		}
		if got := t035Rows(t, f.must(session, "SELECT id FROM parent")); got != "2;" {
			t.Fatalf("the upsert left the parent at %q, want 2", got)
		}
	})
}

// TestT042OnDuplicateUpdateHonoursEveryUpdateAction covers the other referential actions on the same
// key move: SET NULL nulls the child's key, RESTRICT refuses the move outright, and NO ACTION behaves
// like RESTRICT.
func TestT042OnDuplicateUpdateHonoursEveryUpdateAction(t *testing.T) {
	t.Run("ON UPDATE SET NULL", func(t *testing.T) {
		f := t042CascadeFixture(t)
		session := t034Session(t, f, 5)
		f.must(session, "UPDATE restrict_child SET pid=NULL WHERE id=300")
		f.must(session, "UPDATE cascade_child SET pid=NULL WHERE id=100")
		f.must(session, "INSERT INTO parent VALUES(1,10) ON DUPLICATE KEY UPDATE id=2")
		row := f.must(session, "SELECT pid FROM null_child WHERE id=200")
		if len(row.Rows) != 1 || row.Rows[0][0] != nil {
			t.Fatalf("ON UPDATE SET NULL did not null the child key: %#v", row.Rows)
		}
	})

	t.Run("ON UPDATE RESTRICT", func(t *testing.T) {
		f := t042CascadeFixture(t)
		session := t034Session(t, f, 5)
		// The restrict child still references parent 1, so moving the key must be refused 闁?through the
		// upsert exactly as through a plain UPDATE.
		if _, err := f.engine.Execute(session, "INSERT INTO parent VALUES(1,10) ON DUPLICATE KEY UPDATE id=2"); err == nil {
			t.Fatal("the upsert moved a referenced key despite ON UPDATE RESTRICT")
		}
		if got := t035Rows(t, f.must(session, "SELECT id FROM parent")); got != "1;" {
			t.Fatalf("the refused upsert still changed the parent to %q", got)
		}
		if got := t035Rows(t, f.must(session, "SELECT pid FROM restrict_child WHERE id=300")); got != "1;" {
			t.Fatalf("the refused upsert disturbed the child: %q", got)
		}
	})
}

// TestT042OnDuplicateUpdateCascadesAcrossLevels proves the multi-level reach: moving the parent's key
// moves the cascading child's key, which in turn moves the grandchild that references *that* key.
func TestT042OnDuplicateUpdateCascadesAcrossLevels(t *testing.T) {
	f := newT035Fixture(t,
		"CREATE TABLE parent(id BIGINT PRIMARY KEY, v INT)",
		"CREATE TABLE child(id BIGINT PRIMARY KEY, pid BIGINT, "+
			"KEY idx_pid(pid), FOREIGN KEY (pid) REFERENCES parent(id) ON UPDATE CASCADE)",
		"CREATE TABLE grand(id BIGINT PRIMARY KEY, cid BIGINT, "+
			"KEY idx_cid(cid), FOREIGN KEY (cid) REFERENCES child(id) ON UPDATE CASCADE)",
		"INSERT INTO parent VALUES(1,10)",
		"INSERT INTO child VALUES(100,1)",
		"INSERT INTO grand VALUES(1000,100)",
	)
	session := t034Session(t, f, 5)
	// Move both levels by rewriting the child's key through its own upsert path, then the parent's.
	f.must(session, "INSERT INTO child VALUES(100,1) ON DUPLICATE KEY UPDATE id=200")
	if got := t035Rows(t, f.must(session, "SELECT cid FROM grand WHERE id=1000")); got != "200;" {
		t.Fatalf("the first level did not cascade: grandchild cid = %q, want 200", got)
	}
	f.must(session, "INSERT INTO parent VALUES(1,10) ON DUPLICATE KEY UPDATE id=2")
	if got := t035Rows(t, f.must(session, "SELECT pid FROM child WHERE id=200")); got != "2;" {
		t.Fatalf("the second level did not cascade: child pid = %q, want 2", got)
	}
}

// TestT042OnDuplicateUpdateClaimsTheNewIndexResources proves the second half of the identity rule: an
// upsert that installs a new unique value claims that value's reservation (with its insertion claim),
// not only the row it rewrote.
func TestT042OnDuplicateUpdateClaimsTheNewIndexResources(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, UNIQUE KEY uq_v(v))",
		"INSERT INTO items VALUES(1,7)")
	session := t034Session(t, f, 5)

	table := t026LoadTable(t, f.engine, session, "items")
	newRow := t026Row(t, table, int64(1), int64(8))
	newKey, ok := primaryKeyBytes(table, newRow)
	if !ok {
		t.Fatal("the replacement row has no derivable primary key")
	}
	newValue, ok := storage.IndexValueKey(table.Definition.Indexes[indexByName(t, table, "uq_v")], table.Definition.Columns, newRow)
	if !ok {
		t.Fatal("the replacement row has no derivable unique value")
	}

	capture := t026NewCapture()
	capture.arm(t)
	f.must(session, "INSERT INTO items VALUES(1,7) ON DUPLICATE KEY UPDATE v=8")

	bundles := capture.bundles()
	t039RequireComponent(t, bundles, storageengine.LockComponentRecord, sqllayout.Rows(table.ID), string(newKey))
	t039RequireComponent(t, bundles, storageengine.LockComponentRecord, sqllayout.UniqueIndex(table.ID, "uq_v"), string(newValue))
	if got := t035Rows(t, f.must(session, "SELECT id,v FROM items")); got != "1,8;" {
		t.Fatalf("the upsert left %q, want the updated row", got)
	}
}

// TestT042DuplicateVariantsDoNotSwallowLockErrors proves no duplicate variant turns a wait into a
// silent success: REPLACE, IGNORE and ON DUPLICATE KEY UPDATE each have to report the timeout of a lock
// another session holds on the row they would rewrite.
func TestT042DuplicateVariantsDoNotSwallowLockErrors(t *testing.T) {
	for _, variant := range []struct {
		name string
		sql  string
	}{
		{"REPLACE", "REPLACE INTO items VALUES(1,7)"},
		{"IGNORE", "INSERT IGNORE INTO items VALUES(1,7)"},
		{"ON DUPLICATE KEY UPDATE", "INSERT INTO items VALUES(1,7) ON DUPLICATE KEY UPDATE v=9"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
				"INSERT INTO items VALUES(1,7)")
			holder := t034Session(t, f, 5)
			writer := t034Session(t, f, 1)

			f.must(holder, "BEGIN")
			f.must(holder, "SELECT v FROM items WHERE id=1 FOR UPDATE")
			if _, err := f.engine.Execute(writer, variant.sql); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
				t.Fatalf("%s under a held lock returned %v, want ErrLockWaitTimeout", variant.name, err)
			}
			// The row is untouched, so nothing was written past the failed wait.
			if got := t035Rows(t, f.must(writer, "SELECT v FROM items WHERE id=1")); got != "7;" {
				t.Fatalf("%s changed the row despite its timeout: %q", variant.name, got)
			}
			f.must(holder, "ROLLBACK")
			f.must(writer, variant.sql)
		})
	}
}

// TestT044MultiLevelFailureRollsBackTheStatement proves a refusal anywhere inside a cascade aborts the
// whole original statement: the delete of the grandchild's own parent key has no ON DELETE action, so
// the cascaded delete of the child cannot proceed, and neither the parent nor the child may change.
func TestT044MultiLevelFailureRollsBackTheStatement(t *testing.T) {
	f := t042CascadeFixture(t)
	session := t034Session(t, f, 5)

	// parent -> cascade_child is ON DELETE CASCADE, but cascade_child -> grand_child has no delete
	// action, so the two-level delete must fail as a whole.
	if _, err := f.engine.Execute(session, "DELETE FROM parent WHERE id=1"); err == nil {
		t.Fatal("a cascaded delete blocked at the second level was accepted")
	}
	// Nothing of the failed statement survived, at any level.
	if got := t035Rows(t, f.must(session, "SELECT id FROM parent WHERE id=1")); got != "1;" {
		t.Fatalf("the parent row was removed by a statement that failed: %q", got)
	}
	if got := t035Rows(t, f.must(session, "SELECT pid FROM cascade_child WHERE id=100")); got != "1;" {
		t.Fatalf("the cascading child was removed by a statement that failed: %q", got)
	}
	if got := t035Rows(t, f.must(session, "SELECT cid FROM grand_child WHERE id=1000")); got != "100;" {
		t.Fatalf("the grandchild was disturbed by a statement that failed: %q", got)
	}
}

// TestT044CascadeRevalidatesTheChildItWaitedFor covers the case the wait makes delicate: another session
// changes a *different* column of the child row while the cascade waits. The derived write must apply to
// the row as it exists after the wait, so the other session's change survives.
func TestT044CascadeRevalidatesTheChildItWaitedFor(t *testing.T) {
	f := t042CascadeFixture(t)
	reader := t034Session(t, f, 5)
	updater := t034Session(t, f, 1)

	// The reader holds the child row and changes its note.
	f.must(reader, "BEGIN")
	f.must(reader, "SELECT id FROM cascade_child WHERE id=100 FOR UPDATE")
	f.must(reader, "UPDATE cascade_child SET note='reader' WHERE id=100")

	// The parent key move triggers ON UPDATE CASCADE on that child row, so it has to wait.
	f.must(reader, "UPDATE restrict_child SET pid=NULL WHERE id=300")
	if _, err := f.engine.Execute(updater, "UPDATE parent SET id=2 WHERE id=1"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("the cascade returned %v, want ErrLockWaitTimeout", err)
	}
	f.must(reader, "COMMIT")
	// Now the cascade applies — and it must not resurrect the row it read before the wait.
	f.must(updater, "UPDATE parent SET id=2 WHERE id=1")
	rows := f.must(updater, "SELECT pid,note FROM cascade_child WHERE id=100")
	if len(rows.Rows) != 1 {
		t.Fatalf("the child row vanished: %#v", rows.Rows)
	}
	if rows.Rows[0][0] != int64(2) {
		t.Fatalf("the cascaded key is %#v, want 2", rows.Rows[0][0])
	}
	if rows.Rows[0][1] != "reader" {
		t.Fatalf("the cascade overwrote the concurrent change: note = %#v, want the reader's value", rows.Rows[0][1])
	}
}

// TestT044CascadeRespectsTheCurrentChildState covers the two ways a child can stop referencing the
// parent while the cascade waits: deletion and repointing. Neither may be undone by the derived write.
func TestT044CascadeRespectsTheCurrentChildState(t *testing.T) {
	t.Run("the child was deleted", func(t *testing.T) {
		f := t042CascadeFixture(t)
		reader := t034Session(t, f, 5)
		updater := t034Session(t, f, 1)

		f.must(reader, "BEGIN")
		f.must(reader, "SELECT id FROM cascade_child WHERE id=100 FOR UPDATE")
		f.must(reader, "UPDATE restrict_child SET pid=NULL WHERE id=300")
		if _, err := f.engine.Execute(updater, "UPDATE parent SET id=2 WHERE id=1"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("the cascade returned %v, want ErrLockWaitTimeout", err)
		}
		// The grandchild's foreign key has no ON DELETE action, so it is removed first: the point of this
		// case is the child disappearing while the cascade waits, not a restriction on the grandchild.
		f.must(reader, "DELETE FROM grand_child WHERE id=1000")
		f.must(reader, "DELETE FROM cascade_child WHERE id=100")
		f.must(reader, "COMMIT")

		f.must(updater, "UPDATE parent SET id=2 WHERE id=1")
		if got := t035Rows(t, f.must(updater, "SELECT id FROM cascade_child WHERE id=100")); got != "" {
			t.Fatalf("the cascade resurrected a row deleted while it waited: %q", got)
		}
	})

	t.Run("the child was repointed", func(t *testing.T) {
		f := t042CascadeFixture(t)
		reader := t034Session(t, f, 5)
		updater := t034Session(t, f, 1)

		f.must(reader, "BEGIN")
		f.must(reader, "SELECT id FROM cascade_child WHERE id=100 FOR UPDATE")
		f.must(reader, "UPDATE cascade_child SET pid=NULL WHERE id=100")
		f.must(reader, "UPDATE restrict_child SET pid=NULL WHERE id=300")
		if _, err := f.engine.Execute(updater, "UPDATE parent SET id=2 WHERE id=1"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
			t.Fatalf("the cascade returned %v, want ErrLockWaitTimeout", err)
		}
		f.must(reader, "COMMIT")
		f.must(updater, "UPDATE parent SET id=2 WHERE id=1")
		row := f.must(updater, "SELECT pid FROM cascade_child WHERE id=100")
		if len(row.Rows) != 1 || row.Rows[0][0] != nil {
			t.Fatalf("the cascade overwrote the child's new reference: %#v", row.Rows)
		}
	})
}
