//go:build windows

package processmemory

import (
	"fmt"
	"golang.org/x/sys/windows"
)

// LimitWorkingSet caps resident process pages, including file mappings. It does
// not cap system-wide file cache or committed/private virtual memory.
func LimitWorkingSet(bytes int64) (func() error, error) {
	if bytes == 0 {
		return func() error { return nil }, nil
	}
	if bytes < 16<<20 || uint64(bytes) > uint64(^uintptr(0)) {
		return nil, fmt.Errorf("working set limit must be at least 16 MiB and fit address space")
	}
	process := windows.CurrentProcess()
	var minimum, maximum uintptr
	var flags uint32
	windows.GetProcessWorkingSetSizeEx(process, &minimum, &maximum, &flags)
	if minimum == 0 || maximum == 0 {
		return nil, fmt.Errorf("cannot read process working set limits")
	}
	restore := func() error { return windows.SetProcessWorkingSetSizeEx(process, minimum, maximum, flags) }
	if err := windows.SetProcessWorkingSetSizeEx(process, 1<<20, uintptr(bytes), 0x2|0x4); err != nil {
		return nil, fmt.Errorf("set hard working set limit: %w", err)
	}
	var actualMin, actualMax uintptr
	var actualFlags uint32
	windows.GetProcessWorkingSetSizeEx(process, &actualMin, &actualMax, &actualFlags)
	if actualFlags&4 == 0 || actualMax > uintptr(bytes) {
		restore()
		return nil, fmt.Errorf("OS did not enforce requested working set maximum")
	}
	return restore, nil
}
