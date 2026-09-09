package executor

// Historical interpreter used only to characterize old behavior and generate
// migration fixtures. This file is excluded from production builds.
import (
	"errors"
	"fmt"
	"gbaselite/journal"
	"gbaselite/parser"
	"gbaselite/storage"
	"strings"
	"sync"
)

type legacySession struct {
	transaction        *storage.Store
	transactionGate    bool
	transactionVersion uint64
	transactionDirty   bool
	savepoints         []transactionSavepoint
	binlogStatements   []journal.BinlogStatement
}
type legacyEngine struct {
	legacySessions sync.Map // *Session -> *legacySession, scoped to this test engine.
	*Engine
	OptimisticTransactions bool
	ColdRead               bool
	ColdMaterializeBytes   int64
	commitVersion          uint64 // Guarded by txGate.
	Persistence            *storage.Persistence
	txGate                 sync.RWMutex
	copyMu                 sync.Mutex
	persistMu              sync.Mutex
	persistCond            *sync.Cond
	persistNext            uint64
	persistDone            uint64
	persisting             bool
	persistErr             error
	persistFatal           error
	persistSave            func(*storage.Store) error
	binlog                 *journal.Binlog
}

func (e *legacyEngine) Close() error {
	return e.persist()
}

func (e *legacyEngine) SetBinlog(binlog *journal.Binlog) { e.binlog = binlog }

// AvailabilityError reports a fatal persistence failure. Once set, the engine
// rejects SQL until restart so divergent in-memory state cannot overwrite the
// last durable snapshot.
func (e *legacyEngine) AvailabilityError() error {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	return e.persistFatal
}

// persist combines concurrent durable writes into the same snapshot/fsync.
// A leader performs one save and then hands coordination to a pending waiter,
// preventing one request from owning every save during a sustained write burst.
func (e *legacyEngine) persist() error {
	e.persistMu.Lock()
	if e.persistFatal != nil {
		err := e.persistFatal
		e.persistMu.Unlock()
		return err
	}
	e.persistNext++
	generation := e.persistNext
	for e.persistDone < generation {
		if e.persistFatal != nil {
			err := e.persistFatal
			e.persistMu.Unlock()
			return err
		}
		if e.persisting {
			e.persistCond.Wait()
			continue
		}
		e.persisting = true
		target := e.persistNext
		e.persistMu.Unlock()

		save := e.persistSave
		if save == nil {
			save = e.Persistence.Save
		}
		err := save(e.Store)

		e.persistMu.Lock()
		e.persisting = false
		if err != nil {
			e.persistFatal = fmt.Errorf("%w: durable snapshot %s could not be saved: %v; GBaseLite entered fail-closed mode and will not serve or persist SQL until it is restarted after the storage problem is fixed", ErrPersistenceUnavailable, e.Persistence.Path(), err)
			e.persistErr = e.persistFatal
			e.persistDone = e.persistNext
		} else {
			e.persistDone = target
			e.persistErr = nil
		}
		e.persistCond.Broadcast()
		if e.persistFatal != nil {
			err = e.persistFatal
			e.persistMu.Unlock()
			return err
		}
	}
	err := e.persistErr
	e.persistMu.Unlock()
	return err
}

// CloseSession rolls back an unfinished transaction and releases the global
// transaction gate. Protocol servers must call this when a client disconnects.
func (e *legacyEngine) CloseSession(session *Session) {
	if session == nil {
		return
	}
	if session.mvccTransaction != nil {
		session.mvccTransaction.Rollback()
		session.mvccTransaction = nil
	}
	rolledBack := e.legacyState(session).transaction != nil
	e.finishTransaction(session)
	if rolledBack {
		_ = e.persist()
	}
}

func (e *legacyEngine) Execute(session *Session, sql string) (*Result, error) {
	if session != nil {
		session.InitializeSettings()
	}
	expanded, err := parser.ExpandMySQLExecutableComments(sql)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(expanded)
	if cached, ok := e.parseCache.Load(query); ok {
		return e.executeStatement(session, cached.(parser.Statement), query)
	}
	statement, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	if len(query) <= 8192 {
		e.cacheStatement(query, statement)
	}
	return e.executeStatement(session, statement, query)
}

func (e *legacyEngine) ExecuteStatement(session *Session, statement parser.Statement) (*Result, error) {
	return e.executeStatement(session, statement, "")
}

