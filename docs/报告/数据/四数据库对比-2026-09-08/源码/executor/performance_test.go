package executor

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"gbaselite/parser"
	"gbaselite/storage"
)

func TestConcurrentWritesRemainDurable(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE concurrent_durability",
		"USE concurrent_durability",
		"CREATE TABLE records(id INT NOT NULL,value VARCHAR(16),PRIMARY KEY(id))",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			t.Fatal(err)
		}
	}
	const writers = 64
	for id := 1; id <= writers; id++ {
		if _, err := engine.Execute(session, fmt.Sprintf("INSERT INTO records VALUES(%d,'new')", id)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errorsFound := make(chan error, writers)
	for id := 1; id <= writers; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := engine.Execute(&Session{CurrentDatabase: "concurrent_durability"}, fmt.Sprintf("UPDATE records SET value='done' WHERE id=%d", id))
			if err != nil {
				errorsFound <- err
			}
		}(id)
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	database, err := reopened.Store.Database("concurrent_durability")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.Table("records")
	if err != nil {
		t.Fatal(err)
	}
	if count := table.Count(func(row storage.Row) bool { return row[1].Text == "done" }); count != writers {
		t.Fatalf("durable rows = %d, want %d", count, writers)
	}
}

func TestParsedStatementCacheIsConcurrentAndBounded(t *testing.T) {
	engine, err := Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	const workers = 32
	var wg sync.WaitGroup
	errorsFound := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 100; iteration++ {
				if _, err := engine.Execute(&Session{}, "SHOW DATABASES"); err != nil {
					errorsFound <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}

	for index := 0; index < maxParsedStatements*3; index++ {
		statement, err := parser.Parse(fmt.Sprintf("SELECT %d", index))
		if err != nil {
			t.Fatal(err)
		}
		engine.cacheStatement(fmt.Sprintf("SELECT %d", index), statement)
	}
	count := 0
	engine.parseCache.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count > maxParsedStatements || count != engine.parseCount {
		t.Fatalf("parse cache contains %d entries, counter is %d, limit is %d", count, engine.parseCount, maxParsedStatements)
	}
}

func BenchmarkPersistentUpdate(b *testing.B) {
	engine := benchmarkWriteEngine(b)
	session := &Session{CurrentDatabase: "benchmark_writes"}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := engine.Execute(session, "UPDATE records SET value='b' WHERE id=1"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConcurrentPersistentUpdate(b *testing.B) {
	engine := benchmarkWriteEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		session := &Session{CurrentDatabase: "benchmark_writes"}
		for pb.Next() {
			if _, err := engine.Execute(session, "UPDATE records SET value='b' WHERE id=1"); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func benchmarkWriteEngine(b *testing.B) *Engine {
	b.Helper()
	engine, err := Open(b.TempDir(), "root", "123456")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Close() })
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE benchmark_writes",
		"USE benchmark_writes",
		"CREATE TABLE records(id INT NOT NULL,value VARCHAR(32),PRIMARY KEY(id))",
		"INSERT INTO records VALUES(1,'a')",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			b.Fatal(err)
		}
	}
	return engine
}

func BenchmarkStreamTenThousandRows(b *testing.B) {
	table, err := storage.NewTable("records", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "value", Type: storage.TypeVarchar, Length: 32}})
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10_000; index++ {
		if err := table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeVarchar, fmt.Sprintf("value-%d", index)))); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		count := 0
		if err := table.Stream(nil, 0, -1, func(storage.Row) error {
			count++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if count != 10_000 {
			b.Fatalf("streamed %d rows", count)
		}
	}
}

func BenchmarkPrimaryKeyLookupTenThousandRows(b *testing.B) {
	engine, err := Open(b.TempDir(), "root", "123456")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Close() })
	database, err := engine.Store.CreateDatabase("benchmark_lookup")
	if err != nil {
		b.Fatal(err)
	}
	table, err := database.CreateTable("records", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "value", Type: storage.TypeVarchar, Length: 32}})
	if err != nil {
		b.Fatal(err)
	}
	if err := table.AddPrimaryKey([]string{"id"}); err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10_000; index++ {
		if err := table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeVarchar, "value"))); err != nil {
			b.Fatal(err)
		}
	}
	session := &Session{CurrentDatabase: "benchmark_lookup", StreamResults: true}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		result, err := engine.Execute(session, "SELECT * FROM records WHERE id=9999")
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		if err := result.StreamValues(func(storage.Row) error { count++; return nil }); err != nil {
			b.Fatal(err)
		}
		if count != 1 {
			b.Fatalf("found %d rows", count)
		}
	}
}

