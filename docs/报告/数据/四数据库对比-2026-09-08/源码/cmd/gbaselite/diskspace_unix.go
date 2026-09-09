//go:build !windows

package main

import "golang.org/x/sys/unix"

type diagnosticDiskSpaceInfo struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

func readDiagnosticDiskSpace(path string) (diagnosticDiskSpaceInfo, error) {
	var status unix.Statfs_t
	if err := unix.Statfs(path, &status); err != nil {
		return diagnosticDiskSpaceInfo{}, err
	}
	blockSize := uint64(status.Bsize)
	return diagnosticDiskSpaceInfo{
		TotalBytes:     uint64(status.Blocks) * blockSize,
		AvailableBytes: uint64(status.Bavail) * blockSize,
	}, nil
}
