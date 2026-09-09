//go:build windows

package main

import (
	"fmt"
	"sync"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
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
	return handler.execute(requests, status, runServerControlled, reportWindowsServiceFailure)
}

// Preserve the startup error: SCM otherwise exposes only service-specific code 1.
func reportWindowsServiceFailure(err error) {
	log, openErr := eventlog.Open(windowsServiceName)
	if openErr != nil {
		return
	}
	defer log.Close()
	_ = log.Error(1000, "GBaseLite service failed: "+err.Error())
}

func (handler *windowsServiceHandler) execute(requests <-chan svc.ChangeRequest, status chan<- svc.Status, run func([]string, <-chan struct{}, chan<- struct{}) error, report func(error)) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	stop := make(chan struct{})
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- run(handler.serverArgs, stop, ready)
	}()

	select {
	case <-ready:
		status <- svc.Status{State: svc.Running, Accepts: accepted}
	case err := <-result:
		if err != nil {
			report(err)
			return true, 1
		}
		return false, 0
	}

	var stopOnce sync.Once
	for {
		select {
		case err := <-result:
			if err != nil {
				report(err)
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
					report(err)
					return true, 1
				}
				return false, 0
			}
		}
	}
}
