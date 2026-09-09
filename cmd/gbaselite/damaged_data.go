package main

import (
	"errors"
	"flag"
	"fmt"
	"gbaselite/catalog"
	"gbaselite/storage"
	"io"
	"os"
	"path/filepath"
)

// Conservative installer probe. Only definite EOF/truncation is classified as
// damage. Permission failures, unsupported formats and other backends fail closed.
func runCheckDamagedData(args []string) error {
	flags := flag.NewFlagSet("check-damaged-data", flag.ContinueOnError)
	dir := flags.String("directory", "", "stopped instance directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("--directory is required")
	}
	damaged, err := checkDamagedData(*dir)
	if err != nil {
		return err
	}
	if damaged {
		fmt.Println("DAMAGED")
	} else {
		fmt.Println("NO_CONFIRMED_DAMAGE")
	}
	return nil
}
func checkDamagedData(dir string) (bool, error) {
	for _, name := range []string{"versioned/mvcc.db", "databases/store.pages", "databases/store.checkpoint", "databases/store.wal"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return false, fmt.Errorf("automatic damage classification is only supported for legacy snapshots")
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	damaged := false
	for _, name := range []string{"databases/store.gob", "users/users.gob"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			path += ".tmp"
		} else if err != nil {
			return false, err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false, err
		}
		var err error
		if name == "databases/store.gob" {
			_, err = storage.InspectSnapshot(path)
		} else {
			_, err = catalog.InspectUserCatalog(path)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			damaged = true
		} else if err != nil {
			return false, err
		}
	}
	return damaged, nil
}
