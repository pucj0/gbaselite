//go:build windows

package main

import (
	"fmt"
	"sync"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "GBaseLite"

type windowsServiceHandler struct {
	serverArgs []string
}

func runWindowsService(args []string) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service context: %w", err)
	}
	if !isService {
		return fmt.Errorf("the service command must be started by Windows Service Control Manager")
	}
	return svc.Run(windowsServiceName, &windowsServiceHandler{serverArgs: args})
}

func (handler *windowsServiceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	stop := make(chan struct{})
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- runServerControlled(handler.serverArgs, stop, ready)
	}()

	select {
	case <-ready:
		status <- svc.Status{State: svc.Running, Accepts: accepted}
	case err := <-result:
		if err != nil {
			return true, 1
		}
		return false, 0
	}

	var stopOnce sync.Once
	for {
		select {
		case err := <-result:
			if err != nil {
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				stopOnce.Do(func() { close(stop) })
				if err := <-result; err != nil {
					return true, 1
				}
				return false, 0
			}
		}
	}
}
