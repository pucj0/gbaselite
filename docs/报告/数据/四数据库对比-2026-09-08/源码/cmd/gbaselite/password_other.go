//go:build !windows && !linux

package main

import "os"

func readPassword(file *os.File) (string, error) {
	return readLineFromFile(file)
}
