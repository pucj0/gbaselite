package mvcc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Transaction diagnostics: read observation, write set and dependency counters.
//
// The contract these tests pin is boundedness. Ordinary reads are summarised as
// counts, so a transaction that reads a million rows costs the same diagnostics
// state as one that reads nothing; only the write set, which the transaction
// already holds in its bounded buffer and optional staging database, is tracked
// per logical key.

func transactionDiagnostics(t *testing.T, tx *Tx) TransactionInfo {
	t.Helper()
	info, ok := tx.store.txns.Info(tx.ID)
	if !ok {
		t.Fatalf("transaction %s is not registered", tx.ID)
	}
	return info
}

// stagedOpCost is the diagnostics byte cost of one logical write, computed the way
// the store accounts it.
func stagedOpCost(t *testing.T, space, rowKey string, op Op) int64 {
	t.Helper()
	encoded, err := key(space, []byte(rowKey))
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(encoded) + len(encodeStagedOp(op)))
}

// expectedWriteBytes is the final logical write-set size: exactly what the
// diagnostics must report, however the transaction reached that set.
func expectedWriteBytes(t *testing.T, writes map[string]Op) int64 {
	t.Helper()
	total := int64(0)
	for rowKey, op := range writes {
		total += stagedOpCost(t, visibilitySpace, rowKey, op)
	}
	return total
}

func putOp(key, value string) Op {
	return Op{Space: visibilitySpace, Key: []byte(key), Value: []byte(value)}
}
func delOp(key string) Op   { return Op{Space: visibilitySpace, Key: []byte(key), Delete: true} }
func guardOp(key string) Op { return Op{Space: visibilitySpace, Key: []byte(key), Check: true} }

func TestDiagnosticsPointReadObservation(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "a", "1")
	putVisible(t, seed, "b", "22")
	mustCommit(t, seed)

	tx := beginBaseline(t, s)
	if info := transactionDiagnostics(t, tx); info.PointReads != 0 || info.RowsObserved != 0 || info.BytesObserved != 0 {
		t.Fatalf("fresh transaction already reports reads: %+v", info)
	}

	// A miss is a point read but observes no row.
	if _, ok, err := tx.Get(visibilitySpace, []byte("missing")); err != nil || ok {
		t.Fatalf("missing read = ok=%v err=%v", ok, err)
	}
	info := transactionDiagnostics(t, tx)
	if info.PointReads != 1 || info.RowsObserved != 0 || info.BytesObserved != 0 {
		t.Fatalf("miss observation = %+v", info)
	}

	// A hit observes one row and its key plus value bytes.
	value, ok, err := tx.Get(visibilitySpace, []byte("b"))
	if err != nil || !ok {
		t.Fatalf("read b = %v %v", ok, err)
	}
	info = transactionDiagnostics(t, tx)
	if info.PointReads != 2 || info.RowsObserved != 1 {
		t.Fatalf("hit observation = %+v", info)
	}
	if want := uint64(len("b") + len(value)); info.BytesObserved != want {
		t.Fatalf("BytesObserved = %d, want %d", info.BytesObserved, want)
	}

	// A read served by the transaction's own write is still an observation.
	putVisible(t, tx, "own", "written")
	if _, ok, err := tx.Get(visibilitySpace, []byte("own")); err != nil || !ok {
		t.Fatalf("own write read = %v %v", ok, err)
	}
	info = transactionDiagnostics(t, tx)
	if info.PointReads != 3 || info.RowsObserved != 2 {
		t.Fatalf("own-write observation = %+v", info)
	}
	// The observation counters never turn a read into a write or a dependency.
	if info.Writes != 1 || info.WriteBytes == 0 || info.PointDependencies != 0 || info.RangeDependencies != 0 {
		t.Fatalf("read accounting leaked into the write set: %+v", info)
	}
}

