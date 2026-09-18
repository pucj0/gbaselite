package physical

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"
)

// The Phase 2 tests cover the TargetRowDedup contract: stable first occurrence,
// TableID plus physical storage key identity, dropped non-targets, sorter-factory
// lifecycle, and spilled temporary-file cleanup on every exit path.

// dedupCandidate is one mutation candidate: a table identifier, a physical storage
// key, and a payload that says which candidate survived.
type dedupCandidate struct {
	tableID string
	key     []byte
	marker  string
}

func (c dedupCandidate) identity() (RowIdentity, bool) {
	return RowIdentity{TableID: c.tableID, Key: c.key, Valid: true}, true
}

func candidates(rows ...dedupCandidate) Operator[dedupCandidate] {
	return Source[dedupCandidate](func(_ context.Context, yield Yield[dedupCandidate]) error {
		for _, row := range rows {
			if err := yield(row); err != nil {
				return err
			}
		}
		return nil
	})
}

func collectedMarkers(t *testing.T, op Operator[dedupCandidate]) []string {
	t.Helper()
	rows := collect(t, op)
	markers := make([]string, 0, len(rows))
	for _, row := range rows {
		markers = append(markers, row.marker)
	}
	return markers
}

// boundedDedupSorter is a bounded sorter test double. It retains at most maxRetained
// rows in memory and spills the rest to private temporary files, so a dedup run over
// it exercises the real spill path: rows are split across runs, merged during Finish,
// and every run file must be gone once Close returns.
type boundedDedupSorter struct {
	directory   string
	byKey       bool
	maxRetained int
	closed      *int
	batch       []TargetDedupRow[dedupCandidate]
	runs        [][]TargetDedupRow[dedupCandidate]
	files       []string
	closeErr    error
}

func (s *boundedDedupSorter) less(a, b TargetDedupRow[dedupCandidate]) bool {
	if s.byKey && a.Key != b.Key {
		return a.Key < b.Key
	}
	return a.Ordinal < b.Ordinal
}

