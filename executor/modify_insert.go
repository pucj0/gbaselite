package executor

import (
	"context"
	"gbaselite/physical"
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
	outcome, err := writeInsertedRow(ctx, o.Write, o.Session, o.Target, o.Mode, candidate.Values, candidate.Ordinal)
	if err != nil {
		return err
	}
	o.Result.AffectedRows += uint64(outcome.affected)
	if outcome.inserted && generatedOK {
		o.Result.recordGenerated(generated, true)
	}
	return nil
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
