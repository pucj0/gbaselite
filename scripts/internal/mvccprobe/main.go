// mvccprobe measures the complete SQL write path in a disposable database.
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
	n := flag.Int("rows", 100000, "rows")
	workingSet := flag.Int64("working-set-mib", 0, "Windows hard working set limit, zero disables")
	flag.Parse()
	restore, err := processmemory.LimitWorkingSet(*workingSet << 20)
	if err != nil {
		return err
	}
	defer restore()
	if *n < 1000 || *n > 1000000 {
		return fmt.Errorf("rows outside 1000..1000000")
	}
	debug.SetMemoryLimit(64 << 20)
	runtime.GOMAXPROCS(2)
	root, err := filepath.Abs(".tmp")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, "mvcc-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	e, err := executor.Open(dir, "root", "probe")
	if err != nil {
		return err
	}
	defer e.Close()
	s := &executor.Session{}
	exec := func(sql string) (*executor.Result, error) { return e.Execute(s, sql) }
	for _, q := range []string{"CREATE DATABASE test", "USE test", "CREATE TABLE items(id BIGINT PRIMARY KEY AUTO_INCREMENT, v INT, payload VARCHAR(128))"} {
		if _, err = exec(q); err != nil {
			return err
		}
	}
	started := time.Now()
	var peak uint64
	for base := 0; base < *n; base += 250 {
		var q strings.Builder
		q.WriteString("INSERT INTO items(v,payload) VALUES")
		for i := base; i < base+250 && i < *n; i++ {
			if i > base {
				q.WriteByte(',')
			}
			fmt.Fprintf(&q, "(%d,'%s')", i, strings.Repeat("x", 128))
		}
		if _, err = exec(q.String()); err != nil {
			return err
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > peak {
			peak = m.HeapAlloc
		}
	}
	writeTime := time.Since(started)
	started = time.Now()
	if _, err = exec("UPDATE items SET v=v+1"); err != nil {
		return err
	}
	updateTime := time.Since(started)
	started = time.Now()
	for i := 0; i < 100; i++ {
		r, err := exec(fmt.Sprintf("SELECT v FROM items WHERE id=%d", 1+i*(*n/100)))
		if err != nil {
			return err
		}
		if len(r.Rows) != 1 {
			return fmt.Errorf("missing point row")
		}
	}
	pointTime := time.Since(started)
	r, err := exec("SELECT COUNT(*) FROM items")
	if err != nil {
		return err
	}
	if r.Rows[0][0] != int64(*n) {
		return fmt.Errorf("count: %v", r.Rows)
	}
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	info, err := os.Stat(filepath.Join(dir, "versioned", "mvcc.db"))
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"working_set_limit_mib": *workingSet, "rows": *n, "payload_bytes": 128, "batch_rows": 250, "go_memory_limit_mib": 64, "gomaxprocs": 2, "insert_ms": float64(writeTime) / float64(time.Millisecond), "update_all_ms": float64(updateTime) / float64(time.Millisecond), "point_mean_ms": float64(pointTime) / float64(time.Millisecond) / 100, "sampled_insert_heap_peak_bytes": peak, "heap_after_gc_bytes": m.HeapAlloc, "database_bytes": info.Size(), "go_version": runtime.Version(), "caveat": "standalone, one client, local filesystem cache; sampled heap is not RSS or an exact peak"})
}
