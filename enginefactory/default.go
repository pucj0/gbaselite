// Package enginefactory is the application composition root for storage.
// SQL operators depend only on storageengine; alternate backends can instead be
// supplied through executor.OpenOptions.BackendFactory or NewWithStorage.
package enginefactory

import (
	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
)

func Open(directory string, options storageengine.Options) (storageengine.Engine, error) {
	return mvccadapter.Open(directory, options)
}
