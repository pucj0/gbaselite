package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gbaselite/storage"
)

func queryResourceSQLFixture(t *testing.T) (*Engine, *Session, string) {
	t.Helper()
	engine, err := Open(t.TempDir(), "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	db, err := engine.Store.CreateDatabase("budget")
	if err != nil {
		t.Fatal(err)
	}
	table, err := db.CreateTable("items", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "score", Type: storage.TypeInt}, {Name: "label", Type: storage.TypeVarchar, Length: 128}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		label := fmt.Sprintf("Name-%02d-%s", i%11, strings.Repeat("x", 48))
		if i%2 == 0 {
			label = strings.ToLower(label)
		}
		if err = table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, i), storage.MustValue(storage.TypeInt, i%19), storage.MustValue(storage.TypeVarchar, label))); err != nil {
			t.Fatal(err)
		}
	}
	return engine, &Session{CurrentDatabase: "budget"}, t.TempDir()
}

func TestQueryResourcesSQLSortAndDistinctMatchLegacy(t *testing.T) {
	engine, session, directory := queryResourceSQLFixture(t)
	queries := []string{
		"SELECT id,label FROM items ORDER BY score DESC,id ASC LIMIT 15 OFFSET 4",
		"SELECT id+1 AS next_id,JSON_OBJECT('label',label) AS doc FROM items ORDER BY score DESC,id ASC LIMIT 25 OFFSET 3",
		"SELECT DISTINCT label FROM items ORDER BY label LIMIT 9 OFFSET 1",
		"SELECT DISTINCT JSON_OBJECT('n',score) AS doc FROM items ORDER BY doc",
	}
	for _, sql := range queries {
		t.Run(sql, func(t *testing.T) {
			engine.QueryOptions = QueryOptions{}
			expected, err := engine.Execute(session, sql)
			if err != nil {
				t.Fatal(err)
			}
			engine.QueryOptions = QueryOptions{SortMemoryBytes: 128 << 10, ResultMemoryBytes: 4 << 20, MaxTempBytes: 32 << 20, TempDirectory: directory}
			actual, err := engine.Execute(session, sql)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(expected.Rows, actual.Rows) {
				t.Fatalf("changed sorted/distinct rows: expected %#v got %#v", expected.Rows, actual.Rows)
			}
			files, _ := os.ReadDir(directory)
			if len(files) != 0 {
				t.Fatalf("temporary files leaked: %v", files)
			}
		})
	}
}

func TestQueryResourcesSQLBudgetFailuresCleanUp(t *testing.T) {
	engine, session, directory := queryResourceSQLFixture(t)
	for _, sql := range []string{
		"SELECT id,label FROM items",
		"SELECT score,COUNT(*) FROM items GROUP BY score",
		"SELECT id,ROW_NUMBER() OVER (ORDER BY score) AS n FROM items",
	} {
		t.Run(sql, func(t *testing.T) {
			engine.QueryOptions = QueryOptions{ResultMemoryBytes: 512, TempDirectory: directory}
			if _, err := engine.Execute(session, sql); !errors.Is(err, ErrQueryResourceLimit) {
				t.Fatalf("expected resource limit, got %v", err)
			}
		})
	}
	engine.QueryOptions = QueryOptions{SortMemoryBytes: 128 << 10, MaxTempBytes: 512, TempDirectory: directory}
	if _, err := engine.Execute(session, "SELECT id,label FROM items ORDER BY score"); !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("expected disk budget, got %v", err)
	}
	files, _ := os.ReadDir(directory)
	if len(files) != 0 {
		t.Fatalf("temporary files leaked: %v", files)
	}
	engine.QueryOptions = QueryOptions{}
	if _, err := engine.Execute(session, "SELECT COUNT(*) FROM items"); err != nil {
		t.Fatalf("query error poisoned session: %v", err)
	}
}

func TestQueryResourcesSQLDeferredStreamCancellationCleansRuns(t *testing.T) {
	engine, session, directory := queryResourceSQLFixture(t)
	engine.QueryOptions = QueryOptions{SortMemoryBytes: 128 << 10, MaxTempBytes: 32 << 20, TempDirectory: directory}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session.Context = ctx
	session.StreamResults = true
	result, err := engine.Execute(session, "SELECT id,label FROM items ORDER BY score")
	if err != nil {
		t.Fatal(err)
	}
	if result.StreamRows == nil {
		t.Fatal("missing deferred stream")
	}
	files, _ := os.ReadDir(directory)
	if len(files) != 0 {
		t.Fatal("unconsumed result created temporary files")
	}
	rows := 0
	err = result.StreamRows(func([]any) error {
		rows++
		if rows == 10 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, ErrQueryCanceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if rows != 10 {
		t.Fatalf("emitted %d rows after cancel", rows)
	}
	files, _ = os.ReadDir(directory)
	if len(files) != 0 {
		t.Fatalf("temporary files leaked: %v", files)
	}
}

func TestQueryResourcesSQLTransactionLockTimeout(t *testing.T) {
	engine, session, _ := queryResourceSQLFixture(t)
	holder := &Session{CurrentDatabase: "budget"}
	if _, err := engine.Execute(holder, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	defer engine.CloseSession(holder)
	engine.QueryOptions = QueryOptions{Timeout: 15 * time.Millisecond}
	start := time.Now()
	if _, err := engine.Execute(session, "SELECT id FROM items LIMIT 1"); !errors.Is(err, ErrQueryTimeout) {
		t.Fatalf("expected lock timeout, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("lock cancellation was not prompt")
	}
	if _, err := engine.Execute(holder, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	engine.QueryOptions = QueryOptions{}
	if _, err := engine.Execute(session, "SELECT id FROM items LIMIT 1"); err != nil {
		t.Fatalf("timed-out request retained lock: %v", err)
	}
}
