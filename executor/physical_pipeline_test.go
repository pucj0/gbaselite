package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/storageengine"
	"gbaselite/storageengine/testkit"
	"os"
	"strings"
	"testing"
)

// Identical SQL operators execute against the production adapter and a backend
// with independent transaction/iterator implementations and borrowed buffers.
func TestPhysicalPipelineAcrossBackends(t *testing.T) {
	for _, backend := range []string{"mvcc", "memory"} {
		t.Run(backend, func(t *testing.T) {
			options := OpenOptions{}
			if backend == "memory" {
				options.BackendFactory = func(string, storageengine.Options) (storageengine.Engine, error) {
					return testkit.NewMemory(), nil
				}
			}
			e, err := OpenWithOptions(t.TempDir(), "root", "test-only", options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { e.Close() })
			s := &Session{}
			run := func(sql string) *Result {
				t.Helper()
				r, err := e.Execute(s, sql)
				if err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
				return r
			}
			for _, sql := range []string{"CREATE DATABASE pipe", "USE pipe", "CREATE TABLE a(id INT PRIMARY KEY,g INT,v INT,KEY gi(g))", "CREATE TABLE b(id INT PRIMARY KEY,label VARCHAR(8))", "INSERT INTO a VALUES(1,1,30),(2,1,10),(3,2,20),(4,3,NULL)", "INSERT INTO b VALUES(1,'one'),(2,'two')"} {
				run(sql)
			}
			cases := []struct{ sql, want string }{
				{"SELECT id,v+1 AS n FROM a WHERE v>=20 ORDER BY n DESC LIMIT 1 OFFSET 1", "[[3 21]]"},
				{"SELECT a.id,b.label FROM a LEFT JOIN b ON a.g=b.id ORDER BY a.id", "[[1 one] [2 one] [3 two] [4 <nil>]]"},
				{"SELECT b.label,COUNT(*),SUM(a.v) FROM a INNER JOIN b ON a.g=b.id GROUP BY b.label HAVING COUNT(*)>0 ORDER BY SUM(a.v) DESC LIMIT 1", "[[one 2 40]]"},
				{"SELECT COUNT(*),SUM(v),AVG(v) FROM a WHERE id<0", "[[0 <nil> <nil>]]"},
				{"SELECT id,ROW_NUMBER() OVER(PARTITION BY g ORDER BY v) AS rn FROM a ORDER BY id", "[[1 2] [2 1] [3 1] [4 1]]"},
				{"SELECT id,SUM(v) OVER(PARTITION BY g ORDER BY v) AS total FROM a ORDER BY id", "[[1 40] [2 10] [3 20] [4 <nil>]]"},
				{"SELECT id FROM a WHERE id<=2 UNION ALL SELECT id FROM a WHERE id=1 ORDER BY id DESC LIMIT 2", "[[2] [1]]"},
				{"SELECT g FROM a UNION SELECT id FROM b ORDER BY g", "[[1] [2] [3]]"},
				{"SELECT DISTINCT g FROM a ORDER BY g DESC LIMIT 2 OFFSET 1", "[[2] [1]]"},
				{"SELECT v FROM a WHERE id=4 UNION SELECT v FROM a WHERE id=4", "[[<nil>]]"},
				{"SELECT id FROM a WHERE id=1 UNION SELECT id FROM a WHERE id=2 UNION ALL SELECT id FROM a WHERE id=1", "[[1] [2] [1]]"},
				{"SELECT DISTINCT RANK() OVER(ORDER BY g) AS r FROM a ORDER BY r", "[[1] [3] [4]]"},
			}
			for _, c := range cases {
				if got := fmt.Sprint(run(c.sql).Rows); got != c.want {
					t.Errorf("%s: got %s want %s", c.sql, got, c.want)
				}
			}
			run("BEGIN")
			run("UPDATE a SET v=v+1 WHERE id=1")
			if _, err := e.Execute(s, "UPDATE a SET id=9 WHERE id<=2"); err == nil {
				t.Fatal("duplicate UPDATE succeeded")
			}
			if got := fmt.Sprint(run("SELECT id,v FROM a WHERE id<=2 ORDER BY id").Rows); got != "[[1 31] [2 10]]" {
				t.Fatal("statement rollback", got)
			}
			run("ROLLBACK")
			if got := run("DELETE FROM a WHERE g=1 LIMIT 1").AffectedRows; got != 1 {
				t.Fatal("delete limit", got)
			}
			if got := run("UPDATE a SET v=99 LIMIT 0").AffectedRows; got != 0 {
				t.Fatal("zero limit", got)
			}

			// Force Sort and Distinct to spill, then verify resource failures and cleanup.
			run("CREATE TABLE wide(id INT PRIMARY KEY,label VARCHAR(1000))")
			var values []string
			for i := 0; i < 400; i++ {
				values = append(values, fmt.Sprintf("(%d,'%s-%03d')", i, strings.Repeat("x", 600), i%20))
			}
			run("INSERT INTO wide VALUES" + strings.Join(values, ","))
			temp := t.TempDir()
			saved := e.QueryOptions
			e.QueryOptions.SortMemoryBytes = 128 << 10
			e.QueryOptions.TempDirectory = temp
			if got := run("SELECT DISTINCT label FROM wide"); len(got.Rows) != 20 {
				t.Fatal("distinct spill", len(got.Rows))
			}
			if got := run("SELECT id FROM wide ORDER BY label DESC,id LIMIT 3"); len(got.Rows) != 3 {
				t.Fatal("sort spill")
			}
			e.QueryOptions.MaxTempBytes = 1
			if _, err := e.Execute(s, "SELECT DISTINCT label FROM wide"); !errors.Is(err, ErrQueryResourceLimit) {
				t.Fatal("distinct disk budget", err)
			}
			if entries, err := os.ReadDir(temp); err != nil || len(entries) != 0 {
				t.Fatal("temporary files leaked", entries, err)
			}
			e.QueryOptions = saved
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			s.Context = canceled
			if _, err := e.Execute(s, "SELECT * FROM a"); !errors.Is(err, ErrQueryCanceled) {
				t.Fatal("cancel", err)
			}
			s.Context = nil
			// Deterministically cancel during iteration, after one row has been fetched.
			original := e.Backend
			probe := &cancelBackend{Engine: original}
			e.Backend = probe
			active, cancelActive := context.WithCancel(context.Background())
			s.Context = active
			probe.cancel = cancelActive
			if _, err := e.Execute(s, "SELECT * FROM a"); !errors.Is(err, context.Canceled) && !errors.Is(err, ErrQueryCanceled) {
				t.Fatal("mid-scan cancellation", err)
			}
			if probe.open != 0 {
				t.Fatal("canceled scan leaked iterator", probe.open)
			}
			s.Context = nil
			e.Backend = original
			// Materialization rejects input under its budget, without mutating the table.
			e.QueryOptions.ResultMemoryBytes = 256
			if _, err := e.Execute(s, "SELECT ROW_NUMBER() OVER(ORDER BY id) FROM a"); err == nil {
				t.Fatal("window ignored memory budget")
			}
		})
	}
}

