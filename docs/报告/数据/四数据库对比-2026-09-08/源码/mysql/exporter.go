package mysql

import (
	"gbaselite/executor"
	"gbaselite/storage"
)

func Export(store *storage.Store, database, path string) error {
	return executor.ExportSQL(store, database, path)
}
