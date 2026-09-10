package main

import (
	"context"
	"flag"
	"fmt"
	"gbaselite/executor"
	"gbaselite/migration/legacy"
)

func runMigrateLegacy(args []string) error {
	flags := flag.NewFlagSet("migrate-legacy", flag.ContinueOnError)
	source := flags.String("source", "", "stopped snapshot/paged instance directory")
	target := flags.String("target", "", "new, nonexistent MVCC instance directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source == "" || *target == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: migrate-legacy --source <old-directory> --target <new-directory>")
	}
	if err := legacy.Migrate(context.Background(), *source, *target, func(dir string) (legacy.Target, error) { return executor.Open(dir, "", "") }); err != nil {
		return err
	}
	fmt.Println("Legacy migration verified:", *target, "; source preserved. Configure storage.path to the new directory before starting.")
	return nil
}
