package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// executeWithSQL implements non-recursive WITH. Every CTE is materialized from
// the statement snapshot and registered in the statement-local CTE scope, so
// later CTEs and the body bind it like any other relation; the scope disappears
// with the statement and never touches session state or the catalog.
func (e *Engine) executeWithSQL(ctx context.Context, tx storageengine.Txn, session *Session, statement parser.With) (*Result, error) {
	if session.subqueries == nil {
		return nil, errors.New("WITH requires statement context")
	}
	for _, expression := range statement.Expressions {
		schema, rows, err := derivedRelation(ctx, tx, session, expression.Query, expression.Name, expression.Columns)
		if err != nil {
			return nil, err
		}
		session.subqueries.registerCTE(expression.Name, schema, rows)
	}
	return e.executeQuerySQL(ctx, tx, session, statement.Query)
}

// executeQuerySQL runs a Select or Union body through the unified query binding.
func (e *Engine) executeQuerySQL(ctx context.Context, tx storageengine.Txn, session *Session, query parser.Query) (*Result, error) {
	switch value := query.(type) {
	case parser.Select:
		return executePhysicalSelect(ctx, tx, session, value)
	case parser.Union:
		bound, err := bindUnionWithSelect(session, value, func(branch parser.Select) (*boundQuery, error) {
			return bindPhysicalSelect(ctx, tx, session, branch)
		})
		if err != nil {
			return nil, err
		}
		return collectBoundQuery(session, bound, false)
	default:
		return nil, fmt.Errorf("unsupported query %T", query)
	}
}

// executeWithRecursiveSQL implements WITH RECURSIVE with the legacy semantics:
// the seed runs once, each iteration sees only the previous iteration's delta,
// the accumulated relation feeds the body, and the public column names/types
// come from the seed. All work stays on the statement snapshot transaction.
func (e *Engine) executeWithRecursiveSQL(ctx context.Context, tx storageengine.Txn, session *Session, statement parser.WithRecursive) (*Result, error) {
	if session.subqueries == nil {
		return nil, errors.New("WITH RECURSIVE requires statement context")
	}
	seedSchema, seedRows, err := derivedRelation(ctx, tx, session, statement.Seed, statement.Name, nil)
	if err != nil {
		return nil, fmt.Errorf("recursive CTE %s seed: %w", statement.Name, err)
	}
	seedColumns := seedSchema.ColumnsView()
	limit := int64(16 << 20)
	if session.query != nil && session.query.options.ResultMemoryBytes > 0 {
		limit = session.query.options.ResultMemoryBytes
	}
	accumulated := append([]storage.Row(nil), seedRows...)
	used := rowsBytes(accumulated)
	delta := seedRows
	for depth := 0; len(delta) > 0; depth++ {
		if depth >= maxRecursiveCTEDepth {
			return nil, fmt.Errorf("recursive CTE %s exceeded %d iterations", statement.Name, maxRecursiveCTEDepth)
		}
		session.subqueries.registerCTE(statement.Name, seedSchema, delta)
		iterationSchema, iterationRows, err := derivedRelation(ctx, tx, session, statement.Recursive, statement.Name, nil)
		if err != nil {
			return nil, fmt.Errorf("recursive CTE %s iteration %d: %w", statement.Name, depth+1, err)
		}
		if len(iterationSchema.ColumnsView()) != len(seedColumns) {
			return nil, fmt.Errorf("recursive CTE %s returned %d columns, expected %d", statement.Name, len(iterationSchema.ColumnsView()), len(seedColumns))
		}
		if len(iterationRows) == 0 {
			break
		}
		next := make([]storage.Row, len(iterationRows))
		for index, row := range iterationRows {
			converted := make(storage.Row, len(seedColumns))
			for position, column := range seedColumns {
				value, conversionErr := interfaceToColumnValue(row[position].Interface(), column)
				if conversionErr != nil {
					return nil, conversionErr
				}
				converted[position] = value
			}
			next[index] = converted
		}
		used += rowsBytes(next)
		if limit > 0 && used > limit {
			return nil, fmt.Errorf("%w: recursive CTE %s exceeds %d bytes", ErrQueryResourceLimit, statement.Name, limit)
		}
		accumulated = append(accumulated, next...)
		delta = next
	}
	session.subqueries.registerCTE(statement.Name, seedSchema, accumulated)
	return e.executeQuerySQL(ctx, tx, session, statement.Query)
}

func rowsBytes(rows []storage.Row) int64 {
	total := int64(0)
	for _, row := range rows {
		total += storageRowBytes(row)
	}
	return total
}