func TestDiagnosticsRangeReadObservation(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	for i := 0; i < 6; i++ {
		putVisible(t, seed, fmt.Sprintf("k%d", i), strings.Repeat("v", i+1))
	}
	mustCommit(t, seed)

	tx := beginBaseline(t, s)
	rows, bytes := 0, 0
	collect := func(k, v []byte) error {
		rows++
		bytes += len(k) + len(v)
		return nil
	}
	if err := tx.Scan(ctx, visibilitySpace, collect); err != nil {
		t.Fatal(err)
	}
	if err := tx.ScanRange(ctx, visibilitySpace, KeyRange{}, collect); err != nil {
		t.Fatal(err)
	}
	if err := tx.ScanRange(ctx, visibilitySpace, KeyRange{Reverse: true}, collect); err != nil {
		t.Fatal(err)
	}
	if err := tx.ScanRange(ctx, visibilitySpace, KeyRange{Lower: []byte("k2"), Upper: []byte("k4"), LowerInclusive: true, UpperInclusive: true}, collect); err != nil {
		t.Fatal(err)
	}
	info := transactionDiagnostics(t, tx)
	if info.RangeReads != 4 {
		t.Fatalf("RangeReads = %d, want 4", info.RangeReads)
	}
	if info.RowsObserved != uint64(rows) || info.BytesObserved != uint64(bytes) {
		t.Fatalf("observed (%d, %d), want (%d, %d)", info.RowsObserved, info.BytesObserved, rows, bytes)
	}
	if info.PointReads != 0 {
		t.Fatalf("range reads counted as point reads: %+v", info)
	}

	// A consumer that stops early still counts the call and the rows it saw.
	stop := fmt.Errorf("stop")
	before := transactionDiagnostics(t, tx)
	seen := 0
	err := tx.Scan(ctx, visibilitySpace, func(k, v []byte) error {
		seen++
		return stop
	})
	if err != stop {
		t.Fatalf("early stop error = %v", err)
	}
	info = transactionDiagnostics(t, tx)
	if info.RangeReads != before.RangeReads+1 || info.RowsObserved != before.RowsObserved+uint64(seen) {
		t.Fatalf("early stop observation = %+v (before %+v)", info, before)
	}

	// A cancelled scan is still a range read observation.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := tx.Scan(cancelled, visibilitySpace, collect); err == nil {
		t.Fatal("cancelled scan succeeded")
	}
	if info = transactionDiagnostics(t, tx); info.RangeReads != before.RangeReads+2 {
		t.Fatalf("cancelled scan not counted: %+v", info)
	}
}

func TestDiagnosticsChildObservationsBelongToTheChild(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "a", "1")
	mustCommit(t, seed)

	parent := beginBaseline(t, s)
	child, err := parent.Child()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := child.Get(visibilitySpace, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if info := transactionDiagnostics(t, child); info.PointReads != 1 {
		t.Fatalf("child PointReads = %d, want 1", info.PointReads)
	}
	if info := transactionDiagnostics(t, parent); info.PointReads != 0 {
		t.Fatalf("a child read was counted on its parent: %+v", info)
	}
	// A nested read is one observation, not one per ancestor.
	if _, _, err := child.Get(visibilitySpace, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if info := transactionDiagnostics(t, child); info.PointReads != 2 {
		t.Fatalf("nested child PointReads = %d, want 2", info.PointReads)
	}

	// Merging a child folds its observations into the parent: the work was done on
	// the parent's behalf.
	if _, err := child.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if info := transactionDiagnostics(t, parent); info.PointReads != 2 {
		t.Fatalf("merged child observations = %d, want 2", info.PointReads)
	}
}

// TestDiagnosticsWriteSetTracksReplacements is the write-set contract: the
// counters describe the logical write set as it stands, never the history of calls.
func TestDiagnosticsWriteSetTracksReplacements(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)

	writes := map[string]Op{}
	replace := func(rowKey, value string) {
		t.Helper()
		putVisible(t, tx, rowKey, value)
		writes[rowKey] = putOp(rowKey, value)
		info := transactionDiagnostics(t, tx)
		if info.Writes != uint64(len(writes)) {
			t.Fatalf("after Put %s: Writes = %d, want %d", rowKey, info.Writes, len(writes))
		}
		if want := expectedWriteBytes(t, writes); info.WriteBytes != want {
			t.Fatalf("after Put %s: WriteBytes = %d, want %d", rowKey, info.WriteBytes, want)
		}
		if info.WriteBytes != tx.stagedBytes {
			t.Fatalf("WriteBytes %d disagrees with the staged write set %d", info.WriteBytes, tx.stagedBytes)
		}
	}

	// Put A small, larger, smaller: each step reports only the current value.
	replace("A", "s")
	replace("A", strings.Repeat("x", 4096))
	replace("A", "tiny")
	if info := transactionDiagnostics(t, tx); info.Writes != 1 {
		t.Fatalf("replacement created a second write-set key: %+v", info)
	}

	// A second key counts separately.
	replace("B", "b")
	// A delete is a write-set entry, and it replaces the previous value's bytes.
	deleteVisible(t, tx, "B")
	writes["B"] = delOp("B")
	info := transactionDiagnostics(t, tx)
	if info.Writes != 2 || info.WriteBytes != expectedWriteBytes(t, writes) {
		t.Fatalf("delete accounting = %+v, want 2 writes / %d bytes", info, expectedWriteBytes(t, writes))
	}
	if info.WriteBytes != tx.stagedBytes {
		t.Fatalf("WriteBytes %d disagrees with the staged write set %d", info.WriteBytes, tx.stagedBytes)
	}

	// A guard over a key that is already written adds no dependency: the write
	// already carries that conflict.
	if err := tx.Guard(visibilitySpace, []byte("A")); err != nil {
		t.Fatal(err)
	}
	info = transactionDiagnostics(t, tx)
	if info.Writes != 2 || info.PointDependencies != 0 {
		t.Fatalf("guard over a written key = %+v", info)
	}

	// A guard first, then the write: the write replaces the dependency.
	if err := tx.Guard(visibilitySpace, []byte("C")); err != nil {
		t.Fatal(err)
	}
	if info = transactionDiagnostics(t, tx); info.PointDependencies != 1 || info.Writes != 2 {
		t.Fatalf("guard accounting = %+v", info)
	}
	putVisible(t, tx, "C", "c")
	info = transactionDiagnostics(t, tx)
	if info.PointDependencies != 0 || info.Writes != 3 {
		t.Fatalf("write did not replace its guard: %+v", info)
	}

	// A guard on a key that is never written contributes bytes to the staged
	// write-set budget but none to the write-set bytes.
	if err := tx.Guard(visibilitySpace, []byte("unwritten")); err != nil {
		t.Fatal(err)
	}
	info = transactionDiagnostics(t, tx)
	if info.PointDependencies != 1 {
		t.Fatalf("guard accounting = %+v", info)
	}
	if info.WriteBytes >= tx.stagedBytes {
		t.Fatalf("guard bytes leaked into the write set: WriteBytes=%d staged=%d", info.WriteBytes, tx.stagedBytes)
	}
}

