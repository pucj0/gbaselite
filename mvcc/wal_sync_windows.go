//go:build windows

package mvcc

// atomicfile.Replace uses MOVEFILE_WRITE_THROUGH on Windows.
func syncWALDirectory(path string) error { return nil }
