package executor

import (
	"context"
	"fmt"
	"gbaselite/parser"
	"gbaselite/storageengine"
)

func (e *Engine) executeSQLMaintenance(ctx context.Context, session *Session, s parser.MVCCMaintenance) (*Result, error) {
	if e.Replica != nil {
		return nil, fmt.Errorf("MVCC maintenance currently requires standalone mode")
	}
	if session.InTransaction() || session.AutocommitDisabled {
		return nil, fmt.Errorf("MVCC maintenance requires autocommit outside a transaction")
	}
	maintenance, ok := e.Backend.(storageengine.Maintenance)
	if !ok {
		return nil, storageengine.ErrUnsupported
	}
	switch s.Kind {
	case "BACKUP":
		m, err := maintenance.Backup(ctx, s.Path)
		return &Result{Message: fmt.Sprintf("MVCC backup bytes=%d head=%d sha256=%s", m.Bytes, m.Head, m.SHA256)}, err
	case "RESTORE":
		if err := maintenance.RestoreBackup(ctx, s.Path); err != nil {
			return nil, err
		}
		e.mvccMetadata.Lock()
		e.mvccMetadataVersion = ^uint64(0)
		e.mvccMetadata.Unlock()
		if err := e.refreshSQLMetadata(ctx); err != nil {
			return nil, err
		}
		return &Result{Message: "MVCC backup restored; previous transactions invalidated", MetadataChanged: true}, nil
	case "GC":
		return &Result{Message: "MVCC history collection completed"}, maintenance.CompactHistory(ctx)
	case "COMPACT":
		if err := maintenance.CompactHistory(ctx); err != nil {
			return nil, err
		}
		return &Result{Message: "compact MVCC copy verified; source preserved, explicit cutover required"}, maintenance.Compact(ctx, s.Path)
	default:
		return nil, fmt.Errorf("unknown MVCC maintenance operation")
	}
}