func TestDiagnosticsWriteSetSurvivesSpill(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	value := strings.Repeat("x", 4096)
	writes := map[string]Op{}
	for i := 0; i < 40; i++ {
		rowKey := fmt.Sprintf("k%03d", i)
		putVisible(t, tx, rowKey, value)
		writes[rowKey] = putOp(rowKey, value)
	}
	if tx.stage == nil {
		t.Fatalf("write set did not spill: bufferBytes = %d", tx.bufferBytes)
	}

	// Replace an already-staged key with a smaller value: the accounting must use
	// the staged copy as the previous op, exactly as the buffer case does.
	putVisible(t, tx, "k000", "small")
	writes["k000"] = putOp("k000", "small")
	info := transactionDiagnostics(t, tx)
	if info.Writes != uint64(len(writes)) {
		t.Fatalf("Writes = %d, want %d", info.Writes, len(writes))
	}
	if want := expectedWriteBytes(t, writes); info.WriteBytes != want {
		t.Fatalf("WriteBytes = %d, want %d (final logical write set)", info.WriteBytes, want)
	}
	if info.WriteBytes != tx.stagedBytes {
		t.Fatalf("WriteBytes %d disagrees with the staged write set %d", info.WriteBytes, tx.stagedBytes)
	}

	// Deleting a staged key keeps one write-set entry with the tombstone's bytes.
	deleteVisible(t, tx, "k001")
	writes["k001"] = delOp("k001")
	info = transactionDiagnostics(t, tx)
	if info.Writes != uint64(len(writes)) || info.WriteBytes != expectedWriteBytes(t, writes) {
		t.Fatalf("staged delete accounting = %+v", info)
	}
}

