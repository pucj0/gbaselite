package legacy

import (
	"context"
	"crypto/sha256"
	"errors"
	"gbaselite/catalog"
	"gbaselite/executor"
	"gbaselite/storage"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sourceFixture(t *testing.T, dir, format string) {
	t.Helper()
	s := storage.NewStore()
	db, err := s.CreateDatabase("d")
	if err != nil {
		t.Fatal(err)
	}
	table, err := db.CreateTableWithIndexes("items", []storage.Column{{Name: "id", Type: storage.TypeInt}}, []string{"id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = table.Insert(storage.Row{storage.MustValue(storage.TypeInt, int64(9))}); err != nil {
		t.Fatal(err)
	}
	p := storage.NewPersistence(dir)
	if format == "paged" {
		p = storage.NewPagedPersistence(dir, 0)
	}
	if err = p.Save(s); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.OpenUsers(dir, "admin", "fixture-only"); err != nil {
		t.Fatal(err)
	}
}
func digest(t *testing.T, dir string) map[string][32]byte {
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
func openTarget(dir string) (Target, error) { return executor.Open(dir, "", "") }
func TestMigrationFailurePublicationAndRecovery(t *testing.T) {
	for _, format := range []string{"snapshot", "paged"} {
		for _, stage := range []string{"write", "before-verify", "after-verify", "before-rename", "after-rename"} {
			t.Run(format+"/"+stage, func(t *testing.T) {
				root := t.TempDir()
				source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
				sourceFixture(t, source, format)
				before := digest(t, source)
				boom := errors.New("injected " + stage)
				hit := false
				err := migrate(context.Background(), source, target, openTarget, func(point string) error {
					if point == stage {
						hit = true
						return boom
					}
					return nil
				})
				if !hit || !errors.Is(err, boom) {
					t.Fatal(hit, err)
				}
				if !reflect.DeepEqual(before, digest(t, source)) {
					t.Fatal("source changed")
				}
				if stage != "after-rename" {
					if _, err = os.Stat(target); !os.IsNotExist(err) {
						t.Fatal("partial target published", err)
					}
				} else {
					e, err := executor.Open(target, "", "")
					if err != nil {
						t.Fatal(err)
					}
					r, err := e.Execute(&executor.Session{CurrentDatabase: "d"}, "SELECT id FROM items")
					closeErr := e.Close()
					if err != nil || closeErr != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(9) {
						t.Fatal(r, err, closeErr)
					}
					if err = Migrate(context.Background(), source, target, openTarget); err == nil {
						t.Fatal("recovery overwrote published target")
					}
				}
				entries, _ := os.ReadDir(root)
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".gbaselite-migrate-") {
						t.Fatal("staging leaked")
					}
				}
			})
		}
	}
}
func TestPublicationDoesNotReplaceConcurrentTarget(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "staged"), filepath.Join(root, "target")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := publishDirectory(source, target); err == nil {
		t.Fatal("replaced existing empty directory")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
}
