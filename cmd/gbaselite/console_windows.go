//go:build windows

package main

import "golang.org/x/sys/windows"

const utf8ConsoleCodePage = 65001

func configureConsoleEncoding() {
	if _, err := windows.GetConsoleCP(); err == nil {
		_ = windows.SetConsoleCP(utf8ConsoleCodePage)
	}
	if _, err := windows.GetConsoleOutputCP(); err == nil {
		_ = windows.SetConsoleOutputCP(utf8ConsoleCodePage)
	}
}
