package storage

import (
	"encoding/gob"
	"fmt"
	"io"
	"strings"
)

// Snapshot format versions are part of the on-disk contract. Version zero is
// the historical gob shape that had no version field; it is retained as a
// migration source for existing installations.
const (
	LegacySnapshotFormatVersion  uint16 = 0
	CurrentSnapshotFormatVersion uint16 = 3
)

type snapshotMigration struct {
	to    uint16
	apply func(*StoreSnapshot) error
}

// Keep migrations explicit and ordered. The v0 -> v1 migration is currently a
// logical no-op because v1 only adds the marker, but naming it makes a future
// schema change fail closed unless its migration is registered here.
var snapshotMigrations = map[uint16]snapshotMigration{
	LegacySnapshotFormatVersion: {
		to:    1,
		apply: func(*StoreSnapshot) error { return nil },
	},
	1: {
		to:    2,
		apply: migrateAutoIncrementState,
	},
	2: {
		to:    CurrentSnapshotFormatVersion,
		apply: migrateNamedConstraints,
	},
}

func migrateNamedConstraints(snapshot *StoreSnapshot) error {
	for databaseIndex := range snapshot.Databases {
		for tableIndex := range snapshot.Databases[databaseIndex].Tables {
			table := &snapshot.Databases[databaseIndex].Tables[tableIndex]
			for _, expression := range table.Checks {
				table.CheckConstraints = append(table.CheckConstraints, CheckConstraint{Expression: expression})
			}
			table.Checks = nil
			used := make(map[string]bool)
			for _, foreignKey := range table.ForeignKeys {
				if foreignKey.Name != "" {
					used[strings.ToLower(foreignKey.Name)] = true
				}
			}
			for _, check := range table.CheckConstraints {
				if check.Name != "" {
					used[strings.ToLower(check.Name)] = true
				}
			}
			nextName := func(kind string) string {
				for number := 1; ; number++ {
					candidate := fmt.Sprintf("%s_%s_%d", table.Name, kind, number)
					if !used[strings.ToLower(candidate)] {
						used[strings.ToLower(candidate)] = true
						return candidate
					}
				}
			}
			for index := range table.ForeignKeys {
				if table.ForeignKeys[index].Name == "" {
					table.ForeignKeys[index].Name = nextName("ibfk")
				}
			}
			for index := range table.CheckConstraints {
				if table.CheckConstraints[index].Name == "" {
					table.CheckConstraints[index].Name = nextName("chk")
				}
			}
		}
	}
	return nil
}

func migrateAutoIncrementState(snapshot *StoreSnapshot) error {
	for databaseIndex := range snapshot.Databases {
		for tableIndex := range snapshot.Databases[databaseIndex].Tables {
			table := &snapshot.Databases[databaseIndex].Tables[tableIndex]
			table.AutoIncrementNext = make(map[string]int64)
			for columnIndex, column := range table.Columns {
				if !column.AutoIncrement {
					continue
				}
				next := int64(1)
				for _, row := range table.Rows {
					if columnIndex >= len(row) {
						return fmt.Errorf("table %q row has %d values, expected at least %d", table.Name, len(row), columnIndex+1)
					}
					if !row[columnIndex].Null && row[columnIndex].Int64 >= next {
						next = row[columnIndex].Int64 + 1
					}
				}
				table.AutoIncrementNext[normalizeName(column.Name)] = next
			}
		}
	}
	return nil
}

func decodeSnapshot(reader io.Reader) (StoreSnapshot, uint16, error) {
	var snapshot StoreSnapshot
	if err := gob.NewDecoder(reader).Decode(&snapshot); err != nil {
		return StoreSnapshot{}, 0, err
	}
	sourceVersion := snapshot.FormatVersion
	if err := migrateSnapshot(&snapshot); err != nil {
		return StoreSnapshot{}, sourceVersion, err
	}
	return snapshot, sourceVersion, nil
}

func migrateSnapshot(snapshot *StoreSnapshot) error {
	if snapshot.FormatVersion > CurrentSnapshotFormatVersion {
		return fmt.Errorf("snapshot format version %d is newer than the supported version %d", snapshot.FormatVersion, CurrentSnapshotFormatVersion)
	}
	for snapshot.FormatVersion < CurrentSnapshotFormatVersion {
		migration, ok := snapshotMigrations[snapshot.FormatVersion]
		if !ok {
			return fmt.Errorf("no migration registered for snapshot format version %d", snapshot.FormatVersion)
		}
		if migration.to <= snapshot.FormatVersion || migration.to > CurrentSnapshotFormatVersion {
			return fmt.Errorf("invalid snapshot migration from version %d to %d", snapshot.FormatVersion, migration.to)
		}
		if migration.apply != nil {
			if err := migration.apply(snapshot); err != nil {
				return fmt.Errorf("migrate snapshot format version %d to %d: %w", snapshot.FormatVersion, migration.to, err)
			}
		}
		snapshot.FormatVersion = migration.to
	}
	return nil
}
