package executor

import (
	"errors"
	"fmt"
	"gbaselite/journal"
	"gbaselite/parser"
	"gbaselite/storage"
	"sort"
	"strings"
)

var ErrSavepointNotFound = errors.New("SAVEPOINT does not exist")
var ErrResourceLimit = errors.New("resource limit exceeded")

const maxTransactionSavepoints = 32

type transactionSavepoint struct {
	name         string
	snapshot     storage.StoreSnapshot
	binlogLength int
}

func (e *legacyEngine) savepointIndex(session *Session, name string) int {
	for i := len(e.legacyState(session).savepoints) - 1; i >= 0; i-- {
		if strings.EqualFold(e.legacyState(session).savepoints[i].name, name) {
			return i
		}
	}
	return -1
}
func (e *legacyEngine) createSavepoint(session *Session, name string) (*Result, error) {
	if e.legacyState(session).transaction == nil {
		return &Result{Message: "no active transaction"}, nil
	}
	index := e.savepointIndex(session, name)
	if index < 0 && len(e.legacyState(session).savepoints) >= maxTransactionSavepoints {
		return nil, fmt.Errorf("%w: at most %d savepoints per transaction", ErrResourceLimit, maxTransactionSavepoints)
	}
	point := transactionSavepoint{name: name, snapshot: e.legacyState(session).transaction.SharedSnapshot(), binlogLength: len(e.legacyState(session).binlogStatements)}
	if index >= 0 {
		copy(e.legacyState(session).savepoints[index:], e.legacyState(session).savepoints[index+1:])
		e.legacyState(session).savepoints[len(e.legacyState(session).savepoints)-1] = transactionSavepoint{}
		e.legacyState(session).savepoints = e.legacyState(session).savepoints[:len(e.legacyState(session).savepoints)-1]
	}
	e.legacyState(session).savepoints = append(e.legacyState(session).savepoints, point)
	return &Result{Message: "savepoint created"}, nil
}
func (e *legacyEngine) releaseSavepoint(session *Session, name string) (*Result, error) {
	index := e.savepointIndex(session, name)
	if index < 0 {
		return nil, fmt.Errorf("%w: %s", ErrSavepointNotFound, name)
	}
	copy(e.legacyState(session).savepoints[index:], e.legacyState(session).savepoints[index+1:])
	e.legacyState(session).savepoints[len(e.legacyState(session).savepoints)-1] = transactionSavepoint{}
	e.legacyState(session).savepoints = e.legacyState(session).savepoints[:len(e.legacyState(session).savepoints)-1]
	return &Result{Message: "savepoint released"}, nil
}
func (e *legacyEngine) rollbackToSavepoint(session *Session, name string) (*Result, error) {
	index := e.savepointIndex(session, name)
	if index < 0 || e.legacyState(session).transaction == nil {
		return nil, fmt.Errorf("%w: %s", ErrSavepointNotFound, name)
	}
	point := &e.legacyState(session).savepoints[index]
	// Save only the small counter maps, not a second full row-header snapshot.
	type tableKey struct{ database, table string }
	current := make(map[tableKey][]journal.AutoIncrementState)
	for _, state := range collectAutoIncrement(e.legacyState(session).transaction) {
		key := tableKey{strings.ToLower(state.Database), strings.ToLower(state.Table)}
		current[key] = append(current[key], state)
	}
	for di := range point.snapshot.Databases {
		db := &point.snapshot.Databases[di]
		for ti := range db.Tables {
			table := &db.Tables[ti]
			key := tableKey{strings.ToLower(db.Name), strings.ToLower(table.Name)}
			for _, state := range current[key] {
				if previous, ok := table.AutoIncrementNext[state.Column]; ok && state.Next > previous {
					table.AutoIncrementNext[state.Column] = state.Next
				}
			}
		}
	}
	if err := e.legacyState(session).transaction.ReplaceShared(point.snapshot); err != nil {
		return nil, err
	}
	clear(e.legacyState(session).binlogStatements[point.binlogLength:])
	e.legacyState(session).binlogStatements = e.legacyState(session).binlogStatements[:point.binlogLength]
	if e.binlog != nil {
		// Do not replay discarded SQL; replay only surviving counter reservations.
		if counters := collectAutoIncrement(e.legacyState(session).transaction); len(counters) > 0 {
			e.legacyState(session).binlogStatements = append(e.legacyState(session).binlogStatements, journal.BinlogStatement{AutoIncrement: counters})
		}
	}
	clear(e.legacyState(session).savepoints[index+1:])
	e.legacyState(session).savepoints = e.legacyState(session).savepoints[:index+1]
	return &Result{Message: "rolled back to savepoint"}, nil
}

func collectAutoIncrement(store *storage.Store) []journal.AutoIncrementState {
	var states []journal.AutoIncrementState
	for _, databaseName := range store.ListDatabases() {
		database, _ := store.Database(databaseName)
		for _, tableName := range database.ListTables() {
			table, _ := database.Table(tableName)
			counters := table.AutoIncrementState()
			names := make([]string, 0, len(counters))
			for name := range counters {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, column := range names {
				states = append(states, journal.AutoIncrementState{Database: databaseName, Table: tableName, Column: column, Next: counters[column]})
			}
		}
	}
	return states
}

// ReplayAutoIncrement applies a v2 binlog control record to an active, isolated
// replay transaction. It is deliberately not exposed as an SQL command.
func (e *legacyEngine) ReplayAutoIncrement(session *Session, states []journal.AutoIncrementState) error {
	if err := e.AvailabilityError(); err != nil {
		return err
	}
	if session == nil || e.legacyState(session).transaction == nil {
		return errors.New("auto increment replay requires an active transaction")
	}
	for _, state := range states {
		if state.Next < 1 {
			return errors.New("invalid auto increment replay counter")
		}
		db, err := e.legacyState(session).transaction.Database(state.Database)
		if err != nil {
			return err
		}
		table, err := db.Table(state.Table)
		if err != nil {
			return err
		}
		if err := table.AdvanceAutoIncrement(state.Column, state.Next); err != nil {
			return err
		}
	}
	return nil
}

// Renames and destructive DDL need persistent object identities to preserve
// consumed auto-increment reservations across rollback. Reject these cases
// explicitly until that mechanism exists; additive DDL remains transactional.
func changesSavepointIdentity(statement parser.Statement) bool {
	switch value := statement.(type) {
	case parser.DropDatabase, parser.DropTable, parser.RenameTable, parser.Truncate,
		parser.DropColumn, parser.RenameColumn, parser.AlterColumn:
		return true
	case parser.AlterTableBatch:
		for _, action := range value.Actions {
			if changesSavepointIdentity(action) {
				return true
			}
		}
	}
	return false
}
