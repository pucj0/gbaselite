//go:build !windows

package main

import "time"

var probeEpoch = time.Now()

const probeTimerSource = "Go monotonic clock"

func initializeProbeClock() error           { return nil }
func probeClock() int64                     { return time.Since(probeEpoch).Nanoseconds() }
func probeMilliseconds(start int64) float64 { return float64(probeClock()-start) / 1e6 }
