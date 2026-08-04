package executor

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gbaselite/catalog"
	"gbaselite/internal/atomicfile"
	"gbaselite/journal"
	"gbaselite/parser"
	"gbaselite/storage"
)

const Version = "1.0.0"

var ErrPersistenceUnavailable = errors.New("database persistence is unavailable")

type Column struct {
	Name          string
	Type          storage.DataType
	Length        int
	Schema        string
	Table         string
	OriginalName  string
	Nullable      bool
	PrimaryKey    bool
	UniqueKey     bool
	MultipleKey   bool
	AutoIncrement bool
}
type Result struct {
	Columns         []Column
	Rows            [][]any
	StreamRows      func(func([]any) error) error
	StreamValues    func(func(storage.Row) error) error
	AffectedRows    uint64
	LastInsertID    uint64
	Message         string
	MetadataChanged bool
}
type Session struct {
	CurrentDatabase   string
	StreamResults     bool
	Username          string
	Host              string
	RemoteIP          string
	RemotePort        string
	ConnectionID      uint32
	SecureTransport   bool
	TLSVersion        string
	TLSCipher         string
	JournalSessionID  string
	LastInsertID      uint64
	ReplayTimestamp   time.Time
	transaction       *storage.Store
	transactionGate   bool
	binlogStatements  []journal.BinlogStatement
	temporaryTables   map[string]*storage.Table
	viewStack         map[string]bool
	copySource        *navicatCopySource
	copyTargets       map[string]string
	correlationScopes []map[string]any
}

type navicatCopySource struct {
	Database string
	Name     string
	Kind     string
}

var errRelationNotFound = errors.New("relation not found")

type Engine struct {
	Store        *storage.Store
	Persistence  *storage.Persistence
	Users        *catalog.Users
	txGate       sync.RWMutex
	copyMu       sync.Mutex
	persistMu    sync.Mutex
	persistCond  *sync.Cond
	persistNext  uint64
	persistDone  uint64
	persisting   bool
	persistErr   error
	persistFatal error
	persistSave  func(*storage.Store) error
	parseCache   sync.Map
	parseCount   int
	parseMu      sync.Mutex
	binlog       *journal.Binlog
}

func Open(dataDir, username, password string) (*Engine, error) {
	for _, directory := range []string{"databases", "tables", "users", "indexes"} {
		if err := os.MkdirAll(filepath.Join(dataDir, directory), 0o755); err != nil {
			return nil, err
		}
	}
	persistence := storage.NewPersistence(dataDir)
	store, err := persistence.Load()
	if err != nil {
		return nil, err
	}
	users, err := catalog.OpenUsers(dataDir, username, password)
	if err != nil {
		return nil, err
	}
	engine := &Engine{Store: store, Persistence: persistence, Users: users, persistSave: persistence.Save}
	engine.persistCond = sync.NewCond(&engine.persistMu)
	return engine, nil
}

func (e *Engine) Close() error { return e.persist() }

func (e *Engine) SetBinlog(binlog *journal.Binlog) { e.binlog = binlog }

// AvailabilityError reports a fatal persistence failure. Once set, the engine
// rejects SQL until restart so divergent in-memory state cannot overwrite the
// last durable snapshot.
func (e *Engine) AvailabilityError() error {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	return e.persistFatal
}

// persist combines concurrent durable writes into the same snapshot/fsync.
// A leader performs one save and then hands coordination to a pending waiter,
// preventing one request from owning every save during a sustained write burst.
func (e *Engine) persist() error {
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
func (e *Engine) CloseSession(session *Session) {
	if session == nil {
		return
	}
	rolledBack := session.transaction != nil
	e.finishTransaction(session)
	if rolledBack {
		_ = e.persist()
	}
}

func (e *Engine) Execute(session *Session, sql string) (*Result, error) {
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

const maxParsedStatements = 512

func (e *Engine) cacheStatement(query string, statement parser.Statement) {
	e.parseMu.Lock()
	defer e.parseMu.Unlock()
	if _, loaded := e.parseCache.Load(query); loaded {
		return
	}
	if e.parseCount >= maxParsedStatements {
		e.parseCache.Range(func(key, _ any) bool {
			e.parseCache.Delete(key)
			return true
		})
		e.parseCount = 0
	}
	e.parseCache.Store(query, statement)
	e.parseCount++
}

func (e *Engine) ExecuteStatement(session *Session, statement parser.Statement) (*Result, error) {
	return e.executeStatement(session, statement, "")
}

func (e *Engine) executeStatement(session *Session, statement parser.Statement, query string) (*Result, error) {
	if err := e.AvailabilityError(); err != nil {
		return nil, err
	}
	if session == nil {
		session = &Session{}
	}
	databaseAtStart := session.CurrentDatabase
	_, beginning := statement.(parser.Begin)
	if session.transaction == nil && !beginning {
		if requiresAtomicStoreMutation(statement) {
			e.txGate.Lock()
			defer e.txGate.Unlock()
		} else {
			e.txGate.RLock()
			defer e.txGate.RUnlock()
		}
		if err := e.AvailabilityError(); err != nil {
			return nil, err
		}
	}
	store := e.Store
	if session.transaction != nil {
		store = session.transaction
	}
	if err := e.authorizeStatement(session, statement); err != nil {
		return nil, err
	}
	mutated := false
	var result *Result
	var err error
	switch value := statement.(type) {
	case parser.Empty:
		return &Result{Message: "empty query"}, nil
	case parser.Begin:
		if session.transaction != nil {
			return nil, errors.New("transaction already active")
		}
		e.txGate.Lock()
		session.transactionGate = true
		if err := e.AvailabilityError(); err != nil {
			e.finishTransaction(session)
			return nil, err
		}
		session.binlogStatements = nil
		session.transaction, err = e.Store.Clone()
		if err != nil {
			e.finishTransaction(session)
		}
		result = &Result{Message: "transaction started"}
		return result, err
	case parser.Commit:
		if session.transaction == nil {
			return &Result{Message: "no active transaction"}, nil
		}
		statements := append([]journal.BinlogStatement(nil), session.binlogStatements...)
		if err = e.Store.ReplaceShared(session.transaction.SharedSnapshot()); err == nil {
			err = e.persist()
		}
		if err == nil {
			err = e.appendBinlog(session, statements)
		}
		e.finishTransaction(session)
		return &Result{Message: "transaction committed"}, err
	case parser.Rollback:
		if session.transaction == nil {
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
							err = database.AddForeignKey(tableName, storage.ForeignKey{Name: foreignKey.Name, Columns: append([]string(nil), foreignKey.Columns...), RefTable: foreignKey.RefTable, RefColumns: append([]string(nil), foreignKey.RefColumns...), OnDelete: foreignKey.OnDelete, OnUpdate: foreignKey.OnUpdate})
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
		if value.Replace {
			var clone *storage.Store
			clone, err = store.Clone()
			if err == nil {
				result, err = executeInsert(clone, e.Store, session, value)
			}
			if err == nil {
				err = validateStoreCheckConstraints(clone)
			}
			if err == nil {
				err = store.ReplaceShared(clone.SharedSnapshot())
			}
			mutated = err == nil
		} else {
			result, err = executeInsert(store, e.Store, session, value)
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
			affected, err = database.Truncate(table.Name())
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
	if mutated && session.transaction == nil {
		err = e.persist()
		if err != nil {
			return nil, err
		}
	}
	if query != "" && mutated {
		change := journal.BinlogStatement{Database: databaseAtStart, SQL: query, AffectedRows: result.AffectedRows}
		if session.transaction != nil {
			session.binlogStatements = append(session.binlogStatements, change)
		} else if err := e.appendBinlog(session, []journal.BinlogStatement{change}); err != nil {
			return nil, fmt.Errorf("change committed but binlog append failed: %w", err)
		}
	}
	if query != "" && catalogMutation(statement) {
		change := journal.BinlogStatement{Database: databaseAtStart, SQL: query, AffectedRows: result.AffectedRows}
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
		return value.Replace
	case parser.Update, parser.Delete:
		return true
	default:
		return false
	}
}

func (e *Engine) finishTransaction(session *Session) {
	session.transaction = nil
	session.binlogStatements = nil
	if session.transactionGate {
		session.transactionGate = false
		e.txGate.Unlock()
	}
}

func (e *Engine) appendBinlog(session *Session, statements []journal.BinlogStatement) error {
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

func (e *Engine) authorizeStatement(session *Session, statement parser.Statement) error {
	if session == nil || strings.TrimSpace(session.Username) == "" {
		return nil
	}
	require := func(privilege, database, table string) error {
		if e.Users.Allowed(session.Username, session.Host, privilege, database, table) {
			return nil
		}
		if database == "" {
			database = "*"
		}
		if table == "" {
			table = "*"
		}
		return fmt.Errorf("access denied for user %q@%q: %s on %s.%s is required", session.Username, session.Host, privilege, database, table)
	}
	requireTable := func(privilege, name string) error {
		database, table := splitTableName(name)
		if database == "" {
			database = session.CurrentDatabase
		}
		return require(privilege, database, table)
	}
	requireSelect := func(statement parser.Select, ignored map[string]bool) error {
		for _, name := range selectRelationNames(statement) {
			if name == "" || ignored[strings.ToLower(stripQualifier(name))] {
				continue
			}
			if err := requireTable("SELECT", name); err != nil {
				return err
			}
		}
		return nil
	}
	var requireQuery func(parser.Query, map[string]bool) error
	requireQuery = func(query parser.Query, ignored map[string]bool) error {
		switch value := query.(type) {
		case parser.Select:
			return requireSelect(value, ignored)
		case parser.Union:
			for _, branch := range value.Queries {
				if err := requireSelect(branch, ignored); err != nil {
					return err
				}
			}
			return nil
		default:
			return fmt.Errorf("unsupported query %T", query)
		}
	}
	requireAccountAdmin := func() error { return require("CREATE USER", "*", "*") }
	isSelf := func(account parser.Account) bool {
		return strings.EqualFold(account.Username, session.Username) && strings.EqualFold(account.Host, session.Host)
	}

	switch value := statement.(type) {
	case parser.Empty, parser.Begin, parser.Commit, parser.Rollback:
		return nil
	case parser.CreateDatabase:
		return require("CREATE", value.Name, "*")
	case parser.DropDatabase:
		return require("DROP", value.Name, "*")
	case parser.Use:
		if e.Users.HasDatabaseAccess(session.Username, session.Host, value.Database) {
			return nil
		}
		return fmt.Errorf("access denied for user %q@%q to database %q", session.Username, session.Host, value.Database)
	case parser.CreateTable:
		return requireTable("CREATE", value.Name)
	case parser.CreateTableLike:
		if err := requireTable("CREATE", value.Name); err != nil {
			return err
		}
		return requireTable("SELECT", value.Source)
	case parser.CreateTableAs:
		if err := requireTable("CREATE", value.Name); err != nil {
			return err
		}
		return requireQuery(value.Query, nil)
	case parser.DropTable:
		for _, name := range value.Names {
			if err := requireTable("DROP", name); err != nil {
				return err
			}
		}
		return nil
	case parser.CreateView:
		if err := requireTable("CREATE VIEW", value.Name); err != nil {
			return err
		}
		definition, err := parser.Parse(value.Definition)
		if err != nil {
			return err
		}
		if query, ok := definition.(parser.Query); ok {
			return requireQuery(query, nil)
		}
		return nil
	case parser.DropView:
		for _, name := range value.Names {
			if err := requireTable("DROP", name); err != nil {
				return err
			}
		}
		return nil
	case parser.CreateIndex:
		return requireTable("INDEX", value.Table)
	case parser.DropIndex:
		return requireTable("INDEX", value.Table)
	case parser.RenameIndex:
		return requireTable("INDEX", value.Table)
	case parser.AlterColumn:
		return requireTable("ALTER", value.Table)
	case parser.AlterColumnDefault:
		return requireTable("ALTER", value.Table)
	case parser.AddColumn:
		return requireTable("ALTER", value.Table)
	case parser.DropColumn:
		return requireTable("ALTER", value.Table)
	case parser.RenameColumn:
		return requireTable("ALTER", value.Table)
	case parser.AlterForeignKey:
		return requireTable("ALTER", value.Table)
	case parser.AlterCheck:
		return requireTable("ALTER", value.Table)
	case parser.RenameTable:
		for _, pair := range value.Pairs {
			if err := requireTable("ALTER", pair.From); err != nil {
				return err
			}
		}
		return nil
	case parser.AlterTableComment:
		return requireTable("ALTER", value.Table)
	case parser.AlterTableBatch:
		for _, action := range value.Actions {
			if err := e.authorizeStatement(session, action); err != nil {
				return err
			}
		}
		return nil
	case parser.Insert:
		if err := requireTable("INSERT", value.Table); err != nil {
			return err
		}
		if value.Replace {
			if err := requireTable("DELETE", value.Table); err != nil {
				return err
			}
		}
		if value.Select != nil {
			return requireQuery(value.Select, nil)
		}
		return nil
	case parser.Select:
		return requireSelect(value, nil)
	case parser.Union:
		return requireQuery(value, nil)
	case parser.Explain:
		return requireQuery(value.Query, nil)
	case parser.WithRecursive:
		if err := requireSelect(value.Seed, nil); err != nil {
			return err
		}
		ignored := map[string]bool{strings.ToLower(value.Name): true}
		if err := requireSelect(value.Recursive, ignored); err != nil {
			return err
		}
		return requireSelect(value.Query, ignored)
	case parser.With:
		ignored := make(map[string]bool, len(value.Expressions))
		for _, expression := range value.Expressions {
			if err := requireQuery(expression.Query, ignored); err != nil {
				return err
			}
			ignored[strings.ToLower(expression.Name)] = true
		}
		return requireQuery(value.Query, ignored)
	case parser.Update:
		if err := requireTable("UPDATE", value.Table); err != nil {
			return err
		}
		if len(value.Joins) == 0 {
			return nil
		}
		return requireSelect(parser.Select{Table: value.Table, TableAlias: value.TableAlias, Joins: value.Joins, Where: value.Where}, nil)
	case parser.Delete:
		sources := map[string]string{}
		registerDeleteSource := func(name, alias string) {
			if name == "" {
				return
			}
			_, base := splitTableName(name)
			sources[strings.ToLower(name)] = name
			sources[strings.ToLower(base)] = name
			if alias != "" {
				sources[strings.ToLower(alias)] = name
			}
		}
		registerDeleteSource(value.Table, value.TableAlias)
		for _, join := range value.Joins {
			if join.Subquery == nil {
				registerDeleteSource(join.Table, join.TableAlias)
			}
		}
		targets := value.Targets
		if len(targets) == 0 {
			targets = []string{value.Table}
		}
		for _, target := range targets {
			name := sources[strings.ToLower(target)]
			if name == "" {
				_, base := splitTableName(target)
				name = sources[strings.ToLower(base)]
			}
			if name == "" {
				return fmt.Errorf("unknown DELETE target %s", target)
			}
			if err := requireTable("DELETE", name); err != nil {
				return err
			}
		}
		if len(value.Joins) == 0 && len(value.Targets) == 0 {
			return nil
		}
		return requireSelect(parser.Select{Table: value.Table, TableAlias: value.TableAlias, Joins: value.Joins, Where: value.Where}, nil)
	case parser.Truncate:
		return requireTable("DROP", value.Table)
	case parser.Show:
		switch value.What {
		case "DATABASES":
			return nil
		case "TABLES", "FULL TABLES":
			if e.Users.HasDatabaseAccess(session.Username, session.Host, session.CurrentDatabase) {
				return nil
			}
			return fmt.Errorf("access denied for user %q@%q to database %q", session.Username, session.Host, session.CurrentDatabase)
		case "COLUMNS", "CREATE TABLE", "CREATE VIEW", "INDEX":
			database, table := splitTableName(value.Name)
			if database == "" {
				database = session.CurrentDatabase
			}
			if e.Users.HasObjectAccess(session.Username, session.Host, database, table) {
				return nil
			}
			return fmt.Errorf("access denied for user %q@%q to %s.%s", session.Username, session.Host, database, table)
		}
		return nil
	case parser.CreateUser, parser.DropUser, parser.RenameUser:
		return requireAccountAdmin()
	case parser.AlterUser:
		for _, user := range value.Users {
			if !isSelf(user.Account) {
				return requireAccountAdmin()
			}
		}
		return nil
	case parser.SetPassword:
		if value.Account.Username == "" || isSelf(value.Account) {
			return nil
		}
		return requireAccountAdmin()
	case parser.Grant:
		for _, privilege := range value.Privileges {
			normalized, err := catalog.NormalizePrivilege(privilege)
			if err != nil {
				return err
			}
			if !e.Users.CanGrant(session.Username, session.Host, normalized, value.Database, value.Table) {
				return fmt.Errorf("access denied for user %q@%q: cannot grant %s on %s.%s", session.Username, session.Host, normalized, value.Database, value.Table)
			}
		}
		return nil
	case parser.Revoke:
		for _, privilege := range value.Privileges {
			normalized, err := catalog.NormalizePrivilege(privilege)
			if err != nil {
				return err
			}
			if !e.Users.CanGrant(session.Username, session.Host, normalized, value.Database, value.Table) {
				return fmt.Errorf("access denied for user %q@%q: cannot revoke %s on %s.%s", session.Username, session.Host, normalized, value.Database, value.Table)
			}
		}
		return nil
	case parser.ShowGrants:
		if !value.ForAccount || isSelf(value.Account) {
			return nil
		}
		return requireAccountAdmin()
	case parser.ShowCreateUser:
		if isSelf(value.Account) {
			return nil
		}
		return requireAccountAdmin()
	case parser.ExportDatabase:
		return require("SELECT", value.Name, "*")
	default:
		return nil
	}
}

func executeInsert(store, autoIncrementStore *storage.Store, session *Session, statement parser.Insert) (*Result, error) {
	database, table, copyTargetKey, err := resolveInsertTable(store, session, statement.Table)
	if err != nil {
		return nil, err
	}
	var reservationTable *storage.Table
	if autoIncrementStore != nil && autoIncrementStore != store {
		if reservationDatabase, databaseErr := autoIncrementStore.Database(database.Name()); databaseErr == nil {
			reservationTable, _ = reservationDatabase.Table(table.Name())
		}
	}
	columns := table.ColumnsView()
	positions := make([]int, len(columns))
	for i := range positions {
		positions[i] = i
	}
	if len(statement.Columns) > 0 {
		positions = make([]int, len(statement.Columns))
		seen := map[int]bool{}
		for i, name := range statement.Columns {
			index, ok := table.ColumnIndex(name)
			if !ok {
				return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, name)
			}
			if seen[index] {
				return nil, fmt.Errorf("duplicate column %s", name)
			}
			seen[index] = true
			positions[i] = index
		}
	}
	newRow := func() (storage.Row, error) {
		row := make(storage.Row, len(columns))
		for i, column := range columns {
			if column.AutoIncrement {
				var next int64
				next, err = table.NextAutoIncrement(column.Name)
				if err != nil {
					return nil, err
				}
				if reservationTable != nil {
					if reservationErr := reservationTable.AdvanceAutoIncrement(column.Name, next+1); reservationErr != nil {
						return nil, reservationErr
					}
				}
				row[i], err = storage.NewValue(column.Type, next)
				if err != nil {
					return nil, err
				}
			} else if column.HasDefault {
				row[i], err = columnDefaultValue(column)
				if err != nil {
					return nil, err
				}
			} else {
				row[i] = storage.NullValue(column.Type)
			}
		}
		return row, nil
	}
	var lastInsertID uint64
	autoOmitted := false
	for index, column := range columns {
		if !column.AutoIncrement {
			continue
		}
		found := false
		for _, position := range positions {
			if position == index {
				found = true
				break
			}
		}
		if !found {
			autoOmitted = true
		}
	}
	insertRow := func(row storage.Row, generatedAuto bool) (uint64, error) {
		for index, column := range columns {
			if !column.AutoIncrement || row[index].Null {
				continue
			}
			if err := table.AdvanceAutoIncrement(column.Name, row[index].Int64+1); err != nil {
				return 0, err
			}
			if reservationTable != nil {
				if err := reservationTable.AdvanceAutoIncrement(column.Name, row[index].Int64+1); err != nil {
					return 0, err
				}
			}
		}
		if err := validateCheckConstraints(table, row); err != nil {
			return 0, err
		}
		if statement.Replace {
			replaced, replaceErr := database.ReplaceRow(table.Name(), row)
			if replaceErr == nil && generatedAuto && lastInsertID == 0 {
				for index, column := range columns {
					if column.AutoIncrement && !row[index].Null {
						lastInsertID = uint64(row[index].Int64)
						break
					}
				}
			}
			return uint64(replaced), replaceErr
		}
		insertErr := database.Insert(table.Name(), row)
		if errors.Is(insertErr, storage.ErrDuplicateKey) && len(statement.OnDuplicate) > 0 {
			updated, updateErr := executeInsertDuplicateUpdate(database, table, row, statement.OnDuplicate)
			if updateErr != nil {
				return 0, updateErr
			}
			if updated {
				return 1, nil
			}
			return 0, nil
		}
		if statement.Ignore && errors.Is(insertErr, storage.ErrDuplicateKey) {
			return 0, nil
		}
		if insertErr == nil && generatedAuto && lastInsertID == 0 {
			for index, column := range columns {
				if column.AutoIncrement && !row[index].Null {
					lastInsertID = uint64(row[index].Int64)
					break
				}
			}
		}
		if insertErr == nil {
			return 1, nil
		}
		return 0, insertErr
	}
	insertInterfaces := func(values []any) (uint64, error) {
		if len(values) != len(positions) {
			return 0, fmt.Errorf("%w: expected %d values, got %d", storage.ErrColumnCount, len(positions), len(values))
		}
		row, rowErr := newRow()
		if rowErr != nil {
			return 0, rowErr
		}
		generatedAuto := autoOmitted
		for i, raw := range values {
			column := columns[positions[i]]
			if column.AutoIncrement && raw == nil {
				generatedAuto = true
				continue
			}
			converted, conversionErr := storage.NewValue(column.Type, raw)
			if conversionErr != nil {
				return 0, conversionErr
			}
			row[positions[i]] = converted
		}
		return insertRow(row, generatedAuto)
	}
	var affected uint64
	if statement.Select != nil {
		selectSession := *session
		selectSession.StreamResults = false
		selected, selectErr := executeQuery(store, &selectSession, statement.Select)
		if selectErr != nil {
			return nil, selectErr
		}
		rows, collectErr := collectResultRows(selected)
		if collectErr != nil {
			return nil, collectErr
		}
		for _, values := range rows {
			rowAffected, insertErr := insertInterfaces(values)
			if insertErr != nil {
				return nil, insertErr
			}
			affected += rowAffected
		}
	} else {
		if len(statement.SetValues) > 0 {
			if len(statement.SetValues) != len(positions) {
				return nil, fmt.Errorf("%w: expected %d values, got %d", storage.ErrColumnCount, len(positions), len(statement.SetValues))
			}
			row, rowErr := newRow()
			if rowErr != nil {
				return nil, rowErr
			}
			generatedAuto := autoOmitted
			for i, expression := range statement.SetValues {
				expression, rowErr = materializeInSubqueries(store, session, expression)
				if rowErr != nil {
					return nil, rowErr
				}
				raw, evaluationErr := evaluateExprWithContext(expression, table, row, session, store)
				if evaluationErr != nil {
					return nil, evaluationErr
				}
				column := columns[positions[i]]
				if column.AutoIncrement && raw == nil {
					generatedAuto = true
					continue
				}
				row[positions[i]], rowErr = interfaceToColumnValue(raw, column)
				if rowErr != nil {
					return nil, rowErr
				}
			}
			rowAffected, insertErr := insertRow(row, generatedAuto)
			if insertErr != nil {
				return nil, insertErr
			}
			affected += rowAffected
		} else {
			for _, literals := range statement.Values {
				if len(literals) != len(positions) {
					return nil, fmt.Errorf("%w: expected %d values, got %d", storage.ErrColumnCount, len(positions), len(literals))
				}
				row, rowErr := newRow()
				if rowErr != nil {
					return nil, rowErr
				}
				generatedAuto := autoOmitted
				for i, literal := range literals {
					if columns[positions[i]].AutoIncrement && literal.Kind == parser.LiteralNull {
						generatedAuto = true
						continue
					}
					value, conversionErr := literalToValue(literal, columns[positions[i]])
					if conversionErr != nil {
						return nil, conversionErr
					}
					row[positions[i]] = value
				}
				rowAffected, insertErr := insertRow(row, generatedAuto)
				if insertErr != nil {
					return nil, insertErr
				}
				affected += rowAffected
			}
		}
	}
	if copyTargetKey != "" {
		delete(session.copyTargets, copyTargetKey)
	}
	if lastInsertID != 0 {
		session.LastInsertID = lastInsertID
	}
	message := "rows inserted"
	if statement.Replace {
		message = "rows replaced"
	}
	return &Result{AffectedRows: affected, LastInsertID: lastInsertID, Message: message}, nil
}

func executeInsertDuplicateUpdate(database *storage.Database, table *storage.Table, candidate storage.Row, assignments []parser.InsertAssignment) (bool, error) {
	indexes := table.Indexes()
	var duplicate storage.Row
	for _, row := range table.Select(nil) {
		matched := false
		for _, index := range indexes {
			if !index.Unique {
				continue
			}
			all := true
			for _, column := range index.Columns {
				position, ok := table.ColumnIndex(column)
				if !ok || candidate[position].Null || row[position].Null || compareAny(candidate[position].Interface(), row[position].Interface()) != 0 {
					all = false
					break
				}
			}
			if all {
				matched = true
				break
			}
		}
		if matched {
			duplicate = row
			break
		}
	}
	if duplicate == nil {
		return false, storage.ErrDuplicateKey
	}
	updated := append(storage.Row(nil), duplicate...)
	assigned := make(map[int]bool, len(assignments))
	for _, assignment := range assignments {
		value, err := evaluateExprWithLookup(assignment.Value, func(name string) (any, error) {
			if strings.HasPrefix(strings.ToUpper(name), "VALUES(") {
				columnName := strings.TrimSuffix(strings.TrimPrefix(name, "VALUES("), ")")
				position, ok := table.ColumnIndex(columnName)
				if !ok {
					return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, columnName)
				}
				return candidate[position].Interface(), nil
			}
			position, ok := table.ColumnIndex(name)
			if !ok {
				return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, name)
			}
			return updated[position].Interface(), nil
		})
		if err != nil {
			return false, err
		}
		position, ok := table.ColumnIndex(assignment.Column)
		if !ok {
			return false, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, assignment.Column)
		}
		converted, err := interfaceToColumnValue(value, table.ColumnsView()[position])
		if err != nil {
			return false, err
		}
		updated[position] = converted
		assigned[position] = true
	}
	for position, column := range table.ColumnsView() {
		if assigned[position] || column.OnUpdate == "" {
			continue
		}
		if strings.EqualFold(column.OnUpdate, "CURRENT_TIMESTAMP") || strings.EqualFold(column.OnUpdate, "CURRENT_TIMESTAMP()") {
			converted, conversionErr := storage.NewValue(column.Type, time.Now())
			if conversionErr != nil {
				return false, conversionErr
			}
			updated[position] = converted
		}
	}
	if err := validateCheckConstraints(table, updated); err != nil {
		return false, err
	}
	predicate := func(row storage.Row) bool {
		for _, index := range indexes {
			if !index.Unique {
				continue
			}
			same := true
			for _, column := range index.Columns {
				position, ok := table.ColumnIndex(column)
				if !ok || row[position].Interface() != duplicate[position].Interface() {
					same = false
					break
				}
			}
			if same {
				return true
			}
		}
		return false
	}
	_, err := database.ReplaceRowsLimit(table.Name(), predicate, []storage.Row{updated}, 1)
	return err == nil, err
}

func validateCheckConstraints(table *storage.Table, row storage.Row) error {
	for _, definition := range table.Checks() {
		expression, err := parser.ParseExpression(definition)
		if err != nil {
			return fmt.Errorf("%w: %v", storage.ErrCheckConstraint, err)
		}
		value, err := evaluateExpr(expression, table, row)
		if err != nil {
			return fmt.Errorf("%w: %v", storage.ErrCheckConstraint, err)
		}
		if value != nil && !truthy(value) {
			return fmt.Errorf("%w: %s", storage.ErrCheckConstraint, definition)
		}
	}
	return nil
}

func executeAlterTableAction(store *storage.Store, session *Session, statement parser.Statement) (*Result, error) {
	switch value := statement.(type) {
	case parser.CreateIndex:
		_, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			if value.Primary {
				err = table.AddPrimaryKey(value.Columns)
			} else {
				err = table.AddIndex(value.Name, value.Columns, value.Unique)
			}
		}
		return &Result{Message: "index created"}, err
	case parser.DropIndex:
		_, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			err = table.DropIndex(value.Name)
		}
		return &Result{Message: "index dropped"}, err
	case parser.RenameIndex:
		_, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			err = table.RenameIndex(value.OldName, value.NewName)
		}
		return &Result{Message: "index renamed"}, err
	case parser.AlterColumn:
		database, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			var column storage.Column
			column, err = storageColumnDefinition(value.Column)
			if err == nil {
				err = database.AlterColumn(table.Name(), value.OldName, column)
			}
		}
		return &Result{Message: "column altered"}, err
	case parser.AlterColumnDefault:
		database, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			position, exists := table.ColumnIndex(value.Name)
			if !exists {
				err = fmt.Errorf("%w: %s", storage.ErrColumnNotFound, value.Name)
			} else {
				column := table.ColumnsView()[position]
				if value.Drop {
					column.HasDefault = false
					column.Default = storage.Value{}
					column.DefaultExpression = ""
				} else {
					column.HasDefault = true
					column.DefaultExpression = value.DefaultExpression
					if value.DefaultExpression == "" {
						column.Default, err = literalToValue(value.Default, column)
						if err == nil && column.Default.Null && !storage.ColumnNullable(column) {
							err = fmt.Errorf("column %q cannot have DEFAULT NULL", column.Name)
						}
					} else {
						_, err = columnDefaultValue(column)
					}
				}
				if err == nil {
					err = database.AlterColumn(table.Name(), column.Name, column)
				}
			}
		}
		return &Result{Message: "column default altered"}, err
	case parser.AddColumn:
		_, table, err := resolveTable(store, session, value.Table)
		if err == nil && value.IfNotExists {
			if _, exists := table.ColumnIndex(value.Column.Name); exists {
				return &Result{Message: "column already exists"}, nil
			}
		}
		if err == nil && (value.Column.PrimaryKey || value.Column.Unique || value.Column.Check != "") {
			err = errors.New("ADD COLUMN with inline PRIMARY KEY, UNIQUE, or CHECK is not supported; add the column and constraint separately")
		}
		if err == nil {
			var column storage.Column
			column, err = storageColumnDefinition(value.Column)
			if err == nil {
				fill := storage.NullValue(column.Type)
				if column.HasDefault {
					fill, err = columnDefaultValue(column)
				}
				position := -1
				if value.First {
					position = 0
				} else if value.After != "" {
					after, exists := table.ColumnIndex(value.After)
					if !exists {
						err = fmt.Errorf("%w: %s", storage.ErrColumnNotFound, value.After)
					} else {
						position = after + 1
					}
				}
				if err == nil {
					err = table.AddColumn(column, fill, position)
				}
			}
		}
		return &Result{Message: "column added"}, err
	case parser.DropColumn:
		database, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			if _, exists := table.ColumnIndex(value.Name); !exists && value.IfExists {
				return &Result{Message: "column did not exist"}, nil
			}
			err = database.DropColumn(table.Name(), value.Name)
		}
		return &Result{Message: "column dropped"}, err
	case parser.RenameColumn:
		database, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			position, exists := table.ColumnIndex(value.OldName)
			if !exists {
				err = fmt.Errorf("%w: %s", storage.ErrColumnNotFound, value.OldName)
			} else {
				column := table.ColumnsView()[position]
				column.Name = value.NewName
				err = database.AlterColumn(table.Name(), value.OldName, column)
			}
		}
		return &Result{Message: "column renamed"}, err
	case parser.AlterForeignKey:
		database, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			if value.Drop {
				err = table.DropForeignKey(value.Name)
			} else {
				foreignKey := value.ForeignKey
				err = database.AddForeignKey(table.Name(), storage.ForeignKey{Name: foreignKey.Name, Columns: foreignKey.Columns, RefTable: foreignKey.RefTable, RefColumns: foreignKey.RefColumns, OnDelete: foreignKey.OnDelete, OnUpdate: foreignKey.OnUpdate})
			}
		}
		return &Result{Message: "foreign key altered"}, err
	case parser.AlterCheck:
		_, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			if value.Drop {
				err = table.DropCheck(value.Name)
			} else if err = validateCheckDefinition(table, value.Check.Expression); err == nil {
				err = table.AddCheck(storage.CheckConstraint{Name: value.Check.Name, Expression: value.Check.Expression})
			}
		}
		return &Result{Message: "check constraint altered"}, err
	case parser.AlterTableComment:
		_, table, err := resolveTable(store, session, value.Table)
		if err == nil {
			table.SetComment(value.Comment)
		}
		return &Result{Message: "table comment altered"}, err
	default:
		return nil, fmt.Errorf("unsupported ALTER TABLE action %T", statement)
	}
}

