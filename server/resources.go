package server

import (
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
