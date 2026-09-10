package legacy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gbaselite/storage"
)

// MigrateLegacy converts a stopped snapshot/paged instance to a new standalone
// MVCC directory. Only an isolated copy is opened by the legacy reader. The
// target is published after durable reopen and full row verification; the source
// is never modified. Unsupported schemas fail without publishing a target.
func Migrate(ctx context.Context, source, target string, open TargetOpener) error {
	if open == nil {
		return fmt.Errorf("migration target opener required")
	}
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
	engine, err := open(destination)
	if err != nil {
		return err
	}
	if err = engine.ImportSnapshot(ctx, snapshot); err != nil {
		return errors.Join(err, engine.Close())
	}
	if err = engine.Close(); err != nil {
		return err
	}
	engine, err = open(destination)
	if err != nil {
		return err
	}
	err = engine.VerifySnapshot(ctx, snapshot)
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

// Target is a logical snapshot destination. The offline package has no runtime/backend dependency.
type Target interface {
	ImportSnapshot(context.Context, storage.StoreSnapshot) error
	VerifySnapshot(context.Context, storage.StoreSnapshot) error
	Close() error
}
type TargetOpener func(directory string) (Target, error)
