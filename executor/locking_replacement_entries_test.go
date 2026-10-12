package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"gbaselite/sqllayout"
	"gbaselite/storageengine"
)

// locking_replacement_entries_test.go is the P2 suite: an UPDATE must claim an insertion intention
// only for index entries that are *new keys*, and a Gap must block only those.
//
// The defect it was written against: `claimModifyReplacement` asked for the insertion half of every
// index entry of the updated row, including entries whose key was exactly the one the row already had.
// A Gap promises to block the creation of a key inside its bounds and to leave an existing value
// alone, so claiming an intention for an existing entry made a Gap hold up a rewrite that created no
// key at all. Measured before the fix, with only the index interval held and no row lock anywhere: an
// `UPDATE` that changed one non-indexed column timed out after its full 2s budget, while the same
// statement with no gap ran in ~20ms.
//
// The interval is held through the neutral lock API rather than through SQL, and that is what makes the
// measurement meaningful. A SQL range read protects its interval *and* takes the record lock of every
// row the interval matched, so a false block on an index entry would be indistinguishable from the row
// lock that was always going to block. Holding only the Gap isolates the entry claim.
//
// Both directions are asserted, because only one of them is the bug: the entries that are *not* new
// must stop blocking, and the entries that *are* new must keep blocking. A fix that simply dropped the
// intention everywhere would pass the first half and fail the second.

// p2Target is one index entry claim: the namespace it lives in and the key it names.
type p2Target struct {
	tableID  string
	space    string
	indexID  string
	entryKey []byte
}

