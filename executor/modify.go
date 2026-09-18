package executor

import "gbaselite/physical"

// This file owns the A05 unified Modify Pipeline base model. It is additive: the
// existing mutation*.go / physical_binding.go paths stay authoritative until the
// migration phases move them onto these types, so nothing here may change current
// SQL behaviour.
//
// Mutation target identity is a first-class model, not an implementation detail of
// one statement family. Every UPDATE and DELETE candidate --- whether it came from a
// simple table scan, an UPDATE JOIN, or a multi-table DELETE --- must carry the same
// identity model so that target deduplication and the final write operate on one
// notion of "which physical row".

// RowIdentity is the stable identity of one physical storage row inside one target
// table:
//
//	TableID equal AND Key equal  =>  the same mutation target
//	TableID differs              =>  different targets, even when Key bytes match
//
// The model lives in package physical because that is where the stable-dedup
// infrastructure consumes it: physical.TargetRowDedup resolves a candidate to a
// RowIdentity and deduplicates on RowIdentity.StorageKey(), so one definition serves
// both the mutation operators and the operator that feeds them. The executor keeps
// this alias so statement-level code names the identity without a package qualifier.
//
// See physical.RowIdentity for the physical-key rules (primary key encoding, heap row
// key for tables without a primary key, outer-join provenance) and the ownership
// rules for Key.
type RowIdentity = physical.RowIdentity

// NewRowIdentity builds an identity that owns a private copy of key. Statement
// bindings must use it (or RowIdentity.Clone) for every identity they retain past the
// Yield callback that produced the borrowed storage key.
func NewRowIdentity(tableID string, key []byte) RowIdentity {
	return physical.NewRowIdentity(tableID, key)
}

// ModifyResult records what one modify statement produced.
//
// It belongs to the statement, not to the session. FirstGeneratedID is reported
// with the explicit HasGeneratedID flag instead of a zero sentinel so a generated
// id can be represented exactly; publishing it to session.LastInsertID is the
// caller's job and may only happen after the statement child transaction commits.
type ModifyResult struct {
	AffectedRows uint64

	FirstGeneratedID uint64
	HasGeneratedID   bool
}

// recordGenerated keeps the first generated id of the statement. Later rows must
// not overwrite it, which is the same "first generated id wins" rule the current
// INSERT paths implement.
func (m *ModifyResult) recordGenerated(id uint64, generated bool) {
	if !generated || m.HasGeneratedID {
		return
	}
	m.FirstGeneratedID = id
	m.HasGeneratedID = true
}