func BenchmarkRangeQueryTenThousandRows(b *testing.B) {
	engine, err := Open(b.TempDir(), "root", "123456")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Close() })
	database, err := engine.Store.CreateDatabase("benchmark_range")
	if err != nil {
		b.Fatal(err)
	}
	table, err := database.CreateTable("records", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "value", Type: storage.TypeVarchar, Length: 32}})
	if err != nil {
		b.Fatal(err)
	}
	if err := table.AddPrimaryKey([]string{"id"}); err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10_000; index++ {
		if err := table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeVarchar, "value"))); err != nil {
			b.Fatal(err)
		}
	}
	session := &Session{CurrentDatabase: "benchmark_range", StreamResults: true}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		result, err := engine.Execute(session, "SELECT id,value FROM records WHERE id >= 4000 AND id < 5000")
		if err != nil {
			b.Fatal(err)
		}
		count := consumeBenchmarkRows(b, result)
		if count != 1_000 {
			b.Fatalf("range returned %d rows", count)
		}
	}
}

func BenchmarkBulkInsertHundredRows(b *testing.B) {
	engine, err := Open(b.TempDir(), "root", "123456")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Close() })
	session := &Session{}
	for _, query := range []string{
		"CREATE DATABASE benchmark_bulk_insert",
		"USE benchmark_bulk_insert",
		"CREATE TABLE records(id INT NOT NULL,value VARCHAR(32),PRIMARY KEY(id))",
	} {
		if _, err := engine.Execute(session, query); err != nil {
			b.Fatal(err)
		}
	}
	const rowsPerBatch = 100
	b.ReportAllocs()
	b.ReportMetric(rowsPerBatch, "rows/op")
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		var query strings.Builder
		query.Grow(2_500)
		query.WriteString("INSERT INTO records VALUES ")
		for row := 0; row < rowsPerBatch; row++ {
			if row > 0 {
				query.WriteByte(',')
			}
			fmt.Fprintf(&query, "(%d,'value-%d')", iteration*rowsPerBatch+row, row)
		}
		result, err := engine.Execute(session, query.String())
		if err != nil {
			b.Fatal(err)
		}
		if result.AffectedRows != rowsPerBatch {
			b.Fatalf("inserted %d rows", result.AffectedRows)
		}
	}
}

func BenchmarkJoinOneThousandRows(b *testing.B) {
	engine, err := Open(b.TempDir(), "root", "123456")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Close() })
	database, err := engine.Store.CreateDatabase("benchmark_join")
	if err != nil {
		b.Fatal(err)
	}
	accounts, err := database.CreateTable("accounts", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "name", Type: storage.TypeVarchar, Length: 32}})
	if err != nil {
		b.Fatal(err)
	}
	orders, err := database.CreateTable("orders", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "account_id", Type: storage.TypeInt}, {Name: "amount", Type: storage.TypeInt}})
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 1_000; index++ {
		if err := accounts.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeVarchar, "account"))); err != nil {
			b.Fatal(err)
		}
		if err := orders.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeInt, index*10))); err != nil {
			b.Fatal(err)
		}
	}
	session := &Session{CurrentDatabase: "benchmark_join", StreamResults: true}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		result, err := engine.Execute(session, "SELECT a.id,o.amount FROM accounts a INNER JOIN orders o ON a.id=o.account_id WHERE a.id >= 900")
		if err != nil {
			b.Fatal(err)
		}
		count := consumeBenchmarkRows(b, result)
		if count != 100 {
			b.Fatalf("join returned %d rows", count)
		}
	}
}

func consumeBenchmarkRows(b *testing.B, result *Result) int {
	b.Helper()
	count := 0
	switch {
	case result.StreamValues != nil:
		if err := result.StreamValues(func(storage.Row) error { count++; return nil }); err != nil {
			b.Fatal(err)
		}
	case result.StreamRows != nil:
		if err := result.StreamRows(func([]any) error { count++; return nil }); err != nil {
			b.Fatal(err)
		}
	default:
		count = len(result.Rows)
	}
	return count
}

func BenchmarkBeginRollbackTenThousandRows(b *testing.B) {
	engine, err := Open(b.TempDir(), "root", "123456")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Close() })
	database, err := engine.Store.CreateDatabase("benchmark_tx")
	if err != nil {
		b.Fatal(err)
	}
	table, err := database.CreateTable("records", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "value", Type: storage.TypeVarchar, Length: 32}})
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10_000; index++ {
		if err := table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeVarchar, "value"))); err != nil {
			b.Fatal(err)
		}
	}
	session := &Session{CurrentDatabase: "benchmark_tx"}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := engine.Execute(session, "BEGIN"); err != nil {
			b.Fatal(err)
		}
		if _, err := engine.Execute(session, "ROLLBACK"); err != nil {
			b.Fatal(err)
		}
	}
}
