//go:build !windows

package atomicfile

import "os"

// Replace atomically replaces target with source on the same filesystem.
func Replace(source, target string) error {
	return os.Rename(source, target)
}
