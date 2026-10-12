package executor

import (
	"bytes"
	"context"
	"errors"
	"gbaselite/physical"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// This file owns the unified INSERT mutation path. INSERT VALUES and INSERT SELECT
// both lower their source into physical.Operator[InsertCandidate] and hand it to
// InsertOperator, so exactly one implementation assembles, validates and writes an
// inserted row.
//
// What stays outside this operator, deliberately:
//
//   - source execution (a VALUES source, or the SELECT pipeline bound against the
//     parent statement snapshot);
//   - the CHECK / UNIQUE / PRIMARY KEY / secondary index / FK write logic, which is
//     still reached through the existing writeInsertedRow;
//   - statement transaction commit and publishing session.LastInsertID.
//
// The operator writes only through the statement child transaction it is given.

// InsertCandidate is one logical row waiting to be inserted.
//
// INSERT VALUES and INSERT SELECT both lower their source onto this model, so the
// final insert write exists once. Values is the destination row in table column
// order with defaults and NULLs already laid out; Ordinal is the statement-local
// input position, which the generated fallback key and the auto-increment
// reservation size are both derived from.
//
// Ordinal must be assigned in input order by the source. It is not a row identity:
// two candidates may legitimately carry identical Values, and INSERT never
// deduplicates.
type InsertCandidate struct {
	Values  storage.Row
	Ordinal uint64
}

// Target reports that an insert has no pre-existing physical target.
//
// INSERT is the one modify family that creates rows instead of addressing them, so
// it deliberately returns an invalid identity: target deduplication must never
// collapse two INSERT candidates, and two identical VALUES rows are two rows.
func (InsertCandidate) Target() (RowIdentity, bool) {
	return RowIdentity{}, false
}

// insertCounters is the statement-local auto-increment state.
//
// It is owned by the InsertOperator and shared by every row of one statement. The
// state exists per statement rather than per row, so a statement that generated ids
// for earlier rows cannot re-reserve them for later ones.
//
// Reservation gaps are allowed and expected: a reserved block the statement does not
// fully consume (because a later row failed, or because a conflicting row was skipped)
// is not given back, matching the documented MVCC auto-increment gap behaviour of both
// INSERT paths before this refactor.
type insertCounters struct {
	// floors holds the highest explicit value seen per column; it must be pushed to
	// the backend counter before the next reservation.
	floors []uint64
	// sent holds the floor already pushed to the backend counter.
	sent []uint64
	// next is the next id this statement will hand out per column.
	next []uint64
	// last is the last id of the currently reserved block per column.
	last []uint64
}

func newInsertCounters(columns int) *insertCounters {
	return &insertCounters{
		floors: make([]uint64, columns),
		sent:   make([]uint64, columns),
		next:   make([]uint64, columns),
		last:   make([]uint64, columns),
	}
}

// advance pushes the highest explicit auto-increment value seen so far to the backend
// counter, so a later reservation cannot hand out a value that was already used
// explicitly.
func (c *insertCounters) advance(ctx context.Context, engine *Engine, definition versionedTable, columns []storage.Column, position int) error {
	if c.floors[position] <= c.sent[position] {
		return nil
	}
	if err := storageengine.AdvanceCounter(ctx, engine.Backend, definition.counterKey(columns[position].Name), c.floors[position]); err != nil {
		return err
	}
	c.sent[position] = c.floors[position]
	return nil
}

// flush pushes every column's floor to the backend counter. A statement must call it
// once it has finished inserting, so an explicit auto-increment value stays visible
// after the statement ends.
func (c *insertCounters) flush(ctx context.Context, engine *Engine, definition versionedTable, columns []storage.Column) error {
	for position := range columns {
		if err := c.advance(ctx, engine, definition, columns, position); err != nil {
			return err
		}
	}
	return nil
}

// reserve hands out a block of ids for one column, pulling a fresh block from the
// backend counter when the current one is exhausted.
//
// A count greater than one models a source that knows how many rows it is about to
// insert (INSERT VALUES reserves the remaining rows of the statement in one call); a
// count of one models a streaming source whose size is unknown (INSERT SELECT). Both
// policies hand out the same sequence of ids, so the choice cannot change which id any
// row receives; it only decides how far the backend counter runs ahead.
func (c *insertCounters) reserve(ctx context.Context, engine *Engine, definition versionedTable, columns []storage.Column, position int, count uint64) (uint64, error) {
	if c.next[position] == 0 || c.next[position] > c.last[position] {
		if err := c.advance(ctx, engine, definition, columns, position); err != nil {
			return 0, err
		}
		reserved, err := storageengine.ReserveCounter(ctx, engine.Backend, definition.counterKey(columns[position].Name), count)
		if err != nil {
			return 0, err
		}
		c.next[position] = reserved
		c.last[position] = reserved + count - 1
	}
	id := c.next[position]
	c.next[position]++
	return id, nil
}

// InsertOperator writes every InsertCandidate produced by its source.
//
// It owns the statement-local auto-increment state (insertCounters) and the
// statement-local ModifyResult, but it never publishes that result to the session:
// LastInsertID may only be published after the statement child transaction commits,
// which is the caller's job.
type InsertOperator struct {
	// Input yields candidates in statement input order. InsertCandidate.Ordinal must
	// be the statement-local input position, because the generated fallback key and
	// the auto-increment reservation size are both derived from it.
	Input physical.Operator[InsertCandidate]

	// Target is the resolved destination: the column mapping plus the destination
	// definition the write path needs.
	Target *insertTarget
	// Write is the statement child transaction. The operator never commits it.
	Write storageengine.Txn
	// Engine supplies the backend auto-increment counter. It may be nil for a target
	// with no auto-increment column.
	Engine *Engine
	// Session supplies defaults, CURRENT_TIMESTAMP and the statement's conflict mode.
	Session *Session
	// Locks is the statement's lock scope. Every inserted row takes its record lock through it
	// before the write, so a concurrent duplicate waits on the same resource and is rejected by a
	// re-check afterwards instead of both rows being written.
	Locks *dmlLockScope
	// Mode is the statement's duplicate-key policy. The zero value is the plain
	// INSERT policy.
	Mode insertMode
	// Rows is the number of rows the source already knows it will produce, or 0 for a
	// streaming source. It only sizes an auto-increment reservation.
	Rows uint64

	// Result accumulates the statement's affected rows and first generated id.
	Result ModifyResult

	counters *insertCounters
}

// insertApplyHook, when non-nil, observes every candidate the unified INSERT path
// writes. The A05 regression tests install it to prove that INSERT VALUES and INSERT
// SELECT really do reach this single operator, and to count the rows that pass
// through it. It is nil in production and carries no behaviour of its own.
var insertApplyHook func(candidate InsertCandidate, engine string)

// Run writes every candidate and discards the operator's own output, which is what a
// terminal mutation operator does. The statement result is read from Result, and
// Publish must run before it is used.
func (o *InsertOperator) Run(ctx context.Context) error {
	modify := physical.Modify[InsertCandidate, struct{}]{Input: o.Input, Apply: func(ctx context.Context, candidate InsertCandidate) (struct{}, error) {
		return struct{}{}, o.apply(ctx, candidate)
	}}
	return modify.Run(ctx, func(struct{}) error { return nil })
}

// apply writes one candidate and accumulates the statement result.
//
// It is the single implementation both INSERT sources funnel through: keeping Run
// free of the row semantics is what lets a test wrap one candidate at a time.
func (o *InsertOperator) apply(ctx context.Context, candidate InsertCandidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.initCounters()
	if insertApplyHook != nil {
		insertApplyHook(candidate, o.Target.definition.CatalogName)
	}
	generated, generatedOK, err := o.generateAutoIncrement(ctx, candidate.Values, candidate.Ordinal)
	if err != nil {
		return err
	}
	// The record lock is taken before the write, never after: a lock acquired afterwards would
	// leave a window in which two transactions both believe the key is free.
	grant, err := o.lockCandidate(candidate)
	if err != nil {
		return err
	}
	// With the row and its unique values held, the uniqueness decision is re-derived from the
	// latest published index state. A decision taken before a wait is not evidence after it.
	if err := o.verifyUniqueValuesLocked(ctx, candidate, grant); err != nil {
		if errors.Is(err, errSkipInsertRow) {
			// IGNORE turned the duplicate into a skipped row: nothing is written, so nothing is
			// rebased and the affected count stays as it is.
			return nil
		}
		return err
	}
	// *Whether* a conflict exists is decided from the current state, because the snapshot's answer may
	// describe a value another transaction has since released. *Which row* a surviving conflict names
	// stays the one this statement's snapshot saw: recomputing the duplicate policy on a post-snapshot
	// row would rebase the statement onto data it never read (FR-017/018). The re-derivation below
	// therefore only drops conflicts that are gone and reports a conflict that moved to a new owner.
	conflicts, err := o.currentInsertConflicts(ctx, candidate, grant)
	if err != nil {
		return err
	}
	if len(conflicts) == 0 {
		// No conflict remains: the candidate is an ordinary insert, which is what a released unique
		// value means. The insertion claims were taken before the wait, so the identity is still held.
		if err := writeVersionedRow(ctx, o.Write, o.Target.definition, nil, nil, candidate.Values, heapKeyString(o.Write.ID(), candidate.Ordinal)); err != nil {
			return err
		}
		o.Result.AffectedRows++
		if generatedOK {
			o.Result.recordGenerated(generated, true)
		}
		return nil
	}
	outcome, err := writeInsertedRow(ctx, o.Write, o.Session, o.Locks, o.Target, o.Mode, candidate.Values, candidate.Ordinal, conflicts)
	if err != nil {
		return err
	}
	o.Result.AffectedRows += uint64(outcome.affected)
	if outcome.inserted && generatedOK {
		o.Result.recordGenerated(generated, true)
	}
	return nil
}

// currentInsertConflicts re-derives the duplicate conflicts of one candidate from the latest published
// state, under the claims this statement already holds.
//
// The snapshot's conflict list answers "what did this statement see", which is the wrong question after
// a wait: the unique value it saw as taken may have been released by the transaction it waited for. This
// function asks the reservation itself — the record every writer of a unique value claims — and the
// candidate's primary key:
//
//   - a unique conflict survives only while the reservation still names the row the snapshot found;
//   - a reservation that now names a *different* row is a conflict the statement cannot resolve from its
//     own snapshot, so it is refused with the retryable conflict the contract prescribes;
//   - a reservation that is gone means the value is free, and that conflict is dropped;
//   - a primary-key conflict survives while a row still occupies the key. Its *content* is left to the
//     snapshot: the duplicate policy keeps the row this statement read as its target, and
//     updateDuplicateInsertRow reports the conflict if that row has changed since.
func (o *InsertOperator) currentInsertConflicts(ctx context.Context, candidate InsertCandidate, grant storageengine.LockGrant) ([]insertConflict, error) {
	if o.Locks == nil {
		// Without a lock manager there is no claim to re-read under, so the snapshot's answer stands,
		// exactly as the unlocked backend has always behaved.
		return insertConflicts(o.Write, o.Target, candidate.Values)
	}
	snapshot, err := insertConflicts(o.Write, o.Target, candidate.Values)
	if err != nil {
		return nil, err
	}
	if len(snapshot) == 0 {
		return nil, nil
	}
	table := o.Target.definition
	survivors := make([]insertConflict, 0, len(snapshot))
	for _, conflict := range snapshot {
		if conflict.uniqueIndex < 0 {
			// A primary-key conflict is about the row at that key, and whether it is there now. Its
			// content is left to the snapshot: the policy keeps the row this statement read as its
			// target, and updateDuplicateInsertRow reports the conflict if that row has changed since.
			_, exists, err := o.Write.Table(table.ID).Get(conflict.key)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			survivors = append(survivors, conflict)
			continue
		}
		owner, exists, err := o.uniqueReservationOwner(ctx, table, conflict, grant)
		if err != nil {
			return nil, err
		}
		switch {
		case !exists:
			// The reservation is gone: the value this statement's snapshot saw as taken was released
			// while it waited. The statement cannot resolve that from its own snapshot — its duplicate
			// reasoning names a row that no longer owns the value, and the write path validates an insert
			// against that same stale view, where the value is still taken. The contract's answer for a
			// decision taken on released state is the retryable conflict, so the root re-reads the current
			// state instead of this statement inventing one (and instead of a permanent duplicate error
			// for a value that is in fact free).
			return nil, staleTargetConflict()
		case len(owner) > 0 && !bytes.Equal(owner, conflict.key):
			// The value now belongs to a different row than the one this statement's snapshot named.
			// Resolving that would mean recomputing on data this statement never read, so it is the
			// retryable conflict the contract prescribes.
			return nil, staleTargetConflict()
		default:
			survivors = append(survivors, conflict)
		}
	}
	return survivors, nil
}

// uniqueReservationOwner reads the current owner of one unique value under the statement's claim.
func (o *InsertOperator) uniqueReservationOwner(ctx context.Context, table versionedTable, conflict insertConflict, grant storageengine.LockGrant) ([]byte, bool, error) {
	if conflict.uniqueIndex < 0 || conflict.uniqueIndex >= len(table.Definition.Indexes) {
		return nil, false, nil
	}
	index := table.Definition.Indexes[conflict.uniqueIndex]
	if !index.Unique {
		return nil, false, nil
	}
	space := sqllayout.UniqueIndex(table.ID, index.Name)
	observation, err := o.Locks.readLockedValue(ctx, grant, space, conflict.valueKey)
	if err != nil {
		return nil, false, err
	}
	if !observation.Exists {
		return nil, false, nil
	}
	// The entry's value is the owning primary key, which is the encoding the write path installs.
	return observation.Value, true, nil
}

// lockCandidate takes the exclusive row lock one inserted row needs and returns the capability.
//
// The identity is derived from the candidate's own values, so it matches exactly what the write
// path will install. A candidate whose primary key cannot be derived is refused rather than
// written unprotected. The returned grant is what the locked uniqueness re-check reads under.
func (o *InsertOperator) lockCandidate(candidate InsertCandidate) (storageengine.LockGrant, error) {
	if o.Locks == nil {
		return storageengine.LockGrant{}, nil
	}
	table := o.Target.definition
	row := candidate.Values
	key, ok := primaryKeyBytes(table, row)
	if !ok {
		// A table with no primary key is written under the heap identity the insert path
		// synthesises from this statement's ordinal, so that is the identity to lock.
		key = heapKey(o.Write, candidate.Ordinal)
		if len(key) == 0 {
			return storageengine.LockGrant{}, errLockIdentityUnavailable
		}
	}
	extra, err := indexEntryLocks(table, key, row, true)
	if err != nil {
		return storageengine.LockGrant{}, err
	}
	// The row itself is being created, so its claim carries the insertion half as well: the insert
	// intention at the row's key is what a Gap over that range blocks. Its pair — the record lock of
	// the same key — is already in the bundle (`acquireRowLocksForKey` adds the primary row
	// component), and the lock manager refuses an intention that is not paired, so adding a second
	// record component here would only duplicate the same claim.
	extra = append(extra, insertionIntention(sqllayout.Rows(table.ID), table.ID, "", key)...)
	return o.Locks.acquireRowLocksForKey(table, key, extra)
}

// insertionIntention builds the intention half of a claim on one key.
//
// It is deliberately only half: every caller adds it beside the record lock of the same key that
// the bundle already carries (the primary row component, the unique value reservation, or the
// secondary entry). The manager validates the pairing (`MissingRowClaims`) and refuses an unpaired
// intention, so the two halves cannot drift apart — while building them once keeps a key from being
// claimed twice and a diagnostics view from reporting the same resource as two.
//
// The two halves answer different questions, which is why both exist: the intention is what a
// covering Gap blocks (FR-003/FR-005), and the record lock is what decides same-key contention
// (FR-005: "鍚岄敭绔炰簤鐢?record X/鍞竴閿?reservation 鍐冲畾").
func insertionIntention(space, tableID, indexID string, key []byte) []storageengine.LockComponent {
	if len(key) == 0 {
		return nil
	}
	return []storageengine.LockComponent{{
		Mode:    storageengine.LockModeInsertIntention,
		Kind:    storageengine.LockComponentRecord,
		Space:   space,
		TableID: tableID,
		IndexID: indexID,
		Key:     key,
	}}
}

// indexEntryIdentity reports the namespace and key of one index's entry for one row.
//
// It reports ok=false for an index that cannot be named at all: a unique value that does not constrain
// (a NULL part, an unresolvable column) reserves nothing, exactly as it constrains nothing, and an
// index whose columns cannot be resolved is not claimed. That omission cannot lose a duplicate — the
// commit validator still catches it — it only means the loser learns at commit rather than while
// waiting.
//
// It is the one place a claim key is derived for an index entry, so the claim a statement takes and
// the comparison the replacement path makes cannot disagree about what an entry's identity is.
func indexEntryIdentity(table versionedTable, index storage.Index, owner []byte, row storage.Row) (string, []byte, bool, error) {
	if index.Unique {
		valueKey, ok := storage.IndexValueKey(index, table.Definition.Columns, row)
		if !ok {
			return "", nil, false, nil
		}
		return sqllayout.UniqueIndex(table.ID, index.Name), []byte(valueKey), true, nil
	}
	positions, ok := secondaryColumns(table, index)
	if !ok {
		return "", nil, false, nil
	}
	entryKey, _, err := secondaryEntry(table, positions, owner, row)
	if err != nil {
		return "", nil, false, err
	}
	return sqllayout.SecondaryIndex(table.ID, index.Name), entryKey, true, nil
}

// indexEntryClaim builds both halves of one index entry's claim: the insertion intention when the entry
// is a new key, and always the record claim that decides contention on the key itself.
func indexEntryClaim(index storage.Index, tableID, space string, key []byte, inserting bool) []storageengine.LockComponent {
	var components []storageengine.LockComponent
	if inserting {
		components = append(components, insertionIntention(space, tableID, index.Name, key)...)
	}
	if index.Unique {
		return append(components, uniqueValueReservationComponent(space, tableID, index.Name, key))
	}
	return append(components, indexEntryComponent(space, tableID, index.Name, key))
}

// indexEntryLocks builds the exclusive locks of a row's unique and secondary index entries.
//
// Two different identities are built here, and the difference is the whole point of the function:
//
//   - A **unique index** is locked on its *logical value reservation*: the index's own namespace
//     and the encoded value from `storage.IndexValueKey`, which is exactly the key the write path
//     stores the entry under. The entry's *value* is the owning primary key, so the entry key is
//     the unique value alone. Appending the owner — as the secondary-entry encoding does — would
//     give two transactions that write the same unique value under different primary keys two
//     different resources, and they would never meet.
//   - A **non-unique secondary index** is locked on its *physical entry*: the value parts plus the
//     owner suffix, again byte-for-byte what the write path installs. Two rows may legally share
//     such a value, so only the entry (which includes the owner) identifies one row.
//
// `storage.IndexValueKey` is the writer's own encoder, so it carries the published collation,
// NULL and composite semantics with it: a NULL-containing key is not constrained and therefore
// reserves nothing, collation-equal values produce one key, and a composite index reserves its
// whole value. Re-implementing any of that here would let the lock and the write disagree.
//
// `inserting` adds the insertion half of each claim: the index entries of a row that is being
// created are new keys, so a range another transaction protects over that index has to block them.
// A removal (the DELETE path, which locks the entries it takes away) passes false: taking an entry
// away is not an insertion and must not be held up by an interval that only blocks new keys.
func indexEntryLocks(table versionedTable, owner []byte, row storage.Row, inserting bool) ([]storageengine.LockComponent, error) {
	if row == nil || len(owner) == 0 {
		return nil, nil
	}
	var components []storageengine.LockComponent
	for _, index := range table.Definition.Indexes {
		if index.Primary {
			continue
		}
		space, key, ok, err := indexEntryIdentity(table, index, owner, row)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		components = append(components, indexEntryClaim(index, table.ID, space, key, inserting)...)
	}
	return components, nil
}

// indexReplacementLocks builds the claims for the entries an UPDATE installs.
//
// It differs from indexEntryLocks in exactly one question, and that question is why it exists: is this
// entry a *new key*, or is it the entry the row already had?
//
//   - The key is the one the old row already had: the write path updates that entry in place, or skips
//     it entirely when nothing about it changed, and no new key appears in the index. Claiming an
//     insertion intention for it would let an interval that only blocks new keys hold up a rewrite of
//     an existing record — precisely what a Gap promises not to do. The record claim still travels, so
//     two writers of the same entry still meet on it.
//   - The key is new: it carries the intention, because that is the key another transaction's interval
//     is meant to stop. A value that migrates into a protected range is an insertion however it was
//     reached, and a primary-key move rewrites the owner suffix of every secondary entry, so all of
//     those are new keys as well.
//
// The comparison is between the keys the claims themselves name, not between column values: only the
// key decides whether the index gains an entry, and an index whose columns cannot be resolved is
// invisible to both sides of the comparison.
func indexReplacementLocks(table versionedTable, oldKey, newKey []byte, oldRow, updated storage.Row) ([]storageengine.LockComponent, error) {
	if updated == nil || len(newKey) == 0 {
		return nil, nil
	}
	var components []storageengine.LockComponent
	for _, index := range table.Definition.Indexes {
		if index.Primary {
			continue
		}
		space, key, ok, err := indexEntryIdentity(table, index, newKey, updated)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		inserting := true
		if oldRow != nil && len(oldKey) > 0 {
			_, previous, previousOK, err := indexEntryIdentity(table, index, oldKey, oldRow)
			if err != nil {
				return nil, err
			}
			inserting = !previousOK || !bytes.Equal(previous, key)
		}
		components = append(components, indexEntryClaim(index, table.ID, space, key, inserting)...)
	}
	return components, nil
}

// initCounters creates the statement-local counter state on first use. A statement
// whose source produces no candidate at all (an empty INSERT SELECT, for example)
// must still be able to publish its - empty - result.
func (o *InsertOperator) initCounters() {
	if o.counters == nil {
		o.counters = newInsertCounters(len(o.Target.columns))
	}
}

// generateAutoIncrement applies the statement's auto-increment rules to one row in
// place and reports the first id this row generated.
//
// Explicit positive values raise the counter floor; NULL takes the next generated id.
// The reservation size is the remaining statement rows, which is the batch behaviour
// the INSERT VALUES path has always had; a streaming source reports Rows=0, so it asks
// for a single id per row.
func (o *InsertOperator) generateAutoIncrement(ctx context.Context, row storage.Row, ordinal uint64) (uint64, bool, error) {
	generated, ok := uint64(0), false
	for position, column := range o.Target.columns {
		if !column.AutoIncrement {
			continue
		}
		if !row[position].Null && row[position].Int64 > 0 {
			value := uint64(row[position].Int64)
			if value > o.counters.floors[position] {
				o.counters.floors[position] = value
			}
			if value >= o.counters.next[position] {
				o.counters.next[position] = value + 1
			}
			continue
		}
		if !row[position].Null {
			continue
		}
		reserved, err := o.counters.reserve(ctx, o.Engine, o.Target.definition, o.Target.columns, position, o.reservationSize(ordinal))
		if err != nil {
			return 0, false, err
		}
		value, err := storage.NewValue(column.Type, int64(reserved))
		if err != nil {
			return 0, false, err
		}
		row[position] = value
		if !ok {
			generated, ok = reserved, true
		}
	}
	return generated, ok, nil
}

// reservationSize is the size of the next auto-increment block: the rows this
// statement still has to insert. A source that does not declare its size reserves one
// id per row.
func (o *InsertOperator) reservationSize(ordinal uint64) uint64 {
	if o.Rows > ordinal {
		return o.Rows - ordinal
	}
	return 1
}

// Publish flushes the statement's counter floors and returns the statement result.
//
// It must run after every candidate has been applied. The returned result is not yet
// published to the session: the caller may only copy LastInsertID into
// session.LastInsertID after the statement child transaction commits.
func (o *InsertOperator) Publish(ctx context.Context) (*Result, error) {
	o.initCounters()
	if err := o.counters.flush(ctx, o.Engine, o.Target.definition, o.Target.columns); err != nil {
		return nil, err
	}
	result := &Result{AffectedRows: o.Result.AffectedRows}
	if o.Result.HasGeneratedID && o.Result.FirstGeneratedID != 0 {
		result.LastInsertID = o.Result.FirstGeneratedID
	}
	return result, nil
}
