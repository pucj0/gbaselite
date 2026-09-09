package executor

import (
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
)

// ErrSerializationConflict requires retrying the entire transaction from BEGIN.
var ErrSerializationConflict = errors.New("transaction serialization conflict; retry the transaction")

func statementChangesData(statement parser.Statement) bool {
	switch statement.(type) {
	case parser.Empty, parser.Select, parser.Union, parser.With, parser.WithRecursive,
		parser.Show, parser.Explain, parser.Use, parser.Begin, parser.Commit, parser.Rollback,
		parser.Savepoint, parser.RollbackTo, parser.ReleaseSavepoint:
		return false
	default:
		return true
	}
}

func (e *Engine) beginOptimistic(session *Session) (*Result, error) {
	if session.transaction != nil {
		return nil, errors.New("transaction already active")
	}
	if err := acquireQueryMutex(session.query, &e.txGate, false); err != nil {
		return nil, err
	}
	defer e.txGate.RUnlock()
	if err := e.AvailabilityError(); err != nil {
		return nil, err
	}
	snapshot, err := e.Store.Clone()
	if err != nil {
		return nil, err
	}
	session.transaction = snapshot
	session.transactionVersion = e.commitVersion
	session.transactionDirty = false
	session.binlogStatements = nil
	return &Result{Message: "transaction started"}, nil
}

func (e *Engine) commitOptimistic(session *Session) (*Result, error) {
	if session.transaction == nil {
		return &Result{Message: "no active transaction"}, nil
	}
	if !session.transactionDirty {
		e.finishTransaction(session)
		return &Result{Message: "read-only transaction committed"}, nil
	}
	if err := acquireQueryMutex(session.query, &e.txGate, true); err != nil {
		return nil, err
	}
	defer e.txGate.Unlock()
	if err := e.AvailabilityError(); err != nil {
		e.finishTransaction(session)
		return nil, err
	}
	if session.transactionVersion != e.commitVersion {
		e.finishTransaction(session)
		return nil, ErrSerializationConflict
	}
	// Reservations can advance even in transactions that are later rolled back.
	if err := mergeAutoIncrementReservations(session.transaction, e.Store); err != nil {
		e.finishTransaction(session)
		return nil, err
	}
	err := e.Store.ReplaceShared(session.transaction.SharedSnapshot())
	if err == nil {
		err = e.persist()
	}
	if err == nil {
		e.commitVersion++
		err = e.appendBinlog(session, session.binlogStatements)
	}
	e.finishTransaction(session)
	if err != nil {
		return nil, err
	}
	return &Result{Message: "transaction committed"}, nil
}

func mergeAutoIncrementReservations(target, source *storage.Store) error {
	for _, state := range collectAutoIncrement(source) {
		db, err := target.Database(state.Database)
		if err != nil {
			continue
		}
		table, err := db.Table(state.Table)
		if err != nil {
			continue
		}
		if err := table.AdvanceAutoIncrement(state.Column, state.Next); err != nil {
			return fmt.Errorf("preserve auto increment reservation: %w", err)
		}
	}
	return nil
}
