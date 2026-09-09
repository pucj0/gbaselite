package main

import (
	"gbaselite/config"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestResourceOverridesAndRestore(t *testing.T) {
	initialMemory := debug.SetMemoryLimit(-1)
	initialProcs := runtime.GOMAXPROCS(0)
	cfg := config.Default()
	restore := applyResourceSettings(cfg)
	if debug.SetMemoryLimit(-1) != initialMemory || runtime.GOMAXPROCS(0) != initialProcs {
		t.Fatal("zero overrides changed runtime")
	}
	restore()
	cfg.Resources.MemoryLimitMB = 256
	cfg.Resources.MaxProcs = 2
	restore = applyResourceSettings(cfg)
	defer restore()
	if debug.SetMemoryLimit(-1) != 256<<20 || runtime.GOMAXPROCS(0) != 2 {
		t.Fatal("overrides not applied")
	}
	restore()
	if debug.SetMemoryLimit(-1) != initialMemory || runtime.GOMAXPROCS(0) != initialProcs {
		t.Fatal("runtime not restored")
	}
}
