//go:build windows

package storage

// atomicfile.Replace uses MoveFileEx with MOVEFILE_WRITE_THROUGH on Windows.
func syncPageDirectory(path string) error { return nil }
