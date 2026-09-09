//go:build windows

package atomicfile

import (
	"errors"
	"golang.org/x/sys/windows"
	"time"
)

// Replace atomically replaces target and asks Windows to flush the operation.
func Replace(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return retryWindowsReplace(func() error {
		return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	}, time.Sleep)
}

// Retry the same atomic operation on a possibly transient sharing/access
// conflict. Do not delete the destination or re-encode/reopen the source.
// Persistent permission failures and all other errors still reach the caller.
func retryWindowsReplace(operation func() error, pause func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		err := operation()
		if err == nil || attempt >= 5 || (!errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION)) {
			return err
		}
		pause(time.Millisecond << attempt)
	}
}
