package mvcc

// TransactionState is the B01 lifecycle state of one MVCC transaction.
//
// The zero value is deliberately an invalid state rather than ACTIVE, so a
// transaction that never went through TransactionManager registration can never
// be mistaken for a live one. The manager creates every entry as
// TransactionActive and never stores TransactionUnset, so diagnostics cannot
// observe it.
//
// The state set and the transition table below are the single source of truth for
// FR-005 of specs/001-b01-mvcc-transaction-manager/spec.md. Nothing here is
// persisted: the state machine is in-memory bookkeeping only, so the MVCC
// on-disk format is untouched.
type TransactionState uint8

const (
	// TransactionUnset marks a transaction that was never registered. Invalid:
	// no transition may enter or leave it.
	TransactionUnset TransactionState = iota
	// TransactionActive accepts reads and writes.
	TransactionActive
	// TransactionCommitting is a root transaction whose durable publication is
	// in flight. It is the only state a root may leave before it becomes
	// COMMITTED or ABORTED.
	TransactionCommitting
	// TransactionCommitted is a root transaction whose durable publication marker
	// exists, or an empty/read-only root transaction that ended successfully.
	// Terminal.
	TransactionCommitted
	// TransactionAborted is a transaction whose writes were discarded. Terminal.
	TransactionAborted
	// TransactionMerged is a child transaction whose writes were merged into its
	// parent. Terminal, and reachable only by children.
	TransactionMerged
)

// Terminal reports whether no further lifecycle move is allowed.
func (s TransactionState) Terminal() bool {
	switch s {
	case TransactionCommitted, TransactionAborted, TransactionMerged:
		return true
	default:
		return false
	}
}

// String returns the stable diagnostics name used by the public
// transaction-diagnostics contract.
func (s TransactionState) String() string {
	switch s {
	case TransactionActive:
		return "ACTIVE"
	case TransactionCommitting:
		return "COMMITTING"
	case TransactionCommitted:
		return "COMMITTED"
	case TransactionAborted:
		return "ABORTED"
	case TransactionMerged:
		return "MERGED"
	default:
		return "UNSET"
	}
}

// transitionKind classifies one lifecycle edge: whether it exists at all and
// which transaction kind may make it.
type transitionKind uint8

const (
	transitionIllegal transitionKind = iota
	transitionAny
	transitionRootOnly
	transitionChildOnly
)

// transactionTransitionKind is the single table consulted for every lifecycle
// move, both by the manager and by its tests, so a state can never be reached by
// two different rules.
//
// Legal edges:
//
//	ACTIVE     -> COMMITTING   (root only: durable publication starts)
//	ACTIVE     -> COMMITTED    (root only: empty or read-only commit)
//	ACTIVE     -> ABORTED
//	ACTIVE     -> MERGED       (child only: merge into the parent)
//	COMMITTING -> COMMITTED    (root only)
//	COMMITTING -> ABORTED
//
// A terminal state may only confirm itself. That is what makes a repeated
// rollback, or a repeated terminal report, a no-op instead of a second outcome,
// while no edge ever leads back to ACTIVE, so a terminal transaction cannot be
// revived.
func transactionTransitionKind(from, to TransactionState) transitionKind {
	if from == to {
		if from.Terminal() {
			return transitionAny
		}
		return transitionIllegal
	}
	switch from {
	case TransactionActive:
		switch to {
		case TransactionCommitting, TransactionCommitted:
			return transitionRootOnly
		case TransactionAborted:
			return transitionAny
		case TransactionMerged:
			return transitionChildOnly
		}
	case TransactionCommitting:
		switch to {
		case TransactionCommitted:
			return transitionRootOnly
		case TransactionAborted:
			return transitionAny
		}
	}
	return transitionIllegal
}

// transactionTransitionAllowed reports whether the from -> to edge exists,
// ignoring which transaction kind is making the move.
func transactionTransitionAllowed(from, to TransactionState) bool {
	return transactionTransitionKind(from, to) != transitionIllegal
}
