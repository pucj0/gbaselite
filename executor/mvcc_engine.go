package executor

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
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
func (e *Engine) refreshMVCCMetadata(ctx context.Context) error {
	e.mvccMetadata.Lock()
	defer e.mvccMetadata.Unlock()
	head, err := e.Backend.CatalogHead()
	if err != nil {
		return err
	}
	if head == e.mvccMetadataVersion {
		return nil
	}
	mirror := storage.NewStore()
	var tables []versionedTable
	var names []string
	err = e.Backend.Scan(ctx, head, "catalog", func(k, v []byte) error {
		name := string(k)
		if strings.HasPrefix(name, "db/") {
			_, err := mirror.CreateDatabase(strings.TrimPrefix(name, "db/"))
			return err
		}
		if strings.HasPrefix(name, "table/") {
			var table versionedTable
			if err := decodeVersioned(v, &table); err != nil {
				return err
			}
			if err := validateMVCCKeyEncoding(table); err != nil {
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
	e.mvccMetadataVersion = head
	return nil
}

type mvccReadTableCache struct {
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
	catalogKey := []byte("table/" + db + "/" + table)
	value, ok, err := tx.Get("catalog", catalogKey)
	if err != nil {
		return versionedTable{}, nil, nil, err
	}
	if !ok {
		return versionedTable{}, nil, nil, storage.ErrTableNotFound
	}
	// Always read through the current transaction before using decoded metadata.
	// A cached definition is immutable and never handed to mutation/DDL paths.
	if cacheRead {
		if c := session.mvccReadCache; c != nil && bytes.Equal(c.key, catalogKey) && bytes.Equal(c.encoded, value) {
			return c.definition, c.schema, catalogKey, nil
		}
	}
	var definition versionedTable
	if err = decodeVersioned(value, &definition); err != nil {
		return definition, nil, nil, err
	}
	if err = validateMVCCKeyEncoding(definition); err != nil {
		return definition, nil, nil, err
	}
	schema, err := storage.NewTransientTable(table, definition.Definition.Columns)
	if err == nil && cacheRead && len(value) <= 4096 && len(definition.Definition.Columns) <= 32 && len(definition.Definition.Indexes) <= 16 {
		session.mvccReadCache = &mvccReadTableCache{key: catalogKey, encoded: value, definition: definition, schema: schema}
	}
	return definition, schema, catalogKey, err
}
func (e *Engine) executeMVCCStatement(session *Session, statement parser.Statement) (*Result, error) {
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
		return e.executeMVCCMaintenance(ctx, session, maintenance)
	}
	if e.Replica != nil {
		// Bound quorum discovery, not the entire SQL statement. Large replicated
		// transactions obey the user's query deadline instead of a hidden 30s cap.
		barrierCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := e.Backend.Barrier(barrierCtx)
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
	switch statement.(type) {
	case parser.Begin:
		if session.mvccTransaction != nil {
			return nil, errors.New("transaction already active")
		}
		tx, err := e.Backend.Begin(ctx)
		session.mvccTransaction = tx
		return &Result{Message: "MVCC transaction started"}, err
	case parser.Commit:
		if session.mvccTransaction == nil {
			return &Result{Message: "no active transaction"}, nil
		}
		tx := session.mvccTransaction
		session.mvccTransaction = nil
		_, err := tx.Commit(ctx)
		if err != nil {
			return nil, err
		}
		return &Result{Message: "MVCC transaction committed"}, e.refreshMVCCMetadata(ctx)
	case parser.Rollback:
		if session.mvccTransaction != nil {
			session.mvccTransaction.Rollback()
			session.mvccTransaction = nil
		}
		return &Result{Message: "MVCC transaction rolled back"}, nil
	}
	tx := session.mvccTransaction
	if tx == nil && session.AutocommitDisabled && mvccStartsImplicitTransaction(statement) {
		var err error
		tx, err = e.Backend.Begin(ctx)
		if err != nil {
			return nil, err
		}
		session.mvccTransaction = tx
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
	switch value := statement.(type) {
	case parser.Empty:
		return &Result{}, nil
	case parser.Use:
		_, ok, err := tx.Get("catalog", []byte("db/"+strings.ToLower(value.Database)))
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
	case parser.Union:
		return executeUnionWithSelect(session, value, func(query parser.Select) (*Result, error) { return executePhysicalSelect(ctx, tx, session, query) })
	case parser.Explain:
		return executeMVCCExplain(tx, session, value.Query)
	case parser.Show:
		if err := e.refreshMVCCMetadata(ctx); err != nil {
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
		return e.mutateMVCC(ctx, tx, child, session, s)
	}}
	err = modify.Run(ctx, func(r *Result) error { result = r; return nil })
	if err != nil {
		return nil, err
	}
	if _, err = child.Commit(ctx); err != nil {
		if session.mvccTransaction != nil {
			session.mvccTransaction.Rollback()
			session.mvccTransaction = nil
		}
		return nil, err
	}
	if automatic {
		if _, err = tx.Commit(ctx); err != nil {
			return nil, err
		}
		if err = e.refreshMVCCMetadata(ctx); err != nil {
			return nil, err
		}
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
	return e.refreshMVCCMetadata(ctx)
}
func (e *Engine) ReplicationStatus() *Result {
	columns := []Column{{Name: "Node_ID", Type: storage.TypeVarchar}, {Name: "State", Type: storage.TypeVarchar}, {Name: "Leader_ID", Type: storage.TypeVarchar}, {Name: "Leader_Raft_Address", Type: storage.TypeVarchar}, {Name: "Applied_Index", Type: storage.TypeBigInt}}
	if e.Replica == nil {
		return &Result{Columns: columns, Rows: [][]any{{"", "Standalone", "", "", int64(0)}}}
	}
	s := e.Replica.Status()
	return &Result{Columns: columns, Rows: [][]any{{s.ID, s.State, s.LeaderID, s.Leader, int64(s.Applied)}}}
}

// Autocommit mode is independent of whether a lazy transaction has started.
func mvccStartsImplicitTransaction(statement parser.Statement) bool {
	switch s := statement.(type) {
	case parser.Empty, parser.Use, parser.Show, parser.Explain:
		return false
	case parser.Select:
		return s.Table != "" || s.Subquery != nil
	default:
		return true
	}
}
func (e *Engine) SetMVCCAutocommit(session *Session, enabled bool) error {
	if enabled && session.AutocommitDisabled && session.InTransaction() {
		if _, err := e.Execute(session, "COMMIT"); err != nil {
			return err
		}
	}
	session.AutocommitDisabled = !enabled
	return nil
}
