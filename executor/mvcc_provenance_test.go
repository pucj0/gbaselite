package executor

import (
	"context"
	"testing"

	"gbaselite/parser"
	"gbaselite/storage"
)

// This file holds the Phase 7 prerequisite regressions for physical row identity provenance.
//
// The invariant under test is that a scan's physical storage key travels with the row it was
// read with, instead of living only in the counter-keyed identityKeys ledger. A nested-loop
// join re-opens the same input once per outer row, and that ledger keeps advancing, so its
// later writes can overwrite the slots an earlier scan recorded. Reading the key from the row's
// provenance column is therefore the only stable way to address the row again.

// collectJoinProvenance runs an identified join and reports, per joined row, whether the target
// was present and which physical storage key its provenance column carried.
func collectJoinProvenance(t *testing.T, e *Engine, s *Session, query string) []string {
	t.Helper()
	statement, err := parser.Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	update, ok := statement.(parser.Update)
	if !ok {
		t.Fatalf("%s is not an UPDATE", query)
	}
	tx, err := e.Backend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	inputs, err := bindJoinsIdentified(tx, s, parser.Select{Table: update.Table, TableAlias: update.TableAlias, Joins: update.Joins})
	if err != nil {
		t.Fatal(err)
	}
	combined := inputs[len(inputs)-1].combined
	identityAt := provenanceIdentityAt(&inputs[0])
	keyAt := provenanceKeyAt(&inputs[0])
	if identityAt < 0 || keyAt < 0 {
		t.Fatalf("target input carries no provenance columns")
	}
	driver := identityScan(tx, &inputs[0], sqlAccessPlan{kind: sqlAccessAll}, s)
	rows := chainJoinInputs(tx, s, inputs, driver, true)
	seen := make([]string, 0, 8)
	err = rows.Run(context.Background(), func(row storage.Row) error {
		if !ProvenancePresent(row, identityAt) {
			seen = append(seen, "absent")
			return nil
		}
		key, ok := ProvenanceKey(row, keyAt)
		if !ok {
			seen = append(seen, "absent")
			return nil
		}
		seen = append(seen, string(key))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = combined
	return seen
}

// TestMVCCProvenanceSurvivesNestedLoopRescan is the core Phase 7 regression: the same physical
// row must resolve to the same storage key every time a nested-loop join re-scans its input.
//
// The target is driven once per scan while the joined source is probed per outer row, so the
// target input is scanned repeatedly. With a rowid ledger this fails, because the second scan's
// assignments overwrite the slots the first scan recorded; with provenance carried in the row
// the keys stay stable.
func TestMVCCProvenanceSurvivesNestedLoopRescan(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,10),(2,20)")
	// The source fans out so the join walks each target row several times.
	run("INSERT INTO s VALUES(11,1),(12,1),(13,1),(14,2),(15,2)")

	keys := collectJoinProvenance(t, e, s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v")
	if len(keys) == 0 {
		t.Fatal("join produced no rows")
	}
	// Group the keys seen per target row: source rows for tid=1 are contiguous, then tid=2.
	first := make(map[string]int)
	distinct := 0
	for _, key := range keys {
		if key == "absent" {
			t.Fatalf("a real target row reported absent provenance: %v", keys)
		}
		if _, exists := first[key]; !exists {
			first[key] = distinct
			distinct++
		}
	}
	if distinct != 2 {
		t.Fatalf("provenance keys = %v, want exactly the 2 physical rows", keys)
	}
	// Each target row must report the SAME key on every one of its scan passes: 3 rows for
	// tid=1 and 2 rows for tid=2.
	counts := make([]int, 2)
	for _, key := range keys {
		counts[first[key]]++
	}
	want := []int{3, 2}
	for i := range want {
		if counts[i] != want[i] {
			t.Fatalf("target %d reported %d passes, want %d (keys=%v)", i, counts[i], want[i], keys)
		}
	}
}

// TestMVCCProvenanceDistinguishesIdenticalNoPKRows verifies a table without a primary key keeps
// its physical rows apart by their real storage key, not by their values.
func TestMVCCProvenanceDistinguishesIdenticalNoPKRows(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE heap(v INT)")
	run("CREATE TABLE src(id INT PRIMARY KEY,w INT,KEY w_idx(w))")
	run("INSERT INTO heap VALUES(7),(7),(7)")
	run("INSERT INTO src VALUES(1,7),(2,7)")

	keys := collectJoinProvenance(t, e, s, "UPDATE heap JOIN src ON src.w=heap.v SET heap.v=heap.v")
	distinct := make(map[string]struct{})
	for _, key := range keys {
		if key == "absent" {
			t.Fatalf("a real heap row reported absent provenance: %v", keys)
		}
		distinct[key] = struct{}{}
	}
	// Three heap rows with identical values must yield three distinct physical identities.
	if len(distinct) != 3 {
		t.Fatalf("identical-value heap rows collapsed: %d distinct keys from %v", len(distinct), keys)
	}
}

// TestMVCCProvenanceMarksOuterJoinAbsentTarget verifies the absence rule directly on the
// null-extended row shape an outer join produces.
//
// UPDATE JOIN drives from the target and evaluates a LEFT JOIN as its matched subset, so it
// never emits an unmatched target; the multi-table DELETE paths (which are driven from the
// join) do. The rule those paths depend on is therefore asserted on the row shape itself: a
// null-extended side has no provenance, and that — not the target's column values — is what
// decides absence.
func TestMVCCProvenanceMarksOuterJoinAbsentTarget(t *testing.T) {
	// A row shaped like a null-extended joined input: real columns (all NULL) then the two
	// provenance columns, both NULL.
	absent := storage.Row{
		storage.NullValue(storage.TypeInt),
		storage.NullValue(storage.TypeInt),
		storage.NullValue(storage.TypeBigInt),
		storage.NullValue(storage.TypeText),
	}
	if ProvenancePresent(absent, 2) {
		t.Fatal("null-extended row reported a present target")
	}
	if key, ok := ProvenanceKey(absent, 3); ok {
		t.Fatalf("null-extended row yielded key %q", key)
	}

	// A real row whose own values are NULL still carries provenance and remains a target.
	real := storage.Row{
		storage.NullValue(storage.TypeInt),
		storage.NullValue(storage.TypeInt),
	}
	idValue, err := storage.NewValue(storage.TypeBigInt, 1)
	if err != nil {
		t.Fatal(err)
	}
	keyValue, err := storage.NewValue(storage.TypeText, "storage-key")
	if err != nil {
		t.Fatal(err)
	}
	real = append(real, idValue, keyValue)
	if !ProvenancePresent(real, 2) {
		t.Fatal("a real row with NULL values reported absent")
	}
	if key, ok := ProvenanceKey(real, 3); !ok || string(key) != "storage-key" {
		t.Fatalf("real row key = %q ok=%v", key, ok)
	}
}

// TestMVCCProvenancePresentForNullValuedRealRow verifies a real row whose own columns are NULL
// is still a target: absence is decided by provenance, never by column values.
func TestMVCCProvenancePresentForNullValuedRealRow(t *testing.T) {
	e, s, run := rangeTestEngine(t)
	run("CREATE TABLE t(id INT PRIMARY KEY,v INT)")
	run("CREATE TABLE s(id INT PRIMARY KEY,tid INT,KEY tid_idx(tid))")
	run("INSERT INTO t VALUES(1,NULL)")
	run("INSERT INTO s VALUES(11,1)")

	keys := collectJoinProvenance(t, e, s, "UPDATE t JOIN s ON s.tid=t.id SET t.v=t.v")
	if len(keys) != 1 || keys[0] == "absent" {
		t.Fatalf("a real row with NULL values was treated as absent: %v", keys)
	}
}
