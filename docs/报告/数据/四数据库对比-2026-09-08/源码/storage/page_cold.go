package storage

import (
	"errors"
	"fmt"
	"runtime"
)

var ErrColdMaterializationLimit = errors.New("cold table materialization exceeds memory budget")
var errColdStop = errors.New("cold scan complete")

// coldTableData is immutable and shared by read snapshots. Its finalizer only
// releases a reclamation pin: correctness never depends on finalizer timing,
// since delayed finalization retains old disk pages rather than deleting live
// ones. Decoded row payloads are not retained by the table.
type coldTableData struct {
	persistence       *Persistence
	refs              []rowPageRef
	rows              int
	memoryBytes       int64
	columns           int
	indexes           []DiskIndexDescriptor
	indexFingerprints [][32]byte
}

func newColdTableData(p *Persistence, refs []rowPageRef, columns int) *coldTableData {
	cold := &coldTableData{persistence: p, refs: append([]rowPageRef(nil), refs...), columns: columns}
	for _, ref := range refs {
		cold.rows += ref.Rows
		estimate := ref.MemoryBytes
		if estimate <= 0 {
			estimate = int64(ref.Rows)*(int64(columns)*80+24) + int64(ref.Bytes)*4
		}
		cold.memoryBytes += estimate
	}
	p.pages.coldPins.Add(1)
	runtime.SetFinalizer(cold, func(data *coldTableData) { data.persistence.pages.coldPins.Add(-1) })
	return cold
}

// LoadCold loads schemas and page references, not full row values or indexes.
// Legacy gob migration still needs its original full-memory loader once.
func (p *Persistence) LoadCold() (*Store, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pages == nil {
		return nil, fmt.Errorf("cold reads require paged persistence")
	}
	if err := p.initializePages(); err != nil {
		return nil, err
	}
	if p.pages.manifest == nil {
		return p.loadPages()
	}
	manifest, err := decodePageManifest(p.pages.manifestBytes)
	if err != nil {
		return nil, err
	}
	for databaseIndex := range manifest.Snapshot.Databases {
		for tableIndex := range manifest.Snapshot.Databases[databaseIndex].Tables {
			table := &manifest.Snapshot.Databases[databaseIndex].Tables[tableIndex]
			refs := manifest.Databases[databaseIndex].Tables[tableIndex]
			table.coldRows = newColdTableData(p, refs.Pages, len(table.Columns))
			table.coldRows.indexes = append([]DiskIndexDescriptor(nil), refs.Indexes...)
			table.coldRows.indexFingerprints = append([][32]byte(nil), refs.IndexFingerprints...)
		}
	}
	return newStoreFromSnapshot(manifest.Snapshot, true)
}

func (t *Table) IsCold() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cold != nil
}

func (t *Table) ColdDataBytes() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.cold == nil {
		return 0
	}
	return coldMaterializationEstimate(t.cold, len(t.indexes))
}

func coldMaterializationEstimate(cold *coldTableData, indexes int) int64 {
	// Include conservative index map/key and sorted-position overhead. This is
	// an admission estimate, not an operating-system resident-memory guarantee.
	return cold.memoryBytes*int64(1+indexes) + int64(cold.rows)*int64(indexes)*128
}

func (s *Store) HasColdTables() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, database := range s.databases {
		database.mu.RLock()
		for _, table := range database.tables {
			if table.IsCold() {
				database.mu.RUnlock()
				return true
			}
		}
		database.mu.RUnlock()
	}
	return false
}

// Materialize converts cold tables before an execution path that requires
// writable in-memory rows. The estimate is checked across all affected tables
// before allocation. A failed read never substitutes empty rows.
func (s *Store) Materialize(limitBytes int64) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var tables []*Table
	var estimated int64
	for _, database := range s.databases {
		database.mu.RLock()
		for _, table := range database.tables {
			size := table.ColdDataBytes()
			if size == 0 && !table.IsCold() {
				continue
			}
			if size < 0 || estimated > int64(^uint64(0)>>1)-size {
				database.mu.RUnlock()
				return ErrColdMaterializationLimit
			}
			estimated += size
			tables = append(tables, table)
		}
		database.mu.RUnlock()
	}
	if len(tables) == 0 {
		return nil
	}
	if limitBytes <= 0 || estimated > limitBytes {
		return fmt.Errorf("%w: estimated %d bytes, limit %d; use a supported streaming query or raise cold_materialize_mb", ErrColdMaterializationLimit, estimated, limitBytes)
	}
	for _, table := range tables {
		if err := table.Materialize(limitBytes); err != nil {
			return err
		}
	}
	return nil
}

func (t *Table) Materialize(limitBytes int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cold == nil {
		return nil
	}
	if estimate := coldMaterializationEstimate(t.cold, len(t.indexes)); limitBytes <= 0 || estimate > limitBytes {
		return fmt.Errorf("%w: table %q needs approximately %d bytes", ErrColdMaterializationLimit, t.name, estimate)
	}
	rows := make([]Row, 0, t.cold.rows)
	err := t.cold.visit(nil, 0, -1, func(row Row) error { rows = append(rows, row); return nil })
	if err != nil {
		return err
	}
	for _, row := range rows {
		for index, value := range row {
			if err := validateValue(t.columns[index], value); err != nil {
				return err
			}
		}
	}
	replacement := &Table{rows: rows, columns: t.columns, columnIndex: t.columnIndex, indexes: t.indexes}
	if err := replacement.rebuildIndexesLocked(); err != nil {
		return err
	}
	t.rows, t.uniqueRows, t.indexRows = rows, replacement.uniqueRows, replacement.indexRows
	t.dataLength = rowsDataLength(rows)
	t.cold = nil
	return nil
}

func (cold *coldTableData) visit(predicate Predicate, offset, limit int, yield func(Row) error) error {
	defer runtime.KeepAlive(cold)
	if limit == 0 {
		return nil
	}
	if offset < 0 {
		offset = 0
	}
	emitted := 0
	for _, ref := range cold.refs {
		rows, err := cold.persistence.loadRowPage(ref)
		if err != nil {
			return fmt.Errorf("read cold row page: %w", err)
		}
		for _, row := range rows {
			if len(row) != cold.columns {
				return fmt.Errorf("cold row has %d columns; expected %d", len(row), cold.columns)
			}
			if predicate != nil && !predicate(row) {
				continue
			}
			if offset > 0 {
				offset--
				continue
			}
			if err := yield(row); err != nil {
				return err
			}
			emitted++
			if limit >= 0 && emitted >= limit {
				return nil
			}
		}
	}
	runtime.KeepAlive(cold)
	return nil
}

// CheckedCount is the error-preserving counterpart to the legacy in-memory
// Count helper. New storage callers should prefer this method.
func (t *Table) CheckedCount(predicate Predicate) (int, error) {
	count := 0
	err := t.Visit(predicate, func(Row) error { count++; return nil })
	return count, err
}