func (s *boundedDedupSorter) Add(row TargetDedupRow[dedupCandidate]) error {
	s.batch = append(s.batch, row)
	if len(s.batch) <= s.maxRetained {
		return nil
	}
	file, err := os.CreateTemp(s.directory, "dedup-*.run")
	if err != nil {
		return err
	}
	for _, spilled := range s.batch {
		if _, err := fmt.Fprintf(file, "%s\x00%d\x00%s\n", spilled.Key, spilled.Ordinal, spilled.Row.marker); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	s.files = append(s.files, file.Name())
	s.runs = append(s.runs, append([]TargetDedupRow[dedupCandidate](nil), s.batch...))
	s.batch = nil
	return nil
}

func (s *boundedDedupSorter) Finish(yield func(TargetDedupRow[dedupCandidate]) error) error {
	if len(s.runs) == 0 {
		sort.SliceStable(s.batch, func(i, j int) bool { return s.less(s.batch[i], s.batch[j]) })
		for _, row := range s.batch {
			if err := yield(row); err != nil {
				return err
			}
		}
		return nil
	}
	merged := make([]TargetDedupRow[dedupCandidate], 0, len(s.runs)*s.maxRetained)
	for _, run := range s.runs {
		merged = append(merged, run...)
	}
	merged = append(merged, s.batch...)
	sort.SliceStable(merged, func(i, j int) bool { return s.less(merged[i], merged[j]) })
	for _, row := range merged {
		if err := yield(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *boundedDedupSorter) Close() error {
	*s.closed++
	var first error
	for _, path := range s.files {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	s.files = nil
	if s.closeErr != nil && first == nil {
		first = s.closeErr
	}
	return first
}

// dedupHarness wires a TargetRowDedup over bounded sorters and reports the sorter
// lifecycle so a test can assert both the result and the resource accounting.
type dedupHarness struct {
	op            TargetRowDedup[dedupCandidate]
	directory     string
	closed        int
	opened        int
	maxRetained   int
	identityCall  int
	factoryFail   error
	factoryFailOn bool
	newSortCalls  int
}

func newDedupHarness(t *testing.T, maxRetained int, input Operator[dedupCandidate]) *dedupHarness {
	t.Helper()
	h := &dedupHarness{directory: t.TempDir(), maxRetained: maxRetained}
	h.op = TargetRowDedup[dedupCandidate]{
		Input: input,
		Identity: func(row dedupCandidate) (RowIdentity, bool) {
			h.identityCall++
			return row.identity()
		},
		NewSort: h.newSort,
	}
	return h
}

// newSort fails only when factoryFailOn asks it to, so a test can choose which of
// the two sorter constructions fails and still observe the other one's cleanup.
func (h *dedupHarness) newSort(byKey bool) (Sorter[TargetDedupRow[dedupCandidate]], error) {
	h.newSortCalls++
	if h.factoryFail != nil && h.factoryFailOn == byKey {
		return nil, h.factoryFail
	}
	h.opened++
	return &boundedDedupSorter{directory: h.directory, byKey: byKey, maxRetained: h.maxRetained, closed: &h.closed}, nil
}

func (h *dedupHarness) leakedFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(h.directory)
	if err != nil {
		t.Fatal(err)
	}
	leaked := make([]string, 0, len(entries))
	for _, entry := range entries {
		leaked = append(leaked, entry.Name())
	}
	return leaked
}

func TestTargetRowDedupKeepsFirstOccurrenceInSourceOrder(t *testing.T) {
	// T1-K1 appears three times, T2-K1 twice, T3-K9 once. Source order is not sorted,
	// so a key-ordered output would be visible.
	input := candidates(
		dedupCandidate{"t3", []byte{0x09}, "second"},
		dedupCandidate{"t1", []byte{0x01}, "first"},
		dedupCandidate{"t2", []byte{0x01}, "other-table"},
		dedupCandidate{"t1", []byte{0x01}, "duplicate-1"},
		dedupCandidate{"t2", []byte{0x01}, "duplicate-2"},
		dedupCandidate{"t1", []byte{0x02}, "third"},
	)
	harness := newDedupHarness(t, 4, input)
	got := collectedMarkers(t, harness.op)
	want := []string{"second", "first", "other-table", "third"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dedup output = %v, want %v", got, want)
	}
	// Identity is resolved once per input row, and both sorters are closed.
	if harness.identityCall != 6 {
		t.Fatalf("identity calls = %d, want 6", harness.identityCall)
	}
	if harness.closed != 2 {
		t.Fatalf("closed sorters = %d, want 2", harness.closed)
	}
	if leaked := harness.leakedFiles(t); len(leaked) != 0 {
		t.Fatalf("temporary files leaked: %v", leaked)
	}
}

func TestTargetRowDedupSeparatesTablesWithEqualStorageKey(t *testing.T) {
	// Same storage key bytes in three different tables: three distinct targets.
	input := candidates(
		dedupCandidate{"t1", []byte{0x01}, "a"},
		dedupCandidate{"t2", []byte{0x01}, "b"},
		dedupCandidate{"t3", []byte{0x01}, "c"},
		dedupCandidate{"t1", []byte{0x01}, "a-again"},
	)
	harness := newDedupHarness(t, 8, input)
	if got := collectedMarkers(t, harness.op); fmt.Sprint(got) != fmt.Sprint([]string{"a", "b", "c"}) {
		t.Fatalf("dedup output = %v", got)
	}
	// A table prefix boundary must not let one table id absorb key bytes of another.
	boundary := candidates(
		dedupCandidate{"t", []byte{0x01, 0x02}, "t-1-2"},
		dedupCandidate{"t\x01", []byte{0x02}, "t1-2"},
	)
	if got := collectedMarkers(t, newDedupHarness(t, 8, boundary).op); len(got) != 2 {
		t.Fatalf("prefixed table ids collapsed: %v", got)
	}
}

func TestTargetRowDedupDropsCandidatesWithoutPhysicalTarget(t *testing.T) {
	// LEFT JOIN null extension and derived/view rows have no physical provenance, so
	// they must not become mutations. A real row whose columns are all NULL still has
	// a physical key and must survive.
	absent := dedupCandidate{"t1", nil, "outer-join-null-extension"}
	realNullRow := dedupCandidate{"t1", []byte{0x05}, "real-row-with-null-values"}
	input := Source[dedupCandidate](func(_ context.Context, yield Yield[dedupCandidate]) error {
		if err := yield(absent); err != nil {
			return err
		}
		if err := yield(realNullRow); err != nil {
			return err
		}
		// A deliberately invalid identity, reported through the bool channel.
		if err := yield(dedupCandidate{"t1", []byte{0x06}, "invalid-identity"}); err != nil {
			return err
		}
		return yield(realNullRow)
	})
	harness := newDedupHarness(t, 8, input)
	harness.op.Identity = func(row dedupCandidate) (RowIdentity, bool) {
		harness.identityCall++
		identity := RowIdentity{TableID: row.tableID, Key: row.key, Valid: true}
		if row.marker == "invalid-identity" {
			identity.Valid = false
		}
		return identity, true
	}
	if got := collectedMarkers(t, harness.op); fmt.Sprint(got) != fmt.Sprint([]string{"real-row-with-null-values"}) {
		t.Fatalf("dedup output = %v, want only the physically addressable row", got)
	}
	if harness.identityCall != 4 {
		t.Fatalf("identity calls = %d, want one per input row", harness.identityCall)
	}
	if harness.closed != 2 {
		t.Fatalf("closed sorters = %d, want 2", harness.closed)
	}
}

func TestTargetRowDedupSpillsBeyondMemoryThresholdAndCleansUp(t *testing.T) {
	const rows = 40
	input := make([]dedupCandidate, 0, rows)
	for i := 0; i < rows; i++ {
		// Ten distinct targets, each matched four times.
		key := byte(i % 10)
		input = append(input, dedupCandidate{"t1", []byte{key}, fmt.Sprintf("r%02d", i)})
	}
	// maxRetained 3 forces a spill every time the retained batch overflows.
	harness := newDedupHarness(t, 3, candidates(input...))

	got := collectedMarkers(t, harness.op)
	want := []string{"r00", "r01", "r02", "r03", "r04", "r05", "r06", "r07", "r08", "r09"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("spilled dedup output = %v, want %v", got, want)
	}
	if harness.closed != 2 {
		t.Fatalf("closed sorters = %d, want 2", harness.closed)
	}
	if leaked := harness.leakedFiles(t); len(leaked) != 0 {
		t.Fatalf("spilled temporary files leaked after success: %v", leaked)
	}
}

func TestTargetRowDedupReleasesSortersOnEveryExitPath(t *testing.T) {
	input := candidates(
		dedupCandidate{"t1", []byte{0x01}, "a"},
		dedupCandidate{"t1", []byte{0x02}, "b"},
	)
	downstream := errors.New("downstream failed")

	// Downstream error: both sorters opened, both closed, no files left behind.
	harness := newDedupHarness(t, 1, input)
	if err := harness.op.Run(context.Background(), func(dedupCandidate) error { return downstream }); !errors.Is(err, downstream) {
		t.Fatalf("downstream error = %v", err)
	}
	if harness.opened != 2 || harness.closed != 2 {
		t.Fatalf("opened=%d closed=%d, want both sorters closed", harness.opened, harness.closed)
	}
	if leaked := harness.leakedFiles(t); len(leaked) != 0 {
		t.Fatalf("spilled files leaked after downstream error: %v", leaked)
	}

	// Upstream error: the failure happens while filling the first sorter, which must
	// still be closed.
	upstream := errors.New("upstream failed")
	failing := Source[dedupCandidate](func(_ context.Context, yield Yield[dedupCandidate]) error {
		for _, row := range []dedupCandidate{{"t1", []byte{0x01}, "a"}, {"t1", []byte{0x02}, "b"}} {
			if err := yield(row); err != nil {
				return err
			}
		}
		return upstream
	})
	upstreamHarness := newDedupHarness(t, 1, failing)
	if err := upstreamHarness.op.Run(context.Background(), func(dedupCandidate) error { return nil }); !errors.Is(err, upstream) {
		t.Fatalf("upstream error = %v", err)
	}
	if upstreamHarness.opened != 2 || upstreamHarness.closed != 2 {
		t.Fatalf("upstream: opened=%d closed=%d", upstreamHarness.opened, upstreamHarness.closed)
	}
	if leaked := upstreamHarness.leakedFiles(t); len(leaked) != 0 {
		t.Fatalf("spilled files leaked after upstream error: %v", leaked)
	}

	// Context cancellation before the run opens nothing and closes nothing.
	cancelHarness := newDedupHarness(t, 1, input)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cancelHarness.op.Run(cancelled, func(dedupCandidate) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run = %v", err)
	}
	if cancelHarness.newSortCalls != 0 || cancelHarness.closed != 0 {
		t.Fatalf("cancelled run opened sorters: calls=%d closed=%d", cancelHarness.newSortCalls, cancelHarness.closed)
	}

	// Cancellation discovered mid-run still closes what was opened.
	midInput := candidates(
		dedupCandidate{"t1", []byte{0x01}, "a"},
		dedupCandidate{"t1", []byte{0x02}, "b"},
	)
	midHarness := newDedupHarness(t, 1, midInput)
	midCtx, midCancel := context.WithCancel(context.Background())
	seen := 0
	if err := midHarness.op.Run(midCtx, func(dedupCandidate) error {
		seen++
		midCancel()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-run cancellation = %v", err)
	}
	if midHarness.closed != 2 {
		t.Fatalf("mid-run cancellation closed %d sorters, want 2", midHarness.closed)
	}
	if leaked := midHarness.leakedFiles(t); len(leaked) != 0 {
		t.Fatalf("spilled files leaked after cancellation: %v", leaked)
	}

	// A sorter factory failure must not leak the sorter that was already opened: the
	// second factory call fails, so exactly the first sorter is closed.
	factory := newDedupHarness(t, 1, input)
	factory.factoryFail = errors.New("sorter unavailable")
	factory.factoryFailOn = false
	if err := factory.op.Run(context.Background(), func(dedupCandidate) error { return nil }); !errors.Is(err, factory.factoryFail) {
		t.Fatalf("factory failure = %v", err)
	}
	if factory.opened != 1 || factory.closed != 1 {
		t.Fatalf("factory failure: opened=%d closed=%d, want the first sorter closed", factory.opened, factory.closed)
	}
}

func TestTargetRowDedupWithoutTargetsProducesNoMutation(t *testing.T) {
	// Every candidate lacks physical provenance: the operator must emit nothing and
	// must not fail, because an empty mutation set is a valid outcome.
	input := candidates(
		dedupCandidate{"t1", nil, "a"},
		dedupCandidate{"t2", nil, "b"},
	)
	harness := newDedupHarness(t, 8, input)
	harness.op.Identity = func(dedupCandidate) (RowIdentity, bool) { return RowIdentity{}, false }
	if got := collectedMarkers(t, harness.op); len(got) != 0 {
		t.Fatalf("emitted %v for candidates without provenance", got)
	}
	if harness.closed != 2 {
		t.Fatalf("closed sorters = %d, want 2", harness.closed)
	}
}

// --- Goal B: payload preservation ------------------------------------------------

// payloadDroppingSorter is a sorter that keeps only the ledger (key and ordinal) and discards
// the candidate payload, spilling across several private run files. It models a sorter that
// cannot encode an arbitrary candidate, which is exactly the situation the payload-preserving
// contract must survive: the operator has to return the winner's payload regardless.
type payloadDroppingSorter struct {
	directory   string
	byKey       bool
	maxRetained int
	closed      *int
	ledger      []TargetDedupRow[dedupCandidate]
	files       []string
}

func (s *payloadDroppingSorter) less(a, b TargetDedupRow[dedupCandidate]) bool {
	if s.byKey && a.Key != b.Key {
		return a.Key < b.Key
	}
	return a.Ordinal < b.Ordinal
}

func (s *payloadDroppingSorter) Add(row TargetDedupRow[dedupCandidate]) error {
	// Deliberately drop row.Row: only the ledger is retained, which is what makes this sorter
	// unable to return a payload.
	s.ledger = append(s.ledger, TargetDedupRow[dedupCandidate]{Key: row.Key, Ordinal: row.Ordinal})
	if len(s.ledger) <= s.maxRetained {
		return nil
	}
	// Spill a copy of the current ledger into a private run file, so the operator is exercised
	// against a sorter that really does own temporary resources.
	file, err := os.CreateTemp(s.directory, "ledger-*.run")
	if err != nil {
		return err
	}
	for _, spilled := range s.ledger {
		if _, err := fmt.Fprintf(file, "%s\x00%d\n", spilled.Key, spilled.Ordinal); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	s.files = append(s.files, file.Name())
	return nil
}

func (s *payloadDroppingSorter) Finish(yield func(TargetDedupRow[dedupCandidate]) error) error {
	sort.SliceStable(s.ledger, func(i, j int) bool { return s.less(s.ledger[i], s.ledger[j]) })
	for _, row := range s.ledger {
		// The payload is gone; only the ledger survives the round trip.
		if err := yield(TargetDedupRow[dedupCandidate]{Key: row.Key, Ordinal: row.Ordinal}); err != nil {
			return err
		}
	}
	return nil
}

func (s *payloadDroppingSorter) Close() error {
	*s.closed++
	for _, path := range s.files {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	s.files = nil
	return nil
}

// TestTargetRowDedupPreservesPayloadWhenSorterCannot verifies the operator returns each
// winner's payload even when the injected sorter can only persist the dedup ledger.
func TestTargetRowDedupPreservesPayloadWhenSorterCannot(t *testing.T) {
	directory := t.TempDir()
	closed := 0
	input := candidates(
		dedupCandidate{"t1", []byte{0x01}, "winner-1"},
		dedupCandidate{"t2", []byte{0x01}, "winner-2"},
		dedupCandidate{"t1", []byte{0x01}, "loser"},
		dedupCandidate{"t1", []byte{0x02}, "winner-3"},
	)
	op := TargetRowDedup[dedupCandidate]{
		Input:    input,
		Identity: func(row dedupCandidate) (RowIdentity, bool) { return row.identity() },
		NewSort: func(byKey bool) (Sorter[TargetDedupRow[dedupCandidate]], error) {
			return &payloadDroppingSorter{directory: directory, byKey: byKey, maxRetained: 1, closed: &closed}, nil
		},
	}
	got := collectedMarkers(t, op)
	want := []string{"winner-1", "winner-2", "winner-3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("payload-preserving dedup = %v, want %v", got, want)
	}
	if closed != 2 {
		t.Fatalf("closed sorters = %d, want 2", closed)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}

// TestTargetRowDedupPayloadIsOwnedAcrossRuns verifies the operator does not retain the input
// buffer: a second run over cloned candidates must produce the same payloads.
func TestTargetRowDedupPayloadIsOwnedAcrossRuns(t *testing.T) {
	directory := t.TempDir()
	closed := 0
	build := func() Operator[dedupCandidate] {
		rows := []dedupCandidate{
			{"t1", []byte{0x01}, "first"},
			{"t1", []byte{0x01}, "second"},
			{"t2", []byte{0x03}, "third"},
		}
		return Source[dedupCandidate](func(_ context.Context, yield Yield[dedupCandidate]) error {
			for _, row := range rows {
				if err := yield(row); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for run := 0; run < 2; run++ {
		op := TargetRowDedup[dedupCandidate]{
			Input:    build(),
			Identity: func(row dedupCandidate) (RowIdentity, bool) { return row.identity() },
			NewSort: func(byKey bool) (Sorter[TargetDedupRow[dedupCandidate]], error) {
				return &payloadDroppingSorter{directory: directory, byKey: byKey, maxRetained: 8, closed: &closed}, nil
			},
		}
		if got := collectedMarkers(t, op); fmt.Sprint(got) != fmt.Sprint([]string{"first", "third"}) {
			t.Fatalf("run %d = %v", run, got)
		}
	}
	if closed != 4 {
		t.Fatalf("closed sorters = %d, want 4", closed)
	}
}
