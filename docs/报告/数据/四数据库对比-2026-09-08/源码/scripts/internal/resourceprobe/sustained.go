package main

import (
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// Fixed histograms keep the probe's own memory bounded across long runs.
// Quantiles are upper bucket bounds: 0.25 ms bins, with a >=1024 ms tail.
type latencyHistogram struct {
	Count   int     `json:"count"`
	TotalMS float64 `json:"total_ms"`
	MaxMS   float64 `json:"max_ms"`
	bins    [4097]int
}

func (h *latencyHistogram) add(elapsed time.Duration) {
	ms := float64(elapsed.Nanoseconds()) / 1e6
	h.Count++
	h.TotalMS += ms
	if ms > h.MaxMS {
		h.MaxMS = ms
	}
	bin := int(ms * 4)
	if bin >= len(h.bins) {
		bin = len(h.bins) - 1
	}
	h.bins[bin]++
}
func (h *latencyHistogram) percentile(percent int) float64 {
	target := (h.Count*percent + 99) / 100
	sum := 0
	for bin, n := range h.bins {
		sum += n
		if sum >= target {
			if bin == len(h.bins)-1 {
				return h.MaxMS
			}
			return float64(bin+1) / 4
		}
	}
	return 0
}
func runSustained(db *sql.DB, rows, workers int, duration time.Duration) (map[string]any, error) {
	if _, err := db.Exec("ALTER TABLE resource_probe.parent ADD COLUMN hits INT DEFAULT 0"); err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(workers + 1)
	db.SetMaxIdleConns(workers + 1)
	var mu sync.Mutex
	stats := make(map[string]*latencyHistogram)
	committed := int64(0)
	var firstErr error
	record := func(name string, started time.Time, write bool, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		if stats[name] == nil {
			stats[name] = &latencyHistogram{}
		}
		stats[name].add(time.Since(started))
		if write {
			committed++
		}
	}
	start := time.Now()
	deadline := start.Add(duration)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iteration := 0; time.Now().Before(deadline); iteration++ {
				id := 1 + (worker*7919+iteration*97)%rows
				kind := (iteration + worker) % 4
				name, query := "point", fmt.Sprintf("SELECT payload FROM resource_probe.parent WHERE id=%d", id)
				switch kind {
				case 1:
					name = "range"
					query = fmt.Sprintf("SELECT id FROM resource_probe.parent WHERE grp=%d ORDER BY grp LIMIT 20", id%100)
				case 2:
					name = "topk"
					query = "SELECT id FROM resource_probe.parent ORDER BY hits DESC,id LIMIT 20"
				case 3:
					name = "update"
					query = fmt.Sprintf("UPDATE resource_probe.parent SET hits=hits+1 WHERE id=%d", id)
				}
				at := time.Now()
				var err error
				if kind == 3 {
					_, err = db.Exec(query)
				} else {
					var result *sql.Rows
					result, err = db.Query(query)
					if err == nil {
						for result.Next() {
						}
						err = result.Err()
						_ = result.Close()
					}
				}
				record(name, at, kind == 3, err)
				if err != nil {
					return
				}
			}
		}(worker)
	}
	// A bounded 50 ms lock hold exposes the instance-wide transaction gate.
	// Stop issuing new operations at the deadline; finish in-flight commits so
	// final SUM(hits) can verify every acknowledged mutation exactly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			at := time.Now()
			tx, err := db.Begin()
			if err == nil {
				_, err = tx.Exec("UPDATE resource_probe.parent SET hits=hits+1 WHERE id=1")
				if err == nil {
					time.Sleep(50 * time.Millisecond)
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			record("held_transaction", at, true, err)
			if err != nil {
				return
			}
			pause := time.Until(deadline)
			if pause > time.Second {
				pause = time.Second
			}
			if pause > 0 {
				time.Sleep(pause)
			}
		}
	}()
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	var count int
	var hits int64
	if err := db.QueryRow("SELECT COUNT(*),SUM(hits) FROM resource_probe.parent").Scan(&count, &hits); err != nil {
		return nil, err
	}
	if count != rows || hits != committed {
		return nil, fmt.Errorf("sustained verification: rows %d/%d, hits %d/%d", count, rows, hits, committed)
	}
	summary := make(map[string]any)
	total := 0
	for name, h := range stats {
		summary[name] = map[string]any{"count": h.Count, "mean_ms": h.TotalMS / float64(h.Count), "p95_ms": h.percentile(95), "p99_ms": h.percentile(99), "max_ms": h.MaxMS}
		total += h.Count
	}
	elapsed := time.Since(start).Seconds()
	return map[string]any{"rows": rows, "workers": workers, "requested_seconds": duration.Seconds(), "elapsed_seconds": elapsed, "operations": total, "operations_per_second": float64(total) / elapsed, "acknowledged_writes": committed, "latency": summary, "verified": true}, nil
}
