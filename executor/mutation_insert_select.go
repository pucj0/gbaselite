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

// insertTarget is the statement-local view of an INSERT destination: the resolved
// destination definition plus the column mapping. Auto-increment state lives in the
// InsertOperator that owns the statement, not here.
type insertTarget struct {
	definition versionedTable
	columns    []storage.Column
	positions  []int
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
	// The SELECT pipeline is bound against the parent statement snapshot and the
	// operator writes through the child transaction, so a self-referencing source can
	// never read this statement's own output.
	operator := &InsertOperator{
		Input:   insertSelectSource(query.Input, target, session),
		Target:  target,
		Write:   write,
		Engine:  e,
		Session: session,
		Mode:    insertStatementMode(statement),
	}
	if err = operator.Run(ctx); err != nil {
		return nil, err
	}
	// Carry the first generated id in the result; the statement executor publishes it
	// to the session only after the statement transaction commits.
	return operator.Publish(ctx)
}

// insertSelectSource converts the bound SELECT/UNION output into InsertCandidate
// values, one destination row per source row.
//
// Ordinal must track the statement-local input position, so the projection keeps its
// own counter: it decides both the generated fallback key and the auto-increment
// reservation size. buildRow stays stateless and is shared with the VALUES path.
func insertSelectSource(input physical.Operator[[]any], target *insertTarget, session *Session) physical.Operator[InsertCandidate] {
	ordinal := uint64(0)
	return physical.Projection[[]any, InsertCandidate]{Input: input, Project: func(values []any) (InsertCandidate, error) {
		row, err := target.buildRow(session, values)
		if err != nil {
			return InsertCandidate{}, err
		}
		candidate := InsertCandidate{Values: row, Ordinal: ordinal}
		ordinal++
		return candidate, nil
	}}
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
// stays NULL here and is replaced by the InsertOperator's auto-increment step.
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
