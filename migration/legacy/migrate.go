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

// Migrate converts a stopped snapshot/paged instance to a new standalone
// MVCC directory. Only an isolated copy is opened by the legacy reader. The
// target is published after durable reopen and full row verification; the source
// is never modified. Unsupported schemas fail without publishing a target.
func Migrate(ctx context.Context, source, target string, open TargetOpener) error {
	return migrate(ctx, source, target, open, nil)
}

// checkpoint is private and per invocation: tests cannot change another migration.
type checkpoint func(string) error

func (c checkpoint) hit(stage string) error {
	if c != nil {
		return c(stage)
	}
	return nil
}
func migrate(ctx context.Context, source, target string, open TargetOpener, check checkpoint) error {
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
	if err := copyMigrationDirectory(ctx, source, copyDir, check); err != nil {
		return err
	}
	legacy, err := loadLegacyForMigration(copyDir, mode)
	if err != nil {
		return fmt.Errorf("read legacy copy: %w", err)
	}
	snapshot := legacy.Snapshot()
	for _, db := range snapshot.Databases {
		if strings.ContainsAny(db.Name, "/\x00") {
			return fmt.Errorf("unsupported database identifier %q", db.Name)
		}
		for _, view := range db.Views {
			if strings.ContainsAny(view.Name, "/\x00.") {
				return fmt.Errorf("unsupported view identifier %q", view.Name)
			}
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
	if err := copyMigrationDirectory(ctx, filepath.Join(copyDir, "users"), filepath.Join(destination, "users"), check); err != nil {
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
	if err = check.hit("before-verify"); err != nil {
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
	if err = check.hit("after-verify"); err != nil {
		return err
	}
	if err = syncTree(ctx, destination); err != nil {
		return err
	}
	if err = check.hit("before-rename"); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return fmt.Errorf("migration target appeared during conversion")
	}
	if err = publishDirectory(destination, target); err != nil {
		return err
	}
	// Once published, never delete a verified target even if parent sync fails.
	if err = syncDirectory(parent); err != nil {
		return fmt.Errorf("target published; parent sync: %w", err)
	}
	if err = syncDirectory(work); err != nil {
		return fmt.Errorf("target published; staging sync: %w", err)
	}
	return check.hit("after-rename")
}

func copyMigrationDirectory(ctx context.Context, source, target string, check checkpoint) error {
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
		_, copyErr := io.Copy(&copyWriter{ctx: ctx, file: dst, check: check}, in)
		if copyErr == nil {
			copyErr = dst.Sync()
		}
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

type copyWriter struct {
	ctx   context.Context
	file  *os.File
	check checkpoint
}

func (w *copyWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	if err == nil {
		err = w.check.hit("write")
	}
	return n, err
}
