package catalog

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
)

// User catalog format versions are independent from database snapshot
// versions because the files have different schemas and recovery lifecycles.
const (
	LegacyUserCatalogFormatVersion  uint16 = 0
	CurrentUserCatalogFormatVersion uint16 = 1
)

type userCatalogSnapshot struct {
	FormatVersion uint16
	Users         map[string]User
}

type userCatalogMigration struct {
	to    uint16
	apply func(*userCatalogSnapshot) error
}

var userCatalogMigrations = map[uint16]userCatalogMigration{
	LegacyUserCatalogFormatVersion: {
		to:    CurrentUserCatalogFormatVersion,
		apply: func(*userCatalogSnapshot) error { return nil },
	},
}

func encodeUserCatalog(writer io.Writer, users map[string]User) error {
	return gob.NewEncoder(writer).Encode(userCatalogSnapshot{
		FormatVersion: CurrentUserCatalogFormatVersion,
		Users:         users,
	})
}

func migrateUserCatalog(snapshot *userCatalogSnapshot) error {
	if snapshot.FormatVersion > CurrentUserCatalogFormatVersion {
		return fmt.Errorf("user catalog format version %d is newer than the supported version %d", snapshot.FormatVersion, CurrentUserCatalogFormatVersion)
	}
	for snapshot.FormatVersion < CurrentUserCatalogFormatVersion {
		migration, ok := userCatalogMigrations[snapshot.FormatVersion]
		if !ok {
			return fmt.Errorf("no migration registered for user catalog format version %d", snapshot.FormatVersion)
		}
		if migration.to <= snapshot.FormatVersion || migration.to > CurrentUserCatalogFormatVersion {
			return fmt.Errorf("invalid user catalog migration from version %d to %d", snapshot.FormatVersion, migration.to)
		}
		if migration.apply != nil {
			if err := migration.apply(snapshot); err != nil {
				return fmt.Errorf("migrate user catalog format version %d to %d: %w", snapshot.FormatVersion, migration.to, err)
			}
		}
		snapshot.FormatVersion = migration.to
	}
	return nil
}

func decodeUserCatalog(data []byte) (map[string]User, uint16, error) {
	var snapshot userCatalogSnapshot
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&snapshot); err == nil {
		sourceVersion := snapshot.FormatVersion
		if err := migrateUserCatalog(&snapshot); err != nil {
			return nil, sourceVersion, err
		}
		if snapshot.Users == nil {
			snapshot.Users = map[string]User{}
		}
		return snapshot.Users, sourceVersion, nil
	}

	var users map[string]User
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&users); err != nil {
		return nil, LegacyUserCatalogFormatVersion, err
	}
	snapshot = userCatalogSnapshot{FormatVersion: LegacyUserCatalogFormatVersion, Users: users}
	if err := migrateUserCatalog(&snapshot); err != nil {
		return nil, LegacyUserCatalogFormatVersion, err
	}
	return snapshot.Users, LegacyUserCatalogFormatVersion, nil
}
