package storage

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type coldRowAccessor struct {
	cold *coldTableData
	ends []int
	page int
	rows []Row
}

func (cold *coldTableData) rowAccessor() *coldRowAccessor {
	accessor := &coldRowAccessor{cold: cold, ends: make([]int, len(cold.refs)), page: -1}
	total := 0
	for index, ref := range cold.refs {
		total += ref.Rows
		accessor.ends[index] = total
	}
	return accessor
}

func (accessor *coldRowAccessor) row(position int) (Row, error) {
	if position < 0 || position >= accessor.cold.rows {
		return nil, fmt.Errorf("disk index row position %d is outside table", position)
	}
	page := sort.Search(len(accessor.ends), func(index int) bool { return accessor.ends[index] > position })
	if page != accessor.page {
		rows, err := accessor.cold.persistence.loadRowPage(accessor.cold.refs[page])
		if err != nil {
			return nil, err
		}
		accessor.page, accessor.rows = page, rows
	}
	start := 0
	if page > 0 {
		start = accessor.ends[page-1]
	}
	row := accessor.rows[position-start]
	if len(row) != accessor.cold.columns {
		return nil, fmt.Errorf("cold index row column count mismatch")
	}
	return row, nil
}

func (cold *coldTableData) openIndex(name string) (*DiskIndex, error) {
	for _, descriptor := range cold.indexes {
		if strings.EqualFold(name, descriptor.Definition.Name) {
			// Row cache retains the configured global budget. Index traversal keeps
			// only its bounded path pages and uses no additional persistent per-index
			// cache, avoiding budget multiplication by the number of tables/indexes.
			return OpenDiskIndex(filepath.Join(cold.persistence.pages.directory, "disk-indexes"), descriptor, 0)
		}
	}
	return nil, fmt.Errorf("%w: no durable index %q", ErrIndexNotFound, name)
}

func (cold *coldTableData) streamIndex(scan IndexScan, predicate Predicate, offset, limit int, yield func(Row) error) error {
	defer runtime.KeepAlive(cold)
	if limit == 0 {
		return nil
	}
	index, err := cold.openIndex(scan.Name)
	if err != nil {
		return err
	}
	accessor := cold.rowAccessor()
	emitted := 0
	err = index.Scan(scan, func(position int) error {
		row, err := accessor.row(position)
		if err != nil {
			return err
		}
		if predicate != nil && !predicate(row) {
			return nil
		}
		if offset > 0 {
			offset--
			return nil
		}
		if err := yield(row); err != nil {
			return err
		}
		emitted++
		if limit >= 0 && emitted >= limit {
			return errColdStop
		}
		return nil
	})
	if errors.Is(err, errColdStop) {
		return nil
	}
	return err
}

// StreamIndex preserves index order while fetching one row page at a time in
// cold mode. Callback rows are immutable and must not be modified.
func (t *Table) StreamIndex(scan IndexScan, predicate Predicate, offset, limit int, yield func(Row) error) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.cold != nil {
		return t.cold.streamIndex(scan, predicate, offset, limit, yield)
	}
	positions, start, end, err := t.indexIntervalLocked(scan)
	if err != nil {
		return err
	}
	if limit == 0 {
		return nil
	}
	emitted := 0
	visit := func(index int) error {
		row := t.rows[positions[index]]
		if predicate != nil && !predicate(row) {
			return nil
		}
		if offset > 0 {
			offset--
			return nil
		}
		if err := yield(row); err != nil {
			return err
		}
		emitted++
		if limit >= 0 && emitted >= limit {
			return errColdStop
		}
		return nil
	}
	if scan.Descending {
		for index := end - 1; index >= start; index-- {
			if err = visit(index); err != nil {
				break
			}
		}
	} else {
		for index := start; index < end; index++ {
			if err = visit(index); err != nil {
				break
			}
		}
	}
	if errors.Is(err, errColdStop) {
		return nil
	}
	return err
}

// LookupUniqueChecked carries disk I/O errors which the historical in-memory
// LookupUnique triple cannot represent. Cold SQL uses this or StreamIndex.
func (t *Table) LookupUniqueChecked(column string, value Value) (Row, bool, bool, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.cold == nil {
		for name, definition := range t.indexes {
			if !definition.Unique || len(definition.Columns) != 1 || !strings.EqualFold(definition.Columns[0], column) {
				continue
			}
			key := string(appendIndexValueKey(nil, foldIndexValue(value, indexCollation(definition, 0))))
			position, found := t.uniqueRows[name][key]
			if !found {
				return nil, false, true, nil
			}
			return t.rows[position], true, true, nil
		}
		return nil, false, false, nil
	}
	for _, definition := range t.indexes {
		if !definition.Unique || len(definition.Columns) != 1 || !strings.EqualFold(definition.Columns[0], column) {
			continue
		}
		var row Row
		err := t.cold.streamIndex(IndexScan{Name: definition.Name, EqualPrefix: []Value{value}}, nil, 0, 1, func(found Row) error { row = found; return nil })
		return row, row != nil, true, err
	}
	return nil, false, false, nil
}
