package executor

import (
	"context"
	"errors"

	"gbaselite/sqllayout"
	"gbaselite/storageengine"
)

// TableStatistics is the live size of one table as reported by metadata endpoints
// such as SHOW TABLE STATUS and information_schema.TABLES.
type TableStatistics struct {
	Rows        int64
	DataLength  int64
	IndexLength int64
}

// TableStatistics counts the rows and encoded bytes of one table by scanning its
// row and index spaces on a fresh snapshot. The protocol metadata mirror carries
// table definitions without rows, so row counts and sizes must come from the MVCC
// backend to stay truthful; the scan is linear in the table size.
func (e *Engine) TableStatistics(ctx context.Context, database, table string) (TableStatistics, error) {
	if e.Backend == nil {
		return TableStatistics{}, errors.New("storage engine is unavailable")
	}
	tx, err := e.Backend.Begin(ctx)
	if err != nil {
		return TableStatistics{}, err
	}
	defer tx.Rollback()
	definition, _, _, err := loadVersionedTableForRead(tx, &Session{CurrentDatabase: database}, table)
	if err != nil {
		return TableStatistics{}, err
	}
	var statistics TableStatistics
	if err = tx.ScanRange(ctx, sqllayout.Rows(definition.ID), storageengine.KeyRange{}, func(key, value []byte) error {
		statistics.Rows++
		statistics.DataLength += int64(len(key) + len(value))
		return nil
	}); err != nil {
		return TableStatistics{}, err
	}
	for _, index := range definition.Definition.Indexes {
		for _, space := range []string{sqllayout.UniqueIndex(definition.ID, index.Name), sqllayout.SecondaryIndex(definition.ID, index.Name)} {
			if err = tx.ScanRange(ctx, space, storageengine.KeyRange{}, func(key, value []byte) error {
				statistics.IndexLength += int64(len(key) + len(value))
				return nil
			}); err != nil {
				return TableStatistics{}, err
			}
		}
	}
	return statistics, nil
}
