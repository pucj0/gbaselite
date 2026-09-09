// coldprobe measures one isolated paged database in a fresh process.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"gbaselite/executor"
	"gbaselite/storage"
)

type heapReport struct {
	Alloc      uint64 `json:"alloc_bytes"`
	Inuse      uint64 `json:"inuse_bytes"`
	Sys        uint64 `json:"sys_bytes"`
	Released   uint64 `json:"released_bytes"`
	Objects    uint64 `json:"objects"`
	TotalAlloc uint64 `json:"total_alloc_bytes"`
	NumGC      uint32 `json:"num_gc"`
}
type queryReport struct {
	Name    string  `json:"name"`
	Queries int     `json:"queries"`
	Rows    int     `json:"validated_rows"`
	TotalMS float64 `json:"total_ms"`
	MeanMS  float64 `json:"mean_ms"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	MaxMS   float64 `json:"max_ms"`
}
type report struct {
	TimerSource    string                       `json:"timer_source"`
	Phase          string                       `json:"phase"`
	Mode           string                       `json:"mode"`
	PID            int                          `json:"pid"`
	Rows           int                          `json:"rows"`
	PayloadBytes   int                          `json:"payload_bytes"`
	PageCacheBytes int64                        `json:"page_cache_budget_bytes"`
	GoVersion      string                       `json:"go_version"`
	LoadMS         float64                      `json:"load_ms"`
	Cold           bool                         `json:"table_remains_cold"`
	HeapBeforeGC   heapReport                   `json:"heap_before_gc"`
	HeapAfterGC    heapReport                   `json:"heap_after_gc"`
	Queries        []queryReport                `json:"queries,omitempty"`
	Pages          storage.PagePersistenceStats `json:"pages"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func readHeap() heapReport {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return heapReport{m.HeapAlloc, m.HeapInuse, m.HeapSys, m.HeapReleased, m.HeapObjects, m.TotalAlloc, m.NumGC}
}
func emit(result report) error { return json.NewEncoder(os.Stdout).Encode(result) }
func payload(id int) string    { return fmt.Sprintf("%08d:", id) + strings.Repeat("x", 247) }
func run() error {
	directory := flag.String("directory", "", "isolated database below .tmp")
	mode := flag.String("mode", "cold", "seed, full, or cold")
	rows := flag.Int("rows", 100000, "seed and expected row count")
	iterations := flag.Int("iterations", 250, "point query iterations; range uses one fifth")
	cache := flag.Int64("cache-bytes", 4<<20, "encoded page cache budget")
	hold := flag.Duration("hold", 2*time.Second, "sampling hold at each report phase")
	flag.Parse()
	if err := initializeProbeClock(); err != nil {
		return err
	}
	if *directory == "" || *rows < 1000 || *rows > 1000000 || *iterations < 10 || *iterations > 10000 || *cache < 0 || *hold < 0 || *hold > 10*time.Second {
		return fmt.Errorf("invalid probe bounds")
	}
	absolute, err := filepath.Abs(*directory)
	if err != nil {
		return err
	}
	workspace, err := os.Getwd()
	if err != nil {
		return err
	}
	tmp := filepath.Join(workspace, ".tmp")
	relative, err := filepath.Rel(tmp, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("probe directory must be strictly below workspace .tmp")
	}
	if *mode != "seed" && *mode != "full" && *mode != "cold" {
		return fmt.Errorf("invalid mode")
	}
	if *mode == "seed" {
		if entries, err := os.ReadDir(absolute); err == nil && len(entries) > 0 {
			return fmt.Errorf("seed requires a new empty isolated directory")
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	start := probeClock()
	engine, err := executor.OpenWithOptions(absolute, "root", "cold-probe-only", executor.OpenOptions{StorageMode: "paged", PageCacheBytes: *cache, ColdRead: *mode == "cold", ColdMaterializeBytes: 1 << 20})
	if err != nil {
		return err
	}
	loadMS := probeMilliseconds(start)
	if *mode == "seed" {
		database, err := engine.Store.CreateDatabase("cold_probe")
		if err != nil {
			return err
		}
		table, err := database.CreateTable("items", []storage.Column{{Name: "id", Type: storage.TypeBigInt, SQLType: "bigint", MetadataVersion: 1}, {Name: "payload", Type: storage.TypeVarchar, SQLType: "varchar(256)", Length: 256, MetadataVersion: 1}})
		if err != nil {
			return err
		}
		if err = table.AddPrimaryKey([]string{"id"}); err != nil {
			return err
		}
		for i := 0; i < *rows; i++ {
			if err = table.Insert(storage.Row{storage.MustValue(storage.TypeBigInt, int64(i)), storage.MustValue(storage.TypeVarchar, payload(i))}); err != nil {
				return err
			}
		}
		if err = engine.Persistence.Save(engine.Store); err != nil {
			return err
		}
		return emit(report{Phase: "seeded", Mode: *mode, PID: os.Getpid(), Rows: *rows, PayloadBytes: 256, PageCacheBytes: *cache, GoVersion: runtime.Version(), TimerSource: probeTimerSource, LoadMS: probeMilliseconds(start), Pages: engine.Persistence.PagedStats()})
	}
	database, err := engine.Store.Database("cold_probe")
	if err != nil {
		return err
	}
	table, err := database.Table("items")
	if err != nil {
		return err
	}
	if table.RowCount() != *rows {
		return fmt.Errorf("expected %d rows, got %d", *rows, table.RowCount())
	}
	makeReport := func(phase string, queries []queryReport) report {
		before := readHeap()
		runtime.GC()
		return report{Phase: phase, Mode: *mode, PID: os.Getpid(), Rows: *rows, PayloadBytes: 256, PageCacheBytes: *cache, GoVersion: runtime.Version(), TimerSource: probeTimerSource, LoadMS: loadMS, Cold: table.IsCold(), HeapBeforeGC: before, HeapAfterGC: readHeap(), Queries: queries, Pages: engine.Persistence.PagedStats()}
	}
	if err = emit(makeReport("loaded", nil)); err != nil {
		return err
	}
	time.Sleep(*hold)
	if err = emit(report{Phase: "running", Mode: *mode, PID: os.Getpid(), TimerSource: probeTimerSource}); err != nil {
		return err
	}
	session := &executor.Session{CurrentDatabase: "cold_probe", StreamResults: true}
	runQuery := func(sql string, first, want int, countOnly bool) (int, error) {
		result, err := engine.Execute(session, sql)
		if err != nil {
			return 0, err
		}
		seen := 0
		consume := func(values []any) error {
			if countOnly {
				if len(values) != 1 || values[0] != int64(want) {
					return fmt.Errorf("%s: invalid count %#v", sql, values)
				}
			} else {
				expected := first + seen
				if len(values) != 2 || values[0] != int64(expected) || values[1] != payload(expected) {
					return fmt.Errorf("%s: invalid row %d %#v", sql, seen, values)
				}
			}
			seen++
			return nil
		}
		for _, row := range result.Rows {
			if err = consume(row); err != nil {
				return 0, err
			}
		}
		if result.StreamRows != nil {
			if err = result.StreamRows(consume); err != nil {
				return 0, err
			}
		}
		if result.StreamValues != nil {
			if err = result.StreamValues(func(row storage.Row) error {
				values := make([]any, len(row))
				for i, v := range row {
					values[i] = v.Interface()
				}
				return consume(values)
			}); err != nil {
				return 0, err
			}
		}
		expected := want
		if countOnly {
			expected = 1
		}
		if seen != expected {
			return 0, fmt.Errorf("%s: expected %d returned rows, got %d", sql, expected, seen)
		}
		return seen, nil
	}
	measure := func(name string, n int, query func(int) (string, int, int, bool)) (queryReport, error) {
		times := make([]float64, n)
		result := queryReport{Name: name, Queries: n}
		for i := 0; i < n; i++ {
			sql, first, want, countOnly := query(i)
			start := probeClock()
			validated, err := runQuery(sql, first, want, countOnly)
			if err != nil {
				return result, err
			}
			times[i] = probeMilliseconds(start)
			result.TotalMS += times[i]
			result.Rows += validated
		}
		slices.Sort(times)
		result.MeanMS = result.TotalMS / float64(n)
		result.P50MS = times[(n-1)*50/100]
		result.P95MS = times[(n-1)*95/100]
		result.MaxMS = times[n-1]
		return result, nil
	}
	var queries []queryReport
	for _, workload := range []struct {
		name  string
		n     int
		query func(int) (string, int, int, bool)
	}{
		{"primary_key_point", *iterations, func(i int) (string, int, int, bool) {
			id := (i*7919 + *rows/2) % *rows
			return fmt.Sprintf("SELECT id,payload FROM items WHERE id=%d", id), id, 1, false
		}},
		{"indexed_range_limit", max(10, *iterations/5), func(i int) (string, int, int, bool) {
			first := (i*7919 + *rows/3) % (*rows - 128)
			return fmt.Sprintf("SELECT id,payload FROM items WHERE id>=%d AND id<%d ORDER BY id LIMIT 16", first, first+128), first, 16, false
		}},
		{"count_all", max(10, *iterations/5), func(int) (string, int, int, bool) { return "SELECT COUNT(*) FROM items", 0, *rows, true }},
		{"count_indexed_range", max(10, *iterations/5), func(i int) (string, int, int, bool) {
			first := (i*7919 + *rows/4) % (*rows - 128)
			return fmt.Sprintf("SELECT COUNT(*) FROM items WHERE id>=%d AND id<%d", first, first+128), 0, 128, true
		}},
	} {
		result, err := measure(workload.name, workload.n, workload.query)
		if err != nil {
			return err
		}
		queries = append(queries, result)
	}
	if *mode == "cold" && !table.IsCold() {
		return fmt.Errorf("cold workload unexpectedly materialized the table")
	}
	if err = emit(makeReport("queried", queries)); err != nil {
		return err
	}
	time.Sleep(*hold)
	// Read probes deliberately do not call Engine.Close: Close persists a snapshot,
	// which would mix write/maintenance work into this read-only measurement.
	return nil
}
