package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

// locking_diagnostics.go implements the T061 SQL engine diagnostics execution in executor.
// It relies only on the neutral storageengine.LockDiagnostics capability without concrete
// lockmanager or mvcc imports.

func (e *Engine) executeShowEngineLocks(ctx context.Context, session *Session, stmt parser.ShowEngineLocks) (*Result, error) {
	if stmt.Engine != "" {
		upper := strings.ToUpper(stmt.Engine)
		if upper != "INNODB" && upper != "GB_LOCKS" && upper != "MVCC" && upper != "DEFAULT" {
			return nil, fmt.Errorf("unknown engine %q", stmt.Engine)
		}
	}

	diag, err := storageengine.LockDiagnosticsOf(e.Backend)
	if err != nil {
		return nil, storageengine.ErrUnsupported
	}

	switch stmt.Kind {
	case "LOCKS":
		page, err := diag.LocksPage("", stmt.Limit)
		if err != nil {
			return nil, err
		}
		cols := resultColumns("GrantID", "RootID", "ScopeID", "Generation", "Term", "Mode", "ResourceKind", "OpaqueResourceID", "GrantedAt")
		rows := make([][]any, 0, len(page.Rows))
		for _, r := range page.Rows {
			grantedAt := ""
			if !r.GrantedAt.IsZero() {
				grantedAt = r.GrantedAt.Format(time.RFC3339)
			}
			rows = append(rows, []any{
				strconv.FormatUint(r.GrantID, 10),
				r.RootID,
				strconv.FormatUint(r.ScopeID, 10),
				strconv.FormatUint(r.Generation, 10),
				strconv.FormatUint(r.Term, 10),
				string(r.Mode),
				string(r.Kind),
				strconv.FormatUint(uint64(r.Resource), 10),
				grantedAt,
			})
		}
		return &Result{Columns: cols, Rows: rows}, nil

	case "LOCK WAITS":
		page, err := diag.WaitPage("", stmt.Limit)
		if err != nil {
			return nil, err
		}
		cols := resultColumns("RequestID", "RootID", "ScopeID", "RequestedMode", "OpaqueResourceID", "BlockerRootIDs", "Reason", "WaitMilliseconds", "Deadline")
		rows := make([][]any, 0, len(page.Rows))
		for _, w := range page.Rows {
			deadlineStr := ""
			if !w.Deadline.IsZero() {
				deadlineStr = w.Deadline.Format(time.RFC3339)
			}
			rows = append(rows, []any{
				strconv.FormatUint(w.RequestID, 10),
				w.RootID,
				strconv.FormatUint(w.ScopeID, 10),
				string(w.Mode),
				strconv.FormatUint(uint64(w.Resource), 10),
				strings.Join(w.BlockerRoots, ","),
				w.Reason,
				strconv.FormatUint(w.WaitMillis, 10),
				deadlineStr,
			})
		}
		return &Result{Columns: cols, Rows: rows}, nil

	case "DEADLOCKS":
		page, err := diag.DeadlocksPage("", stmt.Limit)
		if err != nil {
			return nil, err
		}
		cols := resultColumns("DetectedOrdinal", "GraphRevision", "VictimRootID", "CycleRootIDs", "AnonymousIDs", "Modes", "Truncated")
		rows := make([][]any, 0, len(page.Rows))
		for _, d := range page.Rows {
			var anonStr []string
			for _, a := range d.Resources {
				anonStr = append(anonStr, strconv.FormatUint(uint64(a), 10))
			}
			var modeStr []string
			for _, m := range d.Modes {
				modeStr = append(modeStr, string(m))
			}
			rows = append(rows, []any{
				strconv.FormatUint(d.DetectedOrdinal, 10),
				strconv.FormatUint(d.GraphRevision, 10),
				d.VictimRootID,
				strings.Join(d.CycleRootIDs, ","),
				strings.Join(anonStr, ","),
				strings.Join(modeStr, ","),
				strconv.FormatBool(d.Truncated),
			})
		}
		return &Result{Columns: cols, Rows: rows}, nil

	case "STATUS":
		stats, err := diag.LockStats()
		if err != nil {
			return nil, err
		}
		eng := stmt.Engine
		if eng == "" {
			eng = "INNODB"
		}
		cols := resultColumns("Type", "Name", "Status")
		statusText := fmt.Sprintf("LOCK STATS: ActiveOwners=%d GrantedComponents=%d PendingRequests=%d WaitEdges=%d MemoryBytes=%d Timeouts=%d Deadlocks=%d ResourceRejections=%d EpochInvalidations=%d",
			stats.ActiveOwners, stats.GrantedComponents, stats.PendingRequests, stats.WaitEdges, stats.MemoryBytes, stats.Timeouts, stats.Deadlocks, stats.ResourceRejections, stats.EpochInvalidations)
		rows := [][]any{{eng, "", statusText}}
		return &Result{Columns: cols, Rows: rows}, nil

	default:
		return nil, fmt.Errorf("unsupported SHOW ENGINE LOCKS command %q", stmt.Kind)
	}
}

func resultColumns(names ...string) []Column {
	cols := make([]Column, len(names))
	for i, name := range names {
		cols[i] = Column{Name: name}
	}
	return cols
}
