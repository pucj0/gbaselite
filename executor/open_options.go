package executor

import (
	"context"
	"fmt"
	"gbaselite/catalog"
	"gbaselite/enginefactory"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// OpenOptions configures the sole runtime transaction engine, MVCC.
type OpenOptions struct {
	// BackendFactory replaces only backend construction; SQL operators stay unchanged.
	BackendFactory        storageengine.Factory
	LocalWAL              bool
	TransactionWriteBytes int64
	Replication           *storageengine.ReplicationOptions
	// Deprecated: only empty or mvcc is accepted. Legacy formats require migration.
	StorageMode string
	// Deprecated legacy settings; nonzero values are rejected by runtime opens.
	PageCacheBytes       int64
	ColdRead             bool
	ColdMaterializeBytes int64
}

func OpenWithOptions(dataDir, username, password string, options OpenOptions) (*Engine, error) {
	mode := strings.ToLower(strings.TrimSpace(options.StorageMode))
	if mode != "" && mode != "mvcc" {
		return nil, fmt.Errorf("storage mode %q was removed; MVCC is the only transaction engine; use migrate-legacy with a new target directory", options.StorageMode)
	}
	if options.LocalWAL && options.Replication != nil {
		return nil, fmt.Errorf("local WAL requires standalone MVCC")
	}
	if options.TransactionWriteBytes < 0 {
		return nil, fmt.Errorf("transaction write bytes must not be negative")
	}
	if options.ColdRead || options.PageCacheBytes != 0 || options.ColdMaterializeBytes != 0 {
		return nil, fmt.Errorf("paged cache/cold-read options were removed; migrate legacy data to MVCC")
	}
	factory := options.BackendFactory
	if factory == nil {
		factory = enginefactory.Open
	}
	backend, err := factory(dataDir, storageengine.Options{LocalWAL: options.LocalWAL, TransactionWriteBytes: options.TransactionWriteBytes, Replication: options.Replication})
	if err != nil {
		return nil, err
	}
	users, err := catalog.OpenUsers(dataDir, username, password)
	if err != nil {
		backend.Close()
		return nil, err
	}
	return NewWithStorage(backend, users)
}

// NewWithStorage takes ownership of backend (including on initialization failure).
// It accepts any implementation of the neutral storage contract.
func NewWithStorage(backend storageengine.Engine, users *catalog.Users) (*Engine, error) {
	if backend == nil || users == nil {
		if backend != nil {
			backend.Close()
		}
		return nil, fmt.Errorf("storage engine and user catalog are required")
	}
	e := &Engine{Backend: backend, Replica: backend.Replica(), Store: storage.NewStore(), Users: users}
	e.QueryOptions = QueryOptions{SortMemoryBytes: 4 << 20, ResultMemoryBytes: 16 << 20, MaxTempBytes: 256 << 20}
	if err := e.refreshMVCCMetadata(context.Background()); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}