func TestDiagnosticsDependencyCounters(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)

	if err := tx.Guard(visibilitySpace, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Guard(visibilitySpace, []byte("a")); err != nil {
		t.Fatal(err)
	}
	info := transactionDiagnostics(t, tx)
	if info.PointDependencies != 1 {
		t.Fatalf("repeated point guard = %+v", info)
	}
	if err := tx.Guard(visibilitySpace, []byte("b")); err != nil {
		t.Fatal(err)
	}
	if info = transactionDiagnostics(t, tx); info.PointDependencies != 2 {
		t.Fatalf("second point guard = %+v", info)
	}

	bounds := KeyRange{Lower: []byte("b"), Upper: []byte("d"), LowerInclusive: true, UpperInclusive: true}
	for i := 0; i < 2; i++ {
		if err := tx.GuardRange(visibilitySpace, bounds); err != nil {
			t.Fatal(err)
		}
	}
	if info = transactionDiagnostics(t, tx); info.RangeDependencies != 1 {
		t.Fatalf("repeated identical range guard = %+v", info)
	}
	if err := tx.GuardRange(visibilitySpace, KeyRange{Lower: []byte("x"), Upper: []byte("z")}); err != nil {
		t.Fatal(err)
	}
	if err := tx.GuardRange(visibilitySpace, KeyRange{}); err != nil {
		t.Fatal(err)
	}
	info = transactionDiagnostics(t, tx)
	if info.RangeDependencies != 3 {
		t.Fatalf("range guard count = %+v", info)
	}
	// A guard is a dependency, never a write: no bytes and no write-set entries.
	if info.Writes != 0 || info.WriteBytes != 0 {
		t.Fatalf("guards contributed to the write set: %+v", info)
	}
	// And the guard bytes are still part of the staged write-set budget, which is
	// what resources.transaction_write_mb limits.
	if tx.stagedBytes == 0 {
		t.Fatal("guards are not accounted in the staged write set")
	}
}

// TestDiagnosticsWriteSetAcrossMerges covers both merge paths: the empty-parent
// transfer and the re-buffering path, including a replacement across the boundary.
func TestDiagnosticsWriteSetAcrossMerges(t *testing.T) {
	ctx := context.Background()
	s := baselineStore(t)

	t.Run("empty parent transfer", func(t *testing.T) {
		parent := beginBaseline(t, s)
		child, err := parent.Child()
		if err != nil {
			t.Fatal(err)
		}
		putVisible(t, child, "c", "child")
		if _, err := child.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		info := transactionDiagnostics(t, parent)
		if info.Writes != 1 || info.WriteBytes != stagedOpCost(t, visibilitySpace, "c", putOp("c", "child")) {
			t.Fatalf("transferred write set = %+v", info)
		}
		if info.WriteBytes != parent.stagedBytes {
			t.Fatalf("WriteBytes %d disagrees with the staged write set %d", info.WriteBytes, parent.stagedBytes)
		}
	})

	t.Run("re-buffered merge", func(t *testing.T) {
		parent := beginBaseline(t, s)
		putVisible(t, parent, "p", "parent")
		child, err := parent.Child()
		if err != nil {
			t.Fatal(err)
		}
		putVisible(t, child, "c", "child")
		if _, err := child.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		want := putOp("p", "parent")
		writes := map[string]Op{"p": want, "c": putOp("c", "child")}
		info := transactionDiagnostics(t, parent)
		if info.Writes != 2 || info.WriteBytes != expectedWriteBytes(t, writes) {
			t.Fatalf("merged write set = %+v, want 2 writes / %d bytes", info, expectedWriteBytes(t, writes))
		}
	})

	t.Run("replacement across the merge boundary", func(t *testing.T) {
		parent := beginBaseline(t, s)
		putVisible(t, parent, "p", "old")
		child, err := parent.Child()
		if err != nil {
			t.Fatal(err)
		}
		replacement := strings.Repeat("n", 512)
		putVisible(t, child, "p", replacement)
		if _, err := child.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		info := transactionDiagnostics(t, parent)
		if info.Writes != 1 {
			t.Fatalf("merge did not replace the logical key: %+v", info)
		}
		if want := stagedOpCost(t, visibilitySpace, "p", putOp("p", replacement)); info.WriteBytes != want {
			t.Fatalf("WriteBytes = %d, want %d", info.WriteBytes, want)
		}
		if info.WriteBytes != parent.stagedBytes {
			t.Fatalf("WriteBytes %d disagrees with the staged write set %d", info.WriteBytes, parent.stagedBytes)
		}
	})
}

