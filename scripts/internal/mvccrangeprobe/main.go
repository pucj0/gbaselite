// mvccrangeprobe profiles the complete in-process SQL path in an isolated store.
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
	n := flag.Int("rows", 10000, "isolated row count (100..1000000)")
	repeats := flag.Int("queries", 10, "range query samples")
	batch := flag.Int("queries-per-sample", 1, "queries averaged within each timing sample; use 100 for sub-millisecond paths")
	prefix := flag.String("profile-prefix", "", "optional path prefix for insert/query CPU profiles")
	out := flag.String("output", "", "optional JSON result path")
	flag.Parse()
	if *n < 100 || *n > 1000000 || *repeats < 1 || *batch < 1 || *batch > 10000 {
		return fmt.Errorf("invalid probe size")
	}
	runtime.GOMAXPROCS(2)
	debug.SetMemoryLimit(64 << 20)
	restore, err := processmemory.LimitWorkingSet(64 << 20)
	if err != nil {
		return err
	}
	defer restore()
	root, err := filepath.Abs(".tmp")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, "mvcc-range-probe-")
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
	exec := func(q string) (*executor.Result, error) { return e.Execute(s, q) }
	for _, q := range []string{"CREATE DATABASE test", "USE test", "CREATE TABLE items(id BIGINT PRIMARY KEY,v INT NOT NULL,payload VARCHAR(128) NOT NULL)"} {
		if _, err = exec(q); err != nil {
			return err
		}
	}
	profile := func(phase string) (func(), error) {
		if *prefix == "" {
			return func() {}, nil
		}
		f, err := os.Create(*prefix + "-" + phase + ".pprof")
		if err != nil {
			return nil, err
		}
		if err = pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return nil, err
		}
		return func() { pprof.StopCPUProfile(); f.Close() }, nil
	}
	stop, err := profile("insert")
	if err != nil {
		return err
	}
	start := time.Now()
	for base := 0; base < *n; base += 250 {
		var q strings.Builder
		q.WriteString("INSERT INTO items VALUES")
		for i := base + 1; i <= base+250 && i <= *n; i++ {
			if i > base+1 {
				q.WriteByte(',')
			}
			fmt.Fprintf(&q, "(%d,1,'%s')", i, strings.Repeat("x", 128))
		}
		if _, err = exec(q.String()); err != nil {
			stop()
			return err
		}
	}
	insert := time.Since(start).Seconds()
	stop()
	q := fmt.Sprintf("SELECT id,v FROM items WHERE id >= %d AND id < %d ORDER BY id", *n/2, *n/2+100)
	validate := func() error {
		r, err := exec(q)
		if err != nil {
			return err
		}
		if len(r.Rows) != 100 {
			return fmt.Errorf("range count %d", len(r.Rows))
		}
		for i, row := range r.Rows {
			if row[0] != int64(*n/2+i) || row[1] != int64(1) {
				return fmt.Errorf("unexpected row: %v", row)
			}
		}
		return nil
	}
	if err = validate(); err != nil {
		return err
	}
	stop, err = profile("query")
	if err != nil {
		return err
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	samples := make([]float64, 0, *repeats)
	for i := 0; i < *repeats; i++ {
		start = time.Now()
		for j := 0; j < *batch; j++ {
			if err = validate(); err != nil {
				stop()
				return err
			}
		}
		samples = append(samples, time.Since(start).Seconds()/float64(*batch))
	}
	runtime.ReadMemStats(&after)
	stop()
	result := map[string]any{"rows": *n, "queries": *repeats, "queries_per_sample": *batch, "insert_seconds": insert, "range_seconds": samples, "query": q, "query_allocated_bytes": after.TotalAlloc - before.TotalAlloc, "query_allocations": after.Mallocs - before.Mallocs, "gomaxprocs": 2, "go_memory_mib": 64, "windows_working_set_mib": 64}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err = os.WriteFile(*out, encoded, 0600); err != nil {
			return err
		}
	}
	fmt.Println(string(encoded))
	return nil
}
