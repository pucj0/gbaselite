package main

import (
	"gbaselite/config"
	"runtime"
	"runtime/debug"
)

// Apply only explicit overrides. Zero preserves GOMEMLIMIT/GOMAXPROCS and Go's
// defaults. No forced GC loop: it would trade latency and CPU for lower memory.
func applyResourceSettings(cfg config.Config) func() {
	var previousMemory int64
	var previousProcs int
	if cfg.Resources.MemoryLimitMB > 0 {
		previousMemory = debug.SetMemoryLimit(int64(cfg.Resources.MemoryLimitMB) << 20)
	}
	if cfg.Resources.MaxProcs > 0 {
		previousProcs = runtime.GOMAXPROCS(cfg.Resources.MaxProcs)
	}
	return func() {
		if cfg.Resources.MemoryLimitMB > 0 {
			debug.SetMemoryLimit(previousMemory)
		}
		if cfg.Resources.MaxProcs > 0 {
			runtime.GOMAXPROCS(previousProcs)
		}
	}
}