// TestDiagnosticsLargeReadStaysBounded is the boundedness contract: counters grow,
// but no per-key read state appears anywhere.
func TestDiagnosticsLargeReadStaysBounded(t *testing.T) {
	ctx := context.Background()
	const rows = 3000
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	for i := 0; i < rows; i++ {
		putVisible(t, seed, fmt.Sprintf("k%05d", i), "value")
	}
	mustCommit(t, seed)

	tx := beginBaseline(t, s)
	before := transactionDiagnostics(t, tx)

	observed := uint64(0)
	bytesObserved := uint64(0)
	for i := 0; i < rows; i++ {
		rowKey := fmt.Sprintf("k%05d", i)
		value, ok, err := tx.Get(visibilitySpace, []byte(rowKey))
		if err != nil || !ok {
			t.Fatalf("read %s = %v %v", rowKey, ok, err)
		}
		observed++
		bytesObserved += uint64(len(rowKey) + len(value))
	}
	for i := 0; i < 5; i++ {
		counted := uint64(0)
		countedBytes := uint64(0)
		if err := tx.Scan(ctx, visibilitySpace, func(k, v []byte) error {
			counted++
			countedBytes += uint64(len(k) + len(v))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		observed += counted
		bytesObserved += countedBytes
	}

	after := transactionDiagnostics(t, tx)
	if after.PointReads != before.PointReads+rows {
		t.Fatalf("PointReads = %d, want %d", after.PointReads, before.PointReads+rows)
	}
	if after.RangeReads != before.RangeReads+5 {
		t.Fatalf("RangeReads = %d, want %d", after.RangeReads, before.RangeReads+5)
	}
	if after.RowsObserved != before.RowsObserved+observed || after.BytesObserved != before.BytesObserved+bytesObserved {
		t.Fatalf("observed (%d, %d), want (%d, %d)", after.RowsObserved, after.BytesObserved,
			before.RowsObserved+observed, before.BytesObserved+bytesObserved)
	}
	// Everything except the counters is unchanged: reading cannot add state.
	before.PointReads, before.RangeReads, before.RowsObserved, before.BytesObserved = 0, 0, 0, 0
	after.PointReads, after.RangeReads, after.RowsObserved, after.BytesObserved = 0, 0, 0, 0
	if before != after {
		t.Fatalf("reading changed non-counter diagnostics state:\nbefore %+v\nafter  %+v", before, after)
	}
	// The registry still holds exactly this one transaction.
	if infos := s.txns.ActiveTransactions(); len(infos) != 1 || infos[0].ID != tx.ID {
		t.Fatalf("registry grew with the reads: %+v", infos)
	}
	requireBoundedDiagnosticsState(t)
}

// requireBoundedDiagnosticsState asserts, by type, that diagnostics state cannot
// grow with the number of keys a transaction read.
func requireBoundedDiagnosticsState(t *testing.T) {
	t.Helper()
	for _, typ := range []reflect.Type{reflect.TypeOf(TransactionInfo{}), reflect.TypeOf(transactionCounters{}), reflect.TypeOf(transactionEntry{})} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			switch field.Type.Kind() {
			case reflect.Map, reflect.Slice, reflect.Array, reflect.Interface, reflect.Func, reflect.Chan:
				t.Fatalf("%s.%s is %s: diagnostics state must not be able to hold a read set", typ, field.Name, field.Type)
			}
		}
	}
	// The only collection a transaction may hold is its bounded write buffer.
	for i := 0; i < reflect.TypeOf(Tx{}).NumField(); i++ {
		field := reflect.TypeOf(Tx{}).Field(i)
		switch field.Type.Kind() {
		case reflect.Map, reflect.Slice:
			if field.Name == "buffered" {
				continue // bounded by writeBufferBytes
			}
			t.Fatalf("Tx.%s is %s: the bounded write buffer must be the only per-transaction collection", field.Name, field.Type)
		}
	}
}

// TestDiagnosticsSnapshotIsReadOnly checks that the counters reach callers as
// copies, like the rest of the diagnostics snapshot.
func TestDiagnosticsSnapshotIsReadOnly(t *testing.T) {
	s := baselineStore(t)
	tx := beginBaseline(t, s)
	if _, _, err := tx.Get(visibilitySpace, []byte("a")); err != nil {
		t.Fatal(err)
	}
	putVisible(t, tx, "a", "value")

	infos := s.txns.ActiveTransactions()
	if len(infos) != 1 {
		t.Fatalf("registry holds %d transactions", len(infos))
	}
	infos[0].PointReads = 999
	infos[0].Writes = 999
	infos[0].WriteBytes = -1
	info := transactionDiagnostics(t, tx)
	if info.PointReads != 1 || info.Writes != 1 {
		t.Fatalf("tampering with a snapshot changed the record: %+v", info)
	}
	if info.WriteBytes != stagedOpCost(t, visibilitySpace, "a", putOp("a", "value")) {
		t.Fatalf("WriteBytes = %d", info.WriteBytes)
	}
}

