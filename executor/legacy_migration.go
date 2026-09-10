package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"gbaselite/sqllayout"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gbaselite/storage"
	"gbaselite/storageengine"
)

// MigrateLegacy converts a stopped snapshot/paged instance to a new standalone
// MVCC directory. Only an isolated copy is opened by the legacy reader. The
// target is published after durable reopen and full row verification; the source
// is never modified. Unsupported schemas fail without publishing a target.
func MigrateLegacy(ctx context.Context, source, target string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return err
	}
	target = filepath.Join(parent, filepath.Base(target))
	for _, pair := range [][2]string{{source, target}, {target, source}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("migration source and target must be separate directories")
		}
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("migration target already exists")
	}
	for _, p := range []string{"gbaselite.pid", filepath.Join("versioned", "mvcc.db"), filepath.Join("replication", "raft.db")} {
		if _, err := os.Lstat(filepath.Join(source, p)); err == nil {
			return fmt.Errorf("legacy migration requires a stopped legacy source without %s", p)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	mode := "snapshot"
	found := false
	for _, name := range []string{"store.gob", "store.pages", "store.checkpoint", "store.wal"} {
		info, err := os.Lstat(filepath.Join(source, "databases", name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("legacy marker must be a regular file: %s", name)
		}
		found = true
		if name != "store.gob" {
			mode = "paged"
		}
	}
	if !found {
		return fmt.Errorf("no committed legacy store found")
	}
	// Require the original accounts. Never silently initialize a replacement admin.
	if _, err := os.Stat(filepath.Join(source, "users", "users.gob")); err != nil {
		return fmt.Errorf("legacy user catalog required: %w", err)
	}
	work, err := os.MkdirTemp(parent, ".gbaselite-migrate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	copyDir := filepath.Join(work, "source")
	if err := copyMigrationDirectory(ctx, source, copyDir); err != nil {
		return err
	}
	legacy, err := loadLegacyForMigration(copyDir, mode)
	if err != nil {
		return fmt.Errorf("read legacy copy: %w", err)
	}
	snapshot := legacy.Snapshot()
	for _, db := range snapshot.Databases {
		if len(db.Views) > 0 {
			return fmt.Errorf("MVCC migration does not support views in database %s", db.Name)
		}
		if strings.ContainsAny(db.Name, "/\x00") {
			return fmt.Errorf("unsupported database identifier %q", db.Name)
		}
		for _, table := range db.Tables {
			if strings.ContainsAny(table.Name, "/\x00.") {
				return fmt.Errorf("unsupported table identifier %q", table.Name)
			}
			for _, column := range table.Columns {
				if column.OnUpdate != "" {
					return fmt.Errorf("MVCC migration does not support ON UPDATE columns: %s.%s", db.Name, table.Name)
				}
			}
		}
	}
	destination := filepath.Join(work, "target")
	if err := copyMigrationDirectory(ctx, filepath.Join(copyDir, "users"), filepath.Join(destination, "users")); err != nil {
		return err
	}
	engine, err := Open(destination, "", "")
	if err != nil {
		return err
	}
	if err = engine.importLegacySnapshot(ctx, snapshot); err != nil {
		return errors.Join(err, engine.Close())
	}
	if err = engine.Close(); err != nil {
		return err
	}
	engine, err = Open(destination, "", "")
	if err != nil {
		return err
	}
	err = engine.verifyLegacySnapshot(ctx, snapshot)
	err = errors.Join(err, engine.Close())
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return fmt.Errorf("migration target appeared during conversion")
	}
	return os.Rename(destination, target)
}

func copyMigrationDirectory(ctx context.Context, source, target string) error {
	return filepath.WalkDir(source, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, p)
		if err != nil {
			return err
		}
		out := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(out, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("migration refuses non-regular file %s", p)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		dst, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(dst, in)
		closeErr := dst.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return os.Chtimes(out, info.ModTime(), info.ModTime())
	})
}

func (e *Engine) importLegacySnapshot(ctx context.Context, snapshot storage.StoreSnapshot) error {
	tx, err := e.Backend.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Create all schemas before resolving foreign keys, including forward references.
	for _, db := range snapshot.Databases {
		if err = tx.Put(sqllayout.Catalog, sqllayout.DatabaseKey(strings.ToLower(db.Name)), []byte{1}); err != nil {
			return err
		}
		for i, table := range db.Tables {
			table.Rows = nil
			definition := versionedTable{CatalogName: strings.ToLower(db.Name) + "." + strings.ToLower(table.Name), ID: fmt.Sprintf("%s/%s/%d", tx.ID(), strings.ToLower(db.Name), i), Definition: table, RowEncoding: mvccCompactRowEncoding, SecondaryEncoding: 1}
			if _, ok := mvccIntegerPrimary(definition); ok {
				definition.KeyEncoding = mvccIntegerKeyEncoding
			}
			encoded, err := encodeVersioned(definition)
			if err != nil {
				return err
			}
			if err = tx.Put(sqllayout.Catalog, sqllayout.TableKey(strings.ToLower(db.Name), strings.ToLower(table.Name)), encoded); err != nil {
				return err
			}
		}
	}
	for _, db := range snapshot.Databases {
		session := &Session{CurrentDatabase: strings.ToLower(db.Name)}
		for _, table := range db.Tables {
			definition, schema, key, err := loadVersionedTable(tx, session, table.Name)
			if err != nil {
				return err
			}
			for _, check := range table.CheckConstraints {
				if err = validateMVCCCheckDefinition(schema, check.Expression); err != nil {
					return err
				}
			}
			if err = prepareMVCCForeignKeys(tx, &definition, session); err != nil {
				return fmt.Errorf("migrate %s: %w", definition.CatalogName, err)
			}
			encoded, err := encodeVersioned(definition)
			if err != nil {
				return err
			}
			if err = tx.Put(sqllayout.Catalog, key, encoded); err != nil {
				return err
			}
			disabled := context.WithValue(ctx, foreignChecksContextKey{}, true)
			for i, row := range table.Rows {
				if err = ctx.Err(); err != nil {
					return err
				}
				if err = writeVersionedRow(disabled, tx, definition, nil, nil, row, fmt.Sprintf("legacy/%020d", i)); err != nil {
					return fmt.Errorf("migrate %s row %d: %w", definition.CatalogName, i, err)
				}
			}
			for col, next := range table.AutoIncrementNext {
				if next < 1 {
					return fmt.Errorf("invalid auto increment counter in %s", definition.CatalogName)
				}
				if err := storageengine.AdvanceCounter(ctx, e.Backend, definition.counterKey(col), uint64(next-1)); err != nil {
					return err
				}
			}
		}
	}
	// Validate every reference against the complete imported transaction.
	for _, db := range snapshot.Databases {
		for _, table := range db.Tables {
			definition, _, _, err := loadVersionedTable(tx, &Session{CurrentDatabase: db.Name}, table.Name)
			if err != nil {
				return err
			}
			for _, row := range table.Rows {
				if err = validateMVCCReferences(ctx, tx, definition, nil, row); err != nil {
					return fmt.Errorf("migrate %s: %w", definition.CatalogName, err)
				}
			}
		}
	}
	_, err = tx.Commit(ctx)
	return err
}

func (e *Engine) verifyLegacySnapshot(ctx context.Context, snapshot storage.StoreSnapshot) error {
	tx, err := e.Backend.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, db := range snapshot.Databases {
		for _, table := range db.Tables {
			definition, _, _, err := loadVersionedTable(tx, &Session{CurrentDatabase: db.Name}, table.Name)
			if err != nil {
				return err
			}
			count := 0
			if err = tx.Scan(ctx, sqllayout.Rows(definition.ID), func(_, _ []byte) error { count++; return nil }); err != nil {
				return err
			}
			if count != len(table.Rows) {
				return fmt.Errorf("migration row count mismatch in %s", definition.CatalogName)
			}
			for i, row := range table.Rows {
				key := []byte(fmt.Sprintf("legacy/%020d", i))
				for _, index := range table.Indexes {
					if index.Primary {
						var ok bool
						key, ok = mvccPrimaryKey(definition, index, row)
						if !ok {
							return fmt.Errorf("invalid primary key")
						}
					}
				}
				got, ok, err := tx.Table(definition.ID).Get(key)
				if err != nil {
					return err
				}
				want, err := encodeMVCCRow(definition, row)
				if err != nil {
					return err
				}
				if !ok || !bytes.Equal(got, want) {
					return fmt.Errorf("migration row verification failed in %s at row %d", definition.CatalogName, i)
				}
			}
		}
	}
	return nil
}
