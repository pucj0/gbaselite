package executor

import (
	"context"
	"gbaselite/parser"
	"gbaselite/sqllayout"
	"gbaselite/storageengine"
)

// joinUpdateSQL implements UPDATE ... JOIN on the MVCC runtime through the unified
// modify pipeline:
//
//	JOIN -> WHERE -> UpdateCandidate -> TargetRowDedup -> LIMIT -> UpdateOperator
//
// The target table is the driving side of the join and its access plan still comes from
// planSQLAccess, so an indexed UPDATE JOIN probes an index exactly like a plain indexed
// UPDATE.
//
// A target row matched by several joined rows is updated once, by the first matching
// source row: TargetRowDedup keeps the first occurrence of every target identity, and only
// then does LIMIT count the surviving targets. Writes go through the statement child
// transaction while the join reads the parent statement snapshot, so a late error rolls the
// whole statement back and no updated row can be re-read by the same statement.
func (e *Engine) joinUpdateSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Update) (*Result, error) {
	definition, schema, catalogKey, err := loadVersionedTable(read, session, statement.Table)
	if err != nil {
		return nil, err
	}
	if err = write.Guard(sqllayout.Catalog, catalogKey); err != nil {
		return nil, err
	}
	qualifier := mutationQualifier(statement.Table, statement.TableAlias)
	// Always qualify the evaluation schema, like the legacy executor, so a correlated
	// subquery can bind an explicitly qualified outer column.
	schema, err = qualifySchema(schema, qualifier)
	if err != nil {
		return nil, err
	}
	limit := -1
	if statement.HasLimit {
		limit = statement.Limit
	}
	// Binding resolves every assignment and the join before anything is read, so an
	// unknown target column, a duplicate assignment or a missing join input fails as a
	// binding error rather than part-way through the mutation.
	plan, assignments, err := bindUpdateJoinPlan(ctx, read, session, statement, definition, schema, limit)
	if err != nil {
		return nil, err
	}
	selected, err := plan.winners(ctx, limit)
	if err != nil {
		return nil, err
	}
	affected, err := plan.apply(ctx, selected, definition, definition.Definition.Columns, assignments, schema, write, session)
	if err != nil {
		return nil, err
	}
	return &Result{AffectedRows: affected}, nil
}
