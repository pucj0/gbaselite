package executor

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"gbaselite/catalog"
	"gbaselite/mvcc"
	"gbaselite/parser"
	"gbaselite/replication"
	"gbaselite/storage"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if err == nil && b.Len() > mvcc.MaxValueBytes {
		err = fmt.Errorf("%w: encoded row/schema exceeds %d bytes", ErrQueryResourceLimit, mvcc.MaxValueBytes)
	}
	return b.Bytes(), err
}
func decodeVersioned(value []byte, out any) error {
	return gob.NewDecoder(bytes.NewReader(value)).Decode(out)
}
func openMVCC(directory, user, password string, options OpenOptions) (*Engine, error) {
	for _, marker := range []string{"store.gob", "store.pages", "store.wal", "store.checkpoint", "store.gob.tmp", "store.pages.tmp", "store.wal.tmp", "store.checkpoint.tmp"} {
		if _, err := os.Stat(filepath.Join(directory, "databases", marker)); err == nil {
			return nil, errors.New("MVCC requires a separate data directory; migrate explicitly with SQL")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if options.Replication == nil {
		if _, err := os.Stat(filepath.Join(directory, "replication", "raft.db")); err == nil {
			return nil, errors.New("replicated data cannot be opened as a standalone node")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	database, err := mvcc.OpenWithOptions(filepath.Join(directory, "versioned"), mvcc.Options{WriteSetLimitBytes: options.TransactionWriteBytes, LocalWAL: options.LocalWAL})
	if err != nil {
		return nil, err
	}
	users, err := catalog.OpenUsers(directory, user, password)
	if err != nil {
		database.Close()
		return nil, err
	}
	engine := &Engine{MVCC: database, Store: storage.NewStore(), Users: users, mvccProposer: database}
	engine.persistCond = sync.NewCond(&engine.persistMu)
	engine.QueryOptions = QueryOptions{SortMemoryBytes: 4 << 20, ResultMemoryBytes: 16 << 20, MaxTempBytes: 256 << 20}
	if options.Replication != nil {
		settings := *options.Replication
		settings.Directory = filepath.Join(directory, "replication")
		node, err := replication.Open(database, settings)
		if err != nil {
			database.Close()
			return nil, err
		}
		engine.Replica = node
		engine.mvccProposer = node
	}
	if err := engine.refreshMVCCMetadata(context.Background()); err != nil {
		engine.Close()
		return nil, err
	}
	return engine, nil
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
	if e.MVCC == nil {
		return nil
	}
	e.mvccMetadata.Lock()
	defer e.mvccMetadata.Unlock()
	head, err := e.MVCC.CatalogHead()
	if err != nil {
		return err
	}
	if head == e.mvccMetadataVersion {
		return nil
	}
	mirror := storage.NewStore()
	var tables []versionedTable
	var names []string
	err = e.MVCC.Scan(ctx, head, "catalog", func(k, v []byte) error {
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
	for i, table := range tables {
		parts := strings.SplitN(names[i], "/", 3)
		db, err := mirror.Database(parts[1])
		if err != nil {
			return err
		}
		var primary []string
		var indexes []storage.Index
		for _, index := range table.Definition.Indexes {
			if index.Primary {
				primary = index.Columns
			} else {
				indexes = append(indexes, index)
			}
		}
		created, createErr := db.CreateTableWithIndexes(parts[2], table.Definition.Columns, primary, indexes)
		if err = createErr; err != nil {
			return err
		}
		created.SetNamedConstraints(table.Definition.ForeignKeys, table.Definition.CheckConstraints)
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

func loadVersionedTable(tx *mvcc.Tx, session *Session, name string) (versionedTable, *storage.Table, []byte, error) {
	return loadVersionedTableInternal(tx, session, name, false)
}
func loadVersionedTableForRead(tx *mvcc.Tx, session *Session, name string) (versionedTable, *storage.Table, []byte, error) {
	return loadVersionedTableInternal(tx, session, name, true)
}
func loadVersionedTableInternal(tx *mvcc.Tx, session *Session, name string, cacheRead bool) (versionedTable, *storage.Table, []byte, error) {
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
		err := e.mvccProposer.Barrier(barrierCtx)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	switch statement.(type) {
	case parser.Begin:
		if session.mvccTransaction != nil {
			return nil, errors.New("transaction already active")
		}
		tx, err := e.MVCC.Begin(ctx, e.mvccProposer)
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
		tx, err = e.MVCC.Begin(ctx, e.mvccProposer)
		if err != nil {
			return nil, err
		}
		session.mvccTransaction = tx
	}
	automatic := tx == nil
	if automatic {
		var err error
		tx, err = e.MVCC.Begin(ctx, e.mvccProposer)
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
		return executeMVCCSelect(ctx, tx, session, value)
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
	result, err := e.mutateMVCC(ctx, tx, child, session, statement)
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
	if e.MVCC == nil {
		return nil
	}
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
	if e.MVCC == nil {
		return errors.New("MVCC autocommit requires the MVCC backend")
	}
	if enabled && session.AutocommitDisabled && session.InTransaction() {
		if _, err := e.Execute(session, "COMMIT"); err != nil {
			return err
		}
	}
	session.AutocommitDisabled = !enabled
	return nil
}
