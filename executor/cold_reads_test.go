package executor

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"gbaselite/storage"
)

func coldTestEngine(t *testing.T, materializeBytes int64) (*legacyEngine, *Session, *storage.Table) {
	t.Helper()
	directory := t.TempDir()
	warm, err := openLegacyWithOptions(directory, "root", "secret", OpenOptions{StorageMode: "paged"})
	if err != nil {
		t.Fatal(err)
	}
	database, err := warm.Store.CreateDatabase("cold")
	if err != nil {
		t.Fatal(err)
	}
	table, err := database.CreateTableWithIndexes("items", []storage.Column{{Name: "id", Type: storage.TypeInt}, {Name: "payload", Type: storage.TypeText}}, []string{"id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 1024; index++ {
		if err := table.Insert(storage.NewRow(storage.MustValue(storage.TypeInt, index), storage.MustValue(storage.TypeText, strings.Repeat("x", 1024)))); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.CreateView("items_view", "SELECT * FROM items", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := warm.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err := openLegacyWithOptions(directory, "root", "secret", OpenOptions{StorageMode: "paged", PageCacheBytes: 4 << 10, ColdRead: true, ColdMaterializeBytes: materializeBytes})
	if err != nil {
		t.Fatal(err)
	}
	database, _ = engine.Store.Database("cold")
	table, _ = database.Table("items")
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	return engine, &Session{CurrentDatabase: "cold"}, table
}

func TestLegacyColdSQLStreamsAndCountsWithoutMaterialization(t *testing.T) {
	engine, session, table := coldTestEngine(t, 1024)
	session.StreamResults = true
	result, err := engine.Execute(session, "SELECT i.id, LENGTH(i.payload) AS bytes FROM items i WHERE i.id >= 1000 LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	if result.StreamRows == nil {
		t.Fatal("query was not streamed")
	}
	var rows [][]any
	if err := result.StreamRows(func(row []any) error { rows = append(rows, row); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, [][]any{{int64(1000), int64(1024)}, {int64(1001), int64(1024)}, {int64(1002), int64(1024)}}) {
		t.Fatalf("rows=%#v", rows)
	}
	result, err = engine.Execute(session, "SELECT COUNT(*) FROM items WHERE id >= 1000")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(24) {
		t.Fatal(result.Rows)
	}
	if !table.IsCold() {
		t.Fatal("cold SELECT materialized full table")
	}
	if stats := engine.Persistence.PagedStats(); stats.CacheBytes > 4096 {
		t.Fatal(stats)
	}
	if _, err := engine.Execute(session, "SELECT SUM(id) FROM items"); !errors.Is(err, storage.ErrColdMaterializationLimit) {
		t.Fatalf("complex query bypassed budget: %v", err)
	}
	if _, err := engine.Execute(session, "UPDATE items SET payload='changed' WHERE id=1"); !errors.Is(err, storage.ErrColdMaterializationLimit) {
		t.Fatalf("write bypassed budget: %v", err)
	}
}

func TestLegacyColdSQLExplicitFallbackPreservesRowsAndTransactions(t *testing.T) {
	engine, session, table := coldTestEngine(t, 32<<20)
	if _, err := engine.Execute(session, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if table.IsCold() {
		t.Fatal("BEGIN did not prepare legacy snapshot APIs")
	}
	if _, err := engine.Execute(session, "UPDATE items SET payload='changed' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(session, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Execute(session, "SELECT LENGTH(payload) FROM items WHERE id=1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(1024) {
		t.Fatal("rollback lost cold data")
	}
}

func TestLegacyColdSQLIndexPointAndOrderedRangeStayCold(t *testing.T) {
	engine, session, table := coldTestEngine(t, 1024)
	before := engine.Persistence.PagedStats()
	result, err := engine.Execute(session, "SELECT id, LENGTH(payload) FROM items WHERE id=999")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Rows, [][]any{{int64(999), int64(1024)}}) {
		t.Fatal(result.Rows)
	}
	if read := engine.Persistence.PagedStats().CacheMisses - before.CacheMisses; read != 1 {
		t.Fatalf("point query read %d row pages", read)
	}
	result, err = engine.Execute(session, "SELECT id FROM items WHERE id >= 1000 ORDER BY id DESC LIMIT 3 OFFSET 1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Rows, [][]any{{int64(1022)}, {int64(1021)}, {int64(1020)}}) {
		t.Fatal(result.Rows)
	}
	if !table.IsCold() {
		t.Fatal("indexed query materialized table")
	}
}

func TestLegacyColdShowViewCannotBypassMaterializationBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		engine, session, table := coldTestEngine(t, 1024)
		session.StreamResults = stream
		if _, err := engine.Execute(session, "SHOW COLUMNS FROM items_view"); !errors.Is(err, storage.ErrColdMaterializationLimit) {
			t.Fatalf("stream=%v SHOW view budget=%v", stream, err)
		}
		if _, err := engine.Execute(session, "SHOW COLUMNS FROM items WHERE Field IN (SELECT payload FROM items)"); !errors.Is(err, storage.ErrColdMaterializationLimit) {
			t.Fatalf("stream=%v SHOW subquery budget=%v", stream, err)
		}
		result, err := engine.Execute(session, "SHOW COLUMNS FROM items")
		if err != nil || len(result.Rows) != 2 {
			t.Fatalf("base metadata=%v %v", result, err)
		}
		if _, err := engine.Execute(session, "SELECT @@version"); err != nil {
			t.Fatalf("system variable forced materialization: %v", err)
		}
		if !table.IsCold() {
			t.Fatal("metadata operation materialized table")
		}
	}
}

func TestLegacyColdExternalSortAndDistinctStayWithinConfiguredExecutionPath(t *testing.T) {
	engine, session, table := coldTestEngine(t, 1024)
	temporary := t.TempDir()
	engine.QueryOptions = QueryOptions{SortMemoryBytes: 256 << 10, ResultMemoryBytes: 64 << 10, MaxTempBytes: 16 << 20, TempDirectory: temporary}
	result, err := engine.Execute(session, "SELECT id, payload FROM items ORDER BY LENGTH(payload), id DESC LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 3 || result.Rows[0][0] != int64(1023) || result.Rows[2][0] != int64(1021) {
		t.Fatalf("cold external order=%v", result.Rows)
	}
	result, err = engine.Execute(session, "SELECT DISTINCT LENGTH(payload) AS n FROM items ORDER BY n LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Rows, [][]any{{int64(1024)}}) {
		t.Fatalf("cold distinct=%v", result.Rows)
	}
	if !table.IsCold() {
		t.Fatal("external ORDER/DISTINCT materialized cold table")
	}
	files, err := os.ReadDir(temporary)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("sort left temporary files: %v", files)
	}
	if _, err := engine.Execute(session, "SELECT id FROM items WHERE JSON_EXTRACT('broken', '$')=1 ORDER BY LENGTH(payload) LIMIT 1"); err == nil {
		t.Fatal("cold ordered WHERE swallowed JSON error")
	}
}

func TestLegacyColdAndFullSelectExposeIdenticalSourceColumnMetadata(t *testing.T) {
	directory := t.TempDir()
	warm, err := openLegacyWithOptions(directory, "root", "secret", OpenOptions{StorageMode: "paged"})
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	for _, query := range []string{"CREATE DATABASE flags", "USE flags", "CREATE TABLE meta(id INT AUTO_INCREMENT PRIMARY KEY, slug VARCHAR(12) UNIQUE, payload TEXT NULL, amount DECIMAL(20,2))", "INSERT INTO meta(slug,payload,amount) VALUES('key',NULL,12.30)"} {
		if _, err := warm.Execute(session, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	queries := []string{"SELECT * FROM meta", "SELECT id AS chosen, slug, amount FROM meta"}
	expected := make([][]Column, len(queries))
	for index, query := range queries {
		result, err := warm.Execute(session, query)
		if err != nil {
			t.Fatal(err)
		}
		expected[index] = result.Columns
	}
	if err := warm.Close(); err != nil {
		t.Fatal(err)
	}
	cold, err := openLegacyWithOptions(directory, "root", "secret", OpenOptions{StorageMode: "paged", ColdRead: true, ColdMaterializeBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	session = &Session{CurrentDatabase: "flags"}
	for index, query := range queries {
		result, err := cold.Execute(session, query)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(expected[index], result.Columns) {
			t.Fatalf("%s\nfull=%#v\ncold=%#v", query, expected[index], result.Columns)
		}
	}
	result, err := cold.Execute(session, "SELECT id+1 AS computed FROM meta")
	if err != nil {
		t.Fatal(err)
	}
	if result.Columns[0].Table != "" || result.Columns[0].OriginalName != "" || result.Columns[0].PrimaryKey {
		t.Fatal("computed expression falsely exposes a writable source column")
	}
}

func TestLegacyColdSystemSchemaCannotHideUserTableSubquery(t *testing.T) {
	engine, session, table := coldTestEngine(t, 1024)
	if _, err := engine.Execute(session, "SELECT (SELECT payload FROM cold.items LIMIT 1) FROM information_schema.tables"); !errors.Is(err, storage.ErrColdMaterializationLimit) {
		t.Fatalf("system schema bypassed cold subquery budget: %v", err)
	}
	if !table.IsCold() {
		t.Fatal("system subquery materialized table")
	}
}
