//go:build windows

package main

import (
	"errors"
	"golang.org/x/sys/windows/svc"
	"testing"
)

func TestWindowsServiceReportsFailure(t *testing.T) {
	for _, readyFirst := range []bool{false, true} {
		want := errors.New("test startup failure")
		status := make(chan svc.Status, 8)
		var reported error
		handler := windowsServiceHandler{}
		specific, code := handler.execute(make(chan svc.ChangeRequest), status, func(_ []string, _ <-chan struct{}, ready chan<- struct{}) error {
			if readyFirst {
				close(ready)
			}
			return want
		}, func(err error) { reported = err })
		if !specific || code != 1 || !errors.Is(reported, want) {
			t.Fatalf("specific=%v code=%d error=%v", specific, code, reported)
		}
	}
}
