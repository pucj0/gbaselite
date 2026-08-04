//go:build !windows

package main

import "fmt"

func runWindowsService(_ []string) error {
	return fmt.Errorf("the service command is only available on Windows")
}
