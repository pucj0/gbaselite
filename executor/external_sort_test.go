package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gbaselite/storage"
)

func TestLegacyExternalSortSpillStableAndCleanup(t *testing.T) {
	directory := t.TempDir()
	q := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: directory, MaxTempBytes: 16 << 20})
	sorter, err := newExternalRowSorter(q, func(a, b []any) int { return int(a[0].(int64) - b[0].(int64)) })
	if err != nil {
		t.Fatal(err)
	}
	defer sorter.Close()
	// More than 64 runs exercises compaction and bounded merge fan-in.
	const rows = 18000
	scratch := []any{int64(0), int64(0), strings.Repeat("x", 96)}
	for i := 0; i < rows; i++ {
		scratch[0] = int64((rows - i) % 17)
		scratch[1] = int64(i)
		if err = sorter.Add(scratch); err != nil {
			t.Fatal(err)
		}
	}
	if len(sorter.runs) == 0 {
		t.Fatal("test did not spill")
	}
	seen := 0
	previous := int64(-1)
	ordinals := make(map[int64]int64)
	err = sorter.Finish(func(row []any) error {
		key, ordinal := row[0].(int64), row[1].(int64)
		if key < previous {
			t.Fatalf("out of order %d < %d", key, previous)
		}
		if last, ok := ordinals[key]; ok && ordinal <= last {
			t.Fatalf("unstable key %d ordinals %d,%d", key, last, ordinal)
		}
		ordinals[key] = ordinal
		previous = key
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != rows {
		t.Fatalf("got %d rows", seen)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("left temporary files: %v", entries)
	}
	if sorter.tempBytes != 0 {
		t.Fatalf("retained temp accounting: %d", sorter.tempBytes)
	}
}

func TestLegacyExternalSortCodecPreservesSQLValues(t *testing.T) {
	date := time.Date(2026, 9, 8, 12, 34, 56, 123, time.FixedZone("offset", 8*3600))
	original := externalSortRow{[]any{nil, "你好", jsonDocument(`{"a":null}`), []byte{0, 255}, int(-3), int64(math.MinInt64), uint64(math.MaxUint64), 1.25, true, date, storage.Decimal("12345678901234567890.01"), collatedText{Text: "AbC", Collation: "utf8mb4_general_ci"}}, 123}
	var buffer bytes.Buffer
	if err := writeSortRow(&buffer, original); err != nil {
		t.Fatal(err)
	}
	decoded, err := readSortRow(&buffer, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.values[9].(time.Time).Equal(date) {
		t.Fatal("date changed")
	}
	decoded.values[9] = date
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("codec changed values: %#v", decoded)
	}
}

func TestLegacyExternalSortCancellationAndDiskLimitCleanup(t *testing.T) {
	for _, kind := range []string{"canceled", "disk", "yield"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			options := QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: directory}
			if kind == "disk" {
				options.MaxTempBytes = 100
			}
			sorter, err := newExternalRowSorter(newQueryControl(ctx, options), func(a, b []any) int { return int(a[0].(int64) - b[0].(int64)) })
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("client disconnected")
			for i := 0; i < 1000; i++ {
				if err = sorter.Add([]any{int64(1000 - i), strings.Repeat("z", 64)}); err != nil {
					break
				}
			}
			if kind == "canceled" {
				cancel()
			}
			if err == nil {
				err = sorter.Finish(func([]any) error {
					if kind == "yield" {
						return sentinel
					}
					return nil
				})
			}
			if kind == "canceled" && !errors.Is(err, ErrQueryCanceled) {
				t.Fatalf("got %v", err)
			}
			if kind == "disk" && !errors.Is(err, ErrQueryResourceLimit) {
				t.Fatalf("got %v", err)
			}
			if kind == "yield" && !errors.Is(err, sentinel) {
				t.Fatalf("got %v", err)
			}
			if err = sorter.Close(); err != nil {
				t.Fatal(err)
			}
			files, _ := os.ReadDir(directory)
			if len(files) != 0 {
				t.Fatalf("temporary files leaked: %v", files)
			}
		})
	}
}

func TestLegacyExternalSortRejectsWideRowsAndCorruptLengths(t *testing.T) {
	sorter, err := newExternalRowSorter(newQueryControl(nil, QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: t.TempDir()}), func(a, b []any) int { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	defer sorter.Close()
	if err = sorter.Add([]any{strings.Repeat("x", 4096)}); !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("wide row: %v", err)
	}
	var buffer bytes.Buffer
	_ = writeSortUint(&buffer, 0)
	_ = writeSortUint(&buffer, math.MaxUint64)
	if _, err = readSortRow(&buffer, 4096); err == nil {
		t.Fatal("accepted huge column count")
	}
	buffer.Reset()
	_ = writeSortUint(&buffer, 0)
	_ = writeSortUint(&buffer, 1)
	buffer.WriteByte(1)
	_ = writeSortUint(&buffer, math.MaxUint64)
	if _, err = readSortRow(&buffer, 4096); err == nil {
		t.Fatal("accepted huge string length")
	}
	buffer.Reset()
	buffer.Write([]byte{1, 2, 3})
	if _, err = readSortRow(&buffer, 4096); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncation: %v", err)
	}
}

func TestLegacyQueryLockDeadlineDoesNotAcquireLater(t *testing.T) {
	var mutex sync.RWMutex
	mutex.Lock()
	q := newQueryControl(context.Background(), QueryOptions{Timeout: 15 * time.Millisecond})
	start := time.Now()
	err := acquireQueryMutex(q, &mutex, false)
	if !errors.Is(err, ErrQueryTimeout) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("lock timeout did not return promptly")
	}
	mutex.Unlock()
	if !mutex.TryLock() {
		t.Fatal("timed out waiter retained lock")
	}
	mutex.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = acquireQueryMutex(newQueryControl(ctx, QueryOptions{}), &mutex, true); !errors.Is(err, ErrQueryCanceled) {
		t.Fatalf("got %v", err)
	}
}

func TestLegacyQueryResultMemoryLimit(t *testing.T) {
	used, err := checkResultMemory(1024, 0, []any{strings.Repeat("x", 800)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = checkResultMemory(1024, used, []any{strings.Repeat("x", 800)}); !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("got %v", err)
	}
	if _, err = checkResultMemory(0, math.MaxInt64, []any{1}); !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("overflow: %v", err)
	}
}
