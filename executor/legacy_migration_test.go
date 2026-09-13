package executor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"gbaselite/migration/legacy"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func migrationFiles(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrateLegacyFormatsToDefaultMVCC(t *testing.T) {
	for _, mode := range []string{"snapshot", "paged"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			target := filepath.Join(root, "target")
			old, err := openLegacyWithOptions(source, "root", "secret", OpenOptions{StorageMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			s := &Session{}
			for _, q := range []string{
				"CREATE DATABASE migration", "USE migration",
				"CREATE TABLE parent(id BIGINT AUTO_INCREMENT PRIMARY KEY,label VARCHAR(50) UNIQUE,amount DECIMAL(12,2), KEY label_lookup(label))",
				"CREATE TABLE child(id INT PRIMARY KEY,parent_id BIGINT,CONSTRAINT fk_parent FOREIGN KEY(parent_id) REFERENCES parent(id))",
				"INSERT INTO parent VALUES(5,'kept',123.45)", "INSERT INTO child VALUES(1,5)",
				"BEGIN", "INSERT INTO parent(label,amount) VALUES('rolled-back',0)", "ROLLBACK",
				"CREATE USER 'reader'@'%' IDENTIFIED BY 'reader-secret'", "GRANT SELECT ON migration.* TO 'reader'@'%'",
			} {
				if _, err := old.Execute(s, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if err = old.Close(); err != nil {
				t.Fatal(err)
			}
			before := migrationFiles(t, source)
			if err = MigrateLegacy(context.Background(), source, target); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, migrationFiles(t, source)) {
				t.Fatal("migration changed source bytes or files")
			}
			e, err := Open(target, "", "")
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if e.Backend == nil {
				t.Fatal("migrated engine is not MVCC")
			}
			session := &Session{CurrentDatabase: "migration"}
			result, err := e.Execute(session, "SELECT id,label,amount FROM parent")
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Rows) != 1 || result.Rows[0][0] != int64(5) || result.Rows[0][1] != "kept" {
				t.Fatalf("rows=%v", result.Rows)
			}
			if !e.Users.Allowed("reader", "%", "SELECT", "migration", "parent") || e.Users.Allowed("reader", "%", "INSERT", "migration", "parent") {
				t.Fatal("grants changed")
			}
			result, err = e.Execute(session, "INSERT INTO parent(label,amount) VALUES('after',1.25)")
			if err != nil {
				t.Fatal(err)
			}
			if result.LastInsertID != 7 {
				t.Fatalf("lost rolled-back auto increment reservation: %d", result.LastInsertID)
			}
			if _, err = e.Execute(session, "DELETE FROM parent WHERE id=5"); err == nil {
				t.Fatal("lost foreign key protection")
			}
			if _, err = e.Execute(session, "INSERT INTO parent(label) VALUES('kept')"); err == nil {
				t.Fatal("lost unique index")
			}
			if _, err = os.Stat(filepath.Join(target, "databases", "store.gob")); !os.IsNotExist(err) {
				t.Fatal("target has legacy persistence")
			}
			if err = MigrateLegacy(context.Background(), source, target); err == nil {
				t.Fatal("overwrote target")
			}
		})
	}
}

func TestMigrateLegacyRejectsUnsupportedAndDamagedSources(t *testing.T) {
	for _, kind := range []string{"truncated", "cancelled", "nested", "live"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			target := filepath.Join(root, "target")
			e, err := openLegacy(source, "root", "secret")
			if err != nil {
				t.Fatal(err)
			}
			s := &Session{}
			for _, q := range []string{"CREATE DATABASE d", "USE d", "CREATE TABLE p(id INT PRIMARY KEY)", "INSERT INTO p VALUES(1)"} {
				if _, err = e.Execute(s, q); err != nil {
					t.Fatal(err)
				}
			}

			if err = e.Close(); err != nil {
				t.Fatal(err)
			}
			if kind == "truncated" {
				if err = os.WriteFile(filepath.Join(source, "databases", "store.gob"), []byte{}, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "live" {
				if err = os.WriteFile(filepath.Join(source, "gbaselite.pid"), []byte("123"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "nested" {
				target = filepath.Join(source, "new")
			}
			before := migrationFiles(t, source)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			if err = MigrateLegacy(ctx, source, target); err == nil {
				t.Fatal("unsafe migration succeeded")
			}
			if !reflect.DeepEqual(before, migrationFiles(t, source)) {
				t.Fatal("failed migration changed source")
			}
			if _, err = os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("failed migration published target: %v", err)
			}
			entries, _ := os.ReadDir(root)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".gbaselite-migrate-") {
					t.Fatal("migration staging directory leaked")
				}
			}
		})
	}
}

func TestDefaultEngineRejectsRetiredRuntimeOptions(t *testing.T) {
	for _, o := range []OpenOptions{{StorageMode: "snapshot"}, {StorageMode: "paged"}, {StorageMode: "legacy"}, {ColdRead: true}, {PageCacheBytes: 4096}, {ColdMaterializeBytes: 1}, {TransactionWriteBytes: -1}} {
		dir := filepath.Join(t.TempDir(), "new")
		if _, err := OpenWithOptions(dir, "root", "secret", o); err == nil {
			t.Fatalf("accepted %+v", o)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid options created data")
		}
	}
}

func MigrateLegacy(ctx context.Context, source, target string) error {
	return legacy.Migrate(ctx, source, target, func(dir string) (legacy.Target, error) { return Open(dir, "", "") })
}

// A legacy source with views migrates them into the MVCC catalog, where they stay
// queryable after reopen and keep the legacy read-only and namespace rules.
func TestMigrateLegacyImportsViews(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	old, err := openLegacy(source, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{}
	for _, q := range []string{
		"CREATE DATABASE mv", "USE mv",
		"CREATE TABLE t(id INT PRIMARY KEY,label VARCHAR(20))",
		"INSERT INTO t VALUES(1,'a'),(2,'b')",
		"CREATE VIEW v AS SELECT id,label FROM t WHERE id=1",
		"CREATE VIEW vv AS SELECT id FROM v",
	} {
		if _, err = old.Execute(s, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	if err = MigrateLegacy(context.Background(), source, target); err != nil {
		t.Fatal(err)
	}
	e, err := Open(target, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	session := &Session{CurrentDatabase: "mv"}
	result, err := e.Execute(session, "SELECT id,label FROM v")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != int64(1) || result.Rows[0][1] != "a" {
		t.Fatalf("migrated view rows=%v", result.Rows)
	}
	if got := fmt.Sprint(mustExecute(t, e, session, "SELECT id FROM vv").Rows); got != "[[1]]" {
		t.Fatalf("nested migrated view rows=%s", got)
	}
	if got := fmt.Sprint(mustExecute(t, e, session, "SHOW TABLES").Rows); got != "[[t] [v] [vv]]" {
		t.Fatalf("migrated relations=%s", got)
	}
	show := mustExecute(t, e, session, "SHOW CREATE VIEW v")
	if got := fmt.Sprint(show.Rows[0][1]); !strings.Contains(got, "CREATE VIEW `v` AS SELECT id,label FROM t WHERE id=1") {
		t.Fatalf("show create view=%s", got)
	}
	if _, err = e.Execute(session, "INSERT INTO v VALUES(3,'c')"); err == nil {
		t.Fatal("migrated view accepted a write")
	}
	if _, err = e.Execute(session, "CREATE TABLE v (id INT)"); err == nil {
		t.Fatal("migrated view released its name")
	}
}

func mustExecute(t *testing.T, e *Engine, session *Session, query string) *Result {
	t.Helper()
	result, err := e.Execute(session, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return result
}

// A legacy source whose foreign keys use CASCADE/SET NULL is a supported MVCC
// target now, so migration must accept it instead of rejecting the schema.
func TestMigrateLegacyAcceptsCascadeForeignKey(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	e, err := openLegacy(source, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{}
	for _, q := range []string{
		"CREATE DATABASE d", "USE d",
		"CREATE TABLE p(id INT PRIMARY KEY)",
		"CREATE TABLE c(id INT PRIMARY KEY,parent_id INT,FOREIGN KEY(parent_id) REFERENCES p(id) ON DELETE CASCADE ON UPDATE SET NULL)",
		"INSERT INTO p VALUES(1)",
		"INSERT INTO c VALUES(10,1)",
	} {
		if _, err = e.Execute(s, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err = MigrateLegacy(context.Background(), source, target); err != nil {
		t.Fatalf("cascade source rejected by migration: %v", err)
	}
	if _, err = os.Stat(filepath.Join(target, "users", "users.gob")); err != nil {
		t.Fatalf("migrated target missing: %v", err)
	}
	// The migrated CASCADE schema must open on the MVCC runtime.
	migrated, err := OpenWithOptions(target, "root", "secret", OpenOptions{StorageMode: "mvcc"})
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	ms := &Session{}
	if _, err = migrated.Execute(ms, "USE d"); err != nil {
		t.Fatal(err)
	}
	deleted, err := migrated.Execute(&Session{CurrentDatabase: "d"}, "DELETE FROM p WHERE id=1")
	if err != nil {
		t.Fatalf("cascade delete after migration: %v", err)
	}
	if deleted.AffectedRows != 1 {
		t.Fatalf("cascade delete affected=%d", deleted.AffectedRows)
	}
	count, err := migrated.Execute(&Session{CurrentDatabase: "d"}, "SELECT COUNT(*) FROM c")
	if err != nil {
		t.Fatal(err)
	}
	if len(count.Rows) != 1 || count.Rows[0][0] != int64(0) {
		t.Fatalf("cascade after migration left children: %v", count.Rows)
	}
}
