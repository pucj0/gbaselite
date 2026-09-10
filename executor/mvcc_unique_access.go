package executor

import (
	"context"
	"gbaselite/parser"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// Existing unique entries are complete even for legacy tables. Only exact,
// non-NULL integer equalities within the SQL comparison precision are eligible.
// Residual WHERE remains mandatory; unsupported conversions use the scan.
type mvccUniqueCandidate struct {
	name, space string
	key         []byte
}

func mvccUniqueAccess(where parser.Expr, table versionedTable, schema *storage.Table) (string, []byte, bool) {
	candidates := mvccUniqueCandidates(where, table, schema)
	if len(candidates) == 0 {
		return "", nil, false
	}
	return candidates[0].space, candidates[0].key, true
}
func mvccUniqueCandidates(where parser.Expr, table versionedTable, schema *storage.Table) []mvccUniqueCandidate {
	var candidates []mvccUniqueCandidate
	hasUnique := false
	for _, idx := range table.Definition.Indexes {
		if idx.Unique && !idx.Primary {
			hasUnique = true
			break
		}
	}
	if !hasUnique {
		return nil
	}
	if !safeMutationIndexExpression(where, schema) {
		return nil
	}
	values := make(map[int]storage.Value)
	var visit func(parser.Expr)
	visit = func(expr parser.Expr) {
		b, ok := expr.(parser.BinaryExpr)
		if !ok {
			return
		}
		if b.Operator == "AND" {
			visit(b.Left)
			visit(b.Right)
			return
		}
		if b.Operator != "=" && b.Operator != "<=>" {
			return
		}
		id, ok := b.Left.(parser.Identifier)
		lit, lok := b.Right.(parser.LiteralExpr)
		if !ok || !lok {
			id, ok = b.Right.(parser.Identifier)
			lit, lok = b.Left.(parser.LiteralExpr)
		}
		if !ok || !lok {
			return
		}
		pos, ok := queryColumnIndex(schema, id.Name)
		if !ok {
			return
		}
		col := table.Definition.Columns[pos]
		if col.Type != storage.TypeInt && col.Type != storage.TypeBigInt {
			return
		}
		v, err := literalToValue(lit.Value, col)
		if err == nil && !v.Null {
			values[pos] = v
		}
	}
	visit(where)
	if len(values) == 0 {
		return nil
	}
	row := make(storage.Row, len(table.Definition.Columns))
	for pos, value := range values {
		row[pos] = value
	}
	for _, idx := range table.Definition.Indexes {
		if !idx.Unique || idx.Primary || len(idx.Columns) == 0 {
			continue
		}
		complete := true
		for _, name := range idx.Columns {
			found := false
			for pos, col := range table.Definition.Columns {
				if strings.EqualFold(col.Name, name) {
					v, ok := values[pos]
					if ok {
						row[pos] = v
						found = true
					}
					break
				}
			}
			if !found {
				complete = false
				break
			}
		}
		if complete {
			key, ok := storage.IndexValueKey(idx, table.Definition.Columns, row)
			if ok {
				candidates = append(candidates, mvccUniqueCandidate{name: idx.Name, space: sqllayout.UniqueIndex(table.ID, idx.Name), key: []byte(key)})
			}
		}
	}
	return candidates
}

func scanMVCCUnique(ctx context.Context, tx storageengine.Txn, table versionedTable, space string, key []byte, yield func([]byte, []byte) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, exists, err := tx.Get(space, key)
	if err != nil || !exists {
		return err
	}
	row, exists, err := tx.Table(table.ID).Get(owner)
	if err != nil || !exists {
		return err
	}
	return yield(owner, row)
}