// p2Session opens a session with a short lock-wait budget, so a blocked statement fails quickly
// instead of holding the suite for the contract's default.
func p2Session(t *testing.T, e *Engine, seconds int) *Session {
	t.Helper()
	session := &Session{}
	for _, sql := range []string{"USE test", "SET SESSION innodb_lock_wait_timeout = " + itoa(seconds)} {
		if _, err := e.Execute(session, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return session
}

// p2Capture runs one statement inside a transaction that is then rolled back, so the claims it makes
// can be learned without applying its effect.
//
// wanted selects which half of the claim to read, and it matters: a statement claims the entries it
// *removes* with a record claim and separately the entries it *installs* with an insertion intention.
// Reading the wrong half would bracket the old key and silently turn a migration case into a case that
// proves nothing.
func p2Capture(t *testing.T, f *uniqueFixture, statement, indexID string, wanted storageengine.LockMode) p2Target {
	t.Helper()
	session := p2Session(t, f.engine, 20)
	f.must(session, "BEGIN")
	claims := captureLockComponents(t, f, session, statement)
	f.must(session, "ROLLBACK")

	read, err := f.engine.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	table, _, _, err := loadVersionedTable(read, session, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range claims {
		if claim.IndexID == indexID && claim.Mode == wanted && len(claim.Key) > 0 {
			return p2Target{tableID: table.ID, space: claim.Space, indexID: indexID, entryKey: claim.Key}
		}
	}
	t.Fatalf("%s claimed no %s on index %s: %+v", statement, wanted, indexID, claims)
	return p2Target{}
}

// p2Bracket is the interval that contains one exact entry key and as little else as possible: a proper
// prefix of the key sorts strictly below it, and the key with one more zero byte sorts strictly above.
func p2Bracket(key []byte) (lower, upper []byte) {
	return append([]byte(nil), key[:len(key)-1]...), append(append([]byte(nil), key...), 0x00)
}

// p2HoldGap holds one interval on an index namespace from a separate root, and returns a release.
func p2HoldGap(t *testing.T, f *uniqueFixture, target p2Target, lower, upper []byte) func() {
	t.Helper()
	ctx := context.Background()
	rawTx, err := f.engine.Backend.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := rawTx.(interface {
		LockingTxn() (storageengine.LockingTxn, bool)
	})
	if !ok {
		t.Fatal("the backend exposes no locking capability")
	}
	locks, available := provider.LockingTxn()
	if !available {
		t.Fatal("the backend exposes no lock manager")
	}
	scope, err := locks.BeginLockScope(ctx, storageengine.LockScopeStatement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locks.Acquire(ctx, scope, storageengine.LockRequest{Components: []storageengine.LockComponent{
		{Mode: storageengine.LockModeIX, Kind: storageengine.LockComponentTableIntent,
			Space: sqllayout.Rows(target.tableID), TableID: target.tableID},
		{Mode: storageengine.LockModeGap, Kind: storageengine.LockComponentInterval,
			Space: target.space, TableID: target.tableID, IndexID: target.indexID,
			Lower: lower, Upper: upper},
	}}); err != nil {
		t.Fatalf("holding the index gap: %v", err)
	}
	return func() { _ = rawTx.Rollback() }
}

// p2Schema is the fixture: a unique index over two columns and a single-column secondary index, so a
// statement touches two different index encodings.
const p2Schema = "CREATE TABLE t(id BIGINT PRIMARY KEY, v INT, a INT, b INT, " +
	"UNIQUE KEY uq_ab(a,b), KEY idx_a(a))"

// p2Seed gives two rows whose index entries are far enough apart to bracket individually.
const p2Seed = "INSERT INTO t VALUES(1,1,7,20),(2,2,50,60)"

// TestP2AnUnchangedIndexEntryIsNotAnInsertion is the structural half: what the statement actually asks
// for, for each of the shapes the round names.
//
// The two indexes are deliberately different shapes, and the primary-key case shows why. A secondary
// entry's key ends with its owner, so moving the primary key gives every secondary entry a new key and
// therefore a genuine insertion. A unique entry's key is the *value alone* — the owner is that entry's
// value, not part of its key — so moving the primary key onto the same unique value addresses the same
// reservation, and the write path updates that reservation in place rather than creating a key. A test
// that expected an intention on both would be demanding a false block.
func TestP2AnUnchangedIndexEntryIsNotAnInsertion(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		// wantIntention says, per index, whether this statement creates a new key in it.
		wantIntention map[string]bool
	}{
		{
			"a non-indexed value changes: no key is created",
			"UPDATE t SET v=99 WHERE id=1",
			map[string]bool{"uq_ab": false, "idx_a": false},
		},
		{
			"an indexed value migrates: both indexes get a new key",
			"UPDATE t SET a=8 WHERE id=1",
			map[string]bool{"uq_ab": true, "idx_a": true},
		},
		{
			"the primary key moves: the owner-suffixed entry is new, the value reservation is not",
			"UPDATE t SET id=3 WHERE id=1",
			map[string]bool{"uq_ab": false, "idx_a": true},
		},
		{
			"an indexed value is rewritten to itself: no key is created",
			"UPDATE t SET a=a WHERE id=1",
			map[string]bool{"uq_ab": false, "idx_a": false},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newUniqueFixture(t, p2Schema, p2Seed)
			session := p2Session(t, f.engine, 20)
			claims := captureLockComponents(t, f, session, testCase.sql)

			intentions := map[string]int{}
			records := map[string]int{}
			for _, claim := range claims {
				if claim.IndexID == "" {
					continue
				}
				if claim.Mode == storageengine.LockModeInsertIntention {
					intentions[claim.IndexID]++
					continue
				}
				if claim.Mode == storageengine.LockModeX {
					records[claim.IndexID]++
				}
			}
			for indexID, want := range testCase.wantIntention {
				// The record claim is what decides contention on an entry, and it must travel in every
				// case: dropping the intention must never mean dropping the protection.
				if records[indexID] == 0 {
					t.Fatalf("%s claimed no record lock on index %s: %+v", testCase.sql, indexID, claims)
				}
				if got := intentions[indexID] > 0; got != want {
					t.Fatalf("%s claimed %d insertion intentions on %s, want a new key reported as %v: %+v",
						testCase.sql, intentions[indexID], indexID, want, claims)
				}
			}
		})
	}
}

// TestP2AGapBlocksOnlyGenuineNewIndexKeys is the behavioural half, measured against a held interval.
func TestP2AGapBlocksOnlyGenuineNewIndexKeys(t *testing.T) {
	cases := []struct {
		name string
		// bracketStatement identifies the entry whose key the held interval brackets; empty means the
		// measured statement itself.
		bracketStatement string
		bracket          storageengine.LockMode
		update           string
		wantBlocked      bool
	}{
		{
			"an unchanged entry is not blocked by an interval over it",
			"", storageengine.LockModeX, "UPDATE t SET v=99 WHERE id=1", false,
		},
		{
			"an entry that migrates into the interval is blocked",
			"", storageengine.LockModeInsertIntention, "UPDATE t SET a=8 WHERE id=1", true,
		},
		{
			"an entry the update removes is not blocked by an interval over it",
			"", storageengine.LockModeX, "UPDATE t SET a=8 WHERE id=1", false,
		},
		{
			"a primary-key move creates new owner entries and is blocked",
			"", storageengine.LockModeInsertIntention, "UPDATE t SET id=3 WHERE id=1", true,
		},
		{
			// The control: the interval is real and narrow, and it brackets another row's entry, so an
			// update of row 1 must be unaffected. Without this the "not blocked" cases above could be
			// explained by a gap that was never effective in the first place.
			"an interval over an unrelated row's entry blocks nothing",
			"UPDATE t SET v=v WHERE id=2", storageengine.LockModeX, "UPDATE t SET v=99 WHERE id=1", false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newUniqueFixture(t, p2Schema, p2Seed)
			bracketStatement := testCase.bracketStatement
			if bracketStatement == "" {
				bracketStatement = testCase.update
			}
			target := p2Capture(t, f, bracketStatement, "idx_a", testCase.bracket)
			lower, upper := p2Bracket(target.entryKey)
			release := p2HoldGap(t, f, target, lower, upper)
			defer release()

			updater := p2Session(t, f.engine, 2)
			started := time.Now()
			_, err := f.engine.Execute(updater, testCase.update)
			elapsed := time.Since(started)
			blocked := errors.Is(err, storageengine.ErrLockWaitTimeout)
			if blocked != testCase.wantBlocked {
				t.Fatalf("%s: err=%v after %v, want blocked=%v", testCase.update, err, elapsed.Round(time.Millisecond), testCase.wantBlocked)
			}
			if err != nil && !blocked {
				t.Fatalf("%s: unexpected error %v", testCase.update, err)
			}
		})
	}
}

// TestP2TheUnchangedEntryStillMeetsAnotherWriter is the safety half of the fix: removing the insertion
// intention from an unchanged entry must not remove the record claim, or two writers of the same entry
// would stop meeting each other. The interval rule and the record rule are independent — a gap does not
// conflict with a record — so the previous two tests cannot see this, and this one has to.
func TestP2TheUnchangedEntryStillMeetsAnotherWriter(t *testing.T) {
	f := newUniqueFixture(t, p2Schema, p2Seed)
	holder := p2Session(t, f.engine, 20)
	waiter := p2Session(t, f.engine, 2)

	// The holder rewrites row 1 without touching any indexed column, so every entry it claims is the
	// entry the row already had.
	f.must(holder, "BEGIN")
	f.must(holder, "UPDATE t SET v=11 WHERE id=1")

	// The waiter rewrites the same row: it must meet the holder on the row and on the same entries.
	if _, err := f.engine.Execute(waiter, "UPDATE t SET v=12 WHERE id=1"); !errors.Is(err, storageengine.ErrLockWaitTimeout) {
		t.Fatalf("a second writer of the same row = %v, want ErrLockWaitTimeout from the holder's claim", err)
	}
	f.must(holder, "COMMIT")
}