func validateCheckDefinition(table *storage.Table, definition string) error {
	expression, err := parser.ParseExpression(definition)
	if err != nil {
		return fmt.Errorf("%w: %v", storage.ErrCheckConstraint, err)
	}
	rows := table.Select(nil)
	if len(rows) == 0 {
		row := make(storage.Row, len(table.ColumnsView()))
		for index, column := range table.ColumnsView() {
			row[index] = storage.NullValue(column.Type)
		}
		if _, err := evaluateExpr(expression, table, row); err != nil {
			return fmt.Errorf("%w: %v", storage.ErrCheckConstraint, err)
		}
		return nil
	}
	for rowIndex, row := range rows {
		value, err := evaluateExpr(expression, table, row)
		if err != nil {
			return fmt.Errorf("%w: %v", storage.ErrCheckConstraint, err)
		}
		if value != nil && !truthy(value) {
			return fmt.Errorf("%w: existing row %d violates %s", storage.ErrCheckConstraint, rowIndex+1, definition)
		}
	}
	return nil
}

func executeRenameTables(store *storage.Store, session *Session, statement parser.RenameTable) (*Result, bool, error) {
	var database *storage.Database
	pairs := make(map[string]string, len(statement.Pairs))
	for _, pair := range statement.Pairs {
		fromDatabase, fromTable := splitTableName(pair.From)
		toDatabase, toTable := splitTableName(pair.To)
		if fromDatabase == "" {
			fromDatabase = session.CurrentDatabase
		}
		if toDatabase == "" {
			toDatabase = fromDatabase
		}
		if fromDatabase == "" || !strings.EqualFold(fromDatabase, toDatabase) {
			return nil, false, errors.New("cross-database RENAME TABLE is not supported")
		}
		resolved, err := store.Database(fromDatabase)
		if err != nil {
			return nil, false, err
		}
		if database != nil && !strings.EqualFold(database.Name(), resolved.Name()) {
			return nil, false, errors.New("all RENAME TABLE pairs must use the same database")
		}
		database = resolved
		pairs[fromTable] = toTable
	}
	if database == nil {
		return &Result{Message: "no tables renamed"}, false, nil
	}
	if err := database.RenameTables(pairs); err != nil {
		return nil, false, err
	}
	session.copySource = nil
	return &Result{AffectedRows: uint64(len(pairs)), Message: "tables renamed", MetadataChanged: true}, true, nil
}

func executeDropTables(store *storage.Store, session *Session, statement parser.DropTable) (*Result, bool, error) {
	type dropTarget struct {
		database *storage.Database
		table    string
	}
	targets := make([]dropTarget, 0, len(statement.Names))
	for _, name := range statement.Names {
		databaseName, tableName := splitTableName(name)
		database, err := selectedDatabase(store, session, databaseName)
		if err != nil {
			if statement.IfExists && errors.Is(err, storage.ErrDatabaseNotFound) {
				continue
			}
			return nil, false, err
		}
		if _, err = database.Table(tableName); err != nil {
			if statement.IfExists && errors.Is(err, storage.ErrTableNotFound) {
				continue
			}
			return nil, false, err
		}
		targets = append(targets, dropTarget{database: database, table: tableName})
	}
	var dropped uint64
	for _, target := range targets {
		if err := target.database.DropTable(target.table); err != nil {
			if statement.IfExists && errors.Is(err, storage.ErrTableNotFound) {
				continue
			}
			return nil, dropped > 0, err
		}
		clearCopySourceAfterDrop(session, target.database.Name(), target.table, "table")
		dropped++
	}
	return &Result{AffectedRows: dropped, Message: "tables dropped"}, dropped > 0, nil
}

func executeCreateView(store *storage.Store, session *Session, statement parser.CreateView) (*Result, bool, error) {
	databaseName, viewName := splitTableName(statement.Name)
	requestedViewName := viewName
	database, err := selectedDatabase(store, session, databaseName)
	if err != nil {
		return nil, false, err
	}
	if !statement.OrReplace && !statement.AlterOnly {
		viewName, err = navicatBackupTarget(database, session, viewName, "view", statement.HasCreateOptions)
		if err != nil {
			return nil, false, err
		}
	}
	session.copySource = nil
	previous, previousErr := database.View(viewName)
	hadPrevious := previousErr == nil
	if statement.AlterOnly && !hadPrevious {
		return nil, false, previousErr
	}
	if err := database.CreateView(viewName, statement.Definition, statement.Columns, statement.OrReplace); err != nil {
		return nil, false, err
	}
	qualifiedName := viewName
	if databaseName != "" {
		qualifiedName = databaseName + "." + viewName
	}
	if _, err := resolveSelectSource(store, session, qualifiedName, "", nil); err != nil &&
		!(statement.HasCreateOptions && errors.Is(err, errRelationNotFound)) {
		if hadPrevious {
			_ = database.CreateView(previous.Name, previous.Definition, previous.Columns, true)
		} else {
			_ = database.DropView(viewName)
		}
		return nil, false, fmt.Errorf("invalid view %s: %w", statement.Name, err)
	}
	renamed := !strings.EqualFold(requestedViewName, viewName)
	if renamed {
		rememberCopyTarget(session, database.Name(), requestedViewName, viewName)
	}
	return &Result{Message: copyCreatedMessage("view", requestedViewName, viewName), MetadataChanged: renamed}, true, nil
}

func executeCreateTableLike(store *storage.Store, session *Session, statement parser.CreateTableLike) (*Result, bool, error) {
	targetDatabaseName, targetTableName := splitTableName(statement.Name)
	targetDatabase, err := selectedDatabase(store, session, targetDatabaseName)
	if err != nil {
		return nil, false, err
	}
	if _, err := targetDatabase.Table(targetTableName); err == nil {
		if statement.IfNotExists {
			return &Result{Message: "table already exists"}, false, nil
		}
		return nil, false, fmt.Errorf("%w: %q", storage.ErrTableExists, targetTableName)
	} else if !errors.Is(err, storage.ErrTableNotFound) {
		return nil, false, err
	}
	if _, err := targetDatabase.View(targetTableName); err == nil {
		if statement.IfNotExists {
			return &Result{Message: "table already exists"}, false, nil
		}
		return nil, false, fmt.Errorf("%w: %q is a view", storage.ErrTableExists, targetTableName)
	}

	sourceDatabaseName, sourceTableName := splitTableName(statement.Source)
	sourceDatabase, err := selectedDatabase(store, session, sourceDatabaseName)
	if err != nil {
		return nil, false, err
	}
	source, err := sourceDatabase.Table(sourceTableName)
	if err != nil {
		return nil, false, err
	}
	snapshot := source.Snapshot()
	var primaryKey []string
	indexes := make([]storage.Index, 0, len(snapshot.Indexes))
	for _, index := range snapshot.Indexes {
		if index.Primary || strings.EqualFold(index.Name, "PRIMARY") {
			primaryKey = append([]string(nil), index.Columns...)
			continue
		}
		indexes = append(indexes, index)
	}
	table, err := targetDatabase.CreateTableWithIndexes(targetTableName, snapshot.Columns, primaryKey, indexes)
	if err != nil {
		return nil, false, err
	}
	for _, check := range snapshot.CheckConstraints {
		if err := table.AddCheck(check); err != nil {
			return nil, false, err
		}
	}
	table.SetComment(snapshot.Comment)
	return &Result{Message: "table created"}, true, nil
}

func executeCreateTableAs(store *storage.Store, session *Session, statement parser.CreateTableAs) (*Result, bool, error) {
	targetDatabaseName, targetTableName := splitTableName(statement.Name)
	database, err := selectedDatabase(store, session, targetDatabaseName)
	if err != nil {
		return nil, false, err
	}
	if _, err := database.Table(targetTableName); err == nil {
		if statement.IfNotExists {
			return &Result{Message: "table already exists"}, false, nil
		}
		return nil, false, fmt.Errorf("%w: %q", storage.ErrTableExists, targetTableName)
	} else if !errors.Is(err, storage.ErrTableNotFound) {
		return nil, false, err
	}
	if _, err := database.View(targetTableName); err == nil {
		if statement.IfNotExists {
			return &Result{Message: "table already exists"}, false, nil
		}
		return nil, false, fmt.Errorf("%w: %q is a view", storage.ErrTableExists, targetTableName)
	}

	selectSession := *session
	selectSession.StreamResults = false
	selected, err := executeQuery(store, &selectSession, statement.Query)
	if err != nil {
		return nil, false, err
	}
	rows, err := collectResultRows(selected)
	if err != nil {
		return nil, false, err
	}
	uniquifyResultColumnNames(selected.Columns)
	columns := make([]storage.Column, len(selected.Columns))
	for index, column := range selected.Columns {
		length := column.Length
		if column.Type == storage.TypeVarchar && length <= 0 {
			length = 65535
		}
		nullable := column.Nullable
		for _, row := range rows {
			if index < len(row) && row[index] == nil {
				nullable = true
				break
			}
		}
		columns[index] = storage.Column{
			Name:            column.Name,
			Type:            column.Type,
			Length:          length,
			MetadataVersion: 1,
			Nullable:        nullable,
		}
	}
	table, err := database.CreateTable(targetTableName, columns)
	if err != nil {
		return nil, false, err
	}
	for _, values := range rows {
		if len(values) != len(columns) {
			return nil, false, fmt.Errorf("%w: expected %d values, got %d", storage.ErrColumnCount, len(columns), len(values))
		}
		row := make(storage.Row, len(values))
		for index, value := range values {
			row[index], err = storage.NewValue(columns[index].Type, value)
			if err != nil {
				return nil, false, err
			}
		}
		if err := table.Insert(row); err != nil {
			return nil, false, err
		}
	}
	return &Result{AffectedRows: uint64(len(rows)), Message: "table created"}, true, nil
}

func executeDropViews(store *storage.Store, session *Session, statement parser.DropView) (*Result, bool, error) {
	type target struct {
		database *storage.Database
		name     string
	}
	targets := make([]target, 0, len(statement.Names))
	for _, name := range statement.Names {
		databaseName, viewName := splitTableName(name)
		database, err := selectedDatabase(store, session, databaseName)
		if err != nil {
			if statement.IfExists && errors.Is(err, storage.ErrDatabaseNotFound) {
				continue
			}
			return nil, false, err
		}
		if _, err := database.View(viewName); err != nil {
			if statement.IfExists && errors.Is(err, storage.ErrViewNotFound) {
				continue
			}
			return nil, false, err
		}
		targets = append(targets, target{database: database, name: viewName})
	}
	for _, target := range targets {
		if err := target.database.DropView(target.name); err != nil {
			return nil, false, err
		}
		clearCopySourceAfterDrop(session, target.database.Name(), target.name, "view")
	}
	return &Result{AffectedRows: uint64(len(targets)), Message: "views dropped"}, len(targets) > 0, nil
}

func executeSelect(store *storage.Store, session *Session, statement parser.Select) (*Result, error) {
	if !statement.Distinct {
		return executeSelectCore(store, session, statement)
	}
	query := statement
	query.Distinct = false
	query.HasLimit = false
	query.Limit, query.Offset = 0, 0
	result, err := executeSelectCore(store, session, query)
	if err != nil {
		return nil, err
	}
	rows, err := collectResultRows(result)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(rows))
	distinct := make([][]any, 0, len(rows))
	for _, row := range rows {
		key := groupedRowKey(row)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		distinct = append(distinct, row)
	}
	start := statement.Offset
	if start > len(distinct) {
		start = len(distinct)
	}
	end := len(distinct)
	if statement.HasLimit && start+statement.Limit < end {
		end = start + statement.Limit
	}
	return &Result{Columns: result.Columns, Rows: distinct[start:end]}, nil
}

func executeQuery(store *storage.Store, session *Session, query parser.Query) (*Result, error) {
	switch value := query.(type) {
	case parser.Select:
		return executeSelect(store, session, value)
	case parser.Union:
		return executeUnion(store, session, value)
	default:
		return nil, fmt.Errorf("unsupported query %T", query)
	}
}

func executeUnion(store *storage.Store, session *Session, statement parser.Union) (*Result, error) {
	if len(statement.Queries) == 0 || len(statement.All) != len(statement.Queries)-1 {
		return nil, errors.New("invalid UNION query")
	}
	var columns []Column
	rows := make([][]any, 0)
	for queryIndex, query := range statement.Queries {
		result, err := executeSelect(store, session, query)
		if err != nil {
			return nil, err
		}
		queryRows, err := collectResultRows(result)
		if err != nil {
			return nil, err
		}
		if queryIndex == 0 {
			columns = append([]Column(nil), result.Columns...)
		} else if len(result.Columns) != len(columns) {
			return nil, fmt.Errorf("%w: UNION query %d returns %d columns, expected %d", storage.ErrColumnCount, queryIndex+1, len(result.Columns), len(columns))
		}
		rows = append(rows, queryRows...)
		if queryIndex > 0 && !statement.All[queryIndex-1] {
			seen := make(map[string]struct{}, len(rows))
			distinct := rows[:0]
			for _, row := range rows {
				key := groupedRowKey(row)
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				distinct = append(distinct, row)
			}
			rows = distinct
		}
	}
	orderPositions := make([]int, len(statement.OrderBy))
	for index, order := range statement.OrderBy {
		name := strings.TrimSpace(order.Column)
		if ordinal, err := strconv.Atoi(name); err == nil {
			if ordinal < 1 || ordinal > len(columns) {
				return nil, fmt.Errorf("ORDER BY position %d is out of range", ordinal)
			}
			orderPositions[index] = ordinal - 1
			continue
		}
		position := -1
		for columnIndex, column := range columns {
			if strings.EqualFold(column.Name, name) {
				position = columnIndex
				break
			}
		}
		if position < 0 {
			return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, name)
		}
		orderPositions[index] = position
	}
	if len(orderPositions) > 0 {
		sort.SliceStable(rows, func(leftIndex, rightIndex int) bool {
			left, right := rows[leftIndex], rows[rightIndex]
			for index, position := range orderPositions {
				comparison := compareAny(left[position], right[position])
				if comparison == 0 {
					continue
				}
				if statement.OrderBy[index].Desc {
					return comparison > 0
				}
				return comparison < 0
			}
			return false
		})
	}
	start := statement.Offset
	if start > len(rows) {
		start = len(rows)
	}
	end := len(rows)
	if statement.HasLimit && start+statement.Limit < end {
		end = start + statement.Limit
	}
	return &Result{Columns: columns, Rows: rows[start:end]}, nil
}

func executeExplain(store *storage.Store, session *Session, query parser.Query) (*Result, error) {
	if union, ok := query.(parser.Union); ok {
		var combined *Result
		for index, branch := range union.Queries {
			result, err := executeExplainSelect(store, session, branch)
			if err != nil {
				return nil, err
			}
			if combined == nil {
				combined = result
			} else {
				combined.Rows = append(combined.Rows, result.Rows...)
			}
			for row := range result.Rows {
				if index > 0 {
					result.Rows[row][1] = "UNION"
				}
			}
		}
		return combined, nil
	}
	statement, ok := query.(parser.Select)
	if !ok {
		return nil, fmt.Errorf("unsupported EXPLAIN query %T", query)
	}
	return executeExplainSelect(store, session, statement)
}

func executeExplainSelect(store *storage.Store, session *Session, statement parser.Select) (*Result, error) {
	columns := []Column{
		{Name: "id", Type: storage.TypeBigInt},
		{Name: "select_type", Type: storage.TypeVarchar},
		{Name: "table", Type: storage.TypeVarchar},
		{Name: "partitions", Type: storage.TypeVarchar},
		{Name: "type", Type: storage.TypeVarchar},
		{Name: "possible_keys", Type: storage.TypeVarchar},
		{Name: "key", Type: storage.TypeVarchar},
		{Name: "key_len", Type: storage.TypeVarchar},
		{Name: "ref", Type: storage.TypeVarchar},
		{Name: "rows", Type: storage.TypeBigInt},
		{Name: "filtered", Type: storage.TypeDouble},
		{Name: "Extra", Type: storage.TypeText},
	}
	result := &Result{Columns: columns}
	if statement.Table == "" && statement.Subquery == nil {
		result.Rows = [][]any{{int64(1), "SIMPLE", nil, nil, nil, nil, nil, nil, nil, int64(1), float64(100), "No tables used"}}
		return result, nil
	}
	if statement.Subquery != nil {
		result.Rows = [][]any{{int64(1), "PRIMARY", "<derived2>", nil, "ALL", nil, nil, nil, nil, int64(0), float64(100), "Using temporary"}}
		return result, nil
	}
	table, err := resolveSelectSource(store, session, statement.Table, statement.TableAlias, nil)
	if err != nil {
		return nil, err
	}
	possible := possibleIndexes(table, statement.Where)
	accessType := "ALL"
	var selectedKey any
	estimatedRows := int64(table.RowCount())
	plan := planIndexAccess(statement, table)
	if plan != nil {
		accessType = plan.AccessType
		selectedKey = plan.Scan.Name
		if rows, scanErr := table.ScanIndex(plan.Scan, nil, 0, -1); scanErr == nil {
			estimatedRows = int64(len(rows))
		}
	}
	var possibleValue any
	if len(possible) > 0 {
		possibleValue = strings.Join(possible, ",")
	}
	extra := make([]string, 0, 2)
	if statement.Where != nil {
		extra = append(extra, "Using where")
	}
	if len(statement.OrderBy) > 0 && (plan == nil || !plan.OrderSatisfied) {
		extra = append(extra, "Using filesort")
	}
	if plan != nil && (plan.Scan.Lower != nil || plan.Scan.Upper != nil) {
		extra = append(extra, "Using index condition")
	}
	tableName := statement.TableAlias
	if tableName == "" {
		_, tableName = splitTableName(statement.Table)
	}
	result.Rows = append(result.Rows, []any{int64(1), "SIMPLE", tableName, nil, accessType, possibleValue, selectedKey, nil, nil, estimatedRows, float64(100), strings.Join(extra, "; ")})
	for _, join := range statement.Joins {
		joinTable, joinErr := resolveSelectSource(store, session, join.Table, join.TableAlias, join.Subquery)
		if joinErr != nil {
			return nil, joinErr
		}
		name := join.TableAlias
		if name == "" {
			_, name = splitTableName(join.Table)
		}
		result.Rows = append(result.Rows, []any{int64(1), "SIMPLE", name, nil, "ALL", nil, nil, nil, nil, int64(joinTable.RowCount()), float64(100), "Using where; Using join buffer"})
	}
	return result, nil
}

func possibleIndexes(table *storage.Table, expression parser.Expr) []string {
	referenced := make(map[string]bool)
	var visit func(parser.Expr)
	visit = func(current parser.Expr) {
		switch value := current.(type) {
		case parser.BinaryExpr:
			visit(value.Left)
			visit(value.Right)
		case parser.Identifier:
			referenced[strings.ToLower(stripQualifier(value.Name))] = true
		case parser.InExpr:
			visit(value.Value)
		case parser.BetweenExpr:
			visit(value.Value)
		}
	}
	visit(expression)
	names := make([]string, 0)
	for _, index := range table.Indexes() {
		if len(index.Columns) > 0 && referenced[strings.ToLower(index.Columns[0])] {
			names = append(names, index.Name)
		}
	}
	sort.Strings(names)
	return names
}

func uniquePredicateIndex(table *storage.Table, expression parser.Expr) (string, bool) {
	comparison, ok := expression.(parser.BinaryExpr)
	if !ok || comparison.Operator != "=" && comparison.Operator != "<=>" {
		return "", false
	}
	identifier, identifierOK := comparison.Left.(parser.Identifier)
	_, literalOK := comparison.Right.(parser.LiteralExpr)
	if !identifierOK || !literalOK {
		identifier, identifierOK = comparison.Right.(parser.Identifier)
		_, literalOK = comparison.Left.(parser.LiteralExpr)
	}
	if !identifierOK || !literalOK {
		return "", false
	}
	for _, index := range table.Indexes() {
		if index.Unique && len(index.Columns) == 1 && strings.EqualFold(index.Columns[0], stripQualifier(identifier.Name)) {
			return index.Name, true
		}
	}
	return "", false
}

type indexAccessPlan struct {
	Scan           storage.IndexScan
	AccessType     string
	OrderSatisfied bool
	Score          int
}

type indexCondition struct {
	equal *storage.Value
	lower *storage.IndexBound
	upper *storage.IndexBound
}

