package storage

import (
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gbaselite/internal/atomicfile"
)

type Persistence struct {
	path                string
	mu                  sync.Mutex
	replaceFile         func(string, string) error
	pages               *pagePersistence
	allowPagedDirectory bool
}

func NewPersistence(dataDir string) *Persistence {
	return &Persistence{
		path:        filepath.Join(dataDir, "databases", "store.gob"),
		replaceFile: atomicfile.Replace,
	}
}

func (p *Persistence) Path() string { return p.path }

func (p *Persistence) Save(store *Store) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pages != nil {
		return p.savePages(store)
	}
	if err := p.rejectPagedDirectory(); err != nil {
		return err
	}
	// A cold snapshot carries process-local page references that gob cannot
	// serialize. Refuse it before touching the destination or a recovery file.
	snapshot := store.persistenceSnapshot()
	for _, database := range snapshot.Databases {
		for _, table := range database.Tables {
			if table.coldRows != nil {
				return fmt.Errorf("cannot save unloaded cold tables as a gob snapshot; use Persistence.ExportSnapshot or explicitly Materialize the store first")
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return fmt.Errorf("create persistence directory: %w", err)
	}
	temporary := p.path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open persistence file: %w", err)
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	encodeErr := gob.NewEncoder(file).Encode(snapshot)
	syncErr := file.Sync()
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("encode persistence data: %w", encodeErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync persistence data: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close persistence data: %w", closeErr)
	}
	if err := p.replaceFile(temporary, p.path); err != nil {
		return fmt.Errorf("replace persistence data without deleting the previous snapshot: %w", err)
	}
	committed = true
	return nil
}

func (p *Persistence) Load() (*Store, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pages != nil {
		return p.loadPages()
	}
	if err := p.rejectPagedDirectory(); err != nil {
		return nil, err
	}
	file, err := os.Open(p.path)
	if errors.Is(err, os.ErrNotExist) {
		if _, temporaryErr := os.Stat(p.path + ".tmp"); temporaryErr == nil {
			return nil, fmt.Errorf("persistence snapshot %s is missing but recovery candidate %s exists; do not delete or overwrite either file, copy the directory, then validate the candidate or restore a known-good backup", p.path, p.path+".tmp")
		} else if !errors.Is(temporaryErr, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect persistence recovery candidate %s: %w", p.path+".tmp", temporaryErr)
		}
		return NewStore(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("open persistence data: %w", err)
	}
	defer file.Close()
	snapshot, _, err := decodeSnapshot(file)
	if err != nil {
		recoveryHint := ""
		if _, temporaryErr := os.Stat(p.path + ".tmp"); temporaryErr == nil {
			recoveryHint = fmt.Sprintf("; recovery candidate %s also exists", p.path+".tmp")
		}
		return nil, fmt.Errorf("decode persistence snapshot %s: %w%s; do not replace the file in place, copy the data directory and restore a known-good backup or validate the recovery candidate", p.path, err, recoveryHint)
	}
	if info, statErr := file.Stat(); statErr == nil {
		fallback := info.ModTime().UTC()
		for databaseIndex := range snapshot.Databases {
			for tableIndex := range snapshot.Databases[databaseIndex].Tables {
				table := &snapshot.Databases[databaseIndex].Tables[tableIndex]
				if table.CreatedAt.IsZero() {
					table.CreatedAt = fallback
				}
				if table.UpdatedAt.IsZero() {
					table.UpdatedAt = fallback
				}
			}
		}
	}
	return newStoreFromSnapshot(snapshot, true)
}

// Refuse to serve the preserved pre-migration gob as current data after an
// accidental configuration downgrade. Only paged migration may bypass this.
func (p *Persistence) rejectPagedDirectory() error {
	if p.allowPagedDirectory {
		return nil
	}
	for _, name := range []string{"store.pages", "store.checkpoint", "store.wal"} {
		path := filepath.Join(filepath.Dir(p.path), name)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("paged storage marker %s exists; use storage mode paged, or export and restore a snapshot into a new directory before switching modes", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect storage mode marker: %w", err)
		}
	}
	return nil
}
