package executor

import (
	"context"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// insertTarget is the statement-local view of an INSERT destination: the column
// mapping plus the auto-increment floor/reservation state that must be flushed
// to the backend counter when the statement finishes.
type insertTarget struct {
	definition versionedTable
	columns    []storage.Column
	positions  []int
	floors     []uint64
	sent       []uint64
	next       []uint64
	last       []uint64
}

// insertSelectSQL implements INSERT ... SELECT on the MVCC runtime. The source
// query is bound once against the statement read snapshot (the parent
// transaction) while rows are written through the statement child transaction.
// That split is what makes a self-referencing source safe: rows inserted by this
// statement live in the child and stay invisible to the parent snapshot the
// SELECT scans, so the source can never re-read its own output. Any row-level
// failure returns an error and the caller rolls the child transaction back, so no
// partially inserted rows survive.
func (e *Engine) insertSelectSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Insert) (*Result, error) {
	target, err := e.bindInsertTarget(write, session, statement)
	if err != nil {
		return nil, err
	}
	query, err := bindSubqueryQuery(ctx, read, session, statement.Select)
	if err != nil {
		return nil, err
	}
	if len(query.Columns) != len(target.positions) {
		return nil, fmt.Errorf("%w: expected %d values, got %d", storage.ErrColumnCount, len(target.positions), len(query.Columns))
	}
	result := &Result{}
	ordinal := uint64(0)
	mode := insertStatementMode(statement)
	lastGenerated := uint64(0)
	modify := physical.Modify[[]any, struct{}]{Input: query.Input, Apply: func(ctx context.Context, values []any) (struct{}, error) {
		if err := ctx.Err(); err != nil {
			return struct{}{}, err
		}
		row, err := target.buildRow(session, values)
		if err != nil {
			return struct{}{}, err
		}
		generated, ok, err := e.resolveInsertAutoIncrement(ctx, target, row)
		if err != nil {
			return struct{}{}, err
		}
		outcome, writeErr := writeInsertedRow(ctx, write, session, target, mode, row, ordinal)
		if writeErr != nil {
			return struct{}{}, writeErr
		}
		ordinal++
		result.AffectedRows += uint64(outcome.affected)
		if outcome.inserted && ok && lastGenerated == 0 {
			lastGenerated = generated
		}
		return struct{}{}, nil
	}}
	if err := modify.Run(ctx, func(struct{}) error { return nil }); err != nil {
		return nil, err
	}
	if err := flushInsertCounters(ctx, e, target); err != nil {
		return nil, err
	}
	// Carry the first generated id in the result; the statement executor
	// publishes it to the session only after the statement transaction commits.
	if lastGenerated != 0 {
		result.LastInsertID = lastGenerated
	}
	return result, nil
}

// bindInsertTarget resolves the destination table and maps the optional column
// list onto table column positions using the same rules as INSERT VALUES.
func (e *Engine) bindInsertTarget(write storageengine.Txn, session *Session, statement parser.Insert) (*insertTarget, error) {
	definition, schema, catalogKey, err := loadVersionedTable(write, session, statement.Table)
	if err != nil {
		return nil, err
	}
	if err = write.Guard(sqllayout.Catalog, catalogKey); err != nil {
		return nil, err
	}
	columns := definition.Definition.Columns
	positions := make([]int, len(columns))
	for i := range positions {
		positions[i] = i
	}
	if len(statement.Columns) > 0 {
		positions = make([]int, len(statement.Columns))
		seen := map[int]bool{}
		for i, name := range statement.Columns {
			position, ok := schema.ColumnIndex(name)
			if !ok {
				return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, name)
			}
			if seen[position] {
				return nil, fmt.Errorf("duplicate column %s", name)
			}
			seen[position] = true
			positions[i] = position
		}
	}
	return &insertTarget{
		definition: definition,
		columns:    columns,
		positions:  positions,
		floors:     make([]uint64, len(columns)),
		sent:       make([]uint64, len(columns)),
		next:       make([]uint64, len(columns)),
		last:       make([]uint64, len(columns)),
	}, nil
}

