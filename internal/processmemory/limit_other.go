//go:build !windows

package processmemory

import "fmt"

func LimitWorkingSet(bytes int64) (func() error, error) {
	if bytes == 0 {
		return func() error { return nil }, nil
	}
	return nil, fmt.Errorf("working_set_limit_mb is Windows-only; use Linux cgroup/container memory limits")
}
