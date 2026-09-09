package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gbaselite/catalog"
	"gbaselite/storage"
)

func runInspectInstance(args []string) error {
	return inspectInstanceCopy(args, os.Stdout)
}

func inspectInstanceCopy(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("inspect-instance", flag.ContinueOnError)
	directory := flags.String("directory", "", "stopped instance data-directory copy to inspect")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *directory == "" {
		return fmt.Errorf("--directory is required")
	}
	absDirectory, err := filepath.Abs(*directory)
	if err != nil {
		return fmt.Errorf("resolve instance directory: %w", err)
	}
	info, err := os.Stat(absDirectory)
	if err != nil {
		return fmt.Errorf("inspect instance directory %s: %w", absDirectory, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("instance path %s is not a directory", absDirectory)
	}

	if _, err := os.Stat(filepath.Join(absDirectory, "versioned", "mvcc.db")); err == nil {
		return fmt.Errorf("MVCC instance inspection requires its versioned backend; legacy snapshot inspection is disabled")
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, marker := range []string{"store.pages", "store.checkpoint", "store.wal"} {
		path := filepath.Join(absDirectory, "databases", marker)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("paged instance inspection is not supported by inspect-instance; preserve the complete directory and validate an isolated copy in paged mode (legacy store.gob may be stale)")
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect paged marker %s: %w", path, err)
		}
	}
	storePath := filepath.Join(absDirectory, "databases", "store.gob")
	userPath := filepath.Join(absDirectory, "users", "users.gob")
	for _, candidate := range []string{storePath + ".tmp", userPath + ".tmp"} {
		if _, err := os.Stat(candidate); err == nil {
			return fmt.Errorf("instance copy contains recovery candidate %s; preserve the copy and inspect the candidate separately before choosing a recovery source", candidate)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect recovery candidate %s: %w", candidate, err)
		}
	}

	snapshot, err := storage.InspectSnapshot(storePath)
	if err != nil {
		return err
	}
	users, err := catalog.InspectUserCatalog(userPath)
	if err != nil {
		return err
	}

	fmt.Fprintln(output, "GBaseLite stopped instance-copy inspection")
	fmt.Fprintf(output, "Data directory: %s\n", absDirectory)
	writeSnapshotInspection(output, "Database snapshot", snapshot)
	fmt.Fprintf(output, "User catalog file: %s\n", users.Path)
	fmt.Fprintf(output, "User catalog size: %d bytes\n", users.Size)
	fmt.Fprintf(output, "User catalog modified UTC: %s\n", users.ModifiedAt.Format("2006-01-02T15:04:05.999999999Z07:00"))
	fmt.Fprintf(output, "User catalog SHA-256: %s\n", users.SHA256)
	fmt.Fprintf(output, "User catalog format: current=%d source=%d\n", users.FormatVersion, users.SourceFormatVersion)
	fmt.Fprintf(output, "User catalog counts: accounts=%d grants=%d privileges=%d\n", users.Accounts, users.Grants, users.Privileges)
	return nil
}
