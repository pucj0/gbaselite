//go:build !windows

package mvcc

import (
	"errors"
	"os"
	"path/filepath"
)

func syncWALDirectory(path string) error {
	f, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
