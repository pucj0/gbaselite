//go:build windows

package main

import (
	"fmt"
	"golang.org/x/sys/windows"
	"unsafe"
)

var perfCounter = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
var perfFrequency = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
var probeTicksPerSecond int64

const probeTimerSource = "Windows QueryPerformanceCounter"

func initializeProbeClock() error {
	result, _, err := perfFrequency.Call(uintptr(unsafe.Pointer(&probeTicksPerSecond)))
	if result == 0 || probeTicksPerSecond <= 0 {
		return fmt.Errorf("QueryPerformanceFrequency: %v", err)
	}
	return nil
}
func probeClock() int64 {
	var ticks int64
	result, _, err := perfCounter.Call(uintptr(unsafe.Pointer(&ticks)))
	if result == 0 {
		panic(fmt.Sprintf("QueryPerformanceCounter: %v", err))
	}
	return ticks
}
func probeMilliseconds(start int64) float64 {
	return float64(probeClock()-start) * 1000 / float64(probeTicksPerSecond)
}
