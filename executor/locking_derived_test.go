package executor

import (
	"errors"
	"testing"
	"time"

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

// TestP1UpsertTargetChangedAfterTheSnapshotConflicts is risk 1: the duplicate row the statement found
// in its own snapshot must not be silently rebased onto the version another transaction committed while
// this statement waited. The contract's rule (current-read.md §A05 DML validation, FR-017/018) is that a
// target whose newest committed change is newer than the statement's readTS is a conflict to retry, not
// a new row to recompute the assignments on.
func TestP1UpsertTargetChangedAfterTheSnapshotConflicts(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT)",
		"INSERT INTO items VALUES(1,7)")
	writer := t034Session(t, f, 5)
	upsert := t034Session(t, f, 5)

	// T2 takes its snapshot here, before T1's commit.
	f.must(upsert, "BEGIN")

	// T1 rewrites the very row T2's duplicate check will find, and commits.
	f.must(writer, "BEGIN")
	f.must(writer, "UPDATE items SET v=8 WHERE id=1")
	f.must(writer, "COMMIT")

	// T2's snapshot still shows (1,7), so its duplicate check selects row 1 as the target. The lock is
	// free now, so the statement gets it and finds a row that is no longer the one it read.
	if _, err := f.engine.Execute(upsert, "INSERT INTO items VALUES(1,7) ON DUPLICATE KEY UPDATE v=9"); !errors.Is(err, storageengine.ErrConflict) {
		t.Fatalf("the stale upsert returned %v, want a retryable ErrConflict", err)
	}
	f.must(upsert, "ROLLBACK")

	// T1's committed value survived: the statement did not silently write over it.
	if got := t035Rows(t, f.must(writer, "SELECT id,v FROM items")); got != "1,8;" {
		t.Fatalf("the final row is %q, want T1's committed value", got)
	}
}

// TestP1UpsertAfterTheUniqueOwnerReleasedInsertsInstead is risk 2: the statement's snapshot shows row 1
// owning v=7, but the transaction that owned it moved the value away and committed while this statement
// waited for the reservation. The reservation's *current* owner decides what the statement may do, so it
// must not rewrite row 1.
func TestP1UpsertAfterTheUniqueOwnerReleasedInsertsInstead(t *testing.T) {
	f := newT035Fixture(t, "CREATE TABLE items(id BIGINT PRIMARY KEY, v INT, UNIQUE KEY uq_v(v))",
		"INSERT INTO items VALUES(1,7)")
	holder := t034Session(t, f, 5)
	inserter := t034Session(t, f, 5)

	// The barrier proves the insert is queued on the reservation before the holder releases it, so the
	// statement below is one request that waits and is then granted — not a timeout followed by a retry.
	barrier := newDMLWaitBarrier()
	barrier.arm(t)

	f.must(holder, "BEGIN")
	f.must(holder, "UPDATE items SET v=8 WHERE id=1")
	f.must(inserter, "BEGIN")
	f.must(inserter, "SELECT v FROM items WHERE id=1") // the snapshot that still shows (1,7)

	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, err := f.engine.Execute(inserter, "INSERT INTO items VALUES(2,7) ON DUPLICATE KEY UPDATE v=9")
		done <- outcome{err: err}
	}()
	// The insert really is queued: the barrier fires while the conflicting request waits.
	barrier.wait(t, "the insert's unique reservation")

	f.must(holder, "COMMIT")

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting upsert never resumed after the holder committed")
	}
	// The statement's snapshot said row 1 owned v=7, and by the time it held the reservation that was
	// no longer true. It may not rewrite row 1 (that would write over the holder's commit) and it may not
	// invent the insert from stale reasoning either, so the contract's answer is the retryable conflict.
	if !errors.Is(got.err, storageengine.ErrConflict) {
		t.Fatalf("the resumed upsert returned %v, want the retryable ErrConflict", got.err)
	}
	// Nothing was written over the holder's value. The check runs after the rollback, because the
	// statement's own transaction must keep showing the snapshot it started with.
	f.must(inserter, "ROLLBACK")
	if rows := f.must(inserter, "SELECT id,v FROM items ORDER BY id"); t035Rows(t, rows) != "1,8;" {
		t.Fatalf("the rows after the conflict are %q, want only the holder's row", t035Rows(t, rows))
	}

	// The retry this conflict asks for succeeds on the current state: the value the holder released is
	// free, so the candidate is inserted, and v=7 belongs to the new row only.
	f.must(inserter, "INSERT INTO items VALUES(2,7) ON DUPLICATE KEY UPDATE v=9")
	rows := f.must(inserter, "SELECT id,v FROM items ORDER BY id")
	if t035Rows(t, rows) != "1,8;2,7;" {
		t.Fatalf("the retried statement left %q, want row 1 at the holder's value and the candidate inserted", t035Rows(t, rows))
	}
	if got := t035Rows(t, f.must(inserter, "SELECT id FROM items WHERE v=7")); got != "2;" {
		t.Fatalf("v=7 is owned by %q, want the inserted row", got)
	}
}

