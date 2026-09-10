package legacy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Flush every finished file, then child directories before their parents.
func syncTree(ctx context.Context, root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		return errors.Join(f.Sync(), f.Close())
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = syncDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}
