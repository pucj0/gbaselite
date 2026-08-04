//go:build linux

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func readPassword(file *os.File) (string, error) {
	descriptor := int(file.Fd())
	mode, err := unix.IoctlGetTermios(descriptor, unix.TCGETS)
	if err != nil {
		return readLineFromFile(file)
	}
	hidden := *mode
	hidden.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(descriptor, unix.TCSETS, &hidden); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(descriptor, unix.TCSETS, mode)
	password, err := readLineFromFile(file)
	fmt.Fprintln(os.Stderr)
	return password, err
}
