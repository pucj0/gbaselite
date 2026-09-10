package executor

import (
	"context"
	"errors"
	"gbaselite/storageengine"
	"gbaselite/storageengine/testkit"
	"testing"
)

type coreOnlyEngine struct{ inner storageengine.Engine }

func (e *coreOnlyEngine) Begin(ctx context.Context) (storageengine.Txn, error) {
	return e.inner.Begin(ctx)
}
func (e *coreOnlyEngine) Close() error { return e.inner.Close() }
func TestCoreOnlyBackendRunsSQL(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "test", OpenOptions{BackendFactory: func(string, storageengine.Options) (storageengine.Engine, error) {
		return &coreOnlyEngine{testkit.NewMemory()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &Session{}
	for _, sql := range []string{"CREATE DATABASE core", "USE core", "CREATE TABLE t(id INT PRIMARY KEY,v INT)", "INSERT INTO t VALUES(1,5)", "UPDATE t SET v=6 WHERE id=1", "SELECT DISTINCT v FROM t", "BEGIN", "DELETE FROM t", "ROLLBACK"} {
		if _, err := e.Execute(s, sql); err != nil {
			t.Fatal(sql, err)
		}
	}
	if _, err := e.Execute(s, "CREATE TABLE generated(id BIGINT AUTO_INCREMENT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(s, "INSERT INTO generated VALUES(NULL)"); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatal("missing counter", err)
	}
	if _, err := e.Execute(s, "BACKUP MVCC TO 'never-created'"); !errors.Is(err, storageengine.ErrUnsupported) {
		t.Fatal("missing maintenance", err)
	}
}
