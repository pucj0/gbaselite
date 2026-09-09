package executor

import (
	"reflect"
	"testing"
)

func TestPagedEngineSQLCommitRollbackAndReopen(t *testing.T) {
	directory := t.TempDir()
	options := OpenOptions{StorageMode: "paged", PageCacheBytes: 16 << 10}
	engine, err := OpenWithOptions(directory, "root", "secret", options)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{}
	run := func(query string) *Result {
		t.Helper()
		result, err := engine.Execute(session, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return result
	}
	run("CREATE DATABASE durable")
	run("USE durable")
	run("CREATE TABLE items(id INT PRIMARY KEY, amount INT, document JSON)")
	run("INSERT INTO items VALUES(1, 10, JSON_OBJECT('a',1))")
	run("BEGIN")
	run("UPDATE items SET amount=20 WHERE id=1")
	run("SAVEPOINT keep")
	run("UPDATE items SET amount=99 WHERE id=1")
	run("ROLLBACK TO keep")
	run("COMMIT")
	run("BEGIN")
	run("DELETE FROM items WHERE id=1")
	run("ROLLBACK")
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = OpenWithOptions(directory, "root", "secret", options)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	session = &Session{CurrentDatabase: "durable"}
	result := run("SELECT id, amount, JSON_EXTRACT(document, '$.a') FROM items")
	if len(result.Rows) != 1 || !reflect.DeepEqual(result.Rows[0][:2], []any{int64(1), int64(20)}) {
		t.Fatalf("reopened values = %#v", result.Rows)
	}
	if !engine.Persistence.PagedStats().Enabled {
		t.Fatal("paged persistence was not selected")
	}
	if _, err := OpenWithOptions(directory, "root", "secret", OpenOptions{StorageMode: "snapshot"}); err == nil {
		t.Fatal("mode downgrade served stale data")
	}
}

func TestOpenWithOptionsValidatesStorageOptions(t *testing.T) {
	for _, options := range []OpenOptions{{StorageMode: "unknown"}, {PageCacheBytes: -1}} {
		if _, err := OpenWithOptions(t.TempDir(), "root", "secret", options); err == nil {
			t.Fatalf("accepted %#v", options)
		}
	}
}
