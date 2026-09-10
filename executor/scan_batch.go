package executor

import (
	"bytes"
	"context"
	"gbaselite/sqllayout"
	"gbaselite/storageengine"
)

const sqlBatchBytes = 64 << 10
const sqlBatchRows = 128

type sqlBatchEntry struct{ value []byte }

// scanBatches owns its encoded values. Consumers run outside storage read views;
// the batch is released before more input is fetched. A single wide row is
// permitted, so the byte target is not a process-memory limit.
func (p sqlAccessPlan) scanBatches(ctx context.Context, tx storageengine.Txn, table versionedTable, rowLimit int, consume func([]sqlBatchEntry) error) error {
	if rowLimit <= 0 || rowLimit > sqlBatchRows {
		rowLimit = sqlBatchRows
	}
	batch := make([]sqlBatchEntry, 0, rowLimit)
	used := 0
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := consume(batch)
		clear(batch)
		batch = batch[:0]
		used = 0
		return err
	}
	visit := func(_, value []byte) error {
		cost := len(value) + 24
		if len(batch) > 0 && (used+cost > sqlBatchBytes || len(batch) >= rowLimit) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, sqlBatchEntry{value: bytes.Clone(value)})
		used += cost
		if len(batch) >= rowLimit {
			return flush()
		}
		return ctx.Err()
	}
	var err error
	if p.kind == sqlAccessAll {
		err = tx.ScanRange(ctx, sqllayout.Rows(table.ID), storageengine.KeyRange{}, visit)
	} else {
		err = p.scan(ctx, tx, table, visit)
	}
	if err != nil {
		return err
	}
	if len(batch) > 0 {
		return flush()
	}
	return nil
}
