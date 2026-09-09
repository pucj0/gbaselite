package executor

import (
	"context"
	"fmt"
	"gbaselite/parser"
)

func (e *Engine) executeMVCCMaintenance(ctx context.Context, session *Session, s parser.MVCCMaintenance) (*Result, error) {
	if e.Replica != nil {
		return nil, fmt.Errorf("MVCC maintenance currently requires standalone mode")
	}
	if session.InTransaction() || session.AutocommitDisabled {
		return nil, fmt.Errorf("MVCC maintenance requires autocommit outside a transaction")
	}
	switch s.Kind {
	case "BACKUP":
		m, err := e.MVCC.Backup(ctx, s.Path)
		return &Result{Message: fmt.Sprintf("MVCC backup bytes=%d head=%d sha256=%s", m.Bytes, m.Head, m.SHA256)}, err
	case "RESTORE":
		if err := e.MVCC.RestoreBackup(ctx, s.Path); err != nil {
			return nil, err
		}
		e.mvccMetadata.Lock()
		e.mvccMetadataVersion = ^uint64(0)
		e.mvccMetadata.Unlock()
		if err := e.refreshMVCCMetadata(ctx); err != nil {
			return nil, err
		}
		return &Result{Message: "MVCC backup restored; previous transactions invalidated", MetadataChanged: true}, nil
	case "GC":
		return &Result{Message: "MVCC history collection completed"}, e.MVCC.CompactHistory(ctx)
	case "COMPACT":
		if err := e.MVCC.CompactHistory(ctx); err != nil {
			return nil, err
		}
		return &Result{Message: "compact MVCC copy verified; source preserved, explicit cutover required"}, e.MVCC.ExportLayout(ctx, s.Path, "flat")
	default:
		return nil, fmt.Errorf("unknown MVCC maintenance operation")
	}
}
