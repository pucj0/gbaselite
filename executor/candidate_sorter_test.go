package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"gbaselite/physical"
)

// This file pins the resource model the unified Modify Pipeline relies on:
//
//   - a candidate's payload really leaves memory when it spills, so a winner is recovered from disk
//     rather than from a side table;
//   - every sorter a statement opens shares one sort-memory pool, so concurrent sorters cannot
//     multiply the configured budget;
//   - a candidate too wide for the pool is refused rather than allocated.

// intCodec is a variable-width candidate codec: the payload value N occupies N bytes. That makes a
// wide candidate expressible without allocating one, which is what the width-limit test needs.
type intCodec struct{}

func (intCodec) encode(value int) ([]byte, error) {
	if value < 0 || value > 1<<20 {
		return nil, fmt.Errorf("int payload %d out of range", value)
	}
	return make([]byte, value), nil
}
func (intCodec) decode(data []byte) (int, error) { return len(data), nil }
func (intCodec) bytes(value int) int             { return value }

// payloadCodec returns the payload a sorter stored for a row.
func payloadCodec(row physical.SortRow[int]) int { return row.Row }

func newTestSorter(t *testing.T, control *queryControl, budget *sorterBudget) physical.Sorter[physical.SortRow[int]] {
	t.Helper()
	sorter, err := newCandidateSorter[int](control, budget, intCodec{}, true)
	if err != nil {
		t.Fatal(err)
	}
	return sorter
}

// TestSorterBudgetBoundsConcurrentSorters verifies the shared pool actually bounds the sum of what
// concurrently live sorters retain: two sorters alive at once charge one pool, not one budget each.
func TestSorterBudgetBoundsConcurrentSorters(t *testing.T) {
	directory := t.TempDir()
	control := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: directory})
	// The pool is the sorter's documented floor, so it is small enough that the sorters spill while
	// sharing it.
	budget := newSorterBudget(64 << 10)
	first := newTestSorter(t, control, budget)
	defer first.Close()
	second := newTestSorter(t, control, budget)
	defer second.Close()

	for i := 0; i < 512; i++ {
		if err := first.Add(physical.SortRow[int]{Key: fmt.Sprintf("a%04d", i), Ordinal: uint64(i), Row: i}); err != nil {
			t.Fatal(err)
		}
		if err := second.Add(physical.SortRow[int]{Key: fmt.Sprintf("b%04d", i), Ordinal: uint64(i), Row: i}); err != nil {
			t.Fatal(err)
		}
		// The pool is the hard bound on what the two sorters retain together.
		if budget.used > budget.total {
			t.Fatalf("pool over-drawn at row %d: used=%d total=%d", i, budget.used, budget.total)
		}
	}
	if budget.used == 0 {
		t.Fatal("two retaining sorters charged nothing to the shared pool")
	}
}

// TestSorterBudgetIsReleasedOnClose verifies a closed sorter returns what it held, so a later sorter
// in the same statement can use the pool rather than spilling needlessly.
func TestSorterBudgetIsReleasedOnClose(t *testing.T) {
	control := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: t.TempDir()})
	budget := newSorterBudget(1 << 20)
	first := newTestSorter(t, control, budget)
	for i := 0; i < 8; i++ {
		if err := first.Add(physical.SortRow[int]{Key: fmt.Sprint(i), Ordinal: uint64(i), Row: i}); err != nil {
			t.Fatal(err)
		}
	}
	if budget.used == 0 {
		t.Fatal("a retaining sorter charged nothing to the pool")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if budget.used != 0 {
		t.Fatalf("pool still holds %d bytes after Close", budget.used)
	}
}

// TestSorterRejectsCandidateWiderThanThePool verifies an over-wide candidate is refused with the
// resource-limit error instead of being allocated outside the budget. A candidate payload is not a
// way around the memory control.
func TestSorterRejectsCandidateWiderThanThePool(t *testing.T) {
	directory := t.TempDir()
	control := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: directory})
	budget := newSorterBudget(64 << 10)
	sorter := newTestSorter(t, control, budget)
	defer sorter.Close()
	// Wider than the row limit the pool implies, so it could not be read back inside the budget.
	tooWide := int(budget.rowLimit()) + 1
	err := sorter.Add(physical.SortRow[int]{Key: "k", Ordinal: 0, Row: tooWide})
	if !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("over-wide payload error = %v, want ErrQueryResourceLimit", err)
	}
	// The same sorter still accepts a candidate that fits.
	if err = sorter.Add(physical.SortRow[int]{Key: "k", Ordinal: 1, Row: 8}); err != nil {
		t.Fatalf("a fitting candidate was refused: %v", err)
	}
}

// TestCandidateSorterRoundTripsPayloadThroughSpill verifies the payload really travels through the
// sorter: with a small budget the sorter spills to run files, and every value handed back is the one
// written.
func TestCandidateSorterRoundTripsPayloadThroughSpill(t *testing.T) {
	directory := t.TempDir()
	control := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: directory})
	budget := newSorterBudget(64 << 10)
	sorter, err := newCandidateSorter[int](control, budget, intCodec{}, false)
	if err != nil {
		t.Fatal(err)
	}
	const rows = 400
	for i := 0; i < rows; i++ {
		if err := sorter.Add(physical.SortRow[int]{Key: fmt.Sprintf("k%03d", i), Ordinal: uint64(i), Row: 1 + i%251}); err != nil {
			t.Fatal(err)
		}
	}
	var got []int
	if err := sorter.Finish(func(row physical.SortRow[int]) error {
		got = append(got, row.Row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != rows {
		t.Fatalf("round-tripped %d rows, want %d", len(got), rows)
	}
	for i, value := range got {
		if value != 1+i%251 {
			t.Fatalf("row %d payload = %d, want %d", i, value, 1+i%251)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}