func (e *legacyEngine) executeStatement(session *Session, statement parser.Statement, query string) (resultOut *Result, errOut error) {
	if err := e.AvailabilityError(); err != nil {
		return nil, err
	}
	if session == nil {
		session = &Session{}
	}
	session.InitializeSettings()
	restoreQuery := startQuery(session, e.QueryOptions)
	defer restoreQuery()
	defer func() {
		if resultOut != nil {
			resultOut.InTransaction = session.mvccTransaction != nil || e.legacyState(session).transaction != nil
			resultOut.AutocommitDisabled = session.AutocommitDisabled
		}
		if errOut == nil && resultOut != nil && len(resultOut.Columns) > 0 {
			resultOut, errOut = bindQueryResult(resultOut, session.query)
		}
	}()
	if err := checkQuery(session); err != nil {
		return nil, err
	}
	databaseAtStart := session.CurrentDatabase
	_, beginning := statement.(parser.Begin)
	if e.legacyState(session).transaction == nil && !beginning {
		if requiresAtomicStoreMutation(statement) || e.OptimisticTransactions && statementChangesData(statement) {
			if err := acquireQueryMutex(session.query, &e.txGate, true); err != nil {
				return nil, err
			}
			defer e.txGate.Unlock()
		} else {
			if err := acquireQueryMutex(session.query, &e.txGate, false); err != nil {
				return nil, err
			}
			defer e.txGate.RUnlock()
		}
		if err := e.AvailabilityError(); err != nil {
			return nil, err
		}
	}
	// Optimistic writes may reserve IDs on the live store. Keep that store's
	// tables stable until the reservation and transaction-local mutation finish.
	if e.OptimisticTransactions && e.legacyState(session).transaction != nil && statementChangesData(statement) {
		if err := acquireQueryMutex(session.query, &e.txGate, false); err != nil {
			return nil, err
		}
		defer e.txGate.RUnlock()
		if err := e.AvailabilityError(); err != nil {
			return nil, err
		}
	}
	store := e.Store
	if e.legacyState(session).transaction != nil {
		store = e.legacyState(session).transaction
	}
	if err := e.authorizeStatement(session, statement); err != nil {
		return nil, err
	}
	if coldResult, handled, coldErr := e.prepareColdStatement(store, session, statement); handled || coldErr != nil {
		return coldResult, coldErr
	}
	if len(e.legacyState(session).savepoints) > 0 && changesSavepointIdentity(statement) {
		return nil, errors.New("unsupported schema identity change while savepoints are active; release savepoints first")
	}
	mutated := false
	var result *Result
	var err error
	switch value := statement.(type) {
	case parser.Empty:
		return &Result{Message: "empty query"}, nil
	case parser.Begin:
		if e.OptimisticTransactions {
			return e.beginOptimistic(session)
		}
		if e.legacyState(session).transaction != nil {
			return nil, errors.New("transaction already active")
		}
		if err := acquireQueryMutex(session.query, &e.txGate, true); err != nil {
			return nil, err
		}
		e.legacyState(session).transactionGate = true
		if err := e.AvailabilityError(); err != nil {
			e.finishTransaction(session)
			return nil, err
		}
		e.legacyState(session).binlogStatements = nil
		e.legacyState(session).transaction, err = e.Store.Clone()
		if err != nil {
			e.finishTransaction(session)
		}
		result = &Result{Message: "transaction started"}
		return result, err
	case parser.Commit:
		if e.OptimisticTransactions {
			return e.commitOptimistic(session)
		}
		if e.legacyState(session).transaction == nil {
			return &Result{Message: "no active transaction"}, nil
		}
		statements := append([]journal.BinlogStatement(nil), e.legacyState(session).binlogStatements...)
		if err = e.Store.ReplaceShared(e.legacyState(session).transaction.SharedSnapshot()); err == nil {
			err = e.persist()
		}
		if err == nil {
			err = e.appendBinlog(session, statements)
		}
		e.finishTransaction(session)
		return &Result{Message: "transaction committed"}, err
	case parser.Savepoint:
		return e.createSavepoint(session, value.Name)
	case parser.RollbackTo:
		return e.rollbackToSavepoint(session, value.Name)
	case parser.ReleaseSavepoint:
		return e.releaseSavepoint(session, value.Name)
	case parser.Rollback:
		if e.legacyState(session).transaction == nil {
			return &Result{Message: "no active transaction"}, nil
		}
		e.finishTransaction(session)
		if err = e.persist(); err != nil {
			return nil, err
		}
		return &Result{Message: "transaction rolled back"}, nil
	case parser.CreateDatabase:
		_, err = store.CreateDatabase(value.Name)
		if value.IfNotExists && errors.Is(err, storage.ErrDatabaseExists) {
			err = nil
		}
		mutated = err == nil
		result = &Result{Message: "database created"}
	case parser.DropDatabase:
		err = store.DropDatabase(value.Name)
		if value.IfExists && errors.Is(err, storage.ErrDatabaseNotFound) {
			err = nil
		}
		mutated = err == nil
		if strings.EqualFold(session.CurrentDatabase, value.Name) {
			session.CurrentDatabase = ""
		}
		result = &Result{Message: "database dropped"}
	case parser.Use:
		_, err = store.Database(value.Database)
		if err == nil {
			session.CurrentDatabase = value.Database
		}
		result = &Result{Message: "database changed"}
	case parser.CreateTable:
		var database *storage.Database
		var table *storage.Table
		databaseName, tableName := splitTableName(value.Name)
		requestedTableName := tableName
		database, err = selectedDatabase(store, session, databaseName)
		if err == nil {
			e.copyMu.Lock()
			if !value.IfNotExists {
				tableName, err = navicatBackupTarget(database, session, tableName, "table", false)
			}
			if err == nil {
				columns := make([]storage.Column, len(value.Columns))
				for i, column := range value.Columns {
					columns[i], err = storageColumnDefinition(column)
					if err != nil {
						break
					}
				}
				if err == nil {
					indexes := make([]storage.Index, len(value.Indexes))
					for i, definition := range value.Indexes {
						indexes[i] = storage.Index{Name: definition.Name, Columns: append([]string(nil), definition.Columns...), Unique: definition.Unique}
					}
					table, err = database.CreateTableWithIndexes(tableName, columns, value.PrimaryKey, indexes)
					if value.IfNotExists && errors.Is(err, storage.ErrTableExists) {
						err = nil
					}
					if err == nil && table != nil {
						for _, check := range value.Checks {
							err = validateCheckDefinition(table, check.Expression)
							if err == nil {
								err = table.AddCheck(storage.CheckConstraint{Name: check.Name, Expression: check.Expression})
							}
							if err != nil {
								break
							}
						}
						for _, foreignKey := range value.ForeignKeys {
							if err != nil {
								break
							}
							err = database.AddForeignKey(tableName, storage.ForeignKey{Name: foreignKey.Name, Columns: append([]string(nil), foreignKey.Columns...), RefTable: foreignKey.RefTable, RefColumns: append([]string(nil), foreignKey.RefColumns...), OnDelete: foreignKey.OnDelete, OnUpdate: foreignKey.OnUpdate}, !session.ForeignKeyChecksDisabled)
						}
						if err != nil {
							_ = database.DropTable(tableName)
							table = nil
						}
					}
					if err == nil && table != nil {
						table.SetComment(value.Comment)
						if !strings.EqualFold(requestedTableName, tableName) {
							rememberCopyTarget(session, database.Name(), requestedTableName, tableName)
						}
					}
				}
			}
			e.copyMu.Unlock()
		}
		session.copySource = nil
		mutated = err == nil
		renamed := err == nil && !strings.EqualFold(requestedTableName, tableName)
		result = &Result{Message: copyCreatedMessage("table", requestedTableName, tableName), MetadataChanged: renamed}
	case parser.CreateTableLike:
		var clone *storage.Store
		clone, err = store.Clone()
		if err == nil {
			result, mutated, err = executeCreateTableLike(clone, session, value)
		}
		if err == nil && mutated {
			err = store.ReplaceShared(clone.SharedSnapshot())
		}
	case parser.CreateTableAs:
		var clone *storage.Store
		clone, err = store.Clone()
		if err == nil {
			result, mutated, err = executeCreateTableAs(clone, session, value)
		}
		if err == nil && mutated {
			err = store.ReplaceShared(clone.SharedSnapshot())
		}
	case parser.CreateView:
		e.copyMu.Lock()
		result, mutated, err = executeCreateView(store, session, value)
		e.copyMu.Unlock()
	case parser.DropTable:
		result, mutated, err = executeDropTables(store, session, value)
	case parser.DropView:
		result, mutated, err = executeDropViews(store, session, value)
	case parser.AlterTableBatch:
		var clone *storage.Store
		clone, err = store.Clone()
		if err == nil {
			for _, action := range value.Actions {
				if _, err = executeAlterTableAction(clone, session, action); err != nil {
					break
				}
			}
		}
		if err == nil {
			err = store.ReplaceShared(clone.SharedSnapshot())
		}
		mutated = err == nil
		result = &Result{Message: "table altered"}
	case parser.CreateIndex, parser.DropIndex, parser.RenameIndex, parser.AlterColumn, parser.AlterColumnDefault, parser.AddColumn, parser.DropColumn, parser.RenameColumn, parser.AlterForeignKey, parser.AlterCheck, parser.AlterTableComment:
		result, err = executeAlterTableAction(store, session, value)
		mutated = err == nil
	case parser.RenameTable:
		result, mutated, err = executeRenameTables(store, session, value)
	case parser.Insert:
		if atomicInsert(value) {
			var clone *storage.Store
			clone, err = store.Clone()
			if err == nil {
				result, err = executeInsert(clone, e.Store, session, value, e.OptimisticTransactions)
			}
			if err == nil {
				err = validateStoreCheckConstraints(clone)
			}
			if err == nil {
				err = store.ReplaceShared(clone.SharedSnapshot())
			}
			mutated = err == nil
		} else {
			result, err = executeInsert(store, e.Store, session, value, e.OptimisticTransactions)
			mutated = err == nil
		}
	case parser.Select:
		result, err = executeQuery(store, session, value)
	case parser.Union:
		result, err = executeQuery(store, session, value)
	case parser.WithRecursive:
		result, err = executeWithRecursive(store, session, value)
	case parser.With:
		result, err = executeWith(store, session, value)
	case parser.Update:
		var clone *storage.Store
		clone, err = store.Clone()
		if err == nil {
			result, err = executeUpdate(clone, session, value)
		}
		if err == nil {
			err = validateStoreCheckConstraints(clone)
		}
		if err == nil {
			err = store.ReplaceShared(clone.SharedSnapshot())
		}
		mutated = err == nil
	case parser.Delete:
		var clone *storage.Store
		clone, err = store.Clone()
		if err == nil {
			result, err = executeDelete(clone, session, value)
		}
		if err == nil {
			err = validateStoreCheckConstraints(clone)
		}
		if err == nil {
			err = store.ReplaceShared(clone.SharedSnapshot())
		}
		mutated = err == nil
	case parser.Truncate:
		var database *storage.Database
		var table *storage.Table
		database, table, err = resolveTable(store, session, value.Table)
		if err == nil {
			var affected int
			affected, err = database.Truncate(table.Name(), !session.ForeignKeyChecksDisabled)
			result = &Result{AffectedRows: uint64(affected), Message: "table truncated"}
			mutated = err == nil
		}
	case parser.Show:
		result, err = executeShow(store, session, value)
	case parser.Explain:
		result, err = executeExplain(store, session, value.Query)
	case parser.CreateUser:
		var created uint64
		for _, user := range value.Users {
			var changed bool
			changed, err = e.Users.CreateAccount(user.Account.Username, user.Account.Host, user.Password, value.IfNotExists)
			if err != nil {
				break
			}
			if changed {
				created++
			}
		}
		result = &Result{AffectedRows: created, Message: "users created"}
	case parser.AlterUser:
		for _, user := range value.Users {
			err = e.Users.AlterAccountPassword(user.Account.Username, user.Account.Host, user.Password, value.IfExists)
			if err != nil {
				break
			}
		}
		result = &Result{AffectedRows: uint64(len(value.Users)), Message: "users altered"}
	case parser.DropUser:
		var dropped uint64
		for _, account := range value.Accounts {
			var changed bool
			changed, err = e.Users.DropAccount(account.Username, account.Host, value.IfExists)
			if err != nil {
				break
			}
			if changed {
				dropped++
			}
		}
		result = &Result{AffectedRows: dropped, Message: "users dropped"}
	case parser.RenameUser:
		for _, pair := range value.Pairs {
			err = e.Users.RenameAccount(pair.From.Username, pair.From.Host, pair.To.Username, pair.To.Host)
			if err != nil {
				break
			}
		}
		result = &Result{AffectedRows: uint64(len(value.Pairs)), Message: "users renamed"}
	case parser.SetPassword:
		account := value.Account
		if account.Username == "" {
			account = parser.Account{Username: session.Username, Host: session.Host}
		}
		err = e.Users.AlterAccountPassword(account.Username, account.Host, value.Password, false)
		result = &Result{AffectedRows: 1, Message: "password changed"}
	case parser.Grant:
		for _, account := range value.Accounts {
			err = e.Users.GrantPrivileges(account.Username, account.Host, value.Privileges, value.Database, value.Table, value.GrantOption)
			if err != nil {
				break
			}
		}
		result = &Result{Message: "privileges granted"}
	case parser.Revoke:
		for _, account := range value.Accounts {
			err = e.Users.RevokePrivileges(account.Username, account.Host, value.Privileges, value.Database, value.Table, value.GrantOptionOnly)
			if err != nil {
				break
			}
		}
		result = &Result{Message: "privileges revoked"}
	case parser.ShowGrants:
		account := value.Account
		if !value.ForAccount {
			account = parser.Account{Username: session.Username, Host: session.Host}
		}
		var grants []string
		grants, err = e.Users.ShowGrants(account.Username, account.Host)
		result = &Result{Columns: []Column{{Name: "Grants for " + account.Username + "@" + account.Host, Type: storage.TypeText}}}
		for _, grant := range grants {
			result.Rows = append(result.Rows, []any{grant})
		}
	case parser.ShowCreateUser:
		var definition string
		definition, err = e.Users.CreateUserSQL(value.Account.Username, value.Account.Host)
		result = &Result{Columns: []Column{{Name: "User", Type: storage.TypeVarchar}, {Name: "Create User", Type: storage.TypeText}}, Rows: [][]any{{value.Account.Username + "@" + value.Account.Host, definition}}}
	case parser.ExportDatabase:
		err = ExportSQL(store, value.Name, value.Path)
		result = &Result{Message: "database exported to " + value.Path}
	default:
		err = fmt.Errorf("unsupported statement %T", statement)
	}
	if err != nil {
		return nil, err
	}
	if mutated && e.legacyState(session).transaction != nil {
		e.legacyState(session).transactionDirty = true
	}
	if mutated && e.legacyState(session).transaction == nil {
		err = e.persist()
		if err != nil {
			return nil, err
		}
	}
	if mutated && e.legacyState(session).transaction == nil && e.OptimisticTransactions {
		e.commitVersion++
	}
	if query != "" && mutated && e.binlog != nil {
		change := journal.BinlogStatement{ForeignKeyChecksDisabled: session.ForeignKeyChecksDisabled, Database: databaseAtStart, SQL: query, AffectedRows: result.AffectedRows}
		if e.legacyState(session).transaction != nil {
			e.legacyState(session).binlogStatements = append(e.legacyState(session).binlogStatements, change)
		} else if err := e.appendBinlog(session, []journal.BinlogStatement{change}); err != nil {
			return nil, fmt.Errorf("change committed but binlog append failed: %w", err)
		}
	}
	if query != "" && catalogMutation(statement) {
		change := journal.BinlogStatement{ForeignKeyChecksDisabled: session.ForeignKeyChecksDisabled, Database: databaseAtStart, SQL: query, AffectedRows: result.AffectedRows}
		if err := e.appendBinlog(session, []journal.BinlogStatement{change}); err != nil {
			return nil, fmt.Errorf("account change committed but binlog append failed: %w", err)
		}
	}
	return result, nil
}

