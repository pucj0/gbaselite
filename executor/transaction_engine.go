package executor

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

type versionedTable struct {
	CatalogName       string
	Referrers         []string
	CounterKeys       map[string]string
	ID                string
	KeyEncoding       uint8
	SecondaryEncoding uint8
	RowEncoding       uint8
	Definition        storage.TableSnapshot
}

func encodeVersioned(value any) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(value)
	if err == nil && b.Len() > storageengine.MaxValueBytes {
		err = fmt.Errorf("%w: encoded row/schema exceeds %d bytes", ErrQueryResourceLimit, storageengine.MaxValueBytes)
	}
	return b.Bytes(), err
}
func decodeVersioned(value []byte, out any) error {
	return gob.NewDecoder(bytes.NewReader(value)).Decode(out)
}
func versionedName(session *Session, table string) (string, string, error) {
	database, name := splitTableName(table)
	if database == "" {
		database = session.CurrentDatabase
	}
	if database == "" {
		return "", "", errors.New("no database selected")
	}
	if strings.ContainsAny(database+name, "/\x00") {
		return "", "", errors.New("unsupported namespace separator in identifier")
	}
	return strings.ToLower(database), strings.ToLower(name), nil
}
func (e *Engine) refreshSQLMetadata(ctx context.Context) error {
	e.catalogMutex.Lock()
	defer e.catalogMutex.Unlock()
	var head uint64
	var err error
	var open func() (storageengine.Iterator, error)
	if revisions, ok := e.Backend.(storageengine.RevisionReader); ok {
		head, err = revisions.CatalogHead()
		if err != nil {
			return err
		}

		open = func() (storageengine.Iterator, error) {
			return revisions.NewIterator(ctx, head, storageengine.ScanRequest{Space: sqllayout.Catalog})
		}
	} else {
		tx, beginErr := e.Backend.Begin(ctx)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()

		head = tx.Snapshot()

		open = func() (storageengine.Iterator, error) {
			return tx.NewIterator(ctx, storageengine.ScanRequest{Space: sqllayout.Catalog})
		}
	}
	if head == e.catalogRevision {
		return nil
	}
	mirror := storage.NewStore()
	var tables []versionedTable
	var names []string
	var views []versionedView
	var viewNames []string
	iterator, err := open()
	if err != nil {
		return err
	}
	err = storageengine.Consume(iterator, func(k, v []byte) error {
		name := string(k)
		if strings.HasPrefix(name, sqllayout.DatabasePrefix) {
			_, err := mirror.CreateDatabase(strings.TrimPrefix(name, sqllayout.DatabasePrefix))
			return err
		}
		if strings.HasPrefix(name, sqllayout.TablePrefix) {
			var table versionedTable
			if err := decodeVersioned(v, &table); err != nil {
				return err
			}
			if err := validateSQLKeyEncoding(table); err != nil {
				return err
			}
			tables = append(tables, table)
			names = append(names, name)
		}
		if strings.HasPrefix(name, sqllayout.ViewPrefix) {
			var view versionedView
			if err := decodeVersioned(v, &view); err != nil {
				return err
			}
			views = append(views, view)
			viewNames = append(viewNames, name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	metadata := mirror.SharedSnapshot()
	for i, table := range tables {
		parts := strings.SplitN(names[i], "/", 3)
		definition := table.Definition
		definition.Rows = nil
		for j := range metadata.Databases {
			if strings.EqualFold(metadata.Databases[j].Name, parts[1]) {
				metadata.Databases[j].Tables = append(metadata.Databases[j].Tables, definition)
			}
		}
	}
	// Views are published into the same metadata mirror so SHOW TABLES,
	// SHOW COLUMNS/DESCRIBE and the protocol metadata handlers list them exactly
	// like the legacy engine, which keeps tables and views in one namespace.
	for i, view := range views {
		parts := strings.SplitN(viewNames[i], "/", 3)
		if len(parts) < 3 {
			continue
		}
		for j := range metadata.Databases {
			if strings.EqualFold(metadata.Databases[j].Name, parts[1]) {
				metadata.Databases[j].Views = append(metadata.Databases[j].Views, storage.ViewSnapshot{
					Name:       parts[2],
					Definition: view.Definition,
					Columns:    append([]string(nil), view.Columns...),
				})
			}
		}
	}
	if err = mirror.ReplaceShared(metadata); err != nil {
		return err
	}
	if err = e.Store.ReplaceShared(mirror.SharedSnapshot()); err != nil {
		return err
	}
	e.catalogRevision = head
	return nil
}

type sqlReadTableCache struct {
	key, encoded []byte
	definition   versionedTable
	schema       *storage.Table
}

func loadVersionedTable(tx storageengine.Txn, session *Session, name string) (versionedTable, *storage.Table, []byte, error) {
	return loadVersionedTableInternal(tx, session, name, false)
}
func loadVersionedTableForRead(tx storageengine.Txn, session *Session, name string) (versionedTable, *storage.Table, []byte, error) {
	return loadVersionedTableInternal(tx, session, name, true)
}
func loadVersionedTableInternal(tx storageengine.Txn, session *Session, name string, cacheRead bool) (versionedTable, *storage.Table, []byte, error) {
	db, table, err := versionedName(session, name)
	if err != nil {
		return versionedTable{}, nil, nil, err
	}
	catalogKey := sqllayout.TableKey(db, table)
	value, ok, err := tx.Get(sqllayout.Catalog, catalogKey)
	if err != nil {
		return versionedTable{}, nil, nil, err
	}
	if !ok {
		return versionedTable{}, nil, nil, storage.ErrTableNotFound
	}
	// Always read through the current transaction before using decoded metadata.
	// A cached definition is immutable and never handed to mutation/DDL paths.
	if cacheRead {
		if c := session.tableReadCache; c != nil && bytes.Equal(c.key, catalogKey) && bytes.Equal(c.encoded, value) {
			return c.definition, c.schema, catalogKey, nil
		}
	}
	var definition versionedTable
	if err = decodeVersioned(value, &definition); err != nil {
		return definition, nil, nil, err
	}
	if err = validateSQLKeyEncoding(definition); err != nil {
		return definition, nil, nil, err
	}
	schema, err := storage.NewTransientTable(table, definition.Definition.Columns)
	if err == nil && cacheRead && len(value) <= 4096 && len(definition.Definition.Columns) <= 32 && len(definition.Definition.Indexes) <= 16 {
		session.tableReadCache = &sqlReadTableCache{key: catalogKey, encoded: value, definition: definition, schema: schema}
	}
	return definition, schema, catalogKey, err
}

// statementTxnHook, when non-nil, observes the read/parent transaction and the statement child
// transaction a mutation is about to run with. The A05 regression tests install it to prove the
// parent-read / child-write split for every DML family. It is nil in production.
var statementTxnHook func(statement parser.Statement, read, write storageengine.Txn)

func (e *Engine) executeSQLStatement(session *Session, statement parser.Statement) (resultOut *Result, errOut error) {
	var closeViewOnExit storageengine.ReadView
	var autoRollbackTx storageengine.Txn
	defer func() {
		if resultOut != nil && (resultOut.StreamRows != nil || resultOut.StreamValues != nil) {
			origStreamRows := resultOut.StreamRows
			origStreamValues := resultOut.StreamValues
			v := closeViewOnExit
			closeViewOnExit = nil
			rtx := autoRollbackTx
			autoRollbackTx = nil
			var cleanupOnce sync.Once
			var streamConsumed atomic.Bool
			doCleanup := func() {
				cleanupOnce.Do(func() {
					if v != nil {
						_ = v.Close()
						v = nil
					}
					if rtx != nil {
						session.ClearActiveIsolation()
						_ = rtx.Rollback()
						rtx = nil
					}
				})
			}
			resultOut.cleanup = doCleanup
			if origStreamRows != nil {
				resultOut.StreamRows = func(yield func([]any) error) error {
					if !streamConsumed.CompareAndSwap(false, true) {
						return storageengine.ErrViewClosed
					}
					defer doCleanup()
					return origStreamRows(yield)
				}
			}
			if origStreamValues != nil {
				resultOut.StreamValues = func(yield func(storage.Row) error) error {
					if !streamConsumed.CompareAndSwap(false, true) {
						return storageengine.ErrViewClosed
					}
					defer doCleanup()
					return origStreamValues(yield)
				}
			}
		}
		if closeViewOnExit != nil {
			_ = closeViewOnExit.Close()
			closeViewOnExit = nil
		}
		if autoRollbackTx != nil {
			session.ClearActiveIsolation()
			_ = autoRollbackTx.Rollback()
			autoRollbackTx = nil
		}
		if errOut == nil {
			return
		}
		switch {
		case errors.Is(errOut, storageengine.ErrDeadlock):
			abortDeadlockSessionTransaction(session)
		case isStaleTargetConflict(errOut):
			abortStaleTargetSessionTransaction(session, errOut)
		}
	}()
	// A transaction whose statement found a stale target cannot be used again: its other statements
	// were computed from the snapshot that has just been proven too old, so the whole unit of work has
	// to be retried. ROLLBACK ends it and BEGIN replaces it, exactly as they do for a deadlock victim;
	// every other statement — COMMIT included — is refused with the original conflict, so a caller can
	// never publish the abandoned work by accident.
	if session.retryConflict != nil && !retryEndsTransaction(statement) {
		return nil, session.retryConflict
	}
	ctx := session.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, foreignChecksContextKey{}, session.ForeignKeyChecksDisabled)
	ctx = storageengine.WithInternalReader(ctx)
	if q := session.query; q != nil && !q.deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, q.deadline)
		defer cancel()
	}
	if maintenance, ok := statement.(parser.MVCCMaintenance); ok {
		return e.executeSQLMaintenance(ctx, session, maintenance)
	}
	if e.Replica != nil {
		// Bound quorum discovery, not the entire SQL statement. Large replicated
		// transactions obey the user's query deadline instead of a hidden 30s cap.
		barrierCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := e.Replica.Barrier(barrierCtx)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	switch statement.(type) {
	case parser.CreateUser, parser.AlterUser, parser.DropUser, parser.RenameUser, parser.SetPassword, parser.Grant, parser.Revoke, parser.ShowGrants, parser.ShowCreateUser:
		if e.Replica != nil {
			return nil, fmt.Errorf("replicated account SQL is not supported; provision matching users on stopped nodes")
		}
		return e.executeAccountStatement(session, statement)
	}
	if err := validateSerializableStatementShape(statement, session); err != nil {
		return nil, err
	}
	switch value := statement.(type) {
	case parser.Begin:
		if session.transaction != nil {
			if outermost := outermostTransaction(session); outermost != nil {
				if c, ok := outermost.(interface{ Closed() bool }); ok && c.Closed() {
					rollbackSessionTransaction(session)
				} else {
					return nil, errors.New("transaction already active")
				}
			} else {
				return nil, errors.New("transaction already active")
			}
		}
		targetLevel := session.EffectiveIsolation()
		if err := e.checkIsolationCapability(targetLevel); err != nil {
			return nil, err
		}
		var tx storageengine.Txn
		var err error
		if session.IsolationEnabled() {
			tx, err = e.beginIsolatedTransaction(ctx, targetLevel)
		} else {
			tx, err = e.Backend.Begin(ctx)
		}
		if err != nil {
			return nil, err
		}
		session.FreezeActiveIsolation()
		session.transaction = tx
		return &Result{Message: "MVCC transaction started"}, nil
	case parser.Commit:
		if session.transaction == nil {
			return &Result{Message: "no active transaction"}, nil
		}
		if err := e.commitSessionTransaction(session); err != nil {
			return nil, err
		}
		return &Result{Message: "MVCC transaction committed"}, e.refreshSQLMetadata(ctx)
	case parser.Rollback:
		rollbackSessionTransaction(session)
		return &Result{Message: "MVCC transaction rolled back"}, nil
	case parser.Savepoint:
		return e.createSavepoint(session, value.Name)
	case parser.RollbackTo:
		return e.rollbackToSavepoint(session, value.Name)
	case parser.ReleaseSavepoint:
		return e.releaseSavepoint(session, value.Name)
	case parser.SetTransaction:
		switch value.Scope {
		case parser.ScopeSession:
			if err := session.SetSessionIsolation(value.Level); err != nil {
				return nil, err
			}
			return &Result{Message: "session isolation level changed"}, nil
		case parser.ScopeNextTransaction:
			if err := session.SetNextTransactionIsolation(value.Level); err != nil {
				return nil, err
			}
			return &Result{Message: "transaction isolation level changed"}, nil
		default:
			return nil, fmt.Errorf("unsupported isolation scope %q", value.Scope)
		}
	case parser.Empty:
		return &Result{}, nil
	}
	tx := session.transaction
	if tx == nil && session.AutocommitDisabled && sqlStartsImplicitTransaction(statement) {
		targetLevel := session.EffectiveIsolation()
		if err := e.checkIsolationCapability(targetLevel); err != nil {
			return nil, err
		}
		var err error
		if session.IsolationEnabled() {
			tx, err = e.beginIsolatedTransaction(ctx, targetLevel)
		} else {
			tx, err = e.Backend.Begin(ctx)
		}
		if err != nil {
			return nil, err
		}
		session.FreezeActiveIsolation()
		session.transaction = tx
	}
	automatic := tx == nil
	if automatic {
		targetLevel := session.EffectiveIsolation()
		if err := e.checkIsolationCapability(targetLevel); err != nil {
			return nil, err
		}
		var err error
		if session.IsolationEnabled() {
			tx, err = e.beginIsolatedTransaction(ctx, targetLevel)
		} else {
			tx, err = e.Backend.Begin(ctx)
		}
		if err != nil {
			return nil, err
		}
		session.FreezeActiveIsolation()
	}
	// Results are consumed while the transaction exists. This backend bounds
	// materialized results; sorted sources spill through the existing operators.
	if automatic {
		autoRollbackTx = tx
	}
	if session.IsolationEnabled() {
		if isoTxn, ok := tx.(storageengine.IsolationTxn); ok {
			isoLevel := isoTxn.IsolationLevel()
			inTxn := session.InTransaction() || session.AutocommitDisabled
			if statementNeedsReadView(statement, isoLevel, inTxn) {
				if automatic || isoLevel == storageengine.IsolationReadCommitted || isoLevel == storageengine.IsolationSerializable {
					view, err := isoTxn.BeginStatementView(ctx, storageengine.StatementViewKindStatement)
					if err != nil {
						return nil, err
					}
					closeViewOnExit = view
				} else {
					_, viewErr := isoTxn.BeginConsistentView(ctx)
					if viewErr != nil {
						return nil, viewErr
					}
				}
			}
		}
	}
	// Subqueries run on the statement snapshot transaction: never on the child
	// write transaction, and never in a transaction of their own.
	session.subqueries = &subqueryRunner{engine: e, session: session, ctx: ctx, tx: tx}
	defer func() { session.subqueries = nil }()
	switch value := statement.(type) {
	case parser.Empty:
		return &Result{}, nil
	case parser.Use:
		_, ok, err := tx.Get(sqllayout.Catalog, sqllayout.DatabaseKey(strings.ToLower(value.Database)))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, storage.ErrDatabaseNotFound
		}
		session.CurrentDatabase = strings.ToLower(value.Database)
		return &Result{Message: "database changed"}, nil
	case parser.Select:
		return executePhysicalSelect(ctx, tx, session, value)
	case parser.With:
		if session.EffectiveIsolation() == parser.LevelSerializable && (session.InTransaction() || session.AutocommitDisabled) {
			return nil, fmt.Errorf("%w: common table expressions in SERIALIZABLE", storageengine.ErrUnsupported)
		}
		return e.executeWithSQL(ctx, tx, session, value)
	case parser.WithRecursive:
		if session.EffectiveIsolation() == parser.LevelSerializable && (session.InTransaction() || session.AutocommitDisabled) {
			return nil, fmt.Errorf("%w: recursive common table expressions in SERIALIZABLE", storageengine.ErrUnsupported)
		}
		return e.executeWithRecursiveSQL(ctx, tx, session, value)

	case parser.Union:
		if session.EffectiveIsolation() == parser.LevelSerializable && (session.InTransaction() || session.AutocommitDisabled) {
			return nil, fmt.Errorf("%w: UNION in SERIALIZABLE", storageengine.ErrUnsupported)
		}
		query, err := bindUnionWithSelect(session, value, func(s parser.Select) (*boundQuery, error) { return bindPhysicalSelect(ctx, tx, session, s) })
		if err != nil {
			return nil, err
		}
		return collectBoundQuery(session, query, false)

	case parser.Explain:
		return executeSQLExplain(tx, session, value.Query)
	case parser.ExportDatabase:
		return e.exportDatabaseSQL(ctx, tx, session, value)
	case parser.Show:
		if err := e.refreshSQLMetadata(ctx); err != nil {
			return nil, err
		}
		if result, handled, err := e.showViewSQL(tx, session, value); handled || err != nil {
			return result, err
		}
		return executeShow(e.Store, session, value)
	case parser.ShowEngineLocks:
		return e.executeShowEngineLocks(ctx, session, value)
	}
	child, err := tx.Child()
	if err != nil {
		return nil, err
	}
	defer child.Rollback()
	if statementTxnHook != nil {
		statementTxnHook(statement, tx, child)
	}
	var result *Result
	modify := physical.Modify[parser.Statement, *Result]{Input: physical.Source[parser.Statement](func(_ context.Context, y physical.Yield[parser.Statement]) error { return y(statement) }), Apply: func(ctx context.Context, s parser.Statement) (*Result, error) {
		return e.mutateSQL(ctx, tx, child, session, s)
	}}
	err = modify.Run(ctx, func(r *Result) error { result = r; return nil })
	if err != nil {
		return nil, err
	}
	if _, err = child.Commit(ctx); err != nil {
		// A failed statement commit aborts the whole user transaction, including
		// every savepoint layer and the outermost transaction.
		rollbackSessionTransaction(session)
		return nil, err
	}
	if automatic {
		if _, err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		if err = e.refreshSQLMetadata(ctx); err != nil {
			return nil, err
		}
	}
	// LastInsertID is published only after the statement transaction committed,
	// so a statement that fails or rolls back cannot leave the session pointing at
	// an id that was never written.
	if result != nil && result.LastInsertID != 0 {
		session.LastInsertID = result.LastInsertID
	}
	return result, nil
}

// PrepareCompatibilityRead refreshes the metadata view used by protocol-level
// compatibility queries and checks quorum before serving them.
func (e *Engine) PrepareCompatibilityRead(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if e.Replica != nil {
		if err := e.Replica.Barrier(ctx); err != nil {
			return err
		}
	}
	return e.refreshSQLMetadata(ctx)
}

// VerifiedReplicationStatus preserves the status schema but only advertises a
// candidate Leader after lightweight quorum verification. It never falls back
// to Barrier when an optional verifier is absent.
func (e *Engine) VerifiedReplicationStatus(ctx context.Context) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.Replica == nil {
		return e.ReplicationStatus(), nil
	}
	status := e.Replica.Status()
	if status.State != "Leader" {
		return replicationStatusResult(status), nil
	}
	if status.ID == "" || status.LeaderID != status.ID {
		return nil, storageengine.ErrNotLeader
	}
	verifier, ok := e.Replica.(storageengine.LeaderVerifier)
	if !ok {
		return nil, storageengine.ErrUnsupported
	}
	// MySQL does not propagate the client's context. Bound server-side work by
	// the same 750 ms ceiling as discovery instead of leaving VerifyLeader queued.
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	if err := verifier.VerifyLeader(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current := e.Replica.Status()
	if current.State != "Leader" || current.ID != status.ID || current.LeaderID != current.ID {
		return nil, storageengine.ErrNotLeader
	}
	return replicationStatusResult(current), nil
}
func (e *Engine) ReplicationStatus() *Result {
	if e.Replica == nil {
		return replicationStatusResult(storageengine.ReplicationStatus{State: "Standalone"})
	}
	return replicationStatusResult(e.Replica.Status())
}
func replicationStatusResult(s storageengine.ReplicationStatus) *Result {
	columns := []Column{{Name: "Node_ID", Type: storage.TypeVarchar}, {Name: "State", Type: storage.TypeVarchar}, {Name: "Leader_ID", Type: storage.TypeVarchar}, {Name: "Leader_Raft_Address", Type: storage.TypeVarchar}, {Name: "Applied_Index", Type: storage.TypeBigInt}}
	return &Result{Columns: columns, Rows: [][]any{{s.ID, s.State, s.LeaderID, s.Leader, int64(s.Applied)}}}
}

// Autocommit mode is independent of whether a lazy transaction has started.
func sqlStartsImplicitTransaction(statement parser.Statement) bool {
	switch s := statement.(type) {
	case parser.Empty, parser.Use, parser.Show, parser.Explain, parser.Savepoint, parser.RollbackTo, parser.ReleaseSavepoint, parser.SetTransaction:
		return false
	case parser.Select:
		return s.Table != "" || s.Subquery != nil
	default:
		return true
	}
}

func (e *Engine) checkIsolationCapability(level parser.IsolationLevel) error {
	if level != parser.LevelRepeatableRead && level != parser.LevelReadCommitted && level != parser.LevelSerializable {
		return fmt.Errorf("%w: transaction isolation level %s is not supported yet", storageengine.ErrUnsupported, level)
	}
	return nil
}

func (e *Engine) SetAutocommit(session *Session, enabled bool) error {
	if enabled && session.AutocommitDisabled && session.InTransaction() {
		if _, err := e.Execute(session, "COMMIT"); err != nil {
			return err
		}
	}
	session.AutocommitDisabled = !enabled
	return nil
}

func (e *Engine) beginIsolatedTransaction(ctx context.Context, level parser.IsolationLevel) (storageengine.Txn, error) {
	isoEngine, ok := e.Backend.(storageengine.IsolationEngine)
	if !ok {
		return nil, fmt.Errorf("%w: backend does not support isolation capabilities", storageengine.ErrUnsupported)
	}
	return isoEngine.BeginIsolated(ctx, storageengine.IsolationLevel(level))
}

func isPlainConsistentRead(statement parser.Statement) bool {
	switch s := statement.(type) {
	case parser.Select:
		return !s.Locking
	case parser.With:
		if sel, ok := s.Query.(parser.Select); ok {
			return !sel.Locking
		}
		return true
	case parser.WithRecursive:
		return true
	case parser.Union:
		return true
	default:
		return false
	}
}

func statementNeedsReadView(statement parser.Statement, level storageengine.IsolationLevel, inTxn bool) bool {
	if level == storageengine.IsolationSerializable {
		if inTxn {
			switch statement.(type) {
			case parser.Update, parser.Delete:
				return true
			}
			return false
		}
		return isPlainConsistentRead(statement)
	}
	if level == storageengine.IsolationReadCommitted {
		switch s := statement.(type) {
		case parser.Select:
			return !s.Locking
		case parser.Update:
			return true
		case parser.Delete:
			return true
		case parser.Insert:
			return s.Select != nil
		case parser.With, parser.WithRecursive, parser.Union:
			return true
		default:
			return false
		}
	}
	return isPlainConsistentRead(statement)
}

func validateSerializableStatementShape(statement parser.Statement, session *Session) error {
	if session.EffectiveIsolation() != parser.LevelSerializable || (!session.InTransaction() && !session.AutocommitDisabled) {
		return nil
	}
	switch s := statement.(type) {
	case parser.Update:
		if len(s.Joins) > 0 {
			return fmt.Errorf("%w: UPDATE JOIN in SERIALIZABLE", storageengine.ErrUnsupported)
		}
		if expressionHasSubquery(s.Where) {
			return fmt.Errorf("%w: subqueries in UPDATE in SERIALIZABLE", storageengine.ErrUnsupported)
		}
	case parser.Delete:
		if len(s.Joins) > 0 || len(s.Targets) > 0 {
			return fmt.Errorf("%w: DELETE JOIN in SERIALIZABLE", storageengine.ErrUnsupported)
		}
		if expressionHasSubquery(s.Where) {
			return fmt.Errorf("%w: subqueries in DELETE in SERIALIZABLE", storageengine.ErrUnsupported)
		}
	case parser.Insert:
		if s.Select != nil {
			return fmt.Errorf("%w: INSERT SELECT in SERIALIZABLE", storageengine.ErrUnsupported)
		}
	case parser.CreateTableAs:
		return fmt.Errorf("%w: CREATE TABLE AS SELECT in SERIALIZABLE", storageengine.ErrUnsupported)
	case parser.With, parser.WithRecursive, parser.Union:
		return fmt.Errorf("%w: CTE and UNION in SERIALIZABLE", storageengine.ErrUnsupported)
	}
	return nil
}