func planIndexAccess(statement parser.Select, table *storage.Table) *indexAccessPlan {
	if selectHasWindow(statement.Items) || len(statement.GroupBy) > 0 || statement.Having != nil || selectHasAggregate(statement.Items) && !(len(statement.Items) == 1 && isCountExpression(statement.Items[0].Expression)) {
		return nil
	}
	conditions := make(map[string]indexCondition)
	var collect func(parser.Expr)
	collect = func(expression parser.Expr) {
		binary, ok := expression.(parser.BinaryExpr)
		if !ok {
			return
		}
		if binary.Operator == "AND" {
			collect(binary.Left)
			collect(binary.Right)
			return
		}
		identifier, identifierOK := binary.Left.(parser.Identifier)
		literal, literalOK := binary.Right.(parser.LiteralExpr)
		operator := binary.Operator
		if !identifierOK || !literalOK {
			identifier, identifierOK = binary.Right.(parser.Identifier)
			literal, literalOK = binary.Left.(parser.LiteralExpr)
			operator = reverseComparisonOperator(operator)
		}
		if !identifierOK || !literalOK || literal.Value.Kind == parser.LiteralNull {
			return
		}
		position, exists := table.ColumnIndex(stripQualifier(identifier.Name))
		if !exists {
			return
		}
		converted, err := literalToValue(literal.Value, table.ColumnsView()[position])
		if err != nil {
			return
		}
		name := strings.ToLower(table.ColumnsView()[position].Name)
		condition := conditions[name]
		switch operator {
		case "=", "<=>":
			condition.equal = &converted
		case ">", ">=":
			condition.lower = &storage.IndexBound{Value: converted, Inclusive: operator == ">="}
		case "<", "<=":
			condition.upper = &storage.IndexBound{Value: converted, Inclusive: operator == "<="}
		default:
			return
		}
		conditions[name] = condition
	}
	collect(statement.Where)

	var best *indexAccessPlan
	for _, definition := range table.Indexes() {
		plan := &indexAccessPlan{Scan: storage.IndexScan{Name: definition.Name}, AccessType: "index"}
		for _, column := range definition.Columns {
			condition := conditions[strings.ToLower(column)]
			if condition.equal == nil {
				break
			}
			plan.Scan.EqualPrefix = append(plan.Scan.EqualPrefix, *condition.equal)
		}
		if len(plan.Scan.EqualPrefix) < len(definition.Columns) {
			condition := conditions[strings.ToLower(definition.Columns[len(plan.Scan.EqualPrefix)])]
			plan.Scan.Lower, plan.Scan.Upper = condition.lower, condition.upper
		}
		hasRange := plan.Scan.Lower != nil || plan.Scan.Upper != nil
		if len(plan.Scan.EqualPrefix) > 0 {
			plan.AccessType = "ref"
			plan.Score += len(plan.Scan.EqualPrefix) * 10
			if definition.Unique && len(plan.Scan.EqualPrefix) == len(definition.Columns) {
				plan.AccessType = "const"
				plan.Score += 10
			}
		}
		if hasRange {
			plan.AccessType = "range"
			plan.Score += 5
		}
		plan.OrderSatisfied, plan.Scan.Descending = indexSatisfiesOrder(definition, len(plan.Scan.EqualPrefix), statement.OrderBy)
		if plan.OrderSatisfied {
			plan.Score += 3
		}
		if len(plan.Scan.EqualPrefix) == 0 && !hasRange && !plan.OrderSatisfied {
			continue
		}
		if best == nil || plan.Score > best.Score {
			best = plan
		}
	}
	return best
}

func reverseComparisonOperator(operator string) string {
	switch operator {
	case ">":
		return "<"
	case ">=":
		return "<="
	case "<":
		return ">"
	case "<=":
		return ">="
	default:
		return operator
	}
}

func indexSatisfiesOrder(index storage.Index, equalityColumns int, order []parser.Order) (bool, bool) {
	if len(order) == 0 {
		return false, false
	}
	if equalityColumns+len(order) > len(index.Columns) {
		return false, false
	}
	descending := order[0].Desc
	for position, item := range order {
		if item.Desc != descending || !strings.EqualFold(stripQualifier(item.Column), index.Columns[equalityColumns+position]) {
			return false, false
		}
	}
	return true, descending
}

func executeIndexProjection(table *storage.Table, scan storage.IndexScan, predicate storage.Predicate, selected []int, columns []Column, offset, limit int, stream bool) (*Result, error) {
	rows, err := table.ScanIndex(scan, predicate, offset, limit)
	if err != nil {
		return nil, err
	}
	result := &Result{Columns: columns}
	completeRow := len(selected) == len(table.ColumnsView())
	for index, position := range selected {
		if position != index {
			completeRow = false
			break
		}
	}
	if stream && completeRow {
		result.StreamValues = func(yield func(storage.Row) error) error {
			for _, row := range rows {
				if err := yield(row); err != nil {
					return err
				}
			}
			return nil
		}
		return result, nil
	}
	project := func(row storage.Row) []any {
		values := make([]any, len(selected))
		for index, position := range selected {
			values[index] = row[position].Interface()
		}
		return values
	}
	if stream {
		result.StreamRows = func(yield func([]any) error) error {
			for _, row := range rows {
				if err := yield(project(row)); err != nil {
					return err
				}
			}
			return nil
		}
		return result, nil
	}
	result.Rows = make([][]any, len(rows))
	for index, row := range rows {
		result.Rows[index] = project(row)
	}
	return result, nil
}