// The probe wraps only the public contract; it works unchanged on both backends.
type cancelBackend struct {
	storageengine.Engine
	cancel context.CancelFunc
	open   int
}

func (e *cancelBackend) Begin(ctx context.Context) (storageengine.Txn, error) {
	tx, err := e.Engine.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &cancelTxn{tx, e}, nil
}

type cancelTxn struct {
	storageengine.Txn
	engine *cancelBackend
}

func (t *cancelTxn) Table(id string) storageengine.Table { return storageengine.BindTable(t, id) }
func (t *cancelTxn) NewIterator(ctx context.Context, r storageengine.ScanRequest) (storageengine.Iterator, error) {
	it, err := t.Txn.NewIterator(ctx, r)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(r.Space, "row/") {
		t.engine.open++
		return &cancelIterator{Iterator: it, engine: t.engine}, nil
	}
	return it, nil
}

type cancelIterator struct {
	storageengine.Iterator
	engine *cancelBackend
	closed bool
}

func (i *cancelIterator) Next() bool {
	ok := i.Iterator.Next()
	if ok && i.engine.cancel != nil {
		cancel := i.engine.cancel
		i.engine.cancel = nil
		cancel()
	}
	return ok
}
func (i *cancelIterator) Close() error {
	if !i.closed {
		i.closed = true
		i.engine.open--
	}
	return i.Iterator.Close()
}