func TestDiagnosticsReadOnlyTransactionHasNoWriteSet(t *testing.T) {
	s := baselineStore(t)
	seed := beginBaseline(t, s)
	putVisible(t, seed, "a", "1")
	mustCommit(t, seed)

	tx := beginBaseline(t, s)
	if _, _, err := tx.Get(visibilitySpace, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Scan(context.Background(), visibilitySpace, func([]byte, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	info := transactionDiagnostics(t, tx)
	if info.Writes != 0 || info.WriteBytes != 0 || info.PointDependencies != 0 || info.RangeDependencies != 0 {
		t.Fatalf("read-only transaction reports a write set: %+v", info)
	}
	if info.PointReads != 1 || info.RangeReads != 1 {
		t.Fatalf("read-only transaction lost its observations: %+v", info)
	}
	// Committing it releases the registry record; counters are not retained.
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.txns.Info(tx.ID); ok {
		t.Fatal("committed transaction is still registered")
	}
}

// TestDiagnosticsCountConflictsOnEveryCommitPath is the regression test for the
// aggregate conflict count: a conflict is a terminal outcome the diagnostics claim to
// report, so the real commit path -- not just a hand-built manager transition -- must
// mark it. Marking it changes no visibility, no validation and no commit decision, so
// the semantic outcome of every path is asserted alongside the counter.
func TestDiagnosticsCountConflictsOnEveryCommitPath(t *testing.T) {
	ctx := context.Background()
	paths := []struct {
		name  string
		store func(t *testing.T) *Store
		// proposer selects the commit path a root uses: the store itself uses the
		// local bounded/group/streaming paths, a distinct proposer value uses the
		// staged path.
		proposer func(s *Store) Proposer
	}{
		{
			name:     "local",
			store:    baselineStore,
			proposer: func(*Store) Proposer { return nil },
		},
		{
			name: "local-wal",
			store: func(t *testing.T) *Store {
				s, err := OpenWithOptions(t.TempDir(), Options{LocalWAL: true})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				return s
			},
			proposer: func(*Store) Proposer { return nil },
		},
		{
			name:     "staged",
			store:    baselineStore,
			proposer: func(s *Store) Proposer { return &forwardingProposer{store: s} },
		},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			s := path.store(t)
			seed := beginBaseline(t, s)
			putVisible(t, seed, "contended", "seed")
			mustCommit(t, seed)
			before := s.txns.Stats()

			// Both writers take one snapshot, so the second committer really
			// conflicts instead of merely losing a race to begin later.
			loser, err := s.Begin(ctx, path.proposer(s))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { loser.Rollback() })
			putVisible(t, loser, "contended", "loser")

			winner, err := s.Begin(ctx, path.proposer(s))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { winner.Rollback() })
			putVisible(t, winner, "contended", "winner")

			if _, err = winner.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = loser.Commit(ctx); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflicted commit = %v, want ErrConflict", err)
			}
			info := loser.Info()
			if info.State != TransactionAborted || info.AbortReason != "conflict" {
				t.Fatalf("conflicted outcome = %+v", info)
			}
			stats := s.txns.Stats()
			if stats.Conflicts != before.Conflicts+1 {
				t.Fatalf("Conflicts = %d, want %d", stats.Conflicts, before.Conflicts+1)
			}
			// The conflict abort is still an abort, counted once.
			if stats.Aborted != before.Aborted+1 {
				t.Fatalf("Aborted = %d, want %d", stats.Aborted, before.Aborted+1)
			}
			if stats.Committed != before.Committed+1 {
				t.Fatalf("Committed = %d, want %d", stats.Committed, before.Committed+1)
			}
			// The winner's write survives: marking the conflict changed nothing else.
			reader := beginBaseline(t, s)
			value, ok, err := reader.Get(visibilitySpace, []byte("contended"))
			if err != nil || !ok || string(value) != "winner" {
				t.Fatalf("winning value = %q (present %v, err %v)", value, ok, err)
			}
		})
	}
}
