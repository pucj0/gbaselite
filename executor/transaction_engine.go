package executor

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
	"time"
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
func (e *Engine) executeSQLStatement(session *Session, statement parser.Statement) (*Result, error) {
	ctx := session.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, foreignChecksContextKey{}, session.ForeignKeyChecksDisabled)
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
	switch value := statement.(type) {
	case parser.Begin:
		if session.transaction != nil {
			return nil, errors.New("transaction already active")
		}
		tx, err := e.Backend.Begin(ctx)
		session.transaction = tx
		return &Result{Message: "MVCC transaction started"}, err
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
	}
	tx := session.transaction
	if tx == nil && session.AutocommitDisabled && sqlStartsImplicitTransaction(statement) {
		var err error
		tx, err = e.Backend.Begin(ctx)
		if err != nil {
			return nil, err
		}
		session.transaction = tx
	}
	automatic := tx == nil
	if automatic {
		var err error
		tx, err = e.Backend.Begin(ctx)
		if err != nil {
			return nil, err
		}
	}
	// Results are consumed while the transaction exists. This backend bounds
	// materialized results; sorted sources spill through the existing operators.
	if automatic {
		defer tx.Rollback()
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
		return e.executeWithSQL(ctx, tx, session, value)
	case parser.WithRecursive:
		return e.executeWithRecursiveSQL(ctx, tx, session, value)

	case parser.Union:
		query, err := bindUnionWithSelect(session, value, func(s parser.Select) (*boundQuery, error) { return bindPhysicalSelect(ctx, tx, session, s) })
		if err != nil {
			return nil, err
		}
		return collectBoundQuery(session, query, false)

	case parser.Explain:
		return executeSQLExplain(tx, session, value.Query)
	case parser.Show:
		if err := e.refreshSQLMetadata(ctx); err != nil {
			return nil, err
		}
		return executeShow(e.Store, session, value)
	}
	child, err := tx.Child()
	if err != nil {
		return nil, err
	}
	defer child.Rollback()
	var result *Result
	modify := physical.Modify[parser.Statement, *Result]{Input: physical.Source[parser.Statement](func(_ context.Context, y physical.Yield[parser.Statement]) error { return y(statement) }), Apply: func(ctx context.Context, s parser.Statement) (*Result, error) {
		return e.mutateSQL(ctx, tx, child, session, s)
	}}
	err = modify.Run(ctx, func(r *Result) error { result = r; return nil })
	if err != nil {
		return nil, err
	}
	if _, err = child.Commit(ctx); err != nil {
		if session.transaction != nil {
			session.transaction.Rollback()
			session.transaction = nil
		}
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
	case parser.Empty, parser.Use, parser.Show, parser.Explain, parser.Savepoint, parser.RollbackTo, parser.ReleaseSavepoint:
		return false
	case parser.Select:
		return s.Table != "" || s.Subquery != nil
	default:
		return true
	}
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
