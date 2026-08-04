package storage

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SnapshotInspection contains structural metadata only. It never exposes
// database object names, SQL definitions, credentials, or row values.
type SnapshotInspection struct {
	Path                string
	Size                int64
	ModifiedAt          time.Time
	SHA256              string
	SourceFormatVersion uint16
	FormatVersion       uint16
	Databases           int
	Tables              int
	Indexes             int
	Views               int
	Rows                int
}

// InspectSnapshot decodes and validates a snapshot without modifying it.
func InspectSnapshot(path string) (SnapshotInspection, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return SnapshotInspection{}, fmt.Errorf("resolve snapshot path: %w", err)
	}
	file, err := os.Open(absPath)
	if err != nil {
		return SnapshotInspection{}, fmt.Errorf("open snapshot %s: %w", absPath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return SnapshotInspection{}, fmt.Errorf("stat snapshot %s: %w", absPath, err)
	}
	if !info.Mode().IsRegular() {
		return SnapshotInspection{}, fmt.Errorf("snapshot %s is not a regular file", absPath)
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return SnapshotInspection{}, fmt.Errorf("hash snapshot %s: %w", absPath, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return SnapshotInspection{}, fmt.Errorf("rewind snapshot %s: %w", absPath, err)
	}

	snapshot, sourceFormatVersion, err := decodeSnapshot(file)
	if err != nil {
		return SnapshotInspection{}, fmt.Errorf("decode snapshot %s: %w", absPath, err)
	}
	if _, err := newStoreFromSnapshot(snapshot, true); err != nil {
		return SnapshotInspection{}, fmt.Errorf("validate snapshot %s: %w", absPath, err)
	}

	inspection := SnapshotInspection{
		Path:                absPath,
		Size:                info.Size(),
		ModifiedAt:          info.ModTime().UTC(),
		SHA256:              fmt.Sprintf("%x", hash.Sum(nil)),
		SourceFormatVersion: sourceFormatVersion,
		FormatVersion:       snapshot.FormatVersion,
		Databases:           len(snapshot.Databases),
	}
	for _, database := range snapshot.Databases {
		inspection.Tables += len(database.Tables)
		inspection.Views += len(database.Views)
		for _, table := range database.Tables {
			inspection.Indexes += len(table.Indexes)
			inspection.Rows += len(table.Rows)
		}
	}
	return inspection, nil
}
