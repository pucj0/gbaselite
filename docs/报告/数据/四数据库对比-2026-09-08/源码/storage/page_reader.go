package storage

import (
	"fmt"
	"path/filepath"
	"sync"
)

// RowPageReader walks one committed table snapshot, decoding at most one page
// at a time. The returned row belongs to the caller. Close releases the page
// generation pin; reaching EOF also closes the reader. Cold SQL uses the same
// checksummed page loader through error-preserving table scan methods.
type RowPageReader struct {
	mu          sync.Mutex
	persistence *Persistence
	pages       []rowPageRef
	pageIndex   int
	rows        []Row
	rowIndex    int
	closed      bool
}

func (p *Persistence) OpenRowPages(databaseName, tableName string) (*RowPageReader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pages == nil {
		return nil, fmt.Errorf("row page iteration requires paged persistence")
	}
	if err := p.initializePages(); err != nil {
		return nil, err
	}
	if p.pages.manifest == nil {
		return nil, fmt.Errorf("paged database has not yet been saved")
	}
	for databaseIndex, database := range p.pages.manifest.Snapshot.Databases {
		if normalizeName(database.Name) != normalizeName(databaseName) {
			continue
		}
		for tableIndex, table := range database.Tables {
			if normalizeName(table.Name) != normalizeName(tableName) {
				continue
			}
			refs := p.pages.manifest.Databases[databaseIndex].Tables[tableIndex].Pages
			p.pages.readers++
			return &RowPageReader{persistence: p, pages: append([]rowPageRef(nil), refs...)}, nil
		}
	}
	return nil, fmt.Errorf("table %q.%q not found in committed pages", databaseName, tableName)
}

func (reader *RowPageReader) Next() (Row, bool, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return nil, false, nil
	}
	if reader.rowIndex >= len(reader.rows) {
		reader.rows = nil
		if reader.pageIndex >= len(reader.pages) {
			reader.closeLocked()
			return nil, false, nil
		}
		rows, err := reader.persistence.loadRowPage(reader.pages[reader.pageIndex])
		if err != nil {
			reader.closeLocked()
			return nil, false, err
		}
		reader.rows, reader.rowIndex = rows, 0
		reader.pageIndex++
	}
	row := reader.rows[reader.rowIndex]
	reader.rows[reader.rowIndex] = nil
	reader.rowIndex++
	return row, true, nil
}

func (reader *RowPageReader) Close() error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.closeLocked()
	return nil
}

func (reader *RowPageReader) closeLocked() {
	if reader.closed {
		return
	}
	reader.closed = true
	reader.rows, reader.pages = nil, nil
	reader.persistence.mu.Lock()
	reader.persistence.pages.readers--
	reader.persistence.mu.Unlock()
}

// ExportSnapshot writes a portable complete gob snapshot at path. It pins this
// Persistence's committed root for the export and uses atomic replacement.
// Paged exports currently materialize all rows; export is an administrative
// operation, not a bounded-memory streaming backup.
func (p *Persistence) ExportSnapshot(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	source, err := filepath.Abs(p.path)
	if err != nil {
		return err
	}
	if normalizeName(absolute) == normalizeName(source) {
		return fmt.Errorf("snapshot export must not overwrite the active persistence root")
	}
	var store *Store
	if p.pages != nil {
		if filepath.Dir(absolute) == p.pages.directory {
			return fmt.Errorf("snapshot export must be outside the active paged database directory")
		}
		store, err = p.loadPages()
	} else {
		legacy := NewPersistence(filepath.Dir(filepath.Dir(p.path)))
		legacy.path = p.path
		store, err = legacy.Load()
	}
	if err != nil {
		return err
	}
	export := &Persistence{path: absolute, replaceFile: p.replaceFile}
	return export.Save(store)
}
