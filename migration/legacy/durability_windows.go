//go:build windows

package legacy

import "golang.org/x/sys/windows"

// Windows does not support the Unix directory-fsync contract. Files are flushed
// separately; use write-through rename and do not allow replacement.
func syncDirectory(string) error { return nil }
func publishDirectory(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}
