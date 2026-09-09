package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gbaselite/catalog"
	"gbaselite/replication"
	"gbaselite/storage"
)

// OpenOptions selects persistence independently of SQL behavior. Paged mode
// reduces durable write amplification; ColdRead additionally loads row pages on demand.
type OpenOptions struct {
	LocalWAL              bool
	TransactionWriteBytes int64
	Replication           *replication.Options
	StorageMode           string
	PageCacheBytes        int64
	ColdRead              bool
	ColdMaterializeBytes  int64
}

func OpenWithOptions(dataDir, username, password string, options OpenOptions) (*Engine, error) {
	if options.LocalWAL && (!strings.EqualFold(options.StorageMode, "mvcc") || options.Replication != nil) {
		return nil, fmt.Errorf("local WAL requires standalone MVCC")
	}
	if options.TransactionWriteBytes < 0 {
		return nil, fmt.Errorf("transaction write bytes must not be negative")
	}
	if options.ColdMaterializeBytes < 0 {
		return nil, fmt.Errorf("cold materialization bytes must not be negative")
	}
	if options.PageCacheBytes < 0 {
		return nil, fmt.Errorf("page cache bytes must not be negative")
	}
	if strings.EqualFold(strings.TrimSpace(options.StorageMode), "mvcc") {
		if options.ColdRead {
			return nil, fmt.Errorf("cold_reads is only for paged mode")
		}
		return openMVCC(dataDir, username, password, options)
	}
	if options.Replication != nil {
		return nil, fmt.Errorf("replication requires mvcc storage mode")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "versioned", "mvcc.db")); err == nil {
		return nil, fmt.Errorf("MVCC data directory cannot be opened with legacy storage mode")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var persistence *storage.Persistence
	switch strings.ToLower(strings.TrimSpace(options.StorageMode)) {
	case "", "snapshot":
		persistence = storage.NewPersistence(dataDir)
	case "paged":
		persistence = storage.NewPagedPersistence(dataDir, options.PageCacheBytes)
	default:
		return nil, fmt.Errorf("unknown storage mode %q: expected snapshot, paged or mvcc", options.StorageMode)
	}
	if options.ColdRead && !persistence.PagedStats().Enabled {
		return nil, fmt.Errorf("cold reads require paged storage mode")
	}
	if options.ColdMaterializeBytes < 0 {
		return nil, fmt.Errorf("cold materialization bytes must not be negative")
	}
	if options.ColdMaterializeBytes == 0 {
		options.ColdMaterializeBytes = 64 << 20
	}
	return openWithPersistenceOptions(dataDir, username, password, persistence, options)
}

func openWithPersistence(dataDir, username, password string, persistence *storage.Persistence) (*Engine, error) {
	return openWithPersistenceOptions(dataDir, username, password, persistence, OpenOptions{})
}

func openWithPersistenceOptions(dataDir, username, password string, persistence *storage.Persistence, options OpenOptions) (*Engine, error) {
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
	engine := &Engine{ColdRead: options.ColdRead, ColdMaterializeBytes: options.ColdMaterializeBytes, Store: store, Persistence: persistence, Users: users, persistSave: persistence.Save}
	engine.persistCond = sync.NewCond(&engine.persistMu)
	return engine, nil
}
