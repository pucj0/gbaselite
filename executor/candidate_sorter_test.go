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

// countingCodec is intCodec plus a record of how often encode ran, so a test can prove an over-wide
// candidate is refused *before* anything is encoded for it. encodeCalls moves only inside encode, so
// a candidate that never reaches encode leaves it at zero.
//
// It is a pointer type on purpose: the codec is shared with the sorter, and the count has to be
// observable after Add returns.
type countingCodec struct {
	encodeCalls int
	// declineEncode, when set, makes encode panic. A test that expects the preflight to reject a
	// candidate uses it so a missed preflight fails loudly instead of quietly allocating.
	declineEncode bool
	// declaredWidth overrides what bytes reports, so a test can claim a width the real payload would
	// not have. Zero means "report the true width".
	declaredWidth int
}

func (c *countingCodec) encode(value int) ([]byte, error) {
	c.encodeCalls++
	if c.declineEncode {
		panic("codec.encode was called for a candidate that should have been refused")
	}
	return intCodec{}.encode(value)
}

func (c *countingCodec) decode(data []byte) (int, error) { return intCodec{}.decode(data) }

func (c *countingCodec) bytes(value int) int {
	if c.declaredWidth > 0 {
		return c.declaredWidth
	}
	return intCodec{}.bytes(value)
}

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

// TestCandidateSorterRefusesOverWideCandidateBeforeEncoding is the preflight contract: a candidate
// whose declared width exceeds the sorter's row limit must be rejected without the codec ever being
// asked to allocate its payload.
//
// The codec panics if encode runs, so a missing or late preflight fails this test loudly rather than
// quietly allocating a large payload first.
func TestCandidateSorterRefusesOverWideCandidateBeforeEncoding(t *testing.T) {
	directory := t.TempDir()
	control := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: directory})
	budget := newSorterBudget(64 << 10)
	codec := &countingCodec{declineEncode: true, declaredWidth: int(budget.rowLimit()) + 1}
	sorter, err := newCandidateSorter[int](control, budget, codec, true)
	if err != nil {
		t.Fatal(err)
	}
	defer sorter.Close()

	err = sorter.Add(physical.SortRow[int]{Key: "ledger-key", Ordinal: 7, Row: 0})
	if !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("over-wide candidate error = %v, want ErrQueryResourceLimit", err)
	}
	if codec.encodeCalls != 0 {
		t.Fatalf("codec.encode ran %d times for an over-wide candidate, want 0", codec.encodeCalls)
	}
	// Nothing was retained, so closing leaves nothing behind.
	if entries, readErr := os.ReadDir(directory); readErr != nil || len(entries) != 0 {
		t.Fatalf("over-wide rejection left temporary files: %v (%v)", entries, readErr)
	}
}

// candidateRowWidth is the serialized width of the row the candidate sorter builds for a payload of
// the given length, derived from the sorter's own size helper rather than from arithmetic repeated in
// the test.
func candidateRowWidth(key string, ordinal uint64, payloadLen int) int64 {
	return serializedRowBytes([]any{key, ordinalLedgerKey(ordinal), make([]byte, payloadLen)})
}

// widthProbeCodec declares the payload width it will encode and then really encodes a payload of
// exactly that size, so a test can place a candidate precisely on the sorter's row limit.
//
// declaredPayload and payloadLen are separate on purpose: a test can lie about the declared width
// (to exercise the sorter's own defensive check) while still encoding a real payload.
type widthProbeCodec struct {
	declaredPayload int
	payloadLen      int
	encodeCalls     int
}

func (c *widthProbeCodec) encode(int) ([]byte, error) {
	c.encodeCalls++
	return make([]byte, c.payloadLen), nil
}

func (c *widthProbeCodec) decode(data []byte) (int, error) { return len(data), nil }

func (c *widthProbeCodec) bytes(int) int { return c.declaredPayload }