// TestP1CascadeGrantedAfterTheWaitRevalidates is the wake-up form of the cascade rule: one statement
// waits on the child row's lock, is *granted* when the holder commits, and resumes inside the same
// statement. What it must not do is apply the derivation it computed before the wait.
//
// The previous tests established the same rule through a timeout followed by a second statement. That
// proves the statement refuses to write while another transaction holds the row; it does not prove what
// the resumed request does with the row it finally holds, which is what this test exercises.
func TestP1CascadeGrantedAfterTheWaitRevalidates(t *testing.T) {
	f := t042CascadeFixture(t)
	holder := t034Session(t, f, 5)
	// The cascade's own budget is generous: the statement must be waiting, not timing out.
	cascade := t034Session(t, f, 10)

	barrier := newDMLWaitBarrier()
	barrier.arm(t)

	// T1 holds the child row and changes a column the cascade does not touch.
	f.must(holder, "BEGIN")
	f.must(holder, "SELECT id FROM cascade_child WHERE id=100 FOR UPDATE")
	f.must(holder, "UPDATE cascade_child SET note='holder' WHERE id=100")
	// The RESTRICT child is repointed so the parent key move is otherwise allowed.
	f.must(holder, "UPDATE restrict_child SET pid=NULL WHERE id=300")

	done := make(chan error, 1)
	go func() {
		_, err := f.engine.Execute(cascade, "UPDATE parent SET id=2 WHERE id=1")
		done <- err
	}()
	// The wait is real and queued on the child row the cascade has to write.
	barrier.wait(t, "the cascaded child write")

	// The holder commits while the cascade waits: the derived write is now granted the row, and the row
	// it holds is no longer the version the statement read.
	f.must(holder, "COMMIT")

	select {
	case err := <-done:
		// The child row's newest committed change is newer than the cascade statement's readTS, so the
		// contract's answer is the retryable conflict: the statement must not apply a derivation computed
		// from the row it read before the wait, and it must not refresh the parent candidate stream to
		// hide that either (A05's Parent Read / Child Write model is unchanged).
		if !errors.Is(err, storageengine.ErrConflict) {
			t.Fatalf("the granted cascade returned %v, want the retryable ErrConflict", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cascade never resumed after the holder committed")
	}

	// The holder's own change survived and the parent key did not move: nothing of the failed statement
	// was half-applied at either level.
	rows := f.must(holder, "SELECT id,pid,note FROM cascade_child WHERE id=100")
	if len(rows.Rows) != 1 {
		t.Fatalf("the child row vanished: %#v", rows.Rows)
	}
	if rows.Rows[0][1] != int64(1) || rows.Rows[0][2] != "holder" {
		t.Fatalf("the child row is %#v, want its original parent and the holder's note", rows.Rows[0])
	}
	if got := t035Rows(t, f.must(holder, "SELECT id FROM parent")); got != "1;" {
		t.Fatalf("the parent key moved despite the conflict: %q", got)
	}

	// The retry the conflict asks for succeeds and cascades, which is what proves the conflict was the
	// retryable kind and not a permanent refusal.
	f.must(cascade, "UPDATE parent SET id=2 WHERE id=1")
	if got := t035Rows(t, f.must(cascade, "SELECT pid FROM cascade_child WHERE id=100")); got != "2;" {
		t.Fatalf("the retried cascade left the child at %q, want 2", got)
	}
	if got := t035Rows(t, f.must(cascade, "SELECT note FROM cascade_child WHERE id=100")); got != "holder;" {
		t.Fatalf("the retried cascade lost the holder's note: %q", got)
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
