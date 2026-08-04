//go:build windows

package main

import "golang.org/x/sys/windows"

type diagnosticDiskSpaceInfo struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

func readDiagnosticDiskSpace(path string) (diagnosticDiskSpaceInfo, error) {
	directory, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return diagnosticDiskSpaceInfo{}, err
	}
	var available uint64
	var total uint64
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(directory, &available, &total, &free); err != nil {
		return diagnosticDiskSpaceInfo{}, err
	}
	return diagnosticDiskSpaceInfo{TotalBytes: total, AvailableBytes: available}, nil
}
