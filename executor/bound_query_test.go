package executor

import (
	"context"
	"fmt"
	"gbaselite/physical"
	"gbaselite/storageengine"
	"gbaselite/storageengine/testkit"
	"strings"
	"testing"
)

func TestDistinctDoesNotMaterializeInputResult(t *testing.T) {
	for _, memory := range []bool{false, true} {
		t.Run(fmt.Sprint(memory), func(t *testing.T) {
			options := OpenOptions{}
			if memory {
				options.BackendFactory = func(string, storageengine.Options) (storageengine.Engine, error) { return testkit.NewMemory(), nil }
			}
			e, err := OpenWithOptions(t.TempDir(), "root", "test", options)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			session := &Session{}
			run := func(q string) *Result {
				t.Helper()
				r, err := e.Execute(session, q)
				if err != nil {
					t.Fatal(q, err)
				}
				return r
			}
			run("CREATE DATABASE d")
			run("USE d")
			run("CREATE TABLE a(id INT PRIMARY KEY,v VARCHAR(200))")
			var rows []string
			for i := 0; i < 200; i++ {
				rows = append(rows, fmt.Sprintf("(%d,'%s')", i, strings.Repeat("x", 100)))
			}
			run("INSERT INTO a VALUES" + strings.Join(rows, ","))
			e.QueryOptions.ResultMemoryBytes = 1024
			for _, q := range []string{"SELECT DISTINCT v FROM a", "SELECT v FROM a UNION SELECT v FROM a"} {
				if r := run(q); len(r.Rows) != 1 {
					t.Fatal(q, len(r.Rows))
				}
			}
			if _, err := e.Execute(session, "SELECT v FROM a"); err == nil {
				t.Fatal("final result budget bypassed")
			}
		})
	}
}
func TestBoundDistinctStreamingBoundary(t *testing.T) {
	session := &Session{query: newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 128 << 10, ResultMemoryBytes: 1024, TempDirectory: t.TempDir()})}
	visited := 0
	input := physical.Source[[]any](func(_ context.Context, y physical.Yield[[]any]) error {
		for i := 0; i < 100; i++ {
			visited++
			if err := y([]any{int64(i % 3)}); err != nil {
				return err
			}
		}
		return nil
	})
	query := bindDistinct(session, []Column{{Name: "n"}}, input, 0, -1)
	r, err := collectBoundQuery(session, query, true)
	if err != nil {
		t.Fatal(err)
	}
	if visited != 0 || len(r.Rows) != 0 {
		t.Fatal("eager result")
	}
	count := 0
	if err := r.StreamRows(func([]any) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 3 || visited != 100 {
		t.Fatal(count, visited)
	}
}
