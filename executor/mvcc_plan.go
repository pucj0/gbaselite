package executor

import (
	"context"
	"errors"
	"gbaselite/parser"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// Query-local access decisions are shared by execution and EXPLAIN. No row
// counts are obtained by scanning and no mutable plan survives catalog changes.
type mvccAccessPlan struct {
	covering   bool
	kind       string
	index      string
	key        []byte
	space      string
	bounds     storageengine.KeyRange
	ordered    bool
	candidates []string
}

const (
	mvccAccessAll     = "ALL"
	mvccAccessPoint   = "const"
	mvccAccessUnique  = "unique"
	mvccAccessRange   = "range"
	mvccAccessOrdered = "index"
)

func validateMVCCSelectShape(s parser.Select) error {
	if s.Locking {
		return errors.New("MVCC SELECT locking reads are not supported; use optimistic writes and retry conflicts")
	}
	if s.Table == "" && s.Subquery == nil {
		return nil
	}
	if s.Subquery != nil {
		return errors.New("derived tables are not supported")
	}
	return nil
}
func planMVCCAccess(s parser.Select, table versionedTable, schema *storage.Table, session *Session) mvccAccessPlan {
	p := mvccAccessPlan{kind: mvccAccessAll}
	p.ordered = mvccPrimaryOrder(s, table, schema) && mvccSafeRangeExpression(s.Where, schema)
	primary := "PRIMARY"
	for _, idx := range table.Definition.Indexes {
		if idx.Primary {
			primary = idx.Name
			break
		}
	}
	if key, ok := mvccPointKey(s.Where, table, schema, session); ok {
		p.kind = mvccAccessPoint
		p.index = primary
		p.key = key
		return p
	}
	bounds, bounded := mvccPrimaryRange(s.Where, table, schema)
	unique := mvccUniqueCandidates(s.Where, table, schema)
	for _, c := range unique {
		p.candidates = append(p.candidates, c.name)
	}
	if bounded || p.ordered {
		p.candidates = append(p.candidates, primary)
	}
	if len(unique) > 0 {
		p.kind = mvccAccessUnique
		p.index = unique[0].name
		p.key = unique[0].key
		p.space = unique[0].space
		return p
	}
	if secondary, ok := planMVCCSecondary(s, table, schema, session); ok {
		secondary.candidates = append(p.candidates, secondary.index)
		return secondary
	}
	if bounded || p.ordered {
		p.kind = mvccAccessRange
		if !bounded {
			p.kind = mvccAccessOrdered
		}
		p.index = primary
		p.bounds = bounds
		if p.ordered {
			p.bounds.Reverse = s.OrderBy[0].Desc
		}
	}
	return p
}
func (p mvccAccessPlan) scan(ctx context.Context, tx storageengine.Txn, table versionedTable, yield func([]byte, []byte) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch p.kind {
	case mvccAccessPoint:
		value, ok, err := tx.Table(table.ID).Get(p.key)
		if err != nil || !ok {
			return err
		}
		return yield(p.key, value)
	case "ref":
		return p.scanSecondary(ctx, tx, table, yield)
	case mvccAccessUnique:
		return scanMVCCUnique(ctx, tx, table, p.space, p.key, yield)
	case mvccAccessRange, mvccAccessOrdered:
		iterator, err := tx.Table(table.ID).Scan(ctx, storageengine.ScanRequest{Range: p.bounds})
		if err != nil {
			return err
		}
		return storageengine.Consume(iterator, yield)
	default:
		iterator, err := tx.Table(table.ID).Scan(ctx, storageengine.ScanRequest{Unordered: true})
		if err != nil {
			return err
		}
		return storageengine.Consume(iterator, yield)
	}
}
