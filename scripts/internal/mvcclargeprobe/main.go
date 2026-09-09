// mvcclargeprobe verifies wide atomic updates and restart contents in an isolated store.
package main

import (
	"context"
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
	"sync"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	rows := flag.Int("rows", 100000, "row count")
	width := flag.Int("payload-bytes", 3072, "ASCII payload bytes per row")
	out := flag.String("output", ".tmp/mvcc-large/wide.json", "result JSON")
	flag.Parse()
	if *rows < 1 || *rows > 1000000 || *width < 1 || *width > 4000 {
		return fmt.Errorf("invalid size")
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
	dir, err := os.MkdirTemp(root, "mvcc-large-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var e *executor.Engine
	open := func() error {
		var err error
		e, err = executor.OpenWithOptions(dir, "root", "probe", executor.OpenOptions{StorageMode: "mvcc"})
		return err
	}
	if err = open(); err != nil {
		return err
	}
	defer func() {
		if e != nil {
			e.Close()
		}
	}()
	s := &executor.Session{Context: context.Background()}
	exec := func(session *executor.Session, q string) (*executor.Result, error) { return e.Execute(session, q) }
	for _, q := range []string{"CREATE DATABASE test", "USE test", fmt.Sprintf("CREATE TABLE items(id BIGINT PRIMARY KEY,v INT NOT NULL,payload VARCHAR(%d) NOT NULL)", *width)} {
		if _, err = exec(s, q); err != nil {
			return err
		}
	}
	payload := strings.Repeat("x", *width)
	start := time.Now()
	for base := 1; base <= *rows; base += 250 {
		var b strings.Builder
		b.WriteString("INSERT INTO items VALUES")
		for i := base; i <= *rows && i < base+250; i++ {
			if i > base {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d,1,'%s')", i, payload)
		}
		if _, err = exec(s, b.String()); err != nil {
			return err
		}
	}
	insertSeconds := time.Since(start).Seconds()
	old := &executor.Session{CurrentDatabase: "test"}
	if _, err = exec(old, "BEGIN"); err != nil {
		return err
	}
	defer func() {
		if e != nil {
			e.CloseSession(old)
		}
	}()
	var peakHeap, peakSys uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			peakHeap = max(peakHeap, m.HeapAlloc)
			peakSys = max(peakSys, m.Sys-m.HeapReleased)
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	start = time.Now()
	updated, updateErr := exec(s, "UPDATE items SET v=v+1")
	updateSeconds := time.Since(start).Seconds()
	close(done)
	wg.Wait()
	if updateErr != nil {
		return updateErr
	}
	if updated.AffectedRows != uint64(*rows) {
		return fmt.Errorf("affected rows %d", updated.AffectedRows)
	}
	oldResult, err := exec(old, "SELECT COUNT(*),SUM(v) FROM items")
	if err != nil {
		return err
	}
	if fmt.Sprint(oldResult.Rows) != fmt.Sprintf("[[%d %d]]", *rows, *rows) {
		return fmt.Errorf("old snapshot changed: %v", oldResult.Rows)
	}
	if _, err = exec(old, "ROLLBACK"); err != nil {
		return err
	}
	verify := func() error {
		for base := 1; base <= *rows; base += 1000 {
			result, err := exec(s, fmt.Sprintf("SELECT id,v,payload FROM items WHERE id>=%d AND id<%d ORDER BY id", base, min(base+1000, *rows+1)))
			if err != nil {
				return err
			}
			if len(result.Rows) != min(1000, *rows-base+1) {
				return fmt.Errorf("row count at %d", base)
			}
			for i, row := range result.Rows {
				if row[0] != int64(base+i) || row[1] != int64(2) || row[2] != payload {
					return fmt.Errorf("content mismatch at %d", base+i)
				}
			}
		}
		return nil
	}
	if err = verify(); err != nil {
		return err
	}
	if err = e.Close(); err != nil {
		return err
	}
	e = nil
	if err = open(); err != nil {
		return err
	}
	s = &executor.Session{CurrentDatabase: "test"}
	if err = verify(); err != nil {
		return err
	}
	pending, err := e.MVCC.PendingBytes()
	if err != nil || pending != 0 {
		return fmt.Errorf("pending bytes %d: %v", pending, err)
	}
	result := map[string]any{"rows": *rows, "payload_bytes": *width, "payload_write_set_lower_bound_bytes": int64(*rows) * int64(*width), "insert_seconds": insertSeconds, "update_seconds": updateSeconds, "affected_rows": updated.AffectedRows, "old_snapshot_verified": true, "full_contents_verified": true, "full_contents_after_reopen_verified": true, "pending_bytes_after_reopen": pending, "update_go_heap_peak_bytes": peakHeap, "update_go_managed_peak_bytes": peakSys, "sample_interval_ms": 100, "gomaxprocs": 2, "go_memory_target_mib": 64, "windows_working_set_mib": 64, "method": "in-process SQL correctness probe, not TCP performance comparison"}
	if err = e.Close(); err != nil {
		return err
	}
	e = nil
	if err = os.RemoveAll(dir); err != nil {
		return err
	}
	result["temporary_database_removed"] = true
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(*out, encoded, 0600); err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}
