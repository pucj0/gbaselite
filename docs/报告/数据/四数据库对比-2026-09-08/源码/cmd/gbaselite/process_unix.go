//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureDetached(command *exec.Cmd)        { command.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func terminateProcess(process *os.Process) error { return process.Signal(syscall.SIGTERM) }
func processIsAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}
