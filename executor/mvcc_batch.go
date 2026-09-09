package executor

import (
	"context"
	"gbaselite/mvcc"
)

const mvccBatchBytes = 64 << 10
const mvccBatchRows = 128

type mvccBatchEntry struct{ value []byte }

// scanBatches owns its encoded values. Consumers run outside storage read views;
// the batch is released before more input is fetched. A single wide row is
// permitted, so the byte target is not a process-memory limit.
func (p mvccAccessPlan) scanBatches(ctx context.Context, tx *mvcc.Tx, table versionedTable, rowLimit int, consume func([]mvccBatchEntry) error) error {
	if rowLimit <= 0 || rowLimit > mvccBatchRows {
		rowLimit = mvccBatchRows
	}
	batch := make([]mvccBatchEntry, 0, rowLimit)
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
		if len(batch) > 0 && (used+cost > mvccBatchBytes || len(batch) >= rowLimit) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, mvccBatchEntry{value: value})
		used += cost
		if len(batch) >= rowLimit {
			return flush()
		}
		return ctx.Err()
	}
	var err error
	if p.kind == mvccAccessAll {
		err = tx.ScanRange(ctx, "row/"+table.ID, mvcc.KeyRange{}, visit)
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