func collectResultRows(result *Result) ([][]any, error) {
	rows := append([][]any(nil), result.Rows...)
	if result.StreamRows != nil {
		if err := result.StreamRows(func(row []any) error {
			rows = append(rows, append([]any(nil), row...))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if result.StreamValues != nil {
		if err := result.StreamValues(func(row storage.Row) error {
			values := make([]any, len(row))
			for index := range row {
				values[index] = row[index].Interface()
			}
			rows = append(rows, values)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

const maxRecursiveCTEDepth = 1000

func executeWithRecursive(store *storage.Store, session *Session, statement parser.WithRecursive) (*Result, error) {
	seed, err := executeSelect(store, session, statement.Seed)
	if err != nil {
		return nil, fmt.Errorf("recursive CTE %s seed: %w", statement.Name, err)
	}
	seedRows, err := collectResultRows(seed)
	if err != nil {
		return nil, err
	}
	seed.Rows, seed.StreamRows, seed.StreamValues = seedRows, nil, nil
	accumulated, err := materializeDerivedTable(statement.Name, seed)
	if err != nil {
		return nil, err
	}
	delta, err := materializeDerivedTable(statement.Name, seed)
	if err != nil {
		return nil, err
	}

	if session.temporaryTables == nil {
		session.temporaryTables = make(map[string]*storage.Table)
	}
	key := strings.ToLower(statement.Name)
	previous, hadPrevious := session.temporaryTables[key]
	defer func() {
		if hadPrevious {
			session.temporaryTables[key] = previous
		} else {
			delete(session.temporaryTables, key)
		}
	}()

	for depth := 0; delta.RowCount() > 0; depth++ {
		if depth >= maxRecursiveCTEDepth {
			return nil, fmt.Errorf("recursive CTE %s exceeded %d iterations", statement.Name, maxRecursiveCTEDepth)
		}
		session.temporaryTables[key] = delta
		iteration, iterationErr := executeSelect(store, session, statement.Recursive)
		if iterationErr != nil {
			return nil, fmt.Errorf("recursive CTE %s iteration %d: %w", statement.Name, depth+1, iterationErr)
		}
		iterationRows, rowsErr := collectResultRows(iteration)
		if rowsErr != nil {
			return nil, rowsErr
		}
		if len(iterationRows) == 0 {
			break
		}
		if len(iteration.Columns) != len(seed.Columns) {
			return nil, fmt.Errorf("recursive CTE %s returned %d columns, expected %d", statement.Name, len(iteration.Columns), len(seed.Columns))
		}
		// A recursive CTE takes its public column names and types from the seed.
		iteration.Columns = append([]Column(nil), seed.Columns...)
		iteration.Rows, iteration.StreamRows, iteration.StreamValues = iterationRows, nil, nil
		delta, err = materializeDerivedTable(statement.Name, iteration)
		if err != nil {
			return nil, err
		}
		if err := appendResultRows(accumulated, iterationRows); err != nil {
			return nil, err
		}
	}

	session.temporaryTables[key] = accumulated
	return executeSelect(store, session, statement.Query)
}

func executeWith(store *storage.Store, session *Session, statement parser.With) (*Result, error) {
	if session.temporaryTables == nil {
		session.temporaryTables = make(map[string]*storage.Table)
	}
	type previousTable struct {
		table  *storage.Table
		exists bool
	}
	previous := make(map[string]previousTable, len(statement.Expressions))
	defer func() {
		for key, value := range previous {
			if value.exists {
				session.temporaryTables[key] = value.table
			} else {
				delete(session.temporaryTables, key)
			}
		}
	}()

	for _, expression := range statement.Expressions {
		key := strings.ToLower(expression.Name)
		if _, exists := previous[key]; !exists {
			table, hadPrevious := session.temporaryTables[key]
			previous[key] = previousTable{table: table, exists: hadPrevious}
		}
		result, err := executeQuery(store, session, expression.Query)
		if err != nil {
			return nil, fmt.Errorf("CTE %s: %w", expression.Name, err)
		}
		if len(expression.Columns) > 0 {
			if len(expression.Columns) != len(result.Columns) {
				return nil, fmt.Errorf("CTE %s defines %d column names for %d result columns", expression.Name, len(expression.Columns), len(result.Columns))
			}
			for index, name := range expression.Columns {
				result.Columns[index].Name = name
			}
		}
		table, err := materializeDerivedTable(expression.Name, result)
		if err != nil {
			return nil, err
		}
		session.temporaryTables[key] = table
	}
	return executeQuery(store, session, statement.Query)
}

func appendResultRows(table *storage.Table, rows [][]any) error {
	columns := table.ColumnsView()
	for _, values := range rows {
		if len(values) != len(columns) {
			return fmt.Errorf("recursive CTE row has %d values, expected %d", len(values), len(columns))
		}
		row := make(storage.Row, len(values))
		for index, value := range values {
			converted, err := storage.NewValue(columns[index].Type, value)
			if err != nil {
				return err
			}
			row[index] = converted
		}
		if err := table.Insert(row); err != nil {
			return err
		}
	}
	return nil
}

func executeSelectCore(store *storage.Store, session *Session, statement parser.Select) (*Result, error) {
	var predicateErr error
	result, err := executeSelectCoreInner(store, session, statement, &predicateErr)
	if err == nil && predicateErr != nil {
		return nil, predicateErr
	}
	return result, err
}

func executeSelectCoreInner(store *storage.Store, session *Session, statement parser.Select, predicateErr *error) (*Result, error) {
	var err error
	if statement.Table == "" && statement.Subquery == nil {
		return executeScalarSelect(session, statement)
	}
	table, err := resolveSelectSource(store, session, statement.Table, statement.TableAlias, statement.Subquery)
	if err != nil {
		return nil, err
	}
	accessTable := table
	if statement.Subquery != nil || len(statement.Joins) > 0 {
		accessTable = nil
	}
	if len(statement.Joins) > 0 {
		qualifier := statement.TableAlias
		if qualifier == "" {
			_, qualifier = splitTableName(statement.Table)
		}
		table, err = qualifyRelation(table, qualifier)
		if err != nil {
			return nil, err
		}
		for _, join := range statement.Joins {
			right, sourceErr := resolveSelectSource(store, session, join.Table, join.TableAlias, join.Subquery)
			if sourceErr != nil {
				return nil, sourceErr
			}
			rightQualifier := join.TableAlias
			if rightQualifier == "" {
				_, rightQualifier = splitTableName(join.Table)
			}
			right, sourceErr = qualifyRelation(right, rightQualifier)
			if sourceErr != nil {
				return nil, sourceErr
			}
			table, err = joinRelations(store, session, table, right, join)
			if err != nil {
				return nil, err
			}
		}
	}
	if statement.TableAlias != "" && len(statement.Joins) == 0 && statement.Subquery == nil {
		table, err = qualifyRelation(table, statement.TableAlias)
		if err != nil {
			return nil, err
		}
	}
	columns := table.ColumnsView()
	predicate := expressionPredicateWithContextCapture(statement.Where, table, session, store, predicateErr)
	indexedRow, indexedFound, indexed := storage.Row(nil), false, false
	var accessPlan *indexAccessPlan
	if accessTable != nil {
		indexedRow, indexedFound, indexed = lookupUniquePredicate(statement.Where, accessTable, accessTable.ColumnsView())
		accessPlan = planIndexAccess(statement, accessTable)
	}
	if selectHasWindow(statement.Items) {
		return executeWindowSelect(table, predicate, statement, columns)
	}
	if len(statement.GroupBy) == 0 && statement.Having == nil && len(statement.Items) == 1 && isCountExpression(statement.Items[0].Expression) {
		name := statement.Items[0].Alias
		if name == "" {
			name = "COUNT(*)"
		}
		count := table.RowCount()
		if indexed {
			count = 0
			if indexedFound && (predicate == nil || predicate(indexedRow)) {
				count = 1
			}
		} else if accessPlan != nil {
			rows, scanErr := accessTable.ScanIndex(accessPlan.Scan, predicate, 0, -1)
			if scanErr != nil {
				return nil, scanErr
			}
			count = len(rows)
		} else if predicate != nil {
			count = table.Count(predicate)
		}
		result := &Result{Columns: []Column{{Name: name, Type: storage.TypeBigInt}}}
		if statement.Offset > 0 || (statement.HasLimit && statement.Limit == 0) {
			return result, nil
		}
		result.Rows = [][]any{{int64(count)}}
		return result, nil
	}
	if len(statement.GroupBy) > 0 || selectHasAggregate(statement.Items) {
		return executeGroupedSelect(table, predicate, statement, columns)
	}
	if statement.Having != nil {
		return nil, errors.New("HAVING requires GROUP BY or an aggregate expression")
	}
	if selectHasExpressions(statement.Items, table) || selectHasExpressionOrder(statement, table) {
		return executeExpressionSelect(store, table, accessTable, accessPlan, predicate, session, statement)
	}
	resultColumnCount := len(statement.Items)
	for _, item := range statement.Items {
		if item.Expression == "*" {
			resultColumnCount += len(columns) - 1
		}
	}
	selected := make([]int, 0, resultColumnCount)
	resultColumns := make([]Column, 0, resultColumnCount)
	sourceSchema, sourceTable := "", ""
	if statement.Subquery == nil && len(statement.Joins) == 0 {
		sourceSchema, sourceTable = splitTableName(statement.Table)
		if sourceSchema == "" {
			sourceSchema = session.CurrentDatabase
		}
	}
	resultColumn := func(column storage.Column, label string) Column {
		result := Column{Name: label, Type: column.Type, Length: column.Length, Schema: sourceSchema, Table: sourceTable, OriginalName: stripQualifier(column.Name), Nullable: storage.ColumnNullable(column), AutoIncrement: column.AutoIncrement}
		if sourceTable != "" {
			switch table.ColumnKey(column.Name) {
			case "PRI":
				result.PrimaryKey = true
			case "UNI":
				result.UniqueKey = true
			case "MUL":
				result.MultipleKey = true
			}
		}
		return result
	}
	for _, item := range statement.Items {
		if item.Expression == "*" {
			for i, column := range columns {
				selected = append(selected, i)
				resultColumns = append(resultColumns, resultColumn(column, stripQualifier(column.Name)))
			}
			continue
		}
		name := strings.TrimSpace(item.Expression)
		index, ok := queryColumnIndex(table, name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, name)
		}
		selected = append(selected, index)
		label := item.Alias
		if label == "" {
			label = stripQualifier(columns[index].Name)
		}
		resultColumns = append(resultColumns, resultColumn(columns[index], label))
	}
	if indexed {
		result := &Result{Columns: resultColumns}
		if !indexedFound || (predicate != nil && !predicate(indexedRow)) || statement.Offset > 0 || (statement.HasLimit && statement.Limit == 0) {
			return result, nil
		}
		completeRow := len(selected) == len(columns)
		for index, selectedIndex := range selected {
			if selectedIndex != index {
				completeRow = false
				break
			}
		}
		if session.StreamResults && completeRow {
			result.StreamValues = func(yield func(storage.Row) error) error { return yield(indexedRow) }
			return result, nil
		}
		projected := make([]any, len(selected))
		for index, position := range selected {
			projected[index] = indexedRow[position].Interface()
		}
		if session.StreamResults {
			result.StreamRows = func(yield func([]any) error) error { return yield(projected) }
		} else {
			result.Rows = [][]any{projected}
		}
		return result, nil
	}

	limit := -1
	if statement.HasLimit {
		limit = statement.Limit
	}
	if len(statement.OrderBy) == 0 {
		if accessPlan != nil {
			return executeIndexProjection(accessTable, accessPlan.Scan, predicate, selected, resultColumns, statement.Offset, limit, session.StreamResults)
		}
		if session.StreamResults {
			completeRow := len(selected) == len(columns)
			for index, selectedIndex := range selected {
				if selectedIndex != index {
					completeRow = false
					break
				}
			}
			if completeRow {
				return &Result{
					Columns: resultColumns,
					StreamValues: func(yield func(storage.Row) error) error {
						return table.Stream(predicate, statement.Offset, limit, yield)
					},
				}, nil
			}
			return &Result{
				Columns: resultColumns,
				StreamRows: func(yield func([]any) error) error {
					return table.StreamProject(predicate, selected, statement.Offset, limit, yield)
				},
			}, nil
		}
		return &Result{
			Columns: resultColumns,
			Rows:    table.Project(predicate, selected, statement.Offset, limit),
		}, nil
	}
	if accessPlan != nil && accessPlan.OrderSatisfied {
		return executeIndexProjection(accessTable, accessPlan.Scan, predicate, selected, resultColumns, statement.Offset, limit, session.StreamResults)
	}

	// Keep only projected columns and ORDER BY keys in memory. This is much
	// smaller than cloning complete storage.Value rows before sorting.
	scanColumns := append([]int(nil), selected...)
	positions := make(map[int]int, len(scanColumns))
	for position, index := range scanColumns {
		if _, exists := positions[index]; !exists {
			positions[index] = position
		}
	}
	orderPositions := make([]int, 0, len(statement.OrderBy))
	orderDescending := make([]bool, 0, len(statement.OrderBy))
	for _, order := range statement.OrderBy {
		index, ok := resolvePlainOrderColumn(order.Column, statement.Items, table)
		if !ok {
			return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, order.Column)
		}
		position, exists := positions[index]
		if !exists {
			position = len(scanColumns)
			positions[index] = position
			scanColumns = append(scanColumns, index)
		}
		orderPositions = append(orderPositions, position)
		orderDescending = append(orderDescending, order.Desc)
	}
	rows := table.Project(predicate, scanColumns, 0, -1)
	sort.SliceStable(rows, func(i, j int) bool {
		for index, position := range orderPositions {
			comparison := compareAny(rows[i][position], rows[j][position])
			if comparison != 0 {
				if orderDescending[index] {
					return comparison > 0
				}
				return comparison < 0
			}
		}
		return false
	})
	start := statement.Offset
	if start > len(rows) {
		start = len(rows)
	}
	end := len(rows)
	if statement.HasLimit && start+statement.Limit < end {
		end = start + statement.Limit
	}
	rows = rows[start:end]
	if len(scanColumns) != len(selected) {
		for index := range rows {
			rows[index] = rows[index][:len(selected)]
		}
	}
	return &Result{Columns: resultColumns, Rows: rows}, nil
}

func resolveSelectSource(store *storage.Store, session *Session, tableName, alias string, subquery parser.Query) (*storage.Table, error) {
	if subquery != nil {
		inner, err := executeQuery(store, session, subquery)
		if err != nil {
			return nil, err
		}
		return materializeDerivedTable(alias, inner)
	}
	if session != nil && !strings.Contains(tableName, ".") {
		if table, ok := session.temporaryTables[strings.ToLower(tableName)]; ok {
			return table, nil
		}
	}
	databaseName, relationName := splitTableName(tableName)
	database, err := selectedDatabase(store, session, databaseName)
	if err != nil {
		return nil, err
	}
	if table, tableErr := database.Table(relationName); tableErr == nil {
		return table, nil
	}
	view, err := database.View(relationName)
	if err != nil {
		legacyName := strings.TrimLeft(relationName, "0123456789")
		if legacyName != "" && legacyName != relationName {
			view, err = database.View(legacyName)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errRelationNotFound, tableName)
	}
	key := strings.ToLower(database.Name() + "." + view.Name)
	if session.viewStack == nil {
		session.viewStack = make(map[string]bool)
	}
	if session.viewStack[key] {
		return nil, fmt.Errorf("circular view reference detected at %s", tableName)
	}
	session.viewStack[key] = true
	defer delete(session.viewStack, key)

	statement, err := parser.Parse(view.Definition)
	if err != nil {
		return nil, fmt.Errorf("parse view %s: %w", tableName, err)
	}
	previousDatabase := session.CurrentDatabase
	session.CurrentDatabase = database.Name()
	defer func() { session.CurrentDatabase = previousDatabase }()
	var result *Result
	switch query := statement.(type) {
	case parser.Query:
		result, err = executeQuery(store, session, query)
	case parser.WithRecursive:
		result, err = executeWithRecursive(store, session, query)
	case parser.With:
		result, err = executeWith(store, session, query)
	default:
		err = fmt.Errorf("view definition must be SELECT, got %T", statement)
	}
	if err != nil {
		return nil, err
	}
	if len(view.Columns) > 0 {
		if len(view.Columns) != len(result.Columns) {
			return nil, fmt.Errorf("view %s defines %d column names for %d result columns", tableName, len(view.Columns), len(result.Columns))
		}
		for index, name := range view.Columns {
			result.Columns[index].Name = name
		}
	} else {
		uniquifyResultColumnNames(result.Columns)
	}
	if alias == "" {
		alias = view.Name
	}
	return materializeDerivedTable(alias, result)
}

func uniquifyResultColumnNames(columns []Column) {
	used := make(map[string]struct{}, len(columns))
	for index := range columns {
		base := columns[index].Name
		if base == "" {
			base = fmt.Sprintf("column_%d", index+1)
		}
		candidate := base
		for suffix := 2; ; suffix++ {
			key := strings.ToLower(candidate)
			if _, exists := used[key]; !exists {
				used[key] = struct{}{}
				columns[index].Name = candidate
				break
			}
			candidate = fmt.Sprintf("%s_%d", base, suffix)
		}
	}
}

func materializeDerivedTable(alias string, result *Result) (*storage.Table, error) {
	columns := make([]storage.Column, len(result.Columns))
	for index, column := range result.Columns {
		length := 0
		if column.Type == storage.TypeVarchar {
			length = 65535
		}
		columns[index] = storage.Column{Name: column.Name, Type: column.Type, Length: length}
	}
	table, err := storage.NewTransientTable(alias, columns)
	if err != nil {
		return nil, fmt.Errorf("invalid derived table %q: %w", alias, err)
	}
	insertInterfaces := func(values []any) error {
		if len(values) != len(columns) {
			return fmt.Errorf("derived row has %d values, expected %d", len(values), len(columns))
		}
		row := make(storage.Row, len(values))
		for index, value := range values {
			converted, conversionErr := storage.NewValue(columns[index].Type, value)
			if conversionErr != nil {
				return conversionErr
			}
			row[index] = converted
		}
		return table.Insert(row)
	}
	for _, row := range result.Rows {
		if err := insertInterfaces(row); err != nil {
			return nil, err
		}
	}
	if result.StreamRows != nil {
		if err := result.StreamRows(insertInterfaces); err != nil {
			return nil, err
		}
	}
	if result.StreamValues != nil {
		if err := result.StreamValues(func(row storage.Row) error { return table.Insert(row) }); err != nil {
			return nil, err
		}
	}
	return table, nil
}

func qualifyRelation(source *storage.Table, qualifier string) (*storage.Table, error) {
	if qualifier == "" {
		return nil, errors.New("query source requires a table name or alias")
	}
	sourceColumns := source.ColumnsView()
	columns := make([]storage.Column, len(sourceColumns))
	for index, column := range sourceColumns {
		column.Name = qualifier + "." + stripQualifier(column.Name)
		columns[index] = column
	}
	qualified, err := storage.NewTransientTable("query_source", columns)
	if err != nil {
		return nil, err
	}
	err = source.Visit(nil, func(row storage.Row) error { return qualified.Insert(row) })
	return qualified, err
}

func joinRelations(store *storage.Store, session *Session, left, right *storage.Table, join parser.Join) (*storage.Table, error) {
	leftColumns := append([]storage.Column(nil), left.ColumnsView()...)
	rightColumns := append([]storage.Column(nil), right.ColumnsView()...)
	if join.Type == "RIGHT" {
		for index := range leftColumns {
			leftColumns[index].MetadataVersion = 1
			leftColumns[index].Nullable = true
		}
	}
	if join.Type == "LEFT" {
		for index := range rightColumns {
			rightColumns[index].MetadataVersion = 1
			rightColumns[index].Nullable = true
		}
	}
	columns := append(append([]storage.Column(nil), leftColumns...), rightColumns...)
	joined, err := storage.NewTransientTable("joined_result", columns)
	if err != nil {
		return nil, err
	}
	join.On, err = prepareWriteExpression(store, session, join.On, joined)
	if err != nil {
		return nil, err
	}
	rightRows := right.Select(nil)
	rightMatched := make([]bool, len(rightRows))
	leftJoinColumn, rightJoinColumn, hashJoin := equalityJoinColumns(join.On, left, right)
	var rightBuckets map[joinHashKey][]int
	if hashJoin {
		rightBuckets = make(map[joinHashKey][]int, len(rightRows))
		for index, row := range rightRows {
			if key, comparable := valueJoinHashKey(row[rightJoinColumn]); comparable {
				rightBuckets[key] = append(rightBuckets[key], index)
			}
		}
	}
	err = left.Visit(nil, func(leftRow storage.Row) error {
		matched := false
		insertMatch := func(rightIndex int) error {
			rightRow := rightRows[rightIndex]
			candidate := make(storage.Row, 0, len(columns))
			candidate = append(candidate, leftRow...)
			candidate = append(candidate, rightRow...)
			if !hashJoin && join.Type != "CROSS" && join.On != nil {
				matchedValue, evaluationErr := evaluateExprWithContext(join.On, joined, candidate, session, store)
				if evaluationErr != nil {
					return evaluationErr
				}
				if !truthy(matchedValue) {
					return nil
				}
			}
			matched = true
			rightMatched[rightIndex] = true
			return joined.Insert(candidate)
		}
		if hashJoin {
			if key, comparable := valueJoinHashKey(leftRow[leftJoinColumn]); comparable {
				for _, rightIndex := range rightBuckets[key] {
					if err := insertMatch(rightIndex); err != nil {
						return err
					}
				}
			}
		} else {
			for rightIndex := range rightRows {
				if err := insertMatch(rightIndex); err != nil {
					return err
				}
			}
		}
		if !matched && join.Type == "LEFT" {
			candidate := append(storage.Row(nil), leftRow...)
			for _, column := range rightColumns {
				candidate = append(candidate, storage.NullValue(column.Type))
			}
			return joined.Insert(candidate)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if join.Type == "RIGHT" {
		for index, rightRow := range rightRows {
			if rightMatched[index] {
				continue
			}
			candidate := make(storage.Row, 0, len(columns))
			for _, column := range leftColumns {
				candidate = append(candidate, storage.NullValue(column.Type))
			}
			candidate = append(candidate, rightRow...)
			if err := joined.Insert(candidate); err != nil {
				return nil, err
			}
		}
	}
	return joined, nil
}

type joinHashKey struct {
	kind   uint8
	number uint64
	text   string
}

func equalityJoinColumns(expression parser.Expr, left, right *storage.Table) (int, int, bool) {
	comparison, ok := expression.(parser.BinaryExpr)
	if !ok || comparison.Operator != "=" {
		return 0, 0, false
	}
	leftIdentifier, leftOK := comparison.Left.(parser.Identifier)
	rightIdentifier, rightOK := comparison.Right.(parser.Identifier)
	if !leftOK || !rightOK {
		return 0, 0, false
	}
	leftColumn, leftFound := queryColumnIndex(left, leftIdentifier.Name)
	rightColumn, rightFound := queryColumnIndex(right, rightIdentifier.Name)
	if leftFound && rightFound {
		return leftColumn, rightColumn, true
	}
	leftColumn, leftFound = queryColumnIndex(left, rightIdentifier.Name)
	rightColumn, rightFound = queryColumnIndex(right, leftIdentifier.Name)
	return leftColumn, rightColumn, leftFound && rightFound
}

func valueJoinHashKey(value storage.Value) (joinHashKey, bool) {
	if value.Null {
		return joinHashKey{}, false
	}
	switch value.Type {
	case storage.TypeInt, storage.TypeBigInt:
		return joinHashKey{kind: 1, number: math.Float64bits(float64(value.Int64))}, true
	case storage.TypeFloat, storage.TypeDouble:
		if value.Float == 0 {
			return joinHashKey{kind: 1}, true
		}
		return joinHashKey{kind: 1, number: math.Float64bits(value.Float)}, true
	case storage.TypeVarchar, storage.TypeText:
		return joinHashKey{kind: 2, text: value.Text}, true
	case storage.TypeBoolean:
		if value.Bool {
			return joinHashKey{kind: 3, number: 1}, true
		}
		return joinHashKey{kind: 3}, true
	case storage.TypeDate, storage.TypeDateTime:
		return joinHashKey{kind: 4, number: uint64(value.Date.UnixNano())}, true
	default:
		return joinHashKey{}, false
	}
}

type projectedExpression struct {
	expression parser.Expr
	column     Column
}

type projectedRow struct {
	values []any
	keys   []any
}

func selectHasExpressions(items []parser.SelectItem, table *storage.Table) bool {
	for _, item := range items {
		if strings.TrimSpace(item.Expression) == "*" {
			continue
		}
		if _, ok := queryColumnIndex(table, item.Expression); !ok {
			return true
		}
	}
	return false
}

func selectHasExpressionOrder(statement parser.Select, table *storage.Table) bool {
	for _, order := range statement.OrderBy {
		if _, ok := resolvePlainOrderColumn(order.Column, statement.Items, table); !ok {
			return true
		}
	}
	return false
}

func executeExpressionSelect(store *storage.Store, table, accessTable *storage.Table, accessPlan *indexAccessPlan, predicate storage.Predicate, session *Session, statement parser.Select) (*Result, error) {
	tableColumns := table.ColumnsView()
	plans := make([]projectedExpression, 0, len(statement.Items))
	for _, item := range statement.Items {
		if strings.TrimSpace(item.Expression) == "*" {
			for _, source := range tableColumns {
				plans = append(plans, projectedExpression{expression: parser.Identifier{Name: source.Name}, column: Column{Name: stripQualifier(source.Name), Type: source.Type}})
			}
			continue
		}
		expression, err := parser.ParseExpression(item.Expression)
		if err != nil {
			return nil, err
		}
		name := item.Alias
		if name == "" {
			name = item.Expression
			if identifier, ok := expression.(parser.Identifier); ok {
				name = stripQualifier(identifier.Name)
			}
		}
		dataType, err := expressionTypeWithSession(expression, table, tableColumns, session)
		if err != nil {
			return nil, err
		}
		plans = append(plans, projectedExpression{expression: expression, column: Column{Name: name, Type: dataType}})
	}
	result := &Result{Columns: make([]Column, len(plans))}
	for index := range plans {
		result.Columns[index] = plans[index].column
	}
	project := func(row storage.Row) ([]any, error) {
		values := make([]any, len(plans))
		for index, plan := range plans {
			value, err := evaluateExprWithContext(plan.expression, table, row, session, store)
			if err != nil {
				return nil, err
			}
			values[index] = value
		}
		return values, nil
	}
	var sourceRows []storage.Row
	indexOrdered, indexLimited := false, false
	if accessTable != nil && accessPlan != nil {
		indexOrdered = len(statement.OrderBy) == 0 || accessPlan.OrderSatisfied
		offset, limit := 0, -1
		if indexOrdered {
			offset = statement.Offset
			if statement.HasLimit {
				limit = statement.Limit
			}
			indexLimited = true
		}
		var err error
		sourceRows, err = accessTable.ScanIndex(accessPlan.Scan, predicate, offset, limit)
		if err != nil {
			return nil, err
		}
	}
	if len(statement.OrderBy) == 0 && session.StreamResults {
		result.StreamRows = func(yield func([]any) error) error {
			if sourceRows != nil {
				for _, row := range sourceRows {
					values, err := project(row)
					if err != nil {
						return err
					}
					if err := yield(values); err != nil {
						return err
					}
				}
				return nil
			}
			limit := -1
			if statement.HasLimit {
				limit = statement.Limit
			}
			return table.Stream(predicate, statement.Offset, limit, func(row storage.Row) error {
				values, err := project(row)
				if err != nil {
					return err
				}
				return yield(values)
			})
		}
		return result, nil
	}

	orderExpressions := make([]parser.Expr, len(statement.OrderBy))
	orderPositions := make([]int, len(statement.OrderBy))
	for index, order := range statement.OrderBy {
		orderPositions[index] = -1
		if ordinal, err := strconv.Atoi(strings.TrimSpace(order.Column)); err == nil {
			if ordinal < 1 || ordinal > len(plans) {
				return nil, fmt.Errorf("invalid ORDER BY position %d", ordinal)
			}
			orderPositions[index] = ordinal - 1
			continue
		}
		for position, plan := range plans {
			if strings.EqualFold(plan.column.Name, stripQualifier(order.Column)) {
				orderPositions[index] = position
				break
			}
		}
		if orderPositions[index] < 0 {
			expression, err := parser.ParseExpression(order.Column)
			if err != nil {
				return nil, err
			}
			orderExpressions[index] = expression
		}
	}
	rows := make([]projectedRow, 0)
	appendRow := func(row storage.Row) error {
		values, err := project(row)
		if err != nil {
			return err
		}
		projected := projectedRow{values: values, keys: make([]any, len(statement.OrderBy))}
		for index, expression := range orderExpressions {
			if orderPositions[index] >= 0 {
				projected.keys[index] = values[orderPositions[index]]
				continue
			}
			projected.keys[index], err = evaluateExprWithContext(expression, table, row, session, store)
			if err != nil {
				return err
			}
		}
		rows = append(rows, projected)
		return nil
	}
	var err error
	if sourceRows != nil {
		for _, row := range sourceRows {
			if err = appendRow(row); err != nil {
				break
			}
		}
	} else {
		err = table.Visit(predicate, appendRow)
	}
	if err != nil {
		return nil, err
	}
	if len(statement.OrderBy) > 0 && !indexOrdered {
		sort.SliceStable(rows, func(i, j int) bool {
			for index := range statement.OrderBy {
				comparison := compareAny(rows[i].keys[index], rows[j].keys[index])
				if comparison != 0 {
					if statement.OrderBy[index].Desc {
						return comparison > 0
					}
					return comparison < 0
				}
			}
			return false
		})
	}
	start := statement.Offset
	if indexLimited {
		start = 0
	}
	if start > len(rows) {
		start = len(rows)
	}
	end := len(rows)
	if statement.HasLimit && !indexLimited && start+statement.Limit < end {
		end = start + statement.Limit
	}
	result.Rows = make([][]any, end-start)
	for index := start; index < end; index++ {
		result.Rows[index-start] = rows[index].values
	}
	return result, nil
}

func expressionType(expression parser.Expr, table *storage.Table, columns []storage.Column) (storage.DataType, error) {
	return expressionTypeWithSession(expression, table, columns, nil)
}

func expressionTypeWithSession(expression parser.Expr, table *storage.Table, columns []storage.Column, session *Session) (storage.DataType, error) {
	switch value := expression.(type) {
	case parser.BinaryExpr:
		if value.Operator == "AND" || value.Operator == "OR" || strings.ContainsAny(value.Operator, "=<>") || value.Operator == "LIKE" {
			return storage.TypeBoolean, nil
		}
		if (value.Operator == "+" || value.Operator == "-") && (isIntervalExpression(value.Left) || isIntervalExpression(value.Right)) {
			return storage.TypeDateTime, nil
		}
		left, err := expressionTypeWithSession(value.Left, table, columns, session)
		if err != nil {
			return "", err
		}
		right, err := expressionTypeWithSession(value.Right, table, columns, session)
		if err != nil {
			return "", err
		}
		if left == storage.TypeDouble || right == storage.TypeDouble || left == storage.TypeFloat || right == storage.TypeFloat {
			return storage.TypeDouble, nil
		}
		return storage.TypeBigInt, nil
	case parser.CaseExpr:
		for _, branch := range value.Whens {
			return expressionTypeWithSession(branch.Then, table, columns, session)
		}
		if value.Else != nil {
			return expressionTypeWithSession(value.Else, table, columns, session)
		}
		return storage.TypeVarchar, nil
	case parser.ScalarSubquery:
		return storage.TypeVarchar, nil
	case parser.ExistsExpr:
		return storage.TypeBoolean, nil
	case parser.Identifier:
		index, ok := queryColumnIndex(table, value.Name)
		if !ok {
			if correlated, exists := correlationScopeValue(session, value.Name); exists {
				return scalarValueType(correlated), nil
			}
			return "", fmt.Errorf("%w: %s", storage.ErrColumnNotFound, value.Name)
		}
		return columns[index].Type, nil
	case parser.LiteralExpr:
		switch value.Value.Kind {
		case parser.LiteralNumber:
			if strings.Contains(value.Value.Text, ".") {
				return storage.TypeDouble, nil
			}
			return storage.TypeBigInt, nil
		case parser.LiteralBoolean:
			return storage.TypeBoolean, nil
		default:
			return storage.TypeVarchar, nil
		}
	case parser.RowExpr:
		return storage.TypeVarchar, nil
	case parser.FunctionExpr:
		name := strings.ToUpper(value.Name)
		switch name {
		case "COUNT":
			return storage.TypeBigInt, nil
		case "LAST_INSERT_ID":
			return storage.TypeBigInt, nil
		case "AVG":
			return storage.TypeDouble, nil
		case "SUM", "MIN", "MAX":
			if len(value.Args) == 1 {
				return expressionTypeWithSession(value.Args[0], table, columns, session)
			}
			return storage.TypeDouble, nil
		case "LENGTH", "OCTET_LENGTH", "CHAR_LENGTH", "CHARACTER_LENGTH", "LOCATE", "INSTR", "ASCII",
			"SIGN", "YEAR", "MONTH", "DAY", "DAYOFMONTH", "DAYOFWEEK", "DAYOFYEAR", "WEEKDAY",
			"QUARTER", "HOUR", "MINUTE", "SECOND", "DATEDIFF":
			return storage.TypeBigInt, nil
		case "ISNULL":
			return storage.TypeBoolean, nil
		case "ABS", "CEIL", "CEILING", "FLOOR", "ROUND", "TRUNCATE", "MOD", "POW", "POWER", "SQRT",
			"EXP", "LN", "LOG", "LOG2", "LOG10", "PI":
			return storage.TypeDouble, nil
		case "CURDATE", "CURRENT_DATE", "DATE", "DATE_SUB", "DATE_ADD", "LAST_DAY":
			return storage.TypeDate, nil
		case "NOW", "CURRENT_TIMESTAMP":
			return storage.TypeDateTime, nil
		case "COALESCE", "IFNULL", "NULLIF", "GREATEST", "LEAST":
			for _, argument := range value.Args {
				if _, isNull := argument.(parser.LiteralExpr); isNull {
					literal := argument.(parser.LiteralExpr)
					if literal.Value.Kind == parser.LiteralNull {
						continue
					}
				}
				return expressionTypeWithSession(argument, table, columns, session)
			}
		case "IF":
			if len(value.Args) == 3 {
				return expressionTypeWithSession(value.Args[1], table, columns, session)
			}
		}
		return storage.TypeVarchar, nil
	case parser.WindowExpr:
		name := strings.ToUpper(value.Function.Name)
		switch name {
		case "ROW_NUMBER", "RANK", "DENSE_RANK", "COUNT":
			return storage.TypeBigInt, nil
		case "AVG":
			return storage.TypeDouble, nil
		case "SUM", "MIN", "MAX":
			if len(value.Function.Args) == 1 {
				return expressionTypeWithSession(value.Function.Args[0], table, columns, session)
			}
		}
		return "", fmt.Errorf("unsupported window function %s", value.Function.Name)
	default:
		return storage.TypeBoolean, nil
	}
}

func isIntervalExpression(expression parser.Expr) bool {
	_, ok := expression.(parser.IntervalExpr)
	return ok
}

type windowPlan struct {
	expression parser.Expr
	window     *parser.WindowExpr
	column     Column
}

func selectHasWindow(items []parser.SelectItem) bool {
	for _, item := range items {
		if !containsFold(item.Expression, "OVER") {
			continue
		}
		expression, err := parser.ParseExpression(item.Expression)
		if err == nil {
			if _, ok := expression.(parser.WindowExpr); ok {
				return true
			}
		}
	}
	return false
}

func containsFold(value, target string) bool {
	if len(target) == 0 {
		return true
	}
	for index := 0; index+len(target) <= len(value); index++ {
		if strings.EqualFold(value[index:index+len(target)], target) {
			return true
		}
	}
	return false
}

func executeWindowSelect(table *storage.Table, predicate storage.Predicate, statement parser.Select, columns []storage.Column) (*Result, error) {
	if len(statement.GroupBy) > 0 || statement.Having != nil {
		return nil, errors.New("window functions with GROUP BY or HAVING require a derived table")
	}
	plans := make([]windowPlan, 0, len(statement.Items))
	result := &Result{}
	for _, item := range statement.Items {
		if strings.TrimSpace(item.Expression) == "*" {
			for _, source := range columns {
				plan := windowPlan{
					expression: parser.Identifier{Name: source.Name},
					column:     Column{Name: stripQualifier(source.Name), Type: source.Type},
				}
				plans = append(plans, plan)
				result.Columns = append(result.Columns, plan.column)
			}
			continue
		}
		expression, err := parser.ParseExpression(item.Expression)
		if err != nil {
			return nil, err
		}
		name := item.Alias
		if name == "" {
			name = item.Expression
			if identifier, ok := expression.(parser.Identifier); ok {
				name = stripQualifier(identifier.Name)
			}
		}
		dataType, err := expressionType(expression, table, columns)
		if err != nil {
			return nil, err
		}
		plan := windowPlan{expression: expression, column: Column{Name: name, Type: dataType}}
		if window, ok := expression.(parser.WindowExpr); ok {
			plan.window = &window
		}
		plans = append(plans, plan)
		result.Columns = append(result.Columns, plan.column)
	}

	rows := make([]storage.Row, 0, table.RowCount())
	if err := table.Visit(predicate, func(row storage.Row) error {
		rows = append(rows, append(storage.Row(nil), row...))
		return nil
	}); err != nil {
		return nil, err
	}
	projected := make([][]any, len(rows))
	for rowIndex, row := range rows {
		projected[rowIndex] = make([]any, len(plans))
		for itemIndex, plan := range plans {
			if plan.window != nil {
				continue
			}
			value, err := evaluateExpr(plan.expression, table, row)
			if err != nil {
				return nil, err
			}
			projected[rowIndex][itemIndex] = value
		}
	}
	for itemIndex, plan := range plans {
		if plan.window == nil {
			continue
		}
		if err := evaluateWindow(table, rows, projected, itemIndex, *plan.window, plan.column.Type); err != nil {
			return nil, err
		}
	}

	if len(statement.OrderBy) > 0 {
		keys := make([][]any, len(rows))
		for rowIndex, row := range rows {
			keys[rowIndex] = make([]any, len(statement.OrderBy))
			for orderIndex, order := range statement.OrderBy {
				position := resultColumnPosition(order.Column, result.Columns)
				if position >= 0 {
					keys[rowIndex][orderIndex] = projected[rowIndex][position]
					continue
				}
				expression, err := parser.ParseExpression(order.Column)
				if err != nil {
					return nil, err
				}
				keys[rowIndex][orderIndex], err = evaluateExpr(expression, table, row)
				if err != nil {
					return nil, err
				}
			}
		}
		indexes := make([]int, len(rows))
		for index := range indexes {
			indexes[index] = index
		}
		sort.SliceStable(indexes, func(i, j int) bool {
			for orderIndex, order := range statement.OrderBy {
				comparison := compareAny(keys[indexes[i]][orderIndex], keys[indexes[j]][orderIndex])
				if comparison != 0 {
					if order.Desc {
						return comparison > 0
					}
					return comparison < 0
				}
			}
			return false
		})
		ordered := make([][]any, len(projected))
		for index, source := range indexes {
			ordered[index] = projected[source]
		}
		projected = ordered
	}
	start := statement.Offset
	if start > len(projected) {
		start = len(projected)
	}
	end := len(projected)
	if statement.HasLimit && start+statement.Limit < end {
		end = start + statement.Limit
	}
	result.Rows = projected[start:end]
	return result, nil
}

func resultColumnPosition(name string, columns []Column) int {
	if ordinal, err := strconv.Atoi(strings.TrimSpace(name)); err == nil {
		if ordinal >= 1 && ordinal <= len(columns) {
			return ordinal - 1
		}
		return -1
	}
	for index, column := range columns {
		if strings.EqualFold(column.Name, stripQualifier(name)) {
			return index
		}
	}
	return -1
}

func evaluateWindow(table *storage.Table, rows []storage.Row, output [][]any, outputIndex int, window parser.WindowExpr, resultType storage.DataType) error {
	name := strings.ToUpper(window.Function.Name)
	if name != "ROW_NUMBER" && name != "RANK" && name != "DENSE_RANK" && name != "COUNT" && name != "SUM" && name != "AVG" && name != "MIN" && name != "MAX" {
		return fmt.Errorf("unsupported window function %s", window.Function.Name)
	}
	if (name == "ROW_NUMBER" || name == "RANK" || name == "DENSE_RANK") && (window.Function.Star || len(window.Function.Args) != 0) {
		return fmt.Errorf("%s does not accept arguments", name)
	}
	if name != "ROW_NUMBER" && name != "RANK" && name != "DENSE_RANK" && !window.Function.Star && len(window.Function.Args) != 1 {
		return fmt.Errorf("%s window function expects one argument", name)
	}

	partitions := make(map[string][]int)
	partitionOrder := make([]string, 0)
	orderKeys := make([][]any, len(rows))
	for rowIndex, row := range rows {
		partitionValues := make([]any, len(window.PartitionBy))
		for index, expression := range window.PartitionBy {
			value, err := evaluateExpr(expression, table, row)
			if err != nil {
				return err
			}
			partitionValues[index] = value
		}
		key := groupedRowKey(partitionValues)
		if _, exists := partitions[key]; !exists {
			partitionOrder = append(partitionOrder, key)
		}
		partitions[key] = append(partitions[key], rowIndex)
		orderKeys[rowIndex] = make([]any, len(window.OrderBy))
		for index, order := range window.OrderBy {
			value, err := evaluateExpr(order.Expression, table, row)
			if err != nil {
				return err
			}
			orderKeys[rowIndex][index] = value
		}
	}
	for _, partitionKey := range partitionOrder {
		indexes := partitions[partitionKey]
		sort.SliceStable(indexes, func(i, j int) bool {
			return compareWindowOrder(orderKeys[indexes[i]], orderKeys[indexes[j]], window.OrderBy) < 0
		})
		switch name {
		case "ROW_NUMBER":
			for position, rowIndex := range indexes {
				output[rowIndex][outputIndex] = int64(position + 1)
			}
		case "RANK", "DENSE_RANK":
			rank, dense := int64(1), int64(1)
			for position, rowIndex := range indexes {
				if position > 0 && compareWindowOrder(orderKeys[indexes[position-1]], orderKeys[rowIndex], window.OrderBy) != 0 {
					rank = int64(position + 1)
					dense++
				}
				if name == "RANK" {
					output[rowIndex][outputIndex] = rank
				} else {
					output[rowIndex][outputIndex] = dense
				}
			}
		default:
			state := aggregateState{}
			if len(window.OrderBy) == 0 {
				for _, rowIndex := range indexes {
					candidate, star, err := windowAggregateCandidate(table, rows[rowIndex], window.Function)
					if err != nil {
						return err
					}
					updateAggregate(&state, windowAggregateKind(name), candidate, star)
				}
				value := finishAggregate(state, windowAggregateKind(name), resultType)
				for _, rowIndex := range indexes {
					output[rowIndex][outputIndex] = value
				}
				continue
			}
			// MySQL's default ordered aggregate frame is RANGE through the
			// current row, so all peers with equal ORDER BY keys share a value.
			for start := 0; start < len(indexes); {
				end := start + 1
				for end < len(indexes) && compareWindowOrder(orderKeys[indexes[start]], orderKeys[indexes[end]], window.OrderBy) == 0 {
					end++
				}
				for position := start; position < end; position++ {
					rowIndex := indexes[position]
					candidate, star, err := windowAggregateCandidate(table, rows[rowIndex], window.Function)
					if err != nil {
						return err
					}
					updateAggregate(&state, windowAggregateKind(name), candidate, star)
				}
				value := finishAggregate(state, windowAggregateKind(name), resultType)
				for position := start; position < end; position++ {
					output[indexes[position]][outputIndex] = value
				}
				start = end
			}
		}
	}
	return nil
}

func compareWindowOrder(left, right []any, orders []parser.WindowOrder) int {
	for index, order := range orders {
		comparison := compareAny(left[index], right[index])
		if comparison != 0 {
			if order.Desc {
				return -comparison
			}
			return comparison
		}
	}
	return 0
}

func windowAggregateCandidate(table *storage.Table, row storage.Row, function parser.FunctionExpr) (any, bool, error) {
	if function.Star {
		return nil, true, nil
	}
	value, err := evaluateExpr(function.Args[0], table, row)
	return value, false, err
}

func windowAggregateKind(name string) aggregateKind {
	switch name {
	case "COUNT":
		return aggregateCount
	case "SUM":
		return aggregateSum
	case "AVG":
		return aggregateAvg
	case "MIN":
		return aggregateMin
	case "MAX":
		return aggregateMax
	default:
		return aggregateNone
	}
}

type groupedSelectItem struct {
	groupPosition   int
	aggregate       aggregateKind
	aggregateColumn int
	resultType      storage.DataType
	expression      parser.Expr
}

type aggregateKind uint8

const (
	aggregateNone aggregateKind = iota
	aggregateCount
	aggregateSum
	aggregateAvg
	aggregateMin
	aggregateMax
)

type aggregateState struct {
	count int64
	sum   float64
	value any
	has   bool
}

type groupedBucket struct {
	groupValues []any
	states      []aggregateState
	aggregates  map[string]aggregateState
}

type aggregateNode struct {
	key        string
	kind       aggregateKind
	expression parser.Expr
	star       bool
}

func collectAggregateNodes(expression parser.Expr, nodes map[string]aggregateNode) {
	switch value := expression.(type) {
	case parser.FunctionExpr:
		name := strings.ToUpper(value.Name)
		var kind aggregateKind
		switch name {
		case "COUNT":
			kind = aggregateCount
		case "SUM":
			kind = aggregateSum
		case "AVG":
			kind = aggregateAvg
		case "MIN":
			kind = aggregateMin
		case "MAX":
			kind = aggregateMax
		}
		if kind != aggregateNone {
			key := functionExpressionName(value)
			if _, exists := nodes[key]; !exists {
				var argument parser.Expr
				if len(value.Args) > 0 {
					argument = value.Args[0]
				}
				nodes[key] = aggregateNode{key: key, kind: kind, expression: argument, star: value.Star}
			}
		}
		for _, argument := range value.Args {
			collectAggregateNodes(argument, nodes)
		}
	case parser.BinaryExpr:
		collectAggregateNodes(value.Left, nodes)
		collectAggregateNodes(value.Right, nodes)
	case parser.CaseExpr:
		if value.Operand != nil {
			collectAggregateNodes(value.Operand, nodes)
		}
		for _, branch := range value.Whens {
			collectAggregateNodes(branch.When, nodes)
			collectAggregateNodes(branch.Then, nodes)
		}
		if value.Else != nil {
			collectAggregateNodes(value.Else, nodes)
		}
	}
}

func executeGroupedSelect(table *storage.Table, predicate storage.Predicate, statement parser.Select, columns []storage.Column) (*Result, error) {
	groupExpressions := make([]parser.Expr, len(statement.GroupBy))
	groupIndexes := make([]int, len(statement.GroupBy))
	groupPositions := make(map[int]int, len(statement.GroupBy))
	groupExpressionKeys := make(map[string]int, len(statement.GroupBy))
	for position, name := range statement.GroupBy {
		resolvedName := name
		if ordinal, err := strconv.Atoi(strings.TrimSpace(name)); err == nil {
			if ordinal < 1 || ordinal > len(statement.Items) {
				return nil, fmt.Errorf("invalid GROUP BY position %d", ordinal)
			}
			resolvedName = statement.Items[ordinal-1].Expression
		} else {
			for _, item := range statement.Items {
				if item.Alias != "" && strings.EqualFold(item.Alias, strings.TrimSpace(name)) {
					resolvedName = item.Expression
					break
				}
			}
		}
		expression, err := parser.ParseExpression(resolvedName)
		if err != nil {
			return nil, err
		}
		groupExpressions[position] = expression
		groupExpressionKeys[normalizedSQL(name)] = position
		groupExpressionKeys[normalizedSQL(resolvedName)] = position
		if identifier, ok := expression.(parser.Identifier); ok {
			if index, found := queryColumnIndex(table, identifier.Name); found {
				groupIndexes[position] = index
				groupPositions[index] = position
			}
		}
	}
	aggregateNodes := make(map[string]aggregateNode)
	for _, item := range statement.Items {
		if expression, err := parser.ParseExpression(item.Expression); err == nil {
			collectAggregateNodes(expression, aggregateNodes)
		}
	}
	if statement.Having != nil {
		collectAggregateNodes(statement.Having, aggregateNodes)
	}

	items := make([]groupedSelectItem, len(statement.Items))
	resultColumns := make([]Column, len(statement.Items))
	for index, item := range statement.Items {
		kind, argument, aggregate := parseAggregateExpression(item.Expression)
		if aggregate && argument != "*" {
			argumentExpression, parseErr := parser.ParseExpression(argument)
			if parseErr != nil {
				return nil, parseErr
			}
			if _, simpleIdentifier := argumentExpression.(parser.Identifier); !simpleIdentifier {
				if _, simpleLiteral := argumentExpression.(parser.LiteralExpr); !simpleLiteral || kind != aggregateCount {
					aggregate = false
				}
			}
		}
		if aggregate {
			columnIndex := -1
			resultType := storage.TypeBigInt
			if argument == "*" && kind != aggregateCount {
				return nil, fmt.Errorf("%s does not accept *", aggregateName(kind))
			}
			if argument != "*" {
				if kind == aggregateCount {
					if _, err := strconv.ParseInt(argument, 10, 64); err == nil {
						argument = "*"
					}
				}
				if argument != "*" {
					var ok bool
					columnIndex, ok = queryColumnIndex(table, argument)
					if !ok {
						return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, argument)
					}
					if kind == aggregateSum || kind == aggregateAvg {
						if !isNumericType(columns[columnIndex].Type) {
							return nil, fmt.Errorf("%s requires a numeric column", aggregateName(kind))
						}
					}
				}
			}
			switch kind {
			case aggregateAvg:
				resultType = storage.TypeDouble
			case aggregateSum:
				if columnIndex >= 0 && (columns[columnIndex].Type == storage.TypeFloat || columns[columnIndex].Type == storage.TypeDouble) {
					resultType = storage.TypeDouble
				}
			case aggregateMin, aggregateMax:
				resultType = columns[columnIndex].Type
			}
			items[index] = groupedSelectItem{aggregate: kind, aggregateColumn: columnIndex, resultType: resultType}
			name := item.Alias
			if name == "" {
				name = item.Expression
			}
			resultColumns[index] = Column{Name: name, Type: resultType}
			continue
		}
		expression, err := parser.ParseExpression(item.Expression)
		if err != nil {
			return nil, err
		}
		position, grouped := groupExpressionKeys[normalizedSQL(item.Expression)]
		resultType, typeErr := expressionType(expression, table, columns)
		if typeErr != nil {
			return nil, typeErr
		}
		if !grouped {
			if identifier, ok := expression.(parser.Identifier); ok {
				if columnIndex, found := queryColumnIndex(table, identifier.Name); found {
					position, grouped = groupPositions[columnIndex]
				}
			}
		}
		if !grouped && !hasAggregateNode(expression) {
			return nil, fmt.Errorf("column %q must appear in GROUP BY or be aggregated", item.Expression)
		}
		var groupedExpression parser.Expr
		if hasAggregateNode(expression) {
			groupedExpression = expression
		}
		items[index] = groupedSelectItem{groupPosition: position, aggregateColumn: -1, resultType: resultType, expression: groupedExpression}
		name := item.Alias
		if name == "" {
			name = item.Expression
			if identifier, ok := expression.(parser.Identifier); ok {
				name = stripQualifier(identifier.Name)
			}
		}
		resultColumns[index] = Column{Name: name, Type: resultType}
	}

	groups := make(map[string]int)
	buckets := make([]groupedBucket, 0)
	if len(groupIndexes) == 0 {
		groups[""] = 0
		buckets = append(buckets, groupedBucket{states: make([]aggregateState, len(items)), aggregates: make(map[string]aggregateState)})
	}
	err := table.Visit(predicate, func(row storage.Row) error {
		groupValues := make([]any, len(groupIndexes))
		for index, expression := range groupExpressions {
			value, evalErr := evaluateExpr(expression, table, row)
			if evalErr != nil {
				return evalErr
			}
			groupValues[index] = value
		}
		key := groupedRowKey(groupValues)
		bucketIndex, exists := groups[key]
		if !exists {
			bucketIndex = len(buckets)
			groups[key] = bucketIndex
			buckets = append(buckets, groupedBucket{groupValues: groupValues, states: make([]aggregateState, len(items)), aggregates: make(map[string]aggregateState)})
		}
		for key, node := range aggregateNodes {
			var candidate any
			if !node.star && node.expression != nil {
				value, evalErr := evaluateExpr(node.expression, table, row)
				if evalErr != nil {
					return evalErr
				}
				candidate = value
			}
			state := buckets[bucketIndex].aggregates[key]
			updateAggregate(&state, node.kind, candidate, node.star)
			buckets[bucketIndex].aggregates[key] = state
		}
		for itemIndex, item := range items {
			if item.aggregate == aggregateNone {
				continue
			}
			var candidate any
			if item.aggregateColumn >= 0 {
				candidate = row[item.aggregateColumn].Interface()
			}
			updateAggregate(&buckets[bucketIndex].states[itemIndex], item.aggregate, candidate, item.aggregateColumn < 0)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resultRows := make([][]any, 0, len(buckets))
	for _, bucket := range buckets {
		resultRow := make([]any, len(items))
		for itemIndex, item := range items {
			if item.aggregate == aggregateNone {
				if item.expression != nil {
					continue
				}
				resultRow[itemIndex] = bucket.groupValues[item.groupPosition]
			} else {
				resultRow[itemIndex] = finishAggregate(bucket.states[itemIndex], item.aggregate, item.resultType)
			}
		}
		for itemIndex := range statement.Items {
			if items[itemIndex].aggregate == aggregateNone && items[itemIndex].expression != nil {
				value, evalErr := evaluateGroupedExpression(items[itemIndex].expression, resultColumns, resultRow, bucket, aggregateNodes)
				if evalErr != nil {
					return nil, evalErr
				}
				resultRow[itemIndex] = value
			}
		}
		if statement.Having != nil {
			value, havingErr := evaluateGroupedResult(statement.Having, statement, resultColumns, groupIndexes, bucket, resultRow, columns)
			if havingErr != nil {
				return nil, havingErr
			}
			if !truthy(value) {
				continue
			}
		}
		resultRows = append(resultRows, resultRow)
	}

	if len(statement.OrderBy) > 0 {
		positions := make([]int, len(statement.OrderBy))
		for index, order := range statement.OrderBy {
			positions[index] = groupedOrderPosition(order.Column, statement.Items, resultColumns)
			if positions[index] < 0 {
				return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, order.Column)
			}
		}
		sort.SliceStable(resultRows, func(i, j int) bool {
			for index, position := range positions {
				comparison := compareAny(resultRows[i][position], resultRows[j][position])
				if comparison != 0 {
					if statement.OrderBy[index].Desc {
						return comparison > 0
					}
					return comparison < 0
				}
			}
			return false
		})
	}
	start := statement.Offset
	if start > len(resultRows) {
		start = len(resultRows)
	}
	end := len(resultRows)
	if statement.HasLimit && start+statement.Limit < end {
		end = start + statement.Limit
	}
	return &Result{Columns: resultColumns, Rows: resultRows[start:end]}, nil
}

func isCountExpression(expression string) bool {
	return strings.EqualFold(strings.ReplaceAll(expression, " ", ""), "COUNT(*)")
}

func selectHasAggregate(items []parser.SelectItem) bool {
	for _, item := range items {
		if _, _, ok := parseAggregateExpression(item.Expression); ok {
			return true
		}
		if expression, err := parser.ParseExpression(item.Expression); err == nil && hasAggregateNode(expression) {
			return true
		}
	}
	return false
}

func hasAggregateNode(expression parser.Expr) bool {
	nodes := make(map[string]aggregateNode)
	collectAggregateNodes(expression, nodes)
	return len(nodes) > 0
}

func evaluateGroupedExpression(expression parser.Expr, resultColumns []Column, resultRow []any, bucket groupedBucket, nodes map[string]aggregateNode) (any, error) {
	return evaluateExprWithLookup(expression, func(name string) (any, error) {
		if node, ok := nodes[name]; ok {
			return finishAggregate(bucket.aggregates[name], node.kind, storage.TypeDouble), nil
		}
		for index, column := range resultColumns {
			if strings.EqualFold(column.Name, name) {
				return resultRow[index], nil
			}
		}
		return nil, fmt.Errorf("unknown grouped expression %s", name)
	})
}

func parseAggregateExpression(expression string) (aggregateKind, string, bool) {
	text := strings.TrimSpace(expression)
	open := strings.IndexByte(text, '(')
	if open <= 0 || !strings.HasSuffix(text, ")") {
		return aggregateNone, "", false
	}
	name := strings.ToUpper(strings.TrimSpace(text[:open]))
	argument := strings.TrimSpace(text[open+1 : len(text)-1])
	var kind aggregateKind
	switch name {
	case "COUNT":
		kind = aggregateCount
	case "SUM":
		kind = aggregateSum
	case "AVG":
		kind = aggregateAvg
	case "MIN":
		kind = aggregateMin
	case "MAX":
		kind = aggregateMax
	default:
		return aggregateNone, "", false
	}
	return kind, argument, true
}

func aggregateName(kind aggregateKind) string {
	switch kind {
	case aggregateCount:
		return "COUNT"
	case aggregateSum:
		return "SUM"
	case aggregateAvg:
		return "AVG"
	case aggregateMin:
		return "MIN"
	case aggregateMax:
		return "MAX"
	default:
		return "aggregate"
	}
}

func isNumericType(dataType storage.DataType) bool {
	return dataType == storage.TypeInt || dataType == storage.TypeBigInt || dataType == storage.TypeFloat || dataType == storage.TypeDouble
}

func updateAggregate(state *aggregateState, kind aggregateKind, candidate any, countAll bool) {
	if kind == aggregateCount {
		if countAll || candidate != nil {
			state.count++
		}
		return
	}
	if candidate == nil {
		return
	}
	switch kind {
	case aggregateSum, aggregateAvg:
		value, _ := numeric(candidate)
		state.sum += value
		state.count++
		state.has = true
	case aggregateMin:
		if !state.has || compareAny(candidate, state.value) < 0 {
			state.value = candidate
			state.has = true
		}
	case aggregateMax:
		if !state.has || compareAny(candidate, state.value) > 0 {
			state.value = candidate
			state.has = true
		}
	}
}

func finishAggregate(state aggregateState, kind aggregateKind, resultType storage.DataType) any {
	switch kind {
	case aggregateCount:
		return state.count
	case aggregateSum:
		if !state.has {
			return nil
		}
		if resultType == storage.TypeBigInt {
			return int64(state.sum)
		}
		return state.sum
	case aggregateAvg:
		if state.count == 0 {
			return nil
		}
		return state.sum / float64(state.count)
	case aggregateMin, aggregateMax:
		if !state.has {
			return nil
		}
		return state.value
	default:
		return nil
	}
}

func resolvePlainOrderColumn(expression string, items []parser.SelectItem, table *storage.Table) (int, bool) {
	text := strings.TrimSpace(expression)
	if ordinal, err := strconv.Atoi(text); err == nil {
		if ordinal < 1 || ordinal > len(items) {
			return 0, false
		}
		text = strings.TrimSpace(items[ordinal-1].Expression)
	}
	for _, item := range items {
		if item.Alias != "" && strings.EqualFold(item.Alias, text) {
			text = strings.TrimSpace(item.Expression)
			break
		}
	}
	return queryColumnIndex(table, text)
}

func resolveGroupColumn(expression string, items []parser.SelectItem, table *storage.Table) (int, bool) {
	text := strings.TrimSpace(expression)
	if ordinal, err := strconv.Atoi(text); err == nil {
		if ordinal < 1 || ordinal > len(items) {
			return 0, false
		}
		text = strings.TrimSpace(items[ordinal-1].Expression)
	}
	for _, item := range items {
		if item.Alias != "" && strings.EqualFold(item.Alias, text) {
			text = strings.TrimSpace(item.Expression)
			break
		}
	}
	if _, _, aggregate := parseAggregateExpression(text); aggregate {
		return 0, false
	}
	return queryColumnIndex(table, text)
}

func groupedOrderPosition(expression string, items []parser.SelectItem, columns []Column) int {
	text := strings.TrimSpace(expression)
	if ordinal, err := strconv.Atoi(text); err == nil {
		if ordinal >= 1 && ordinal <= len(columns) {
			return ordinal - 1
		}
		return -1
	}
	for index, column := range columns {
		if strings.EqualFold(column.Name, stripQualifier(text)) || normalizedSQL(items[index].Expression) == normalizedSQL(text) {
			return index
		}
	}
	return -1
}

func normalizedSQL(expression string) string {
	return strings.ToUpper(strings.Join(strings.Fields(expression), ""))
}

func evaluateGroupedResult(expr parser.Expr, statement parser.Select, resultColumns []Column, groupIndexes []int, bucket groupedBucket, resultRow []any, tableColumns []storage.Column) (any, error) {
	return evaluateExprWithLookup(expr, func(name string) (any, error) {
		name = stripQualifier(name)
		for index, column := range resultColumns {
			if strings.EqualFold(column.Name, name) {
				return resultRow[index], nil
			}
		}
		for index, item := range statement.Items {
			if normalizedSQL(stripQualifier(item.Expression)) == normalizedSQL(name) {
				return resultRow[index], nil
			}
		}
		for position, columnIndex := range groupIndexes {
			if strings.EqualFold(stripQualifier(tableColumns[columnIndex].Name), name) {
				return bucket.groupValues[position], nil
			}
		}
		return nil, fmt.Errorf("unknown HAVING column %s", name)
	})
}

func groupedRowKey(values []any) string {
	var key strings.Builder
	for _, value := range values {
		if text, ok := value.(string); ok {
			value = strings.ToLower(text)
		}
		text := fmt.Sprintf("%T:%v", value, value)
		fmt.Fprintf(&key, "%d:%s;", len(text), text)
	}
	return key.String()
}

func executeScalarSelect(session *Session, statement parser.Select) (*Result, error) {
	result := &Result{Rows: [][]any{make([]any, len(statement.Items))}}
	for i, item := range statement.Items {
		expression := strings.ToUpper(strings.ReplaceAll(item.Expression, " ", ""))
		name := item.Alias
		if name == "" {
			name = item.Expression
		}
		column := Column{Name: name, Type: storage.TypeVarchar}
		var value any
		switch {
		case expression == "VERSION()" || expression == "@@VERSION":
			value = "GBaseLite " + Version
		case expression == "DATABASE()":
			value = session.CurrentDatabase
		case expression == "CURRENT_USER()" || expression == "USER()":
			if session.Username == "" {
				value = "root@%"
			} else {
				value = session.Username + "@" + session.Host
			}
		case expression == "CONNECTION_ID()":
			value = int64(0)
			column.Type = storage.TypeBigInt
		case expression == "LAST_INSERT_ID()":
			value = int64(session.LastInsertID)
			column.Type = storage.TypeBigInt
		case expression == "NOW()" || expression == "CURRENT_TIMESTAMP()" || expression == "CURRENT_TIMESTAMP":
			value = time.Now().Format("2006-01-02 15:04:05")
			column.Type = storage.TypeDateTime
		case strings.HasPrefix(expression, "@@"):
			value = ""
		default:
			if n, parseErr := strconv.ParseInt(item.Expression, 10, 64); parseErr == nil {
				value = n
				column.Type = storage.TypeBigInt
			} else {
				parsed, parseErr := parser.ParseExpression(item.Expression)
				if parseErr != nil {
					return nil, fmt.Errorf("unsupported scalar expression %q: %w", item.Expression, parseErr)
				}
				value, parseErr = evaluateExprWithLookup(parsed, func(name string) (any, error) {
					if correlated, exists := correlationScopeValue(session, name); exists {
						return correlated, nil
					}
					return nil, fmt.Errorf("unknown scalar identifier %s", name)
				})
				if parseErr != nil {
					return nil, parseErr
				}
				column.Type = scalarExpressionType(parsed, value)
			}
		}
		result.Columns = append(result.Columns, column)
		result.Rows[0][i] = value
	}
	return result, nil
}

func scalarValueType(value any) storage.DataType {
	switch value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return storage.TypeBigInt
	case float32, float64:
		return storage.TypeDouble
	case bool:
		return storage.TypeBoolean
	case time.Time:
		return storage.TypeDateTime
	default:
		return storage.TypeVarchar
	}
}

func scalarExpressionType(expression parser.Expr, value any) storage.DataType {
	if function, ok := expression.(parser.FunctionExpr); ok {
		switch strings.ToUpper(function.Name) {
		case "CURDATE", "CURRENT_DATE", "DATE", "LAST_DAY":
			return storage.TypeDate
		case "DATE_ADD", "DATE_SUB":
			if len(function.Args) > 0 && scalarExpressionType(function.Args[0], nil) == storage.TypeDate {
				return storage.TypeDate
			}
			return storage.TypeDateTime
		case "NOW", "CURRENT_TIMESTAMP":
			return storage.TypeDateTime
		}
	}
	return scalarValueType(value)
}

func executeUpdate(store *storage.Store, session *Session, statement parser.Update) (*Result, error) {
	if len(statement.Joins) > 0 {
		return executeJoinUpdate(store, session, statement)
	}
	database, table, err := resolveTable(store, session, statement.Table)
	if err != nil {
		return nil, err
	}
	qualifier := mutationQualifier(statement.Table, statement.TableAlias)
	evaluationTable, err := qualifyRelation(table, qualifier)
	if err != nil {
		return nil, err
	}
	statement.Where, err = prepareWriteExpression(store, session, statement.Where, evaluationTable)
	if err != nil {
		return nil, err
	}
	columns := table.ColumnsView()
	type preparedAssignment struct {
		position   int
		expression parser.Expr
	}
	assignments := make([]preparedAssignment, 0, len(statement.Assignments))
	assigned := make(map[int]string, len(statement.Assignments))
	for _, assignment := range statement.Assignments {
		assignmentQualifier, columnName := splitMutationColumn(assignment.Column)
		if assignmentQualifier != "" && !strings.EqualFold(assignmentQualifier, qualifier) && !strings.EqualFold(assignmentQualifier, table.Name()) {
			return nil, fmt.Errorf("UPDATE can only assign columns of target table %s", qualifier)
		}
		index, ok := table.ColumnIndex(columnName)
		if !ok {
			return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, assignment.Column)
		}
		if previous, duplicate := assigned[index]; duplicate {
			return nil, fmt.Errorf("duplicate update column %q (also specified as %q)", assignment.Column, previous)
		}
		assigned[index] = assignment.Column
		expression, err := prepareWriteExpression(store, session, assignment.Value, evaluationTable)
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, preparedAssignment{position: index, expression: expression})
	}
	limit := -1
	if statement.HasLimit {
		limit = statement.Limit
	}
	replacements := make(map[int]storage.Row)
	for rowIndex, current := range table.Snapshot().Rows {
		if limit >= 0 && len(replacements) >= limit {
			break
		}
		if statement.Where != nil {
			matched, evaluationErr := evaluateExprWithContext(statement.Where, evaluationTable, current, session, store)
			if evaluationErr != nil {
				return nil, evaluationErr
			}
			if !truthy(matched) {
				continue
			}
		}
		candidate := append(storage.Row(nil), current...)
		for _, assignment := range assignments {
			raw, err := evaluateExprWithContext(assignment.expression, evaluationTable, candidate, session, store)
			if err != nil {
				return nil, err
			}
			candidate[assignment.position], err = interfaceToColumnValue(raw, columns[assignment.position])
			if err != nil {
				return nil, err
			}
		}
		for position, column := range columns {
			if _, explicit := assigned[position]; explicit || column.OnUpdate == "" {
				continue
			}
			if strings.EqualFold(column.OnUpdate, "CURRENT_TIMESTAMP") || strings.EqualFold(column.OnUpdate, "CURRENT_TIMESTAMP()") {
				candidate[position], err = storage.NewValue(column.Type, time.Now())
				if err != nil {
					return nil, err
				}
			}
		}
		if err := validateCheckConstraints(table, candidate); err != nil {
			return nil, err
		}
		replacements[rowIndex] = candidate
	}
	affected, err := database.ApplyRowMutations([]storage.RowMutation{{Table: table.Name(), Replacements: replacements}})
	return &Result{AffectedRows: uint64(affected), Message: "rows updated"}, err
}
func executeDelete(store *storage.Store, session *Session, statement parser.Delete) (*Result, error) {
	if len(statement.Joins) > 0 || len(statement.Targets) > 0 {
		return executeMultiTableDelete(store, session, statement)
	}
	database, table, err := resolveTable(store, session, statement.Table)
	if err != nil {
		return nil, err
	}
	qualifier := mutationQualifier(statement.Table, statement.TableAlias)
	evaluationTable, err := qualifyRelation(table, qualifier)
	if err != nil {
		return nil, err
	}
	statement.Where, err = prepareWriteExpression(store, session, statement.Where, evaluationTable)
	if err != nil {
		return nil, err
	}
	limit := -1
	if statement.HasLimit {
		limit = statement.Limit
	}
	deleteIndexes := make([]int, 0)
	for rowIndex, row := range table.Snapshot().Rows {
		if limit >= 0 && len(deleteIndexes) >= limit {
			break
		}
		if statement.Where != nil {
			matched, evaluationErr := evaluateExprWithContext(statement.Where, evaluationTable, row, session, store)
			if evaluationErr != nil {
				return nil, evaluationErr
			}
			if !truthy(matched) {
				continue
			}
		}
		deleteIndexes = append(deleteIndexes, rowIndex)
	}
	affected, err := database.ApplyRowMutations([]storage.RowMutation{{Table: table.Name(), Delete: deleteIndexes}})
	return &Result{AffectedRows: uint64(affected), Message: "rows deleted"}, err
}

const mutationRowIndexColumn = "__gbaselite_row_index"

type mutationTarget struct {
	database  *storage.Database
	table     *storage.Table
	qualifier string
	rowIndex  string
}

func executeJoinUpdate(store *storage.Store, session *Session, statement parser.Update) (*Result, error) {
	relation, targets, err := buildMutationJoin(store, session, statement.Table, statement.TableAlias, statement.Joins)
	if err != nil {
		return nil, err
	}
	target := targets[strings.ToLower(mutationQualifier(statement.Table, statement.TableAlias))]
	if target == nil {
		return nil, fmt.Errorf("UPDATE target %s is not a base table", statement.Table)
	}
	statement.Where, err = prepareWriteExpression(store, session, statement.Where, relation)
	if err != nil {
		return nil, err
	}
	targetColumns := target.table.ColumnsView()
	type preparedAssignment struct {
		tablePosition  int
		joinedPosition int
		expression     parser.Expr
	}
	assignments := make([]preparedAssignment, 0, len(statement.Assignments))
	assigned := make(map[int]string, len(statement.Assignments))
	for _, assignment := range statement.Assignments {
		qualifier, columnName := splitMutationColumn(assignment.Column)
		if qualifier != "" && !strings.EqualFold(qualifier, target.qualifier) && !strings.EqualFold(qualifier, target.table.Name()) {
			return nil, fmt.Errorf("UPDATE JOIN can only assign columns of target table %s", target.qualifier)
		}
		tablePosition, ok := target.table.ColumnIndex(columnName)
		if !ok {
			return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, assignment.Column)
		}
		if previous, duplicate := assigned[tablePosition]; duplicate {
			return nil, fmt.Errorf("duplicate update column %q (also specified as %q)", assignment.Column, previous)
		}
		joinedPosition, ok := relation.ColumnIndex(target.qualifier + "." + targetColumns[tablePosition].Name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, assignment.Column)
		}
		expression, expressionErr := prepareWriteExpression(store, session, assignment.Value, relation)
		if expressionErr != nil {
			return nil, expressionErr
		}
		assigned[tablePosition] = assignment.Column
		assignments = append(assignments, preparedAssignment{tablePosition: tablePosition, joinedPosition: joinedPosition, expression: expression})
	}
	rowIndexPosition, ok := relation.ColumnIndex(target.rowIndex)
	if !ok {
		return nil, fmt.Errorf("internal row index for %s is unavailable", target.qualifier)
	}
	limit := -1
	if statement.HasLimit {
		limit = statement.Limit
	}
	replacements := make(map[int]storage.Row)
	targetRows := target.table.Snapshot().Rows
	for _, joinedRow := range relation.Select(nil) {
		if statement.Where != nil {
			matched, evaluationErr := evaluateExprWithContext(statement.Where, relation, joinedRow, session, store)
			if evaluationErr != nil {
				return nil, evaluationErr
			}
			if !truthy(matched) {
				continue
			}
		}
		if joinedRow[rowIndexPosition].Null {
			continue
		}
		rowIndex := int(joinedRow[rowIndexPosition].Int64)
		if _, exists := replacements[rowIndex]; exists {
			continue
		}
		if limit >= 0 && len(replacements) >= limit {
			break
		}
		joinedCandidate := append(storage.Row(nil), joinedRow...)
		candidate := append(storage.Row(nil), targetRows[rowIndex]...)
		for _, assignment := range assignments {
			raw, evaluationErr := evaluateExprWithContext(assignment.expression, relation, joinedCandidate, session, store)
			if evaluationErr != nil {
				return nil, evaluationErr
			}
			converted, conversionErr := interfaceToColumnValue(raw, targetColumns[assignment.tablePosition])
			if conversionErr != nil {
				return nil, conversionErr
			}
			candidate[assignment.tablePosition] = converted
			joinedCandidate[assignment.joinedPosition] = converted
		}
		for position, column := range targetColumns {
			if _, explicit := assigned[position]; explicit || column.OnUpdate == "" {
				continue
			}
			if strings.EqualFold(column.OnUpdate, "CURRENT_TIMESTAMP") || strings.EqualFold(column.OnUpdate, "CURRENT_TIMESTAMP()") {
				candidate[position], err = storage.NewValue(column.Type, time.Now())
				if err != nil {
					return nil, err
				}
			}
		}
		if err := validateCheckConstraints(target.table, candidate); err != nil {
			return nil, err
		}
		replacements[rowIndex] = candidate
	}
	affected, err := target.database.ApplyRowMutations([]storage.RowMutation{{Table: target.table.Name(), Replacements: replacements}})
	return &Result{AffectedRows: uint64(affected), Message: "rows updated"}, err
}

func executeMultiTableDelete(store *storage.Store, session *Session, statement parser.Delete) (*Result, error) {
	if statement.HasLimit {
		return nil, errors.New("LIMIT is not supported for multi-table DELETE")
	}
	relation, available, err := buildMutationJoin(store, session, statement.Table, statement.TableAlias, statement.Joins)
	if err != nil {
		return nil, err
	}
	requested := append([]string(nil), statement.Targets...)
	if len(requested) == 0 {
		requested = []string{mutationQualifier(statement.Table, statement.TableAlias)}
	}
	targets := make([]*mutationTarget, 0, len(requested))
	seenTargets := make(map[*mutationTarget]bool)
	for _, name := range requested {
		target := lookupMutationTarget(available, name)
		if target == nil {
			return nil, fmt.Errorf("unknown DELETE target %s", name)
		}
		if !seenTargets[target] {
			seenTargets[target] = true
			targets = append(targets, target)
		}
	}
	statement.Where, err = prepareWriteExpression(store, session, statement.Where, relation)
	if err != nil {
		return nil, err
	}
	positions := make(map[*mutationTarget]int, len(targets))
	deleteIndexes := make(map[*mutationTarget]map[int]bool, len(targets))
	for _, target := range targets {
		position, ok := relation.ColumnIndex(target.rowIndex)
		if !ok {
			return nil, fmt.Errorf("internal row index for %s is unavailable", target.qualifier)
		}
		positions[target] = position
		deleteIndexes[target] = make(map[int]bool)
	}
	for _, row := range relation.Select(nil) {
		if statement.Where != nil {
			matched, evaluationErr := evaluateExprWithContext(statement.Where, relation, row, session, store)
			if evaluationErr != nil {
				return nil, evaluationErr
			}
			if !truthy(matched) {
				continue
			}
		}
		for _, target := range targets {
			value := row[positions[target]]
			if !value.Null {
				deleteIndexes[target][int(value.Int64)] = true
			}
		}
	}
	byDatabase := make(map[*storage.Database][]storage.RowMutation)
	for _, target := range targets {
		indexes := make([]int, 0, len(deleteIndexes[target]))
		for rowIndex := range deleteIndexes[target] {
			indexes = append(indexes, rowIndex)
		}
		sort.Ints(indexes)
		byDatabase[target.database] = append(byDatabase[target.database], storage.RowMutation{Table: target.table.Name(), Delete: indexes})
	}
	var affected uint64
	for database, mutations := range byDatabase {
		count, mutationErr := database.ApplyRowMutations(mutations)
		if mutationErr != nil {
			return nil, mutationErr
		}
		affected += uint64(count)
	}
	return &Result{AffectedRows: affected, Message: "rows deleted"}, nil
}

func buildMutationJoin(store *storage.Store, session *Session, tableName, alias string, joins []parser.Join) (*storage.Table, map[string]*mutationTarget, error) {
	targets := make(map[string]*mutationTarget)
	qualifier := mutationQualifier(tableName, alias)
	relation, target, err := resolveMutationRelation(store, session, tableName, qualifier, nil)
	if err != nil {
		return nil, nil, err
	}
	registerMutationTarget(targets, target, tableName)
	for _, join := range joins {
		rightQualifier := mutationQualifier(join.Table, join.TableAlias)
		right, rightTarget, sourceErr := resolveMutationRelation(store, session, join.Table, rightQualifier, join.Subquery)
		if sourceErr != nil {
			return nil, nil, sourceErr
		}
		registerMutationTarget(targets, rightTarget, join.Table)
		relation, err = joinRelations(store, session, relation, right, join)
		if err != nil {
			return nil, nil, err
		}
	}
	return relation, targets, nil
}

func resolveMutationRelation(store *storage.Store, session *Session, tableName, qualifier string, subquery parser.Query) (*storage.Table, *mutationTarget, error) {
	if subquery == nil {
		if database, table, err := resolveTable(store, session, tableName); err == nil {
			relation, relationErr := qualifyMutationTable(table, qualifier)
			if relationErr != nil {
				return nil, nil, relationErr
			}
			return relation, &mutationTarget{database: database, table: table, qualifier: qualifier, rowIndex: qualifier + "." + mutationRowIndexColumn}, nil
		}
	}
	source, err := resolveSelectSource(store, session, tableName, qualifier, subquery)
	if err != nil {
		return nil, nil, err
	}
	qualified, err := qualifyRelation(source, qualifier)
	return qualified, nil, err
}

func qualifyMutationTable(source *storage.Table, qualifier string) (*storage.Table, error) {
	if qualifier == "" {
		return nil, errors.New("mutation source requires a table name or alias")
	}
	sourceColumns := source.ColumnsView()
	columns := make([]storage.Column, 0, len(sourceColumns)+1)
	for _, column := range sourceColumns {
		column.Name = qualifier + "." + stripQualifier(column.Name)
		columns = append(columns, column)
	}
	columns = append(columns, storage.Column{Name: qualifier + "." + mutationRowIndexColumn, Type: storage.TypeBigInt, MetadataVersion: 1, Nullable: false})
	qualified, err := storage.NewTransientTable("mutation_source", columns)
	if err != nil {
		return nil, err
	}
	for rowIndex, sourceRow := range source.Snapshot().Rows {
		row := append(storage.Row(nil), sourceRow...)
		indexValue, valueErr := storage.NewValue(storage.TypeBigInt, int64(rowIndex))
		if valueErr != nil {
			return nil, valueErr
		}
		row = append(row, indexValue)
		if err := qualified.Insert(row); err != nil {
			return nil, err
		}
	}
	return qualified, nil
}

func mutationQualifier(tableName, alias string) string {
	if alias != "" {
		return alias
	}
	_, table := splitTableName(tableName)
	return table
}

func registerMutationTarget(targets map[string]*mutationTarget, target *mutationTarget, sourceName string) {
	if target == nil {
		return
	}
	targets[strings.ToLower(target.qualifier)] = target
	targets[strings.ToLower(target.table.Name())] = target
	targets[strings.ToLower(sourceName)] = target
}

func lookupMutationTarget(targets map[string]*mutationTarget, name string) *mutationTarget {
	if target := targets[strings.ToLower(name)]; target != nil {
		return target
	}
	_, unqualified := splitTableName(name)
	return targets[strings.ToLower(unqualified)]
}

func splitMutationColumn(name string) (string, string) {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[:dot], name[dot+1:]
	}
	return "", name
}

func validateStoreCheckConstraints(store *storage.Store) error {
	for _, databaseName := range store.ListDatabases() {
		database, err := store.Database(databaseName)
		if err != nil {
			return err
		}
		for _, tableName := range database.ListTables() {
			table, _ := database.Table(tableName)
			for _, row := range table.Select(nil) {
				if err := validateCheckConstraints(table, row); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func executeShow(store *storage.Store, session *Session, statement parser.Show) (*Result, error) {
	switch statement.What {
	case "DATABASES":
		result := &Result{Columns: []Column{{Name: "Database", Type: storage.TypeVarchar}}}
		for _, name := range store.ListDatabases() {
			result.Rows = append(result.Rows, []any{name})
		}
		return result, nil
	case "TABLES":
		database, err := selectedDatabase(store, session, "")
		if err != nil {
			return nil, err
		}
		result := &Result{Columns: []Column{{Name: "Tables_in_" + database.Name(), Type: storage.TypeVarchar}}}
		for _, name := range database.ListRelations() {
			result.Rows = append(result.Rows, []any{name})
		}
		return result, nil
	case "FULL TABLES":
		database, err := selectedDatabase(store, session, "")
		if err != nil {
			return nil, err
		}
		result := &Result{Columns: []Column{{Name: "Tables_in_" + database.Name(), Type: storage.TypeVarchar}, {Name: "Table_type", Type: storage.TypeVarchar}}}
		for _, name := range database.ListRelations() {
			relationType := "BASE TABLE"
			if _, err := database.View(name); err == nil {
				relationType = "VIEW"
			}
			result.Rows = append(result.Rows, []any{name, relationType})
		}
		return result, nil
	case "COLUMNS":
		resolvedName := resolveCopyTargetReference(store, session, statement.Name)
		table, err := resolveSelectSource(store, session, resolvedName, "", nil)
		if err != nil {
			return nil, err
		}
		result := &Result{Columns: []Column{{Name: "Field", Type: storage.TypeVarchar}, {Name: "Type", Type: storage.TypeVarchar}, {Name: "Null", Type: storage.TypeVarchar}, {Name: "Key", Type: storage.TypeVarchar}, {Name: "Default", Type: storage.TypeVarchar}, {Name: "Extra", Type: storage.TypeVarchar}}}
		if statement.Full {
			result.Columns = []Column{
				{Name: "Field", Type: storage.TypeVarchar}, {Name: "Type", Type: storage.TypeVarchar},
				{Name: "Collation", Type: storage.TypeVarchar}, {Name: "Null", Type: storage.TypeVarchar},
				{Name: "Key", Type: storage.TypeVarchar}, {Name: "Default", Type: storage.TypeVarchar},
				{Name: "Extra", Type: storage.TypeVarchar}, {Name: "Privileges", Type: storage.TypeVarchar},
				{Name: "Comment", Type: storage.TypeText},
			}
		}
		for _, column := range table.ColumnsView() {
			if statement.Pattern != "" && !likeMatch(column.Name, statement.Pattern) {
				continue
			}
			nullable := "NO"
			if storage.ColumnNullable(column) {
				nullable = "YES"
			}
			var defaultValue any
			if column.HasDefault {
				if column.DefaultExpression != "" {
					defaultValue = column.DefaultExpression
				} else {
					defaultValue = column.Default.Interface()
				}
			}
			extra := ""
			if column.AutoIncrement {
				extra = "auto_increment"
			}
			if column.OnUpdate != "" {
				if extra != "" {
					extra += " "
				}
				extra += "on update " + column.OnUpdate
			}
			row := []any{column.Name, columnSQLType(column), nullable, table.ColumnKey(column.Name), defaultValue, extra}
			if statement.Full {
				var collation any
				if column.Type == storage.TypeVarchar || column.Type == storage.TypeText {
					collation = "utf8mb4_general_ci"
				}
				row = []any{column.Name, columnSQLType(column), collation, nullable, table.ColumnKey(column.Name), defaultValue, extra, "select,insert,update,references", column.Comment}
			}
			result.Rows = append(result.Rows, row)
		}
		if statement.Where != nil {
			metadata, metadataErr := materializeDerivedTable("show_columns", result)
			if metadataErr != nil {
				return nil, metadataErr
			}
			predicate := expressionPredicateWithContext(statement.Where, metadata, session, store)
			positions := make([]int, len(metadata.ColumnsView()))
			for index := range positions {
				positions[index] = index
			}
			result.Rows = metadata.Project(predicate, positions, 0, -1)
		}
		return result, nil
	case "CREATE TABLE":
		databaseName, relationName := splitTableName(statement.Name)
		database, err := selectedDatabase(store, session, databaseName)
		if err != nil {
			return nil, err
		}
		relationName = resolveCopyTargetName(session, database, relationName)
		if table, tableErr := database.Table(relationName); tableErr == nil {
			session.copySource = &navicatCopySource{Database: database.Name(), Name: table.Name(), Kind: "table"}
			return &Result{Columns: []Column{{Name: "Table", Type: storage.TypeVarchar}, {Name: "Create Table", Type: storage.TypeText}}, Rows: [][]any{{table.Name(), createTableSQL(table)}}}, nil
		}
		view, viewErr := database.View(relationName)
		if viewErr != nil {
			return nil, viewErr
		}
		session.copySource = &navicatCopySource{Database: database.Name(), Name: view.Name, Kind: "view"}
		return createViewResult(view), nil
	case "CREATE VIEW":
		databaseName, viewName := splitTableName(statement.Name)
		database, err := selectedDatabase(store, session, databaseName)
		if err != nil {
			return nil, err
		}
		viewName = resolveCopyTargetName(session, database, viewName)
		view, err := database.View(viewName)
		if err != nil {
			return nil, err
		}
		session.copySource = &navicatCopySource{Database: database.Name(), Name: view.Name, Kind: "view"}
		return createViewResult(view), nil
	case "CREATE DATABASE":
		return &Result{Columns: []Column{{Name: "Database", Type: storage.TypeVarchar}, {Name: "Create Database", Type: storage.TypeText}}, Rows: [][]any{{statement.Name, "CREATE DATABASE " + quoteIdentifier(statement.Name)}}}, nil
	case "CREATE DATABASE IF NOT EXISTS":
		return &Result{Columns: []Column{{Name: "Database", Type: storage.TypeVarchar}, {Name: "Create Database", Type: storage.TypeText}}, Rows: [][]any{{statement.Name, "CREATE DATABASE IF NOT EXISTS " + quoteIdentifier(statement.Name)}}}, nil
	case "INDEX":
		databaseName, tableName := splitTableName(statement.Name)
		database, err := selectedDatabase(store, session, databaseName)
		if err != nil {
			return nil, err
		}
		table, err := database.Table(tableName)
		result := indexMetadataResult()
		if errors.Is(err, storage.ErrTableNotFound) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		for _, definition := range table.Indexes() {
			nonUnique := int64(1)
			if definition.Unique {
				nonUnique = 0
			}
			for position, column := range definition.Columns {
				nullable := "YES"
				if columnPosition, ok := table.ColumnIndex(column); ok && !storage.ColumnNullable(table.ColumnsView()[columnPosition]) {
					nullable = ""
				}
				result.Rows = append(result.Rows, []any{
					table.Name(), nonUnique, definition.Name, int64(position + 1), column, "A",
					int64(table.RowCount()), nil, nil, nullable, "BTREE", "", "",
				})
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported SHOW %s", statement.What)
	}
}

func indexMetadataResult() *Result {
	return &Result{Columns: []Column{
		{Name: "Table", Type: storage.TypeVarchar}, {Name: "Non_unique", Type: storage.TypeBigInt},
		{Name: "Key_name", Type: storage.TypeVarchar}, {Name: "Seq_in_index", Type: storage.TypeBigInt},
		{Name: "Column_name", Type: storage.TypeVarchar}, {Name: "Collation", Type: storage.TypeVarchar},
		{Name: "Cardinality", Type: storage.TypeBigInt}, {Name: "Sub_part", Type: storage.TypeBigInt},
		{Name: "Packed", Type: storage.TypeVarchar}, {Name: "Null", Type: storage.TypeVarchar},
		{Name: "Index_type", Type: storage.TypeVarchar}, {Name: "Comment", Type: storage.TypeText},
		{Name: "Index_comment", Type: storage.TypeText},
	}}
}

func createViewResult(view storage.View) *Result {
	definition := createViewSQL(view)
	return &Result{
		Columns: []Column{{Name: "View", Type: storage.TypeVarchar}, {Name: "Create View", Type: storage.TypeText}, {Name: "character_set_client", Type: storage.TypeVarchar}, {Name: "collation_connection", Type: storage.TypeVarchar}},
		Rows:    [][]any{{view.Name, definition, "utf8mb4", "utf8mb4_general_ci"}},
	}
}

func createViewSQL(view storage.View) string {
	columns := ""
	if len(view.Columns) > 0 {
		quoted := make([]string, len(view.Columns))
		for index, column := range view.Columns {
			quoted[index] = "`" + strings.ReplaceAll(column, "`", "``") + "`"
		}
		columns = " (" + strings.Join(quoted, ", ") + ")"
	}
	return "CREATE VIEW `" + strings.ReplaceAll(view.Name, "`", "``") + "`" + columns + " AS " + view.Definition
}

func selectedDatabase(store *storage.Store, session *Session, explicit string) (*storage.Database, error) {
	name := explicit
	if name == "" {
		name = session.CurrentDatabase
	}
	if name == "" {
		return nil, errors.New("no database selected")
	}
	return store.Database(name)
}
func resolveTable(store *storage.Store, session *Session, name string) (*storage.Database, *storage.Table, error) {
	databaseName, tableName := splitTableName(name)
	database, err := selectedDatabase(store, session, databaseName)
	if err != nil {
		return nil, nil, err
	}
	table, err := database.Table(tableName)
	return database, table, err
}

func navicatBackupTarget(database *storage.Database, session *Session, requestedName, kind string, useExistingAsSource bool) (string, error) {
	source := session.copySource
	sourceName := ""
	if source != nil && strings.EqualFold(source.Database, database.Name()) && strings.EqualFold(source.Kind, kind) &&
		(strings.EqualFold(requestedName, source.Name) || isNavicatCopyName(requestedName, source.Name)) {
		sourceName = source.Name
	} else if inferred, ok := navicatCopySourceName(requestedName); ok && relationExistsAs(database, inferred, kind) {
		sourceName = inferred
	} else if useExistingAsSource && relationExistsAs(database, requestedName, kind) {
		sourceName = requestedName
	}
	if sourceName == "" {
		return requestedName, nil
	}
	if !isNavicatCopyName(requestedName, sourceName) && !relationExists(database, requestedName) {
		return requestedName, nil
	}
	operationTime := time.Now()
	if !session.ReplayTimestamp.IsZero() {
		operationTime = session.ReplayTimestamp
	}
	prefix := sourceName + "_copy_" + operationTime.Format("060102")
	maxSequence := 0
	for _, name := range database.ListRelations() {
		if len(name) != len(prefix)+2 || !strings.EqualFold(name[:len(prefix)], prefix) {
			continue
		}
		digits := name[len(prefix):]
		if digits[0] < '0' || digits[0] > '9' || digits[1] < '0' || digits[1] > '9' {
			continue
		}
		sequence := int(digits[0]-'0')*10 + int(digits[1]-'0')
		if sequence > maxSequence {
			maxSequence = sequence
		}
	}
	if maxSequence >= 99 {
		return "", fmt.Errorf("daily Navicat copy sequence exhausted for %s", sourceName)
	}
	return fmt.Sprintf("%s%02d", prefix, maxSequence+1), nil
}

func copyCreatedMessage(kind, requestedName, actualName string) string {
	if strings.EqualFold(requestedName, actualName) {
		return kind + " created"
	}
	return fmt.Sprintf("%s created as `%s`", kind, strings.ReplaceAll(actualName, "`", "``"))
}

func clearCopySourceAfterDrop(session *Session, databaseName, relationName, kind string) {
	source := session.copySource
	if source == nil {
		return
	}
	if strings.EqualFold(source.Database, databaseName) && strings.EqualFold(source.Name, relationName) && strings.EqualFold(source.Kind, kind) {
		session.copySource = nil
	}
}

func navicatCopySourceName(requestedName string) (string, bool) {
	lower := strings.ToLower(requestedName)
	marker := strings.LastIndex(lower, "_copy")
	if marker <= 0 {
		return "", false
	}
	for _, character := range requestedName[marker+len("_copy"):] {
		if character < '0' || character > '9' {
			return "", false
		}
	}
	return requestedName[:marker], true
}

func isNavicatCopyName(requestedName, sourceName string) bool {
	if strings.EqualFold(requestedName, sourceName) {
		return true
	}
	requested := strings.ToLower(requestedName)
	prefix := strings.ToLower(sourceName) + "_copy"
	if !strings.HasPrefix(requested, prefix) {
		return false
	}
	for _, character := range requested[len(prefix):] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func relationExists(database *storage.Database, name string) bool {
	if _, err := database.Table(name); err == nil {
		return true
	}
	_, err := database.View(name)
	return err == nil
}

func relationExistsAs(database *storage.Database, name, kind string) bool {
	if strings.EqualFold(kind, "table") {
		_, err := database.Table(name)
		return err == nil
	}
	_, err := database.View(name)
	return err == nil
}

func copyTargetKey(databaseName, requestedName string) string {
	return strings.ToLower(databaseName) + "." + strings.ToLower(requestedName)
}

func rememberCopyTarget(session *Session, databaseName, requestedName, actualName string) {
	if session.copyTargets == nil {
		session.copyTargets = make(map[string]string)
	}
	session.copyTargets[copyTargetKey(databaseName, requestedName)] = actualName
}

func resolveCopyTargetName(session *Session, database *storage.Database, requestedName string) string {
	if actualName, ok := session.copyTargets[copyTargetKey(database.Name(), requestedName)]; ok && relationExists(database, actualName) {
		return actualName
	}
	return requestedName
}

func resolveCopyTargetReference(store *storage.Store, session *Session, name string) string {
	databaseName, relationName := splitTableName(name)
	database, err := selectedDatabase(store, session, databaseName)
	if err != nil {
		return name
	}
	actualName := resolveCopyTargetName(session, database, relationName)
	if strings.EqualFold(actualName, relationName) {
		return name
	}
	if databaseName != "" {
		return databaseName + "." + actualName
	}
	return actualName
}

func resolveInsertTable(store *storage.Store, session *Session, name string) (*storage.Database, *storage.Table, string, error) {
	databaseName, tableName := splitTableName(name)
	database, err := selectedDatabase(store, session, databaseName)
	if err != nil {
		return nil, nil, "", err
	}
	key := copyTargetKey(database.Name(), tableName)
	if actualName, ok := session.copyTargets[key]; ok {
		table, tableErr := database.Table(actualName)
		return database, table, key, tableErr
	}
	table, err := database.Table(tableName)
	return database, table, "", err
}

func splitTableName(name string) (string, string) {
	if dot := strings.Index(name, "."); dot >= 0 {
		return name[:dot], name[dot+1:]
	}
	return "", name
}
func expressionPredicate(expr parser.Expr, table *storage.Table) storage.Predicate {
	return expressionPredicateWithContext(expr, table, nil, nil)
}

func prepareWriteExpression(store *storage.Store, session *Session, expression parser.Expr, outer *storage.Table) (parser.Expr, error) {
	return materializeInSubqueriesForOuter(store, session, expression, outer)
}

func isOuterColumnReferenceError(err error, outer *storage.Table) bool {
	if err == nil || outer == nil {
		return false
	}
	message := err.Error()
	lowerMessage := strings.ToLower(message)
	name := ""
	for _, marker := range []string{"unknown column ", "unknown scalar identifier "} {
		if position := strings.LastIndex(lowerMessage, marker); position >= 0 {
			name = strings.TrimSpace(message[position+len(marker):])
			break
		}
	}
	if name == "" && errors.Is(err, storage.ErrColumnNotFound) {
		if position := strings.LastIndex(message, ":"); position >= 0 {
			name = strings.TrimSpace(message[position+1:])
		}
	}
	if name == "" {
		return false
	}
	name = strings.Trim(name, "`'\"")
	qualified := strings.Contains(name, ".")
	for _, column := range outer.ColumnsView() {
		if strings.EqualFold(column.Name, name) {
			return true
		}
		if !qualified && strings.EqualFold(stripQualifier(column.Name), name) {
			return true
		}
	}
	return false
}

func materializeInSubqueries(store *storage.Store, session *Session, expression parser.Expr) (parser.Expr, error) {
	return materializeInSubqueriesForOuter(store, session, expression, nil)
}

func materializeInSubqueriesForOuter(store *storage.Store, session *Session, expression parser.Expr, outer *storage.Table) (parser.Expr, error) {
	if expression == nil {
		return nil, nil
	}
	switch value := expression.(type) {
	case parser.ScalarSubquery:
		if value.Query == nil {
			return nil, errors.New("scalar subquery is missing its SELECT")
		}
		result, err := executeQuery(store, session, value.Query)
		if err != nil {
			if isOuterColumnReferenceError(err, outer) {
				return value, nil
			}
			return nil, err
		}
		if len(result.Columns) != 1 {
			return nil, errors.New("scalar subquery must return exactly one column")
		}
		rows, err := collectResultRows(result)
		if err != nil {
			return nil, err
		}
		if len(rows) > 1 {
			return nil, errors.New("scalar subquery returned more than one row")
		}
		if len(rows) == 0 {
			return queryResultLiteral(nil), nil
		}
		return queryResultLiteral(rows[0][0]), nil
	case parser.ExistsExpr:
		if value.Query == nil {
			return nil, errors.New("EXISTS is missing its SELECT")
		}
		result, err := executeQuery(store, session, value.Query)
		if err != nil {
			if isOuterColumnReferenceError(err, outer) {
				return value, nil
			}
			return nil, err
		}
		rows, err := collectResultRows(result)
		if err != nil {
			return nil, err
		}
		return queryResultLiteral(len(rows) > 0), nil
	case parser.BinaryExpr:
		var err error
		value.Left, err = materializeInSubqueriesForOuter(store, session, value.Left, outer)
		if err != nil {
			return nil, err
		}
		value.Right, err = materializeInSubqueriesForOuter(store, session, value.Right, outer)
		return value, err
	case parser.UnaryExpr:
		inner, err := materializeInSubqueriesForOuter(store, session, value.Value, outer)
		value.Value = inner
		return value, err
	case parser.InExpr:
		var err error
		value.Value, err = materializeInSubqueriesForOuter(store, session, value.Value, outer)
		if err != nil {
			return nil, err
		}
		for index := range value.Values {
			value.Values[index], err = materializeInSubqueriesForOuter(store, session, value.Values[index], outer)
			if err != nil {
				return nil, err
			}
		}
		if value.Subquery == nil {
			return value, nil
		}
		result, err := executeQuery(store, session, value.Subquery)
		if err != nil {
			if isOuterColumnReferenceError(err, outer) {
				return value, nil
			}
			return nil, err
		}
		if len(result.Columns) == 0 {
			return nil, errors.New("IN subquery must return at least one column")
		}
		rows, err := collectResultRows(result)
		if err != nil {
			return nil, err
		}
		value.Values = make([]parser.Expr, 0, len(rows))
		for _, row := range rows {
			value.Values = append(value.Values, queryResultExpression(row))
		}
		value.Subquery = nil
		return value, nil
	case parser.BetweenExpr:
		var err error
		value.Value, err = materializeInSubqueriesForOuter(store, session, value.Value, outer)
		if err != nil {
			return nil, err
		}
		value.Lower, err = materializeInSubqueriesForOuter(store, session, value.Lower, outer)
		if err != nil {
			return nil, err
		}
		value.Upper, err = materializeInSubqueriesForOuter(store, session, value.Upper, outer)
		return value, err
	case parser.IsExpr:
		var err error
		value.Value, err = materializeInSubqueriesForOuter(store, session, value.Value, outer)
		if err != nil {
			return nil, err
		}
		value.Target, err = materializeInSubqueriesForOuter(store, session, value.Target, outer)
		return value, err
	case parser.FunctionExpr:
		for index := range value.Args {
			var err error
			value.Args[index], err = materializeInSubqueriesForOuter(store, session, value.Args[index], outer)
			if err != nil {
				return nil, err
			}
		}
		return value, nil
	case parser.RowExpr:
		for index := range value.Values {
			var err error
			value.Values[index], err = materializeInSubqueriesForOuter(store, session, value.Values[index], outer)
			if err != nil {
				return nil, err
			}
		}
		return value, nil
	case parser.IntervalExpr:
		inner, err := materializeInSubqueriesForOuter(store, session, value.Value, outer)
		value.Value = inner
		return value, err
	case parser.CaseExpr:
		var err error
		value.Operand, err = materializeInSubqueriesForOuter(store, session, value.Operand, outer)
		if err != nil {
			return nil, err
		}
		for index := range value.Whens {
			value.Whens[index].When, err = materializeInSubqueriesForOuter(store, session, value.Whens[index].When, outer)
			if err != nil {
				return nil, err
			}
			value.Whens[index].Then, err = materializeInSubqueriesForOuter(store, session, value.Whens[index].Then, outer)
			if err != nil {
				return nil, err
			}
		}
		value.Else, err = materializeInSubqueriesForOuter(store, session, value.Else, outer)
		return value, err
	default:
		return expression, nil
	}
}

func queryResultLiteral(value any) parser.Expr {
	literal := parser.Literal{Kind: parser.LiteralString, Text: fmt.Sprint(value)}
	switch typed := value.(type) {
	case nil:
		literal = parser.Literal{Kind: parser.LiteralNull}
	case bool:
		literal = parser.Literal{Kind: parser.LiteralBoolean, Text: strconv.FormatBool(typed)}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		literal = parser.Literal{Kind: parser.LiteralNumber, Text: fmt.Sprint(typed)}
	case time.Time:
		literal.Text = typed.Format("2006-01-02 15:04:05.999999")
	case []byte:
		literal.Text = string(typed)
	}
	return parser.LiteralExpr{Value: literal}
}

func queryResultExpression(row []any) parser.Expr {
	if len(row) == 1 {
		return queryResultLiteral(row[0])
	}
	result := parser.RowExpr{Values: make([]parser.Expr, len(row))}
	for index, value := range row {
		result.Values[index] = queryResultLiteral(value)
	}
	return result
}

func expressionPredicateWithContext(expr parser.Expr, table *storage.Table, session *Session, store *storage.Store) storage.Predicate {
	return expressionPredicateWithContextCapture(expr, table, session, store, nil)
}

func expressionPredicateWithContextCapture(expr parser.Expr, table *storage.Table, session *Session, store *storage.Store, evaluationErr *error) storage.Predicate {
	if expr == nil {
		return nil
	}
	return func(row storage.Row) bool {
		if evaluationErr != nil && *evaluationErr != nil {
			return false
		}
		value, err := evaluateExprWithContext(expr, table, row, session, store)
		if err != nil && evaluationErr != nil {
			*evaluationErr = err
		}
		return err == nil && truthy(value)
	}
}

func lookupUniquePredicate(expr parser.Expr, table *storage.Table, columns []storage.Column) (storage.Row, bool, bool) {
	comparison, ok := expr.(parser.BinaryExpr)
	if !ok || comparison.Operator != "=" && comparison.Operator != "<=>" {
		return nil, false, false
	}
	identifier, identifierOK := comparison.Left.(parser.Identifier)
	literal, literalOK := comparison.Right.(parser.LiteralExpr)
	if !identifierOK || !literalOK {
		identifier, identifierOK = comparison.Right.(parser.Identifier)
		literal, literalOK = comparison.Left.(parser.LiteralExpr)
	}
	if !identifierOK || !literalOK || literal.Value.Kind == parser.LiteralNull {
		return nil, false, false
	}
	position, exists := queryColumnIndex(table, identifier.Name)
	if !exists {
		return nil, false, false
	}
	value, err := literalToValue(literal.Value, columns[position])
	if err != nil {
		return nil, false, false
	}
	return table.LookupUnique(columns[position].Name, value)
}
func evaluateExpr(expr parser.Expr, table *storage.Table, row storage.Row) (any, error) {
	return evaluateExprWithLookup(expr, func(name string) (any, error) {
		index, ok := queryColumnIndex(table, name)
		if !ok {
			return nil, fmt.Errorf("unknown column %s", name)
		}
		return row[index].Interface(), nil
	})
}

func evaluateExprWithContext(expr parser.Expr, table *storage.Table, row storage.Row, session *Session, store *storage.Store) (any, error) {
	lookup := func(name string) (any, error) {
		if session != nil && strings.EqualFold(name, "LAST_INSERT_ID()") {
			return int64(session.LastInsertID), nil
		}
		index, ok := queryColumnIndex(table, name)
		if ok {
			return row[index].Interface(), nil
		}
		if correlated, exists := correlationScopeValue(session, name); exists {
			return correlated, nil
		}
		return nil, fmt.Errorf("unknown column %s", name)
	}
	if session == nil || store == nil {
		return evaluateExprWithLookup(expr, lookup)
	}
	scope := make(map[string]any)
	for index, column := range table.ColumnsView() {
		value := row[index].Interface()
		scope[column.Name] = value
		scope[stripQualifier(column.Name)] = value
	}
	session.correlationScopes = append(session.correlationScopes, scope)
	defer func() { session.correlationScopes = session.correlationScopes[:len(session.correlationScopes)-1] }()
	materialized, err := materializeInSubqueries(store, session, expr)
	if err != nil {
		return nil, err
	}
	return evaluateExprWithLookup(materialized, lookup)
}

func correlationScopeValue(session *Session, name string) (any, bool) {
	if session == nil {
		return nil, false
	}
	for index := len(session.correlationScopes) - 1; index >= 0; index-- {
		for key, value := range session.correlationScopes[index] {
			if strings.EqualFold(key, name) {
				return value, true
			}
		}
	}
	return nil, false
}

func evaluateExprWithLookup(expr parser.Expr, lookup func(string) (any, error)) (any, error) {
	switch value := expr.(type) {
	case parser.CaseExpr:
		var operand any
		var err error
		if value.Operand != nil {
			operand, err = evaluateExprWithLookup(value.Operand, lookup)
			if err != nil {
				return nil, err
			}
		}
		for _, branch := range value.Whens {
			condition, branchErr := evaluateExprWithLookup(branch.When, lookup)
			if branchErr != nil {
				return nil, branchErr
			}
			matched := truthy(condition)
			if value.Operand != nil {
				matched = operand != nil && condition != nil && compareAny(operand, condition) == 0
			}
			if matched {
				return evaluateExprWithLookup(branch.Then, lookup)
			}
		}
		if value.Else != nil {
			return evaluateExprWithLookup(value.Else, lookup)
		}
		return nil, nil
	case parser.ScalarSubquery:
		return nil, errors.New("scalar subquery requires query context")
	case parser.ExistsExpr:
		return nil, errors.New("EXISTS requires query context")
	case parser.Identifier:
		return lookup(value.Name)
	case parser.LiteralExpr:
		return literalInterface(value.Value)
	case parser.RowExpr:
		row := make([]any, len(value.Values))
		for index, expression := range value.Values {
			item, err := evaluateExprWithLookup(expression, lookup)
			if err != nil {
				return nil, err
			}
			row[index] = item
		}
		return row, nil
	case parser.FunctionExpr:
		return evaluateFunction(value, lookup)
	case parser.IntervalExpr:
		amount, err := evaluateExprWithLookup(value.Value, lookup)
		if err != nil {
			return nil, err
		}
		number, ok := numeric(amount)
		if !ok || number != float64(int64(number)) {
			return nil, fmt.Errorf("INTERVAL value must be an integer, got %v", amount)
		}
		return sqlInterval{amount: int64(number), unit: strings.ToUpper(value.Unit)}, nil
	case parser.UnaryExpr:
		inner, err := evaluateExprWithLookup(value.Value, lookup)
		if err != nil {
			return nil, err
		}
		if inner == nil {
			return nil, nil
		}
		return !truthy(inner), nil
	case parser.InExpr:
		candidate, err := evaluateExprWithLookup(value.Value, lookup)
		if err != nil || candidate == nil {
			return nil, err
		}
		matched, containsNull := false, false
		for _, expression := range value.Values {
			item, itemErr := evaluateExprWithLookup(expression, lookup)
			if itemErr != nil {
				return nil, itemErr
			}
			equal, unknown, compareErr := sqlInEqual(candidate, item)
			if compareErr != nil {
				return nil, compareErr
			}
			if unknown {
				containsNull = true
			}
			if equal {
				matched = true
				break
			}
		}
		if value.Not {
			if !matched && containsNull {
				return nil, nil
			}
			return !matched, nil
		}
		if !matched && containsNull {
			return nil, nil
		}
		return matched, nil
	case parser.BetweenExpr:
		candidate, err := evaluateExprWithLookup(value.Value, lookup)
		if err != nil {
			return nil, err
		}
		lower, err := evaluateExprWithLookup(value.Lower, lookup)
		if err != nil {
			return nil, err
		}
		upper, err := evaluateExprWithLookup(value.Upper, lookup)
		if err != nil {
			return nil, err
		}
		if candidate == nil || lower == nil || upper == nil {
			return nil, nil
		}
		matched := compareAny(candidate, lower) >= 0 && compareAny(candidate, upper) <= 0
		if value.Not {
			matched = !matched
		}
		return matched, nil
	case parser.IsExpr:
		candidate, err := evaluateExprWithLookup(value.Value, lookup)
		if err != nil {
			return nil, err
		}
		target, err := evaluateExprWithLookup(value.Target, lookup)
		if err != nil {
			return nil, err
		}
		matched := false
		if target == nil {
			matched = candidate == nil
		} else if boolean, ok := target.(bool); ok {
			matched = candidate != nil && truthy(candidate) == boolean
		}
		if value.Not {
			matched = !matched
		}
		return matched, nil
	case parser.BinaryExpr:
		left, err := evaluateExprWithLookup(value.Left, lookup)
		if err != nil {
			return nil, err
		}
		right, err := evaluateExprWithLookup(value.Right, lookup)
		if err != nil {
			return nil, err
		}
		switch value.Operator {
		case "AND":
			return sqlAnd(left, right), nil
		case "OR":
			return sqlOr(left, right), nil
		case "=":
			if left == nil || right == nil {
				return nil, nil
			}
			return compareAny(left, right) == 0, nil
		case "<=>":
			if left == nil || right == nil {
				return left == nil && right == nil, nil
			}
			return compareAny(left, right) == 0, nil
		case "!=", "<>":
			if left == nil || right == nil {
				return nil, nil
			}
			return compareAny(left, right) != 0, nil
		case ">":
			if left == nil || right == nil {
				return nil, nil
			}
			return compareAny(left, right) > 0, nil
		case "<":
			if left == nil || right == nil {
				return nil, nil
			}
			return compareAny(left, right) < 0, nil
		case ">=":
			if left == nil || right == nil {
				return nil, nil
			}
			return compareAny(left, right) >= 0, nil
		case "<=":
			if left == nil || right == nil {
				return nil, nil
			}
			return compareAny(left, right) <= 0, nil
		case "LIKE", "NOT LIKE":
			if left == nil || right == nil {
				return nil, nil
			}
			matched := likeMatch(fmt.Sprint(left), fmt.Sprint(right))
			if value.Operator == "NOT LIKE" {
				matched = !matched
			}
			return matched, nil
		case "+", "-", "*", "/", "%":
			if left == nil || right == nil {
				return nil, nil
			}
			if value.Operator == "+" || value.Operator == "-" {
				if interval, ok := right.(sqlInterval); ok {
					date, dateErr := coerceSQLTime(left)
					if dateErr != nil {
						return nil, dateErr
					}
					if value.Operator == "-" {
						interval.amount = -interval.amount
					}
					return addSQLInterval(date, interval)
				}
				if interval, ok := left.(sqlInterval); ok && value.Operator == "+" {
					date, dateErr := coerceSQLTime(right)
					if dateErr != nil {
						return nil, dateErr
					}
					return addSQLInterval(date, interval)
				}
			}
			leftNumber, lok := numeric(left)
			rightNumber, rok := numeric(right)
			if !lok || !rok {
				return nil, fmt.Errorf("arithmetic operator %s requires numeric operands", value.Operator)
			}
			switch value.Operator {
			case "+":
				return leftNumber + rightNumber, nil
			case "-":
				return leftNumber - rightNumber, nil
			case "*":
				return leftNumber * rightNumber, nil
			case "/":
				if rightNumber == 0 {
					return nil, nil
				}
				return leftNumber / rightNumber, nil
			default:
				return math.Mod(leftNumber, rightNumber), nil
			}
		}
	}
	return nil, errors.New("invalid expression")
}

func functionExpressionName(function parser.FunctionExpr) string {
	if function.Star {
		return strings.ToUpper(function.Name) + "(*)"
	}
	arguments := make([]string, len(function.Args))
	for index, argument := range function.Args {
		switch value := argument.(type) {
		case parser.Identifier:
			arguments[index] = value.Name
		case parser.LiteralExpr:
			arguments[index] = value.Value.Text
		default:
			arguments[index] = "?"
		}
	}
	return strings.ToUpper(function.Name) + "(" + strings.Join(arguments, ",") + ")"
}

func evaluateFunction(function parser.FunctionExpr, lookup func(string) (any, error)) (any, error) {
	name := strings.ToUpper(function.Name)
	if name == "COUNT" || name == "SUM" || name == "AVG" || name == "MIN" || name == "MAX" {
		return lookup(functionExpressionName(function))
	}
	arguments := make([]any, len(function.Args))
	for index, expression := range function.Args {
		value, err := evaluateExprWithLookup(expression, lookup)
		if err != nil {
			return nil, err
		}
		arguments[index] = value
	}
	require := func(count int) error {
		if len(arguments) != count {
			return fmt.Errorf("%s expects %d arguments", name, count)
		}
		return nil
	}
	switch name {
	case "LAST_INSERT_ID", "VALUES":
		return lookup(functionExpressionName(function))
	case "COALESCE":
		if len(arguments) == 0 {
			return nil, errors.New("COALESCE expects at least one argument")
		}
		for _, argument := range arguments {
			if argument != nil {
				return argument, nil
			}
		}
		return nil, nil
	case "IFNULL":
		if err := require(2); err != nil {
			return nil, err
		}
		if arguments[0] != nil {
			return arguments[0], nil
		}
		return arguments[1], nil
	case "NULLIF":
		if err := require(2); err != nil {
			return nil, err
		}
		if arguments[0] == nil || arguments[1] == nil {
			return arguments[0], nil
		}
		if compareAny(arguments[0], arguments[1]) == 0 {
			return nil, nil
		}
		return arguments[0], nil
	case "IF":
		if err := require(3); err != nil {
			return nil, err
		}
		if truthy(arguments[0]) {
			return arguments[1], nil
		}
		return arguments[2], nil
	case "GREATEST", "LEAST":
		if len(arguments) < 2 {
			return nil, fmt.Errorf("%s expects at least 2 arguments", name)
		}
		result := arguments[0]
		if result == nil {
			return nil, nil
		}
		for _, argument := range arguments[1:] {
			if argument == nil {
				return nil, nil
			}
			comparison := compareAny(argument, result)
			if (name == "GREATEST" && comparison > 0) || (name == "LEAST" && comparison < 0) {
				result = argument
			}
		}
		return result, nil
	case "CONCAT":
		var result strings.Builder
		for _, argument := range arguments {
			if argument == nil {
				return nil, nil
			}
			result.WriteString(fmt.Sprint(argument))
		}
		return result.String(), nil
	case "CONCAT_WS":
		if len(arguments) == 0 || arguments[0] == nil {
			return nil, nil
		}
		parts := make([]string, 0, len(arguments)-1)
		for _, argument := range arguments[1:] {
			if argument != nil {
				parts = append(parts, fmt.Sprint(argument))
			}
		}
		return strings.Join(parts, fmt.Sprint(arguments[0])), nil
	case "LOWER", "LCASE":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		return strings.ToLower(fmt.Sprint(arguments[0])), nil
	case "UPPER", "UCASE":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		return strings.ToUpper(fmt.Sprint(arguments[0])), nil
	case "LENGTH", "OCTET_LENGTH":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		return int64(len([]byte(fmt.Sprint(arguments[0])))), nil
	case "CHAR_LENGTH", "CHARACTER_LENGTH":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		return int64(utf8.RuneCountInString(fmt.Sprint(arguments[0]))), nil
	case "TRIM":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		return strings.TrimSpace(fmt.Sprint(arguments[0])), nil
	case "LTRIM":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		return strings.TrimLeftFunc(fmt.Sprint(arguments[0]), func(r rune) bool { return r == ' ' }), nil
	case "RTRIM":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		return strings.TrimRightFunc(fmt.Sprint(arguments[0]), func(r rune) bool { return r == ' ' }), nil
	case "LEFT", "RIGHT":
		if err := require(2); err != nil || anyNil(arguments) {
			return nil, err
		}
		count, err := mysqlIntegerArgument(name, arguments[1])
		if err != nil {
			return nil, err
		}
		characters := []rune(fmt.Sprint(arguments[0]))
		if count <= 0 {
			return "", nil
		}
		if count >= int64(len(characters)) {
			return string(characters), nil
		}
		if name == "LEFT" {
			return string(characters[:count]), nil
		}
		return string(characters[int64(len(characters))-count:]), nil
	case "SUBSTRING", "SUBSTR", "MID":
		if len(arguments) < 2 || len(arguments) > 3 {
			return nil, fmt.Errorf("%s expects 2 or 3 arguments", name)
		}
		if anyNil(arguments) {
			return nil, nil
		}
		position, err := mysqlIntegerArgument(name, arguments[1])
		if err != nil {
			return nil, err
		}
		characters := []rune(fmt.Sprint(arguments[0]))
		start := position - 1
		if position < 0 {
			start = int64(len(characters)) + position
		}
		if position == 0 || start >= int64(len(characters)) {
			return "", nil
		}
		if start < 0 {
			start = 0
		}
		end := int64(len(characters))
		if len(arguments) == 3 {
			length, lengthErr := mysqlIntegerArgument(name, arguments[2])
			if lengthErr != nil {
				return nil, lengthErr
			}
			if length <= 0 {
				return "", nil
			}
			if start+length < end {
				end = start + length
			}
		}
		return string(characters[start:end]), nil
	case "REPLACE":
		if err := require(3); err != nil || anyNil(arguments) {
			return nil, err
		}
		return strings.ReplaceAll(fmt.Sprint(arguments[0]), fmt.Sprint(arguments[1]), fmt.Sprint(arguments[2])), nil
	case "REVERSE":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		characters := []rune(fmt.Sprint(arguments[0]))
		for left, right := 0, len(characters)-1; left < right; left, right = left+1, right-1 {
			characters[left], characters[right] = characters[right], characters[left]
		}
		return string(characters), nil
	case "REPEAT":
		if err := require(2); err != nil || anyNil(arguments) {
			return nil, err
		}
		count, err := mysqlIntegerArgument(name, arguments[1])
		if err != nil {
			return nil, err
		}
		if count <= 0 {
			return "", nil
		}
		text := fmt.Sprint(arguments[0])
		if count > 1_000_000 || int64(len(text))*count > 16*1024*1024 {
			return nil, fmt.Errorf("REPEAT result is too large")
		}
		return strings.Repeat(text, int(count)), nil
	case "LPAD", "RPAD":
		if err := require(3); err != nil || anyNil(arguments) {
			return nil, err
		}
		length, err := mysqlIntegerArgument(name, arguments[1])
		if err != nil {
			return nil, err
		}
		if length < 0 || length > 1_000_000 {
			return nil, nil
		}
		text := []rune(fmt.Sprint(arguments[0]))
		if int64(len(text)) >= length {
			return string(text[:length]), nil
		}
		padding := []rune(fmt.Sprint(arguments[2]))
		if len(padding) == 0 {
			return nil, nil
		}
		needed := int(length) - len(text)
		filled := make([]rune, needed)
		for index := range filled {
			filled[index] = padding[index%len(padding)]
		}
		if name == "LPAD" {
			return string(filled) + string(text), nil
		}
		return string(text) + string(filled), nil
	case "LOCATE", "INSTR":
		expected := 2
		if name == "LOCATE" && len(arguments) == 3 {
			expected = 3
		}
		if err := require(expected); err != nil || anyNil(arguments) {
			return nil, err
		}
		haystack, needle := fmt.Sprint(arguments[1]), fmt.Sprint(arguments[0])
		start := int64(1)
		if name == "INSTR" {
			haystack, needle = fmt.Sprint(arguments[0]), fmt.Sprint(arguments[1])
		} else if len(arguments) == 3 {
			parsedStart, err := mysqlIntegerArgument(name, arguments[2])
			if err != nil {
				return nil, err
			}
			start = parsedStart
		}
		characters := []rune(haystack)
		if start < 1 || start > int64(len(characters))+1 {
			return int64(0), nil
		}
		offset := int(start - 1)
		bytePosition := strings.Index(string(characters[offset:]), needle)
		if bytePosition < 0 {
			return int64(0), nil
		}
		return int64(offset + utf8.RuneCountInString(string(characters[offset:])[:bytePosition]) + 1), nil
	case "ASCII":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		text := fmt.Sprint(arguments[0])
		if text == "" {
			return int64(0), nil
		}
		return int64(text[0]), nil
	case "SPACE":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		count, err := mysqlIntegerArgument(name, arguments[0])
		if err != nil {
			return nil, err
		}
		if count <= 0 {
			return "", nil
		}
		if count > 16*1024*1024 {
			return nil, fmt.Errorf("SPACE result is too large")
		}
		return strings.Repeat(" ", int(count)), nil
	case "ISNULL":
		if err := require(1); err != nil {
			return nil, err
		}
		return arguments[0] == nil, nil
	case "ABS", "CEIL", "CEILING", "FLOOR", "SQRT", "EXP", "LN", "LOG2", "LOG10", "SIGN":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		number, err := mysqlNumericArgument(name, arguments[0])
		if err != nil {
			return nil, err
		}
		switch name {
		case "ABS":
			return math.Abs(number), nil
		case "CEIL", "CEILING":
			return math.Ceil(number), nil
		case "FLOOR":
			return math.Floor(number), nil
		case "SQRT":
			if number < 0 {
				return nil, nil
			}
			return math.Sqrt(number), nil
		case "EXP":
			return math.Exp(number), nil
		case "LN":
			if number <= 0 {
				return nil, nil
			}
			return math.Log(number), nil
		case "LOG2":
			if number <= 0 {
				return nil, nil
			}
			return math.Log2(number), nil
		case "LOG10":
			if number <= 0 {
				return nil, nil
			}
			return math.Log10(number), nil
		default:
			if number < 0 {
				return int64(-1), nil
			}
			if number > 0 {
				return int64(1), nil
			}
			return int64(0), nil
		}
	case "ROUND", "TRUNCATE":
		if (name == "ROUND" && (len(arguments) < 1 || len(arguments) > 2)) || (name == "TRUNCATE" && len(arguments) != 2) {
			return nil, fmt.Errorf("%s expects %s arguments", name, map[bool]string{true: "1 or 2", false: "2"}[name == "ROUND"])
		}
		if anyNil(arguments) {
			return nil, nil
		}
		number, err := mysqlNumericArgument(name, arguments[0])
		if err != nil {
			return nil, err
		}
		digits := int64(0)
		if len(arguments) == 2 {
			digits, err = mysqlIntegerArgument(name, arguments[1])
			if err != nil {
				return nil, err
			}
		}
		return mysqlRound(number, digits, name == "TRUNCATE"), nil
	case "MOD", "POW", "POWER":
		if err := require(2); err != nil || anyNil(arguments) {
			return nil, err
		}
		left, err := mysqlNumericArgument(name, arguments[0])
		if err != nil {
			return nil, err
		}
		right, err := mysqlNumericArgument(name, arguments[1])
		if err != nil {
			return nil, err
		}
		if name == "MOD" {
			if right == 0 {
				return nil, nil
			}
			return math.Mod(left, right), nil
		}
		return math.Pow(left, right), nil
	case "PI":
		if err := require(0); err != nil {
			return nil, err
		}
		return math.Pi, nil
	case "LOG":
		if len(arguments) < 1 || len(arguments) > 2 {
			return nil, errors.New("LOG expects 1 or 2 arguments")
		}
		if anyNil(arguments) {
			return nil, nil
		}
		if len(arguments) == 1 {
			number, err := mysqlNumericArgument(name, arguments[0])
			if err != nil {
				return nil, err
			}
			if number <= 0 {
				return nil, nil
			}
			return math.Log(number), nil
		}
		base, err := mysqlNumericArgument(name, arguments[0])
		if err != nil {
			return nil, err
		}
		number, err := mysqlNumericArgument(name, arguments[1])
		if err != nil {
			return nil, err
		}
		if base <= 0 || base == 1 || number <= 0 {
			return nil, nil
		}
		return math.Log(number) / math.Log(base), nil
	case "NOW", "CURRENT_TIMESTAMP":
		if err := require(0); err != nil {
			return nil, err
		}
		return time.Now().Format("2006-01-02 15:04:05"), nil
	case "CURDATE", "CURRENT_DATE":
		if err := require(0); err != nil {
			return nil, err
		}
		return normalizeSQLDate(time.Now()), nil
	case "DATE":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		return coerceSQLDate(arguments[0])
	case "DATE_SUB", "DATE_ADD":
		if err := require(2); err != nil || arguments[0] == nil || arguments[1] == nil {
			return nil, err
		}
		date, err := coerceSQLDate(arguments[0])
		if err != nil {
			return nil, err
		}
		interval, ok := arguments[1].(sqlInterval)
		if !ok {
			return nil, fmt.Errorf("%s second argument must be INTERVAL", name)
		}
		if name == "DATE_SUB" {
			interval.amount = -interval.amount
		}
		return addSQLInterval(date, interval)
	case "YEAR", "MONTH", "DAY", "DAYOFMONTH", "DAYOFWEEK", "DAYOFYEAR", "WEEKDAY", "QUARTER", "HOUR", "MINUTE", "SECOND":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		date, err := coerceSQLTime(arguments[0])
		if err != nil {
			return nil, err
		}
		switch name {
		case "YEAR":
			return int64(date.Year()), nil
		case "MONTH":
			return int64(date.Month()), nil
		case "DAY", "DAYOFMONTH":
			return int64(date.Day()), nil
		case "DAYOFWEEK":
			return int64(date.Weekday()) + 1, nil
		case "DAYOFYEAR":
			return int64(date.YearDay()), nil
		case "WEEKDAY":
			return int64((int(date.Weekday()) + 6) % 7), nil
		case "QUARTER":
			return int64((int(date.Month())-1)/3 + 1), nil
		case "HOUR":
			return int64(date.Hour()), nil
		case "MINUTE":
			return int64(date.Minute()), nil
		default:
			return int64(date.Second()), nil
		}
	case "DATEDIFF":
		if err := require(2); err != nil || anyNil(arguments) {
			return nil, err
		}
		left, err := coerceSQLTime(arguments[0])
		if err != nil {
			return nil, err
		}
		right, err := coerceSQLTime(arguments[1])
		if err != nil {
			return nil, err
		}
		left, right = normalizeSQLDate(left), normalizeSQLDate(right)
		return int64(left.Sub(right) / (24 * time.Hour)), nil
	case "LAST_DAY":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		date, err := coerceSQLTime(arguments[0])
		if err != nil {
			return nil, err
		}
		return normalizeSQLDate(time.Date(date.Year(), date.Month()+1, 0, 0, 0, 0, 0, time.Local)), nil
	case "DATE_FORMAT":
		if err := require(2); err != nil || anyNil(arguments) {
			return nil, err
		}
		date, err := coerceSQLTime(arguments[0])
		if err != nil {
			return nil, err
		}
		return mysqlDateFormat(date, fmt.Sprint(arguments[1])), nil
	case "MONTHNAME", "DAYNAME":
		if err := require(1); err != nil || arguments[0] == nil {
			return nil, err
		}
		date, err := coerceSQLTime(arguments[0])
		if err != nil {
			return nil, err
		}
		if name == "MONTHNAME" {
			return date.Month().String(), nil
		}
		return date.Weekday().String(), nil
	default:
		return nil, fmt.Errorf("unsupported function %s", function.Name)
	}
}

func anyNil(values []any) bool {
	for _, value := range values {
		if value == nil {
			return true
		}
	}
	return false
}

func mysqlNumericArgument(function string, value any) (float64, error) {
	if number, ok := numeric(value); ok {
		return number, nil
	}
	switch converted := value.(type) {
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(converted), 64)
		if err == nil {
			return number, nil
		}
	case bool:
		if converted {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("%s requires a numeric argument, got %v", function, value)
}

func mysqlIntegerArgument(function string, value any) (int64, error) {
	number, err := mysqlNumericArgument(function, value)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number > math.MaxInt64 || number < math.MinInt64 {
		return 0, fmt.Errorf("%s integer argument is out of range", function)
	}
	return int64(number), nil
}

func mysqlRound(number float64, digits int64, truncate bool) float64 {
	if digits > 308 {
		return number
	}
	if digits < -308 {
		return 0
	}
	round := math.Round
	if truncate {
		round = math.Trunc
	}
	if digits >= 0 {
		scale := math.Pow10(int(digits))
		return round(number*scale) / scale
	}
	scale := math.Pow10(int(-digits))
	return round(number/scale) * scale
}

func coerceSQLTime(value any) (time.Time, error) {
	switch date := value.(type) {
	case time.Time:
		return date, nil
	case string:
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999",
			"2006-01-02 15:04:05",
			"2006-01-02",
		} {
			parsed, err := time.ParseInLocation(layout, date, time.Local)
			if err == nil {
				return parsed, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("invalid date/time value %v", value)
}

func mysqlDateFormat(date time.Time, format string) string {
	var result strings.Builder
	for index := 0; index < len(format); index++ {
		if format[index] != '%' || index+1 >= len(format) {
			result.WriteByte(format[index])
			continue
		}
		index++
		switch format[index] {
		case '%':
			result.WriteByte('%')
		case 'Y':
			fmt.Fprintf(&result, "%04d", date.Year())
		case 'y':
			fmt.Fprintf(&result, "%02d", date.Year()%100)
		case 'm':
			fmt.Fprintf(&result, "%02d", date.Month())
		case 'c':
			fmt.Fprintf(&result, "%d", date.Month())
		case 'M':
			result.WriteString(date.Month().String())
		case 'b':
			result.WriteString(date.Month().String()[:3])
		case 'd':
			fmt.Fprintf(&result, "%02d", date.Day())
		case 'e':
			fmt.Fprintf(&result, "%d", date.Day())
		case 'j':
			fmt.Fprintf(&result, "%03d", date.YearDay())
		case 'H':
			fmt.Fprintf(&result, "%02d", date.Hour())
		case 'k':
			fmt.Fprintf(&result, "%d", date.Hour())
		case 'h', 'I':
			hour := date.Hour() % 12
			if hour == 0 {
				hour = 12
			}
			fmt.Fprintf(&result, "%02d", hour)
		case 'l':
			hour := date.Hour() % 12
			if hour == 0 {
				hour = 12
			}
			fmt.Fprintf(&result, "%d", hour)
		case 'i':
			fmt.Fprintf(&result, "%02d", date.Minute())
		case 's', 'S':
			fmt.Fprintf(&result, "%02d", date.Second())
		case 'f':
			fmt.Fprintf(&result, "%06d", date.Nanosecond()/1000)
		case 'p':
			if date.Hour() < 12 {
				result.WriteString("AM")
			} else {
				result.WriteString("PM")
			}
		case 'W':
			result.WriteString(date.Weekday().String())
		case 'a':
			result.WriteString(date.Weekday().String()[:3])
		case 'w':
			fmt.Fprintf(&result, "%d", date.Weekday())
		case 'T':
			result.WriteString(date.Format("15:04:05"))
		case 'r':
			result.WriteString(date.Format("03:04:05 PM"))
		default:
			result.WriteByte(format[index])
		}
	}
	return result.String()
}

type sqlInterval struct {
	amount int64
	unit   string
}

func normalizeSQLDate(value time.Time) time.Time {
	value = value.In(time.Local)
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func coerceSQLDate(value any) (time.Time, error) {
	switch date := value.(type) {
	case time.Time:
		return normalizeSQLDate(date), nil
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
			parsed, err := time.ParseInLocation(layout, date, time.Local)
			if err == nil {
				return normalizeSQLDate(parsed), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("invalid DATE value %v", value)
}

func addSQLInterval(date time.Time, interval sqlInterval) (time.Time, error) {
	switch interval.unit {
	case "DAY", "DAYS":
		return date.AddDate(0, 0, int(interval.amount)), nil
	case "WEEK", "WEEKS":
		return date.AddDate(0, 0, int(interval.amount*7)), nil
	case "MONTH", "MONTHS":
		return date.AddDate(0, int(interval.amount), 0), nil
	case "QUARTER", "QUARTERS":
		return date.AddDate(0, int(interval.amount*3), 0), nil
	case "YEAR", "YEARS":
		return date.AddDate(int(interval.amount), 0, 0), nil
	case "HOUR", "HOURS":
		return date.Add(time.Duration(interval.amount) * time.Hour), nil
	case "MINUTE", "MINUTES":
		return date.Add(time.Duration(interval.amount) * time.Minute), nil
	case "SECOND", "SECONDS":
		return date.Add(time.Duration(interval.amount) * time.Second), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported INTERVAL unit %s", interval.unit)
	}
}

func sqlAnd(left, right any) any {
	if (left != nil && !truthy(left)) || (right != nil && !truthy(right)) {
		return false
	}
	if left == nil || right == nil {
		return nil
	}
	return true
}

func sqlOr(left, right any) any {
	if (left != nil && truthy(left)) || (right != nil && truthy(right)) {
		return true
	}
	if left == nil || right == nil {
		return nil
	}
	return false
}

func sqlInEqual(left, right any) (bool, bool, error) {
	leftRow, leftIsRow := left.([]any)
	rightRow, rightIsRow := right.([]any)
	if leftIsRow || rightIsRow {
		if !leftIsRow || !rightIsRow || len(leftRow) != len(rightRow) {
			return false, false, fmt.Errorf("IN row column count mismatch")
		}
		unknown := false
		for index := range leftRow {
			equal, itemUnknown, err := sqlInEqual(leftRow[index], rightRow[index])
			if err != nil {
				return false, false, err
			}
			if !equal && !itemUnknown {
				return false, false, nil
			}
			unknown = unknown || itemUnknown
		}
		return !unknown, unknown, nil
	}
	if left == nil || right == nil {
		return false, true, nil
	}
	return compareAny(left, right) == 0, false, nil
}

func likeMatch(value, pattern string) bool {
	input := []rune(strings.ToLower(value))
	wildcard := []rune(strings.ToLower(pattern))
	inputPosition, patternPosition := 0, 0
	starPosition, retryInput := -1, 0
	for inputPosition < len(input) {
		switch {
		case patternPosition < len(wildcard) && (wildcard[patternPosition] == '_' || wildcard[patternPosition] == input[inputPosition]):
			inputPosition++
			patternPosition++
		case patternPosition < len(wildcard) && wildcard[patternPosition] == '%':
			starPosition = patternPosition
			patternPosition++
			retryInput = inputPosition
		case starPosition >= 0:
			patternPosition = starPosition + 1
			retryInput++
			inputPosition = retryInput
		default:
			return false
		}
	}
	for patternPosition < len(wildcard) && wildcard[patternPosition] == '%' {
		patternPosition++
	}
	return patternPosition == len(wildcard)
}
func literalInterface(value parser.Literal) (any, error) {
	switch value.Kind {
	case parser.LiteralNull:
		return nil, nil
	case parser.LiteralString:
		return value.Text, nil
	case parser.LiteralBoolean:
		return strings.EqualFold(value.Text, "true"), nil
	case parser.LiteralNumber:
		if strings.Contains(value.Text, ".") {
			return strconv.ParseFloat(value.Text, 64)
		}
		return strconv.ParseInt(value.Text, 10, 64)
	}
	return nil, errors.New("invalid literal")
}
func literalToValue(literal parser.Literal, column storage.Column) (storage.Value, error) {
	if literal.Kind == parser.LiteralNull {
		return storage.NullValue(column.Type), nil
	}
	raw, err := literalInterface(literal)
	if err != nil {
		return storage.Value{}, err
	}
	if column.Type == storage.TypeDate || column.Type == storage.TypeDateTime {
		if text, ok := raw.(string); ok {
			return storage.NewValue(column.Type, text)
		}
	}
	if column.Type == storage.TypeVarchar || column.Type == storage.TypeText {
		if _, ok := raw.(string); !ok {
			raw = fmt.Sprint(raw)
		}
	}
	return storage.NewValue(column.Type, raw)
}

func interfaceToColumnValue(raw any, column storage.Column) (storage.Value, error) {
	if raw == nil {
		return storage.NullValue(column.Type), nil
	}
	if column.Type == storage.TypeInt || column.Type == storage.TypeBigInt {
		if number, ok := numeric(raw); ok && math.Trunc(number) == number && number >= math.MinInt64 && number <= math.MaxInt64 {
			raw = int64(number)
		}
	}
	if column.Type == storage.TypeVarchar || column.Type == storage.TypeText {
		if _, ok := raw.(string); !ok {
			raw = fmt.Sprint(raw)
		}
	}
	return storage.NewValue(column.Type, raw)
}
func compareAny(left, right any) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	if leftDate, ok := left.(time.Time); ok {
		if rightDate, rightOK := comparableSQLTime(right); rightOK {
			if leftDate.Before(rightDate) {
				return -1
			}
			if leftDate.After(rightDate) {
				return 1
			}
			return 0
		}
	}
	if rightDate, ok := right.(time.Time); ok {
		if leftDate, leftOK := comparableSQLTime(left); leftOK {
			if leftDate.Before(rightDate) {
				return -1
			}
			if leftDate.After(rightDate) {
				return 1
			}
			return 0
		}
	}
	lf, lok := numeric(left)
	rf, rok := numeric(right)
	if lok && rok {
		if lf < rf {
			return -1
		}
		if lf > rf {
			return 1
		}
		return 0
	}
	ls, rs := fmt.Sprint(left), fmt.Sprint(right)
	return strings.Compare(ls, rs)
}

func comparableSQLTime(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed, true
	case string:
		for _, layout := range []string{
			time.RFC3339Nano,
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05",
			"2006-01-02",
		} {
			parsed, err := time.Parse(layout, typed)
			if err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}
func numeric(value any) (float64, bool) {
	switch n := value.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}
func truthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case nil:
		return false
	case int64:
		return v != 0
	case int:
		return v != 0
	case uint64:
		return v != 0
	case float64:
		return v != 0
	case string:
		return v != ""
	}
	return false
}
func stripQualifier(name string) string {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[dot+1:]
	}
	return strings.Trim(name, "`")
}

func queryColumnIndex(table *storage.Table, name string) (int, bool) {
	name = strings.TrimSpace(name)
	if index, ok := table.ColumnIndex(name); ok {
		return index, true
	}
	if strings.Contains(name, ".") {
		for _, column := range table.ColumnsView() {
			if strings.Contains(column.Name, ".") {
				return 0, false
			}
		}
	}
	return table.ColumnIndex(stripQualifier(name))
}
func columnSQLType(column storage.Column) string {
	return storage.ColumnSQLType(column)
}

func storageColumnDefinition(column parser.ColumnDef) (storage.Column, error) {
	dataType, err := storage.ParseDataType(column.Type)
	if err != nil {
		return storage.Column{}, err
	}
	length := column.Length
	sqlType := strings.TrimSpace(column.SQLType)
	if dataType == storage.TypeVarchar && length == 0 {
		switch strings.ToUpper(strings.TrimSpace(column.Type)) {
		case "CHAR", "CHARACTER", "NCHAR", "NATIONAL", "BINARY":
			length = 1
		default:
			length = 255
		}
		if !strings.Contains(sqlType, "(") {
			sqlType = fmt.Sprintf("%s(%d)", sqlType, length)
		}
	}
	definition := storage.Column{Name: column.Name, Type: dataType, SQLType: sqlType, Length: length, MetadataVersion: 1, Nullable: column.Nullable, HasDefault: column.HasDefault, DefaultExpression: column.DefaultExpression, AutoIncrement: column.AutoIncrement, Comment: column.Comment, OnUpdate: column.OnUpdate}
	if column.PrimaryKey || column.AutoIncrement {
		definition.Nullable = false
	}
	if column.HasDefault && column.DefaultExpression == "" {
		definition.Default, err = literalToValue(column.Default, definition)
		if err != nil {
			return storage.Column{}, fmt.Errorf("column %q default: %w", column.Name, err)
		}
	}
	return definition, nil
}

func columnDefaultValue(column storage.Column) (storage.Value, error) {
	if column.DefaultExpression == "" {
		return column.Default, nil
	}
	switch strings.ToLower(strings.TrimSpace(column.DefaultExpression)) {
	case "current_timestamp", "current_timestamp()", "localtimestamp", "localtimestamp()", "now()":
		return storage.NewValue(column.Type, time.Now())
	default:
		return storage.Value{}, fmt.Errorf("unsupported default expression %q for column %q", column.DefaultExpression, column.Name)
	}
}
func createTableSQL(table *storage.Table) string {
	parts := make([]string, 0)
	for _, column := range table.ColumnsView() {
		definition := fmt.Sprintf("  `%s` %s", column.Name, columnSQLType(column))
		if !storage.ColumnNullable(column) {
			definition += " NOT NULL"
		} else {
			definition += " NULL"
		}
		if column.HasDefault {
			if column.DefaultExpression != "" {
				definition += " DEFAULT " + column.DefaultExpression
			} else {
				definition += " DEFAULT " + sqlLiteral(column.Default)
			}
		}
		if column.AutoIncrement {
			definition += " AUTO_INCREMENT"
		}
		if column.Comment != "" {
			definition += " COMMENT '" + strings.ReplaceAll(column.Comment, "'", "''") + "'"
		}
		parts = append(parts, definition)
	}
	for _, definition := range table.Indexes() {
		columns := make([]string, len(definition.Columns))
		for position, column := range definition.Columns {
			columns[position] = quoteIdentifier(column)
		}
		kind := "KEY"
		if definition.Primary || strings.EqualFold(definition.Name, "PRIMARY") {
			parts = append(parts, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(columns, ", ")))
			continue
		}
		if definition.Unique {
			kind = "UNIQUE KEY"
		}
		parts = append(parts, fmt.Sprintf("  %s %s (%s)", kind, quoteIdentifier(definition.Name), strings.Join(columns, ", ")))
	}
	for _, foreignKey := range table.ForeignKeys() {
		columns := make([]string, len(foreignKey.Columns))
		referenced := make([]string, len(foreignKey.RefColumns))
		for index := range columns {
			columns[index] = quoteIdentifier(foreignKey.Columns[index])
			referenced[index] = quoteIdentifier(foreignKey.RefColumns[index])
		}
		definition := fmt.Sprintf("  CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)", quoteIdentifier(foreignKey.Name), strings.Join(columns, ", "), quoteIdentifier(foreignKey.RefTable), strings.Join(referenced, ", "))
		if foreignKey.OnDelete != "" {
			definition += " ON DELETE " + foreignKey.OnDelete
		}
		if foreignKey.OnUpdate != "" {
			definition += " ON UPDATE " + foreignKey.OnUpdate
		}
		parts = append(parts, definition)
	}
	for _, check := range table.CheckConstraints() {
		parts = append(parts, "  CONSTRAINT "+quoteIdentifier(check.Name)+" CHECK ("+check.Expression+")")
	}
	definition := fmt.Sprintf("CREATE TABLE `%s` (\n%s\n)", table.Name(), strings.Join(parts, ",\n"))
	comment, _, _ := table.Metadata()
	if comment != "" {
		definition += " COMMENT='" + strings.ReplaceAll(comment, "'", "''") + "'"
	}
	return definition
}

type BackupOptions struct {
	Databases       []string
	SchemaOnly      bool
	DataOnly        bool
	AddDropDatabase bool
}

func ExportSQL(store *storage.Store, databaseName, path string) error {
	return BackupSQL(store, path, BackupOptions{Databases: []string{databaseName}})
}

func BackupSQL(store *storage.Store, path string, options BackupOptions) error {
	if options.SchemaOnly && options.DataOnly {
		return errors.New("schema-only and data-only cannot be used together")
	}
	snapshot := store.Snapshot()
	selectAll := len(options.Databases) == 0
	wanted := make(map[string]bool, len(options.Databases))
	for _, name := range options.Databases {
		wanted[strings.ToLower(name)] = true
	}
	var databases []storage.DatabaseSnapshot
	for _, database := range snapshot.Databases {
		if selectAll || wanted[strings.ToLower(database.Name)] {
			databases = append(databases, database)
			if !selectAll {
				delete(wanted, strings.ToLower(database.Name))
			}
		}
	}
	if len(wanted) > 0 {
		for name := range wanted {
			return fmt.Errorf("%w: %q", storage.ErrDatabaseNotFound, name)
		}
	}
	if len(databases) == 0 {
		return errors.New("backup contains no databases")
	}
	sort.Slice(databases, func(i, j int) bool { return strings.ToLower(databases[i].Name) < strings.ToLower(databases[j].Name) })

	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}
	temporary := absPath + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	if _, err = fmt.Fprintf(file, "-- GBaseLite logical backup\n-- Generated: %s\n\nSET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS=0;\n\n", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	for _, database := range databases {
		if err := writeDatabaseBackup(file, database, options); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintln(file, "SET FOREIGN_KEY_CHECKS=1;"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := atomicfile.Replace(temporary, absPath); err != nil {
		return fmt.Errorf("replace backup without deleting the previous backup: %w", err)
	}
	committed = true
	return nil
}

func writeDatabaseBackup(writer io.Writer, database storage.DatabaseSnapshot, options BackupOptions) error {
	databaseName := quoteIdentifier(database.Name)
	if !options.DataOnly {
		if options.AddDropDatabase {
			if _, err := fmt.Fprintf(writer, "DROP DATABASE IF EXISTS %s;\n", databaseName); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(writer, "CREATE DATABASE IF NOT EXISTS %s;\n", databaseName); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "USE %s;\n\n", databaseName); err != nil {
		return err
	}
	for _, table := range database.Tables {
		if !options.DataOnly {
			if options.AddDropDatabase {
				if _, err := fmt.Fprintf(writer, "DROP TABLE IF EXISTS %s;\n", quoteIdentifier(table.Name)); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(writer, "%s;\n", createTableSnapshotSQL(table)); err != nil {
				return err
			}
		}
		if !options.SchemaOnly && len(table.Rows) > 0 {
			if err := writeTableRows(writer, table); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
	}
	if options.DataOnly {
		return nil
	}
	views, err := viewSnapshotsInDependencyOrder(database.Views)
	if err != nil {
		return err
	}
	for _, view := range views {
		if options.AddDropDatabase {
			if _, err := fmt.Fprintf(writer, "DROP VIEW IF EXISTS %s;\n", quoteIdentifier(view.Name)); err != nil {
				return err
			}
		}
		storageView := storage.View{Name: view.Name, Definition: view.Definition, Columns: view.Columns}
		if _, err := fmt.Fprintln(writer, createViewSQL(storageView)+";"); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(writer)
	return err
}

func writeTableRows(writer io.Writer, table storage.TableSnapshot) error {
	const rowsPerStatement = 100
	for start := 0; start < len(table.Rows); start += rowsPerStatement {
		end := start + rowsPerStatement
		if end > len(table.Rows) {
			end = len(table.Rows)
		}
		if _, err := fmt.Fprintf(writer, "INSERT INTO %s VALUES\n", quoteIdentifier(table.Name)); err != nil {
			return err
		}
		for rowIndex := start; rowIndex < end; rowIndex++ {
			values := make([]string, len(table.Rows[rowIndex]))
			for valueIndex, value := range table.Rows[rowIndex] {
				values[valueIndex] = sqlLiteral(value)
			}
			suffix := ","
			if rowIndex == end-1 {
				suffix = ";"
			}
			if _, err := fmt.Fprintf(writer, "(%s)%s\n", strings.Join(values, ","), suffix); err != nil {
				return err
			}
		}
	}
	return nil
}

func createTableSnapshotSQL(table storage.TableSnapshot) string {
	parts := make([]string, 0, len(table.Columns)+len(table.Indexes))
	for _, column := range table.Columns {
		definition := fmt.Sprintf("  %s %s", quoteIdentifier(column.Name), columnSQLType(column))
		if !storage.ColumnNullable(column) {
			definition += " NOT NULL"
		}
		if column.HasDefault {
			if column.DefaultExpression != "" {
				definition += " DEFAULT " + column.DefaultExpression
			} else {
				definition += " DEFAULT " + sqlLiteral(column.Default)
			}
		}
		if column.AutoIncrement {
			definition += " AUTO_INCREMENT"
		}
		parts = append(parts, definition)
	}
	for _, index := range table.Indexes {
		columns := make([]string, len(index.Columns))
		for position, column := range index.Columns {
			columns[position] = quoteIdentifier(column)
		}
		if index.Primary || strings.EqualFold(index.Name, "PRIMARY") {
			parts = append(parts, "  PRIMARY KEY ("+strings.Join(columns, ", ")+")")
		} else if index.Unique {
			parts = append(parts, "  UNIQUE KEY "+quoteIdentifier(index.Name)+" ("+strings.Join(columns, ", ")+")")
		} else {
			parts = append(parts, "  KEY "+quoteIdentifier(index.Name)+" ("+strings.Join(columns, ", ")+")")
		}
	}
	for _, foreignKey := range table.ForeignKeys {
		columns := make([]string, len(foreignKey.Columns))
		referenced := make([]string, len(foreignKey.RefColumns))
		for index := range columns {
			columns[index] = quoteIdentifier(foreignKey.Columns[index])
			referenced[index] = quoteIdentifier(foreignKey.RefColumns[index])
		}
		definition := "  CONSTRAINT " + quoteIdentifier(foreignKey.Name) + " FOREIGN KEY (" + strings.Join(columns, ", ") + ") REFERENCES " + quoteIdentifier(foreignKey.RefTable) + " (" + strings.Join(referenced, ", ") + ")"
		if foreignKey.OnDelete != "" {
			definition += " ON DELETE " + foreignKey.OnDelete
		}
		if foreignKey.OnUpdate != "" {
			definition += " ON UPDATE " + foreignKey.OnUpdate
		}
		parts = append(parts, definition)
	}
	checks := append([]storage.CheckConstraint(nil), table.CheckConstraints...)
	for _, expression := range table.Checks {
		checks = append(checks, storage.CheckConstraint{Expression: expression})
	}
	for _, check := range checks {
		if check.Name != "" {
			parts = append(parts, "  CONSTRAINT "+quoteIdentifier(check.Name)+" CHECK ("+check.Expression+")")
		} else {
			parts = append(parts, "  CHECK ("+check.Expression+")")
		}
	}
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n)", quoteIdentifier(table.Name), strings.Join(parts, ",\n"))
}

func viewSnapshotsInDependencyOrder(source []storage.ViewSnapshot) ([]storage.ViewSnapshot, error) {
	views := make(map[string]storage.ViewSnapshot, len(source))
	for _, view := range source {
		views[strings.ToLower(view.Name)] = view
	}
	state := make(map[string]uint8, len(views))
	ordered := make([]storage.ViewSnapshot, 0, len(views))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("circular view dependency involving %s", name)
		case 2:
			return nil
		}
		state[name] = 1
		view := views[name]
		statement, err := parser.Parse(view.Definition)
		if err != nil {
			return err
		}
		if query, ok := statement.(parser.Query); ok {
			for _, dependency := range selectRelationNames(query) {
				_, relation := splitTableName(dependency)
				key := strings.ToLower(relation)
				if _, isView := views[key]; isView {
					if err := visit(key); err != nil {
						return err
					}
				}
			}
		}
		state[name] = 2
		ordered = append(ordered, view)
		return nil
	}
	for _, view := range source {
		if err := visit(strings.ToLower(view.Name)); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func viewsInDependencyOrder(database *storage.Database) ([]storage.View, error) {
	views := make(map[string]storage.View)
	for _, name := range database.ListViews() {
		view, _ := database.View(name)
		views[strings.ToLower(name)] = view
	}
	state := make(map[string]uint8, len(views))
	ordered := make([]storage.View, 0, len(views))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("circular view dependency involving %s", name)
		case 2:
			return nil
		}
		state[name] = 1
		view := views[name]
		statement, err := parser.Parse(view.Definition)
		if err != nil {
			return err
		}
		if query, ok := statement.(parser.Query); ok {
			for _, dependency := range selectRelationNames(query) {
				_, relation := splitTableName(dependency)
				key := strings.ToLower(relation)
				if _, isView := views[key]; isView {
					if err := visit(key); err != nil {
						return err
					}
				}
			}
		}
		state[name] = 2
		ordered = append(ordered, view)
		return nil
	}
	for _, name := range database.ListViews() {
		if err := visit(strings.ToLower(name)); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func selectRelationNames(query parser.Query) []string {
	switch value := query.(type) {
	case parser.Select:
		return selectRelationNamesFromSelect(value)
	case parser.Union:
		var names []string
		for _, branch := range value.Queries {
			names = append(names, selectRelationNamesFromSelect(branch)...)
		}
		return names
	default:
		return nil
	}
}

func selectRelationNamesFromSelect(query parser.Select) []string {
	var names []string
	if query.Subquery != nil {
		names = append(names, selectRelationNames(query.Subquery)...)
	} else if query.Table != "" {
		names = append(names, query.Table)
	}
	for _, join := range query.Joins {
		if join.Subquery != nil {
			names = append(names, selectRelationNames(join.Subquery)...)
		} else if join.Table != "" {
			names = append(names, join.Table)
		}
		names = append(names, expressionRelationNames(join.On)...)
	}
	for _, item := range query.Items {
		if expression, err := parser.ParseExpression(item.Expression); err == nil {
			names = append(names, expressionRelationNames(expression)...)
		}
	}
	names = append(names, expressionRelationNames(query.Where)...)
	names = append(names, expressionRelationNames(query.Having)...)
	return names
}

func expressionRelationNames(expression parser.Expr) []string {
	if expression == nil {
		return nil
	}
	switch value := expression.(type) {
	case parser.ScalarSubquery:
		if value.Query != nil {
			return selectRelationNames(value.Query)
		}
	case parser.ExistsExpr:
		if value.Query != nil {
			return selectRelationNames(value.Query)
		}
	case parser.BinaryExpr:
		return append(expressionRelationNames(value.Left), expressionRelationNames(value.Right)...)
	case parser.UnaryExpr:
		return expressionRelationNames(value.Value)
	case parser.RowExpr:
		var names []string
		for _, item := range value.Values {
			names = append(names, expressionRelationNames(item)...)
		}
		return names
	case parser.InExpr:
		names := expressionRelationNames(value.Value)
		for _, item := range value.Values {
			names = append(names, expressionRelationNames(item)...)
		}
		if value.Subquery != nil {
			names = append(names, selectRelationNames(value.Subquery)...)
		}
		return names
	case parser.FunctionExpr:
		var names []string
		for _, argument := range value.Args {
			names = append(names, expressionRelationNames(argument)...)
		}
		return names
	case parser.CaseExpr:
		var names []string
		names = append(names, expressionRelationNames(value.Operand)...)
		for _, branch := range value.Whens {
			names = append(names, expressionRelationNames(branch.When)...)
			names = append(names, expressionRelationNames(branch.Then)...)
		}
		return append(names, expressionRelationNames(value.Else)...)
	}
	return nil
}
func sqlLiteral(value storage.Value) string {
	if value.Null {
		return "NULL"
	}
	switch value.Type {
	case storage.TypeInt, storage.TypeBigInt, storage.TypeFloat, storage.TypeDouble, storage.TypeBoolean:
		return value.String()
	default:
		text := strings.ReplaceAll(value.String(), "\\", "\\\\")
		return "'" + strings.ReplaceAll(text, "'", "''") + "'"
	}
}

func quoteIdentifier(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}

var _ = time.Time{}