// TestCandidateSorterPreflightIsCalibratedAtTheLimit verifies the width the candidate sorter
// preflights agrees with the width the external sorter computes for the row it actually receives, and
// that the preflight — not the sorter's later defensive check — is what stops an over-wide candidate.
//
// The probe declares a payload width and encodes exactly that many bytes, so the declared row width
// and the real one are the same number. Both cases sit either side of the boundary:
//
//   - a candidate whose serialized row is exactly the limit is accepted and encoded;
//   - a candidate one byte over the limit is refused, and encode never runs — which is only possible
//     if the preflight rejected it before the payload was built.
func TestCandidateSorterPreflightIsCalibratedAtTheLimit(t *testing.T) {
	directory := t.TempDir()
	control := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: directory})
	budget := newSorterBudget(64 << 10)
	limit := budget.rowLimit()

	const key = "k"
	const ordinal = uint64(1)
	// Walk the payload down from the limit until the whole row fits inside it exactly.
	atLimitPayload := 0
	for payload := int(limit); payload > 0; payload-- {
		if candidateRowWidth(key, ordinal, payload) <= limit {
			atLimitPayload = payload
			break
		}
	}
	if width := candidateRowWidth(key, ordinal, atLimitPayload); width != limit {
		t.Fatalf("could not place a candidate exactly on the limit: width = %d, limit = %d", width, limit)
	}

	t.Run("exactly at the limit is accepted and encoded", func(t *testing.T) {
		probe := &widthProbeCodec{declaredPayload: atLimitPayload, payloadLen: atLimitPayload}
		sorter, err := newCandidateSorter[int](control, budget, probe, true)
		if err != nil {
			t.Fatal(err)
		}
		defer sorter.Close()
		err = sorter.Add(physical.SortRow[int]{Key: key, Ordinal: ordinal, Row: 0})
		if err != nil {
			t.Fatalf("a candidate exactly at the row limit was refused: %v", err)
		}
		if probe.encodeCalls != 1 {
			t.Fatalf("codec.encode ran %d times, want 1", probe.encodeCalls)
		}
		// The sorter's own check agrees, which is what proves the preflight was not too generous.
		if width := candidateRowWidth(key, ordinal, probe.payloadLen); width != limit {
			t.Fatalf("accepted candidate measured %d, want exactly %d", width, limit)
		}
	})

	t.Run("one byte over the limit is refused before encoding", func(t *testing.T) {
		over := candidateRowWidth(key, ordinal, atLimitPayload+1)
		if over != limit+1 {
			t.Fatalf("the over-limit candidate measured %d, want %d", over, limit+1)
		}
		// The declared width is the payload cell's own width, which is how the candidate sorter sizes
		// the column it is about to build: a materialised payload of N bytes costs 9 + N.
		probe := &widthProbeCodec{declaredPayload: atLimitPayload + 1, payloadLen: atLimitPayload + 1}
		sorter, err := newCandidateSorter[int](control, budget, probe, true)
		if err != nil {
			t.Fatal(err)
		}
		defer sorter.Close()
		err = sorter.Add(physical.SortRow[int]{Key: key, Ordinal: ordinal, Row: 0})
		if !errors.Is(err, ErrQueryResourceLimit) {
			t.Fatalf("error = %v, want ErrQueryResourceLimit", err)
		}
		if probe.encodeCalls != 0 {
			t.Fatalf("codec.encode ran %d times for an over-wide candidate, want 0", probe.encodeCalls)
		}
	})
}

// TestSorterAddStaysTheAuthoritativeWidthCheck verifies the preflight is an optimisation and not the
// only guard: a codec that under-declares its width is still caught by the sorter's own check, so a
// wrong or drifting codec cannot push an over-wide row past the budget.
func TestSorterAddStaysTheAuthoritativeWidthCheck(t *testing.T) {
	control := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: t.TempDir()})
	budget := newSorterBudget(64 << 10)
	limit := budget.rowLimit()
	// The codec claims one byte, but really encodes a payload that makes the row over-wide.
	probe := &widthProbeCodec{declaredPayload: 1, payloadLen: int(limit)}
	sorter, err := newCandidateSorter[int](control, budget, probe, true)
	if err != nil {
		t.Fatal(err)
	}
	defer sorter.Close()
	err = sorter.Add(physical.SortRow[int]{Key: "k", Ordinal: 1, Row: 0})
	if !errors.Is(err, ErrQueryResourceLimit) {
		t.Fatalf("error = %v, want the sorter's own width check to reject the row", err)
	}
	if probe.encodeCalls != 1 {
		t.Fatalf("codec.encode ran %d times, want 1 (the preflight passed a false declaration)", probe.encodeCalls)
	}
}

// TestCandidateSorterStillEncodesAndRoundTripsNormalCandidates verifies the preflight did not turn
// into a guard that skips encoding: a normal candidate is encoded, spilled, and recovered exactly.
func TestCandidateSorterStillEncodesAndRoundTripsNormalCandidates(t *testing.T) {
	directory := t.TempDir()
	control := newQueryControl(context.Background(), QueryOptions{SortMemoryBytes: 64 << 10, TempDirectory: directory})
	budget := newSorterBudget(64 << 10)
	codec := &countingCodec{}
	sorter, err := newCandidateSorter[int](control, budget, codec, false)
	if err != nil {
		t.Fatal(err)
	}
	const rows = 300
	for i := 0; i < rows; i++ {
		if err := sorter.Add(physical.SortRow[int]{Key: fmt.Sprintf("k%03d", i), Ordinal: uint64(i), Row: 1 + i%97}); err != nil {
			t.Fatal(err)
		}
	}
	if codec.encodeCalls != rows {
		t.Fatalf("codec.encode ran %d times, want %d (once per accepted candidate)", codec.encodeCalls, rows)
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
		if value != 1+i%97 {
			t.Fatalf("row %d payload = %d, want %d", i, value, 1+i%97)
		}
	}
	if entries, readErr := os.ReadDir(directory); readErr != nil || len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v (%v)", entries, readErr)
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
