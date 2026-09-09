package executor

import (
	"fmt"
	"gbaselite/catalog"
	"gbaselite/storage"
)

// loadLegacyForMigration reads only the isolated copy made by MigrateLegacy.
// Format recovery may update that copy; the original instance is never opened.
// There is no legacy transaction executor in production builds.
func loadLegacyForMigration(directory, format string) (*storage.Store, error) {
	var persistence *storage.Persistence
	switch format {
	case "snapshot":
		persistence = storage.NewPersistence(directory)
	case "paged":
		persistence = storage.NewPagedPersistence(directory, 0)
	default:
		return nil, fmt.Errorf("unknown legacy migration format %q", format)
	}
	store, err := persistence.Load()
	if err != nil {
		return nil, err
	}
	// Normalize historical account formats on the copy without creating an admin.
	if _, err := catalog.OpenUsers(directory, "", ""); err != nil {
		return nil, err
	}
	return store, nil
}
