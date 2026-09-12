package executor

import (
	"fmt"
	"testing"

	"gbaselite/storageengine"
	"gbaselite/storageengine/testkit"
)

// TestMVCCMetadataRefreshWithoutRevisionReader covers backends that only expose
// Begin/Close: the refresh fallback must track the transaction snapshot as its
// catalog revision, otherwise the second revision looks unchanged and metadata
// silently stays stale.
func TestMVCCMetadataRefreshWithoutRevisionReader(t *testing.T) {
	e, err := OpenWithOptions(t.TempDir(), "root", "test", OpenOptions{BackendFactory: func(string, storageengine.Options) (storageengine.Engine, error) {
		return &coreOnlyEngine{testkit.NewMemory()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &Session{}
	run := func(query string) *Result {
		t.Helper()
		result, err := e.Execute(s, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return result
	}
	run("CREATE DATABASE meta")
	run("USE meta")
	run("CREATE TABLE t1(id INT PRIMARY KEY,v INT)")
	if got := fmt.Sprint(run("SHOW TABLES").Rows); got != "[[t1]]" {
		t.Fatalf("first metadata revision = %s", got)
	}
	run("CREATE TABLE IF NOT EXISTS t2(id INT PRIMARY KEY)")
	// The second revision must reflect both tables, not the stale first mirror.
	if got := fmt.Sprint(run("SHOW TABLES").Rows); got != "[[t1] [t2]]" {
		t.Fatalf("second metadata revision = %s", got)
	}
	if got := fmt.Sprint(run("SHOW COLUMNS FROM t2").Rows[0][0]); got != "id" {
		t.Fatalf("t2 columns = %s", got)
	}
	run("CREATE TABLE IF NOT EXISTS t3(id INT PRIMARY KEY)")
	if got := fmt.Sprint(run("SHOW TABLES").Rows); got != "[[t1] [t2] [t3]]" {
		t.Fatalf("third metadata revision = %s", got)
	}
	if _, err := e.Execute(s, "CREATE TABLE IF NOT EXISTS t1(id INT PRIMARY KEY,v INT)"); err != nil {
		// Idempotent DDL must not blank the mirror either.
		t.Fatal(err)
	}
	if got := fmt.Sprint(run("SHOW TABLES").Rows); got != "[[t1] [t2] [t3]]" {
		t.Fatalf("after idempotent DDL = %s", got)
	}
}
