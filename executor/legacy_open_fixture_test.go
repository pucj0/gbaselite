package executor

import (
	"fmt"
	"gbaselite/catalog"
	"gbaselite/storage"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// openLegacy constructs historical test fixtures, never a runtime Engine.
func openLegacy(dir, user, password string) (*legacyEngine, error) {
	return openLegacyWithOptions(dir, user, password, OpenOptions{})
}
func openLegacyWithOptions(dir, user, password string, o OpenOptions) (*legacyEngine, error) {
	if _, err := os.Stat(filepath.Join(dir, "versioned", "mvcc.db")); err == nil {
		return nil, fmt.Errorf("legacy migration cannot open MVCC data")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var p *storage.Persistence
	switch strings.ToLower(o.StorageMode) {
	case "", "snapshot":
		p = storage.NewPersistence(dir)
	case "paged":
		p = storage.NewPagedPersistence(dir, o.PageCacheBytes)
	default:
		return nil, fmt.Errorf("unknown legacy format")
	}
	if o.ColdRead && !p.PagedStats().Enabled {
		return nil, fmt.Errorf("cold reads require legacy paged data")
	}
	if o.PageCacheBytes < 0 || o.ColdMaterializeBytes < 0 {
		return nil, fmt.Errorf("negative legacy buffer")
	}
	if o.ColdMaterializeBytes == 0 {
		o.ColdMaterializeBytes = 64 << 20
	}
	return openWithPersistenceOptions(dir, user, password, p, o)
}
func openWithPersistence(dataDir, username, password string, persistence *storage.Persistence) (*legacyEngine, error) {
	return openWithPersistenceOptions(dataDir, username, password, persistence, OpenOptions{})
}

func openWithPersistenceOptions(dataDir, username, password string, persistence *storage.Persistence, options OpenOptions) (*legacyEngine, error) {
	for _, directory := range []string{"databases", "tables", "users", "indexes"} {
		if err := os.MkdirAll(filepath.Join(dataDir, directory), 0o755); err != nil {
			return nil, err
		}
	}
	var store *storage.Store
	var err error
	if options.ColdRead {
		store, err = persistence.LoadCold()
	} else {
		store, err = persistence.Load()
	}
	if err != nil {
		return nil, err
	}
	users, err := catalog.OpenUsers(dataDir, username, password)
	if err != nil {
		return nil, err
	}
	engine := &legacyEngine{Engine: &Engine{Store: store, Users: users}, ColdRead: options.ColdRead, ColdMaterializeBytes: options.ColdMaterializeBytes, Persistence: persistence, persistSave: persistence.Save}
	engine.persistCond = sync.NewCond(&engine.persistMu)
	return engine, nil
}
