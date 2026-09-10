//go:build !linux && !windows

package legacy

import "fmt"

// Atomic no-replace publication is implemented for the supported deployment OSes.
func publishDirectory(source, target string) error {
	return fmt.Errorf("legacy migration publication is supported on Linux and Windows")
}
