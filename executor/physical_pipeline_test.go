package executor

import (
	"fmt"
	"gbaselite/storageengine"
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
					return &memoryBackend{data: map[string]map[string][]byte{}, counters: map[string]uint64{}}, nil
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
			// Materialization rejects input under its budget, without mutating the table.
			e.QueryOptions.ResultMemoryBytes = 256
			if _, err := e.Execute(s, "SELECT ROW_NUMBER() OVER(ORDER BY id) FROM a"); err == nil {
				t.Fatal("window ignored memory budget")
			}
		})
	}
}
