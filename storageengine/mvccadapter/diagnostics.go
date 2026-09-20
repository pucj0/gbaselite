package mvccadapter

import (
	"gbaselite/mvcc"
	"gbaselite/storageengine"
)

// Transaction diagnostics for the MVCC-backed engine.
//
// The capability is declared here, on the concrete engine, and not on
// storageengine.Engine or storageengine.Txn: those are the minimal contracts every
// backend must satisfy, and widening them would force the memory test double and any
// future backend to fabricate a registry they do not have. A caller that wants
// diagnostics asserts for storageengine.TransactionDiagnostics and handles absence
// as ErrUnsupported.
//
// The adapter reads the MVCC transaction registry and translates it into the neutral
// DTOs. It adds no state of its own, so diagnostics cannot drift from the registry
// the commit path actually uses, and it never touches visibility, conflict
// validation, garbage collection or durability.
var _ storageengine.TransactionDiagnostics = (*engine)(nil)

// ActiveTransactions returns a translated copy of the registry snapshot. Each
// call takes a fresh snapshot; nothing is cached and nothing is retained.
func (e *engine) ActiveTransactions() []storageengine.TransactionInfo {
	snapshot := e.store.ActiveTransactions()
	transactions := make([]storageengine.TransactionInfo, 0, len(snapshot))
	for _, info := range snapshot {
		transactions = append(transactions, transactionInfo(info))
	}
	return transactions
}

// TransactionStats returns the translated aggregate counters and GC horizon.
func (e *engine) TransactionStats() storageengine.TransactionStats {
	stats := e.store.TransactionStats()
	return storageengine.TransactionStats{
		ActiveRoot:      stats.ActiveRoot,
		ActiveChildren:  stats.ActiveChildren,
		Committed:       stats.Committed,
		Aborted:         stats.Aborted,
		Conflicts:       stats.Conflicts,
		OldestReadTS:    stats.OldestReadTS,
		HasOldestReadTS: stats.HasOldestReadTS,
	}
}

// transactionInfo maps one registry entry onto the neutral DTO. Every exported
// field is mapped explicitly so adding a field to the backend structure cannot leak
// an unclassified value through this capability by default.
func transactionInfo(info mvcc.TransactionInfo) storageengine.TransactionInfo {
	return storageengine.TransactionInfo{
		ID:                info.ID,
		ParentID:          info.ParentID,
		StartTS:           info.StartTS,
		ReadTS:            info.ReadTS,
		CommitTS:          info.CommitTS,
		HasCommitTS:       info.HasCommitTS,
		State:             transactionState(info.State),
		Generation:        info.Generation,
		StartedAt:         info.StartedAt,
		PointReads:        info.PointReads,
		RangeReads:        info.RangeReads,
		RowsObserved:      info.RowsObserved,
		BytesObserved:     info.BytesObserved,
		Writes:            info.Writes,
		WriteBytes:        info.WriteBytes,
		PointDependencies: info.PointDependencies,
		RangeDependencies: info.RangeDependencies,
		AbortReason:       info.AbortReason,
	}
}

// transactionState classifies a backend lifecycle state for diagnostics.
//
// An unrecognized value, including a transaction that was never registered, maps to
// TransactionUnknown rather than to TransactionActive. Claiming ACTIVE for a state
// this adapter does not understand would report a usable transaction that the commit
// path may already have ended, which is exactly the failure a diagnostic must not
// introduce. New states therefore fail closed until they are mapped on purpose.
func transactionState(state mvcc.TransactionState) storageengine.TransactionState {
	switch state {
	case mvcc.TransactionActive:
		return storageengine.TransactionActive
	case mvcc.TransactionCommitting:
		return storageengine.TransactionCommitting
	case mvcc.TransactionCommitted:
		return storageengine.TransactionCommitted
	case mvcc.TransactionAborted:
		return storageengine.TransactionAborted
	case mvcc.TransactionMerged:
		return storageengine.TransactionMerged
	default:
		return storageengine.TransactionUnknown
	}
}
