package server

import (
	"gbaselite/storage"
	"runtime/metrics"
	"strconv"
)

// Sample only on SHOW STATUS; there is no background polling goroutine and
// no forced garbage collection. Runtime-managed bytes are not process RSS.
func resourceStatusRows() [][]any {
	samples := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/gc/gomemlimit:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/sched/gomaxprocs:threads"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
	}
	metrics.Read(samples)
	uintValue := func(i int) uint64 {
		if samples[i].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return samples[i].Value.Uint64()
	}
	total, released := uintValue(0), uintValue(1)
	managed := uint64(0)
	if total > released {
		managed = total - released
	}
	rows := [][]any{
		{"Gbaselite_go_managed_bytes", strconv.FormatUint(managed, 10)},
		{"Gbaselite_heap_bytes", strconv.FormatUint(uintValue(2), 10)},
		{"Gbaselite_heap_released_bytes", strconv.FormatUint(released, 10)},
		{"Gbaselite_memory_limit_bytes", strconv.FormatUint(uintValue(3), 10)},
		{"Gbaselite_gc_cycles", strconv.FormatUint(uintValue(4), 10)},
		{"Gbaselite_goroutines", strconv.FormatUint(uintValue(5), 10)},
		{"Gbaselite_max_procs", strconv.FormatUint(uintValue(6), 10)},
	}
	if samples[7].Value.Kind() == metrics.KindFloat64 {
		rows = append(rows, []any{"Gbaselite_gc_cpu_seconds", strconv.FormatFloat(samples[7].Value.Float64(), 'f', 6, 64)})
	}
	return rows
}

// Only the persisted-page cache payload is counted here. Decoded rows and the
// process working set are separate from this explicitly bounded cache.
func pagedResourceStatusRows(stats storage.PagePersistenceStats, cold bool) [][]any {
	mode := "snapshot"
	if stats.Enabled {
		mode = "paged"
	}
	return [][]any{
		{"Gbaselite_storage_mode", mode},
		{"Gbaselite_cold_reads", strconv.FormatBool(cold)},
		{"Gbaselite_page_generation", strconv.FormatUint(stats.Generation, 10)},
		{"Gbaselite_page_cache_bytes", strconv.FormatInt(stats.CacheBytes, 10)},
		{"Gbaselite_page_cache_budget_bytes", strconv.FormatInt(stats.CacheBudgetBytes, 10)},
		{"Gbaselite_page_cache_hits", strconv.FormatUint(stats.CacheHits, 10)},
		{"Gbaselite_page_cache_misses", strconv.FormatUint(stats.CacheMisses, 10)},
		{"Gbaselite_pages_written", strconv.FormatUint(stats.PagesWritten, 10)},
		{"Gbaselite_page_bytes_written", strconv.FormatUint(stats.PageBytesWritten, 10)},
		{"Gbaselite_wal_bytes_written", strconv.FormatUint(stats.WALBytesWritten, 10)},
		{"Gbaselite_checkpoints", strconv.FormatUint(stats.Checkpoints, 10)},
		{"Gbaselite_pages_reclaimed", strconv.FormatUint(stats.PagesReclaimed, 10)},
		{"Gbaselite_disk_index_builds", strconv.FormatUint(stats.DiskIndexBuilds, 10)},
		{"Gbaselite_disk_index_bytes_written", strconv.FormatUint(stats.DiskIndexBytesWritten, 10)},
	}
}