// bindSubqueryQuery binds the SELECT/UNION source against the statement read
// snapshot. No new transaction is created; the caller's transaction is reused.
func bindSubqueryQuery(ctx context.Context, read storageengine.Txn, session *Session, source parser.Query) (*boundQuery, error) {
	switch query := source.(type) {
	case parser.Select:
		return bindPhysicalSelect(ctx, read, session, query)
	case parser.Union:
		return bindUnionWithSelect(session, query, func(branch parser.Select) (*boundQuery, error) {
			return bindPhysicalSelect(ctx, read, session, branch)
		})
	default:
		return nil, fmt.Errorf("INSERT SELECT source %T is not supported", source)
	}
}

// buildRow lays out one destination row: defaults and NULLs first, then the
// source values in the mapped column order. A NULL for an auto-increment column
// stays NULL here and is replaced by resolveInsertAutoIncrement.
func (t *insertTarget) buildRow(session *Session, values []any) (storage.Row, error) {
	row := make(storage.Row, len(t.columns))
	for i, column := range t.columns {
		row[i] = storage.NullValue(column.Type)
		if column.HasDefault {
			value, err := columnDefaultValue(column, session)
			if err != nil {
				return nil, err
			}
			row[i] = value
		}
	}
	for valueIndex, raw := range values {
		position := t.positions[valueIndex]
		column := t.columns[position]
		if column.AutoIncrement && raw == nil {
			continue
		}
		// JSON and collation wrappers carry their payload as text, matching the
		// conversion the legacy executor applies to INSERT SELECT rows.
		switch typed := raw.(type) {
		case jsonDocument:
			raw = string(typed)
		case collatedText:
			raw = typed.Text
		}
		value, err := interfaceToColumnValue(raw, column)
		if err != nil {
			return nil, err
		}
		row[position] = value
	}
	return row, nil
}

// resolveInsertAutoIncrement mirrors the INSERT VALUES counter rules: explicit
// positive values raise the floor, NULL/omitted values take the next reserved
// id. Reservation happens one id at a time so the ids handed out match the
// single-row INSERT path; reservations left unused by a failure are the
// documented MVCC auto-increment gaps.
func (e *Engine) resolveInsertAutoIncrement(ctx context.Context, target *insertTarget, row storage.Row) (uint64, bool, error) {
	generated, ok := uint64(0), false
	for i, column := range target.columns {
		if !column.AutoIncrement {
			continue
		}
		if !row[i].Null && row[i].Int64 > 0 {
			value := uint64(row[i].Int64)
			if value > target.floors[i] {
				target.floors[i] = value
			}
			if value >= target.next[i] {
				target.next[i] = value + 1
			}
			continue
		}
		if !row[i].Null {
			continue
		}
		if target.next[i] == 0 || target.next[i] > target.last[i] {
			if err := advanceInsertCounter(ctx, e, target, i); err != nil {
				return 0, false, err
			}
			reserved, err := storageengine.ReserveCounter(ctx, e.Backend, target.definition.counterKey(column.Name), 1)
			if err != nil {
				return 0, false, err
			}
			target.next[i] = reserved
			target.last[i] = reserved
		}
		id := target.next[i]
		target.next[i]++
		value, err := storage.NewValue(column.Type, int64(id))
		if err != nil {
			return 0, false, err
		}
		row[i] = value
		if !ok {
			generated, ok = id, true
		}
	}
	return generated, ok, nil
}

// advanceInsertCounter pushes the highest explicit auto-increment value seen so
// far to the backend counter before the next reservation.
func advanceInsertCounter(ctx context.Context, e *Engine, target *insertTarget, position int) error {
	if target.floors[position] <= target.sent[position] {
		return nil
	}
	if err := storageengine.AdvanceCounter(ctx, e.Backend, target.definition.counterKey(target.columns[position].Name), target.floors[position]); err != nil {
		return err
	}
	target.sent[position] = target.floors[position]
	return nil
}

func flushInsertCounters(ctx context.Context, e *Engine, target *insertTarget) error {
	for i := range target.columns {
		if err := advanceInsertCounter(ctx, e, target, i); err != nil {
			return err
		}
	}
	return nil
}
