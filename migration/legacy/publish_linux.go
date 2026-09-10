//go:build linux

package legacy

import "golang.org/x/sys/unix"

// Never replace a target created after validation, including an empty directory.
func publishDirectory(source, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}
