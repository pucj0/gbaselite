package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
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
