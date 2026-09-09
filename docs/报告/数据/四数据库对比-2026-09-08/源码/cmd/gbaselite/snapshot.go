package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"gbaselite/storage"
)

func runInspectSnapshot(args []string) error {
	return inspectSnapshots(args, os.Stdout)
}

func inspectSnapshots(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("inspect-snapshot", flag.ContinueOnError)
	file := flags.String("file", "", "snapshot file to inspect")
	compare := flags.String("compare", "", "optional second snapshot file to compare")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("--file is required")
	}

	primary, err := storage.InspectSnapshot(*file)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "GBaseLite snapshot inspection")
	writeSnapshotInspection(output, "Primary", primary)
	if *compare == "" {
		return nil
	}

	secondary, err := storage.InspectSnapshot(*compare)
	if err != nil {
		return err
	}
	writeSnapshotInspection(output, "Compared", secondary)
	fmt.Fprintf(output, "Identical SHA-256: %t\n", primary.SHA256 == secondary.SHA256)
	fmt.Fprintf(output, "Compared file newer: %t\n", secondary.ModifiedAt.After(primary.ModifiedAt))
	return nil
}

func writeSnapshotInspection(output io.Writer, label string, inspection storage.SnapshotInspection) {
	fmt.Fprintf(output, "%s file: %s\n", label, inspection.Path)
	fmt.Fprintf(output, "%s size: %d bytes\n", label, inspection.Size)
	fmt.Fprintf(output, "%s modified UTC: %s\n", label, inspection.ModifiedAt.Format("2006-01-02T15:04:05.999999999Z07:00"))
	fmt.Fprintf(output, "%s SHA-256: %s\n", label, inspection.SHA256)
	fmt.Fprintf(output, "%s format: current=%d source=%d\n", label, inspection.FormatVersion, inspection.SourceFormatVersion)
	fmt.Fprintf(output, "%s counts: databases=%d tables=%d indexes=%d views=%d rows=%d\n", label, inspection.Databases, inspection.Tables, inspection.Indexes, inspection.Views, inspection.Rows)
}