func requiresAtomicStoreMutation(statement parser.Statement) bool {
	switch value := statement.(type) {
	case parser.AlterTableBatch, parser.CreateTableLike, parser.CreateTableAs:
		return true
	case parser.Insert:
		return atomicInsert(value)
	case parser.Update, parser.Delete:
		return true
	default:
		return false
	}
}

func (e *legacyEngine) finishTransaction(session *Session) {
	e.legacyState(session).transaction = nil
	e.legacyState(session).transactionDirty = false
	e.legacyState(session).transactionVersion = 0
	e.legacyState(session).savepoints = nil
	e.legacyState(session).binlogStatements = nil
	if e.legacyState(session).transactionGate {
		e.legacyState(session).transactionGate = false
		e.txGate.Unlock()
	}
}

func (e *legacyEngine) appendBinlog(session *Session, statements []journal.BinlogStatement) error {
	if e.binlog == nil || len(statements) == 0 {
		return nil
	}
	return e.binlog.Append(journal.BinlogRecord{
		SessionID:    session.JournalSessionID,
		ConnectionID: session.ConnectionID,
		Username:     session.Username,
		RemoteIP:     session.RemoteIP,
		Statements:   statements,
	})
}

func catalogMutation(statement parser.Statement) bool {
	switch statement.(type) {
	case parser.CreateUser, parser.AlterUser, parser.DropUser, parser.RenameUser, parser.SetPassword, parser.Grant, parser.Revoke:
		return true
	default:
		return false
	}
}

func (e *legacyEngine) legacyState(session *Session) *legacySession {
	if state, ok := e.legacySessions.Load(session); ok {
		return state.(*legacySession)
	}
	state, _ := e.legacySessions.LoadOrStore(session, &legacySession{})
	return state.(*legacySession)
}
