// mvccperfprobe profiles isolated SQL workloads without touching installed services.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"gbaselite/executor"
	"gbaselite/internal/processmemory"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	rows := flag.Int("rows", 100000, "isolated row count (100..1000000)")
	output := flag.String("output", ".tmp/mvcc-perf/probe.json", "JSON output")
	profiles := flag.Bool("profiles", false, "write CPU profiles beside output")
	flag.Parse()
	if *rows < 100 || *rows > 1000000 {
		return fmt.Errorf("invalid row count")
	}
	runtime.GOMAXPROCS(2)
	debug.SetMemoryLimit(64 << 20)
	restore, err := processmemory.LimitWorkingSet(64 << 20)
	if err != nil {
		return err
	}
	defer restore()
	if err = os.MkdirAll(filepath.Dir(*output), 0700); err != nil {
		return err
	}
	if err = os.MkdirAll(".tmp", 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(".tmp", "mvcc-perf-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	e, err := executor.Open(dir, "root", "probe")
	if err != nil {
		return err
	}
	defer e.Close()
	session := &executor.Session{}
	exec := func(q string) (*executor.Result, error) { return e.Execute(session, q) }
	for _, q := range []string{"CREATE DATABASE bench", "USE bench", "CREATE TABLE items(id BIGINT PRIMARY KEY,v INT NOT NULL,payload VARCHAR(128) NOT NULL)"} {
		if _, err = exec(q); err != nil {
			return err
		}
	}
	result := map[string]any{"rows": *rows, "go_memory_mib": 64, "windows_working_set_mib": 64, "gomaxprocs": 2, "access": "in-process SQL", "profiles": *profiles}
	phase := func(name string, fn func() error) error {
		var f *os.File
		if *profiles {
			f, err = os.Create(*output + "-" + name + ".pprof")
			if err != nil {
				return err
			}
			if err = pprof.StartCPUProfile(f); err != nil {
				f.Close()
				return err
			}
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		phaseErr := fn()
		elapsed := time.Since(start).Seconds()
		runtime.ReadMemStats(&after)
		if f != nil {
			pprof.StopCPUProfile()
			f.Close()
		}
		result[name] = map[string]any{"seconds": elapsed, "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "allocations": after.Mallocs - before.Mallocs}
		return phaseErr
	}
	if err = phase("insert", func() error {
		payload := strings.Repeat("x", 128)
		for base := 0; base < *rows; base += 250 {
			var q strings.Builder
			q.WriteString("INSERT INTO items VALUES")
			for i := base + 1; i <= base+250 && i <= *rows; i++ {
				if i > base+1 {
					q.WriteByte(',')
				}
				fmt.Fprintf(&q, "(%d,1,'%s')", i, payload)
			}
			if _, err := exec(q.String()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	validate := func(value int64) error {
		r, err := exec("SELECT COUNT(*),SUM(v) FROM items")
		if err != nil {
			return err
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(*rows) || r.Rows[0][1] != int64(*rows)*value {
			return fmt.Errorf("unexpected aggregate %v", r.Rows)
		}
		return nil
	}
	if err = validate(1); err != nil {
		return err
	}
	if err = phase("aggregate", func() error {
		for i := 0; i < 5; i++ {
			if err := validate(1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err = phase("update", func() error {
		r, err := exec("UPDATE items SET v=v+1")
		if err != nil {
			return err
		}
		if r.AffectedRows != uint64(*rows) {
			return fmt.Errorf("affected rows %d", r.AffectedRows)
		}
		return nil
	}); err != nil {
		return err
	}
	if err = validate(2); err != nil {
		return err
	}
	result["verified"] = true
	result["aggregate_queries"] = 5
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*output, encoded, 0600)
}
