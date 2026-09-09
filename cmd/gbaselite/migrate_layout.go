package main

import (
	"context"
	"flag"
	"fmt"
	"gbaselite/mvcc"
	"os"
	"path/filepath"
)

func runMigrateLayout(args []string) error {
	flags := flag.NewFlagSet("migrate-layout", flag.ContinueOnError)
	source := flags.String("source", "", "stopped standalone instance versioned directory")
	target := flags.String("target", "", "new, nonexistent versioned directory")
	layout := flags.String("layout", "flat", "flat or nested")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source == "" || *target == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: migrate-layout --source <versioned-dir> --target <new-dir> --layout flat|nested")
	}
	if _, err := os.Stat(filepath.Join(*source, "mvcc.db")); err != nil {
		return err
	}
	for _, p := range []string{filepath.Join(*source, "raft.db"), filepath.Join(filepath.Dir(*source), "replication", "raft.db")} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("Raft instance requires coordinated snapshot migration")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	store, err := mvcc.Open(*source)
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.ExportLayout(context.Background(), *target, *layout); err != nil {
		return err
	}
	fmt.Println("Migration verified:", *target, "; source preserved. Cut over only while the instance is stopped.")
	return nil
}
