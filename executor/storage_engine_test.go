package executor

import (
	"fmt"
	"gbaselite/storageengine"
	"gbaselite/storageengine/testkit"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSQLRunsOnIndependentStorageEngine(t *testing.T) {
	backend := testkit.NewMemory()
	called := false
	e, err := OpenWithOptions(t.TempDir(), "root", "test-only", OpenOptions{BackendFactory: func(_ string, _ storageengine.Options) (storageengine.Engine, error) {
		called = true
		return backend, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if !called {
		t.Fatal("factory not used")
	}
	s := &Session{}
	run := func(s *Session, q string) *Result {
		t.Helper()
		r, err := e.Execute(s, q)
		if err != nil {
			t.Fatal(q, err)
		}
		return r
	}
	for _, q := range []string{"CREATE DATABASE portable", "USE portable", "CREATE TABLE items(id BIGINT AUTO_INCREMENT PRIMARY KEY,v INT UNIQUE,grp INT,KEY group_idx(grp))", "INSERT INTO items(v,grp) VALUES(10,1),(20,1),(30,2)", "BEGIN", "UPDATE items SET v=99 WHERE id=1", "ROLLBACK"} {
		run(s, q)
	}
	if got := fmt.Sprint(run(s, "SELECT id,v FROM items ORDER BY id DESC LIMIT 2").Rows); got != "[[3 30] [2 20]]" {
		t.Fatal(got)
	}
	if got := fmt.Sprint(run(s, "SELECT SUM(v),COUNT(*) FROM items").Rows); got != "[[60 3]]" {
		t.Fatal("borrowed batch bytes", got)
	}
	if got := fmt.Sprint(run(s, "SELECT id,v FROM items WHERE grp=1 ORDER BY id").Rows); got != "[[1 10] [2 20]]" {
		t.Fatal("secondary", got)
	}
	run(s, "BEGIN")
	run(s, "UPDATE items SET v=11 WHERE id=1")
	if _, err = e.Execute(s, "INSERT INTO items(v,grp) VALUES(40,1),(20,1)"); err == nil {
		t.Fatal("unique violation accepted")
	}
	run(s, "COMMIT")
	if got := fmt.Sprint(run(s, "SELECT SUM(v),COUNT(*) FROM items").Rows); got != "[[61 3]]" {
		t.Fatal("statement rollback", got)
	}
	old := &Session{CurrentDatabase: "portable"}
	defer e.CloseSession(old)
	run(old, "BEGIN")
	run(s, "UPDATE items SET v=12 WHERE id=1")
	if got := fmt.Sprint(run(old, "SELECT v FROM items WHERE id=1").Rows); got != "[[11]]" {
		t.Fatal("snapshot", got)
	}
	run(old, "ROLLBACK")
	run(s, "CREATE TABLE children(id INT PRIMARY KEY,parent_id BIGINT,FOREIGN KEY(parent_id) REFERENCES items(id))")
	run(s, "INSERT INTO children VALUES(1,2)")
	if _, err = e.Execute(s, "DELETE FROM items WHERE id=2"); err == nil {
		t.Fatal("foreign key protection lost")
	}
	if got := fmt.Sprint(run(s, "SELECT items.v FROM items INNER JOIN children ON items.id=children.parent_id").Rows); got != "[[20]]" {
		t.Fatal("join", got)
	}
	if _, err = e.Execute(s, "BACKUP MVCC TO 'not-created'"); err != storageengine.ErrUnsupported {
		t.Fatalf("optional capability: %v", err)
	}
}

func TestSQLPackagesDoNotImportPhysicalBackends(t *testing.T) {
	for _, dir := range []string{"executor", "server", "parser", "planner", "sql", "physical"} {
		root := filepath.Join("..", dir)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := goparser.ParseFile(token.NewFileSet(), path, nil, goparser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				name, _ := strconv.Unquote(imp.Path.Value)
				if strings.Contains(name, "bbolt") || name == "gbaselite/mvcc" || name == "gbaselite/replication" || strings.HasPrefix(name, "gbaselite/storageengine/mvccadapter") {
					t.Errorf("%s directly imports physical backend %s", path, name)
				}
				if name == "gbaselite/enginefactory" && filepath.Base(path) != "open_options.go" {
					t.Errorf("backend composition leaked into operator %s", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
