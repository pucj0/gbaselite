package mvcc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// Visibility contract: every reader path must obey one rule for the same fixture.
//
//	A row is visible at a snapshot when the newest *committed* version at or below
//	that snapshot exists and is not a tombstone. An uncommitted version is skipped;
//	it never hides the older committed version it shadows.
//
// The fixture below is materialized in both physical layouts (nested buckets and
// the flat version bucket) and every reader path is compared against that rule,
// not against another implementation. The physical paths stay separate on
// purpose: point, cached, flat-lookup and forward-scan readers exist for
// performance, and only their semantics are shared.
const visibilitySpace = "s"

// visibilitySnapshots brackets every committed and uncommitted version in the
// fixture, plus both ends of the uint64 range.
var visibilitySnapshots = []uint64{0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, ^uint64(0)}

const visibilityHead = 50

var visibilityLayouts = []string{"nested", "flat"}

// fixtureVersion is one physical version of one key.
//
// committed must agree with visibilityCommitted: the publication marker records a
// whole commit sequence, not a single row, so a version at sequence N is either
// committed for every key or for none. The builder enforces that agreement.
type fixtureVersion struct {
	version   uint64
	committed bool
	deleted   bool
	value     string
}

type fixtureKey struct {
	key      string
	versions []fixtureVersion
}

// visibilityCommitted holds the sequences that carry a publication marker.
var visibilityCommitted = map[uint64]bool{10: true, 20: true, 30: true, 40: true, 50: true}

// visibilityFixture covers snapshot cutoffs, tombstones and revival, unpublished
// values, unpublished deletes, an empty value, a long history, a directory-only
// row, keys containing a zero byte, and prefix-related keys.
var visibilityFixture = []fixtureKey{
	{key: "a", versions: []fixtureVersion{
		{version: 10, committed: true, value: "a10"},
		{version: 20, committed: true, value: "a20"},
		{version: 30, committed: true, value: "a30"},
	}},
	// A committed tombstone hides the older value and a later value revives it.
	{key: "b", versions: []fixtureVersion{
		{version: 10, committed: true, value: "b10"},
		{version: 20, committed: true, deleted: true},
		{version: 30, committed: true, value: "b30"},
	}},
	// Only unpublished versions: never visible at any snapshot, including one
	// above the head (the crash window between install and publication).
	{key: "c", versions: []fixtureVersion{
		{version: 15, value: "c15"},
		{version: 55, value: "c55"},
	}},
	// An unpublished newer value must fall back to the committed older value.
	{key: "d", versions: []fixtureVersion{
		{version: 10, committed: true, value: "d10"},
		{version: 25, value: "d25"},
		{version: 55, value: "d55"},
	}},
	// An unpublished delete must not hide the committed value.
	{key: "e", versions: []fixtureVersion{
		{version: 10, committed: true, value: "e10"},
		{version: 25, deleted: true},
	}},
	// A committed live row with an empty value is present, not missing.
	{key: "empty", versions: []fixtureVersion{
		{version: 10, committed: true},
	}},
	// Five committed versions cross the flat scan reader's bounded-walk fallback.
	{key: "f", versions: []fixtureVersion{
		{version: 10, committed: true, value: "f10"},
		{version: 20, committed: true, value: "f20"},
		{version: 30, committed: true, value: "f30"},
		{version: 40, committed: true, value: "f40"},
		{version: 50, committed: true, value: "f50"},
	}},
	// A row directory entry with no physical version at all.
	{key: "g"},
	// A key containing a zero byte exercises the flat key escaping.
	{key: "n\x00x", versions: []fixtureVersion{
		{version: 10, committed: true, value: "n10"},
		{version: 20, committed: true, deleted: true},
		{version: 30, committed: true, value: "n30"},
	}},
	{key: "pre", versions: []fixtureVersion{{version: 10, committed: true, value: "pre10"}}},
	{key: "pre2", versions: []fixtureVersion{{version: 20, committed: true, value: "pre20"}}},
}

// buildVisibilityFixture materializes the shared fixture in one physical layout.
func buildVisibilityFixture(t *testing.T, flat bool) *Store {
	t.Helper()
	s := newContractStore(t, flat)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for _, fixture := range visibilityFixture {
			logical, err := key(visibilitySpace, []byte(fixture.key))
			if err != nil {
				return err
			}
			if len(fixture.versions) == 0 {
				// A row directory entry with no physical version.
				if flat {
					if err := tx.Bucket(dataBucket).Put(logical, []byte{1}); err != nil {
						return err
					}
				} else if _, err := tx.Bucket(dataBucket).CreateBucket(logical); err != nil {
					return err
				}
				continue
			}
			for _, version := range fixture.versions {
				if version.committed != visibilityCommitted[version.version] {
					t.Fatalf("fixture key %q version %d: committed=%v disagrees with the global publication marker set",
						fixture.key, version.version, version.committed)
				}
				if version.committed {
					if err := tx.Bucket(commitsBucket).Put(sequence(version.version), []byte{1}); err != nil {
						return err
					}
				}
				payload := []byte{0}
				if !version.deleted {
					payload = append([]byte{1}, version.value...)
				}
				if err := putVersion(tx, logical, sequence(version.version), payload); err != nil {
					return err
				}
			}
		}
		return tx.Bucket(metaBucket).Put([]byte("head"), sequence(visibilityHead))
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// newContractStore opens an empty store in the requested layout.
func newContractStore(t *testing.T, flat bool) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if flat {
		if err := s.db.Update(func(tx *bolt.Tx) error {
			if _, err := tx.CreateBucket(flatVersionsBucket); err != nil {
				return err
			}
			return tx.Bucket(metaBucket).Put(layoutKey, []byte{1})
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// visibilityExpectation is the contract rule evaluated directly from the fixture
// table: the newest committed version at or below the snapshot, unless it is a
// tombstone.
func visibilityExpectation(snapshot uint64) map[string]string {
	want := make(map[string]string)
	for _, fixture := range visibilityFixture {
		var newest *fixtureVersion
		for i := range fixture.versions {
			version := &fixture.versions[i]
			if !version.committed || version.version > snapshot {
				continue
			}
			if newest == nil || version.version > newest.version {
				newest = version
			}
		}
		if newest == nil || newest.deleted {
			continue
		}
		want[fixture.key] = newest.value
	}
	return want
}

// visibilityVersionExpectation is the version each readable path must report: the
// newest committed version at or below the snapshot, whether it is a value or a
// tombstone. Keys without such a version resolve to 0.
func visibilityVersionExpectation(snapshot uint64) map[string]uint64 {
	want := make(map[string]uint64)
	for _, fixture := range visibilityFixture {
		var newest uint64
		for _, version := range fixture.versions {
			if version.committed && version.version <= snapshot && version.version > newest {
				newest = version.version
			}
		}
		want[fixture.key] = newest
	}
	return want
}

// putVisible and deleteVisible write into the contract namespace. The shared
// baseline helpers write to their own namespace, so the contract tests cannot
// reuse them for fixtures.
func putVisible(t *testing.T, tx *Tx, key, value string) {
	t.Helper()
	if err := tx.Put(visibilitySpace, []byte(key), []byte(value)); err != nil {
		t.Fatal(err)
	}
}

func deleteVisible(t *testing.T, tx *Tx, key string) {
	t.Helper()
	if err := tx.Delete(visibilitySpace, []byte(key)); err != nil {
		t.Fatal(err)
	}
}

func storeIsFlat(t *testing.T, s *Store) bool {
	t.Helper()
	flat := false
	if err := s.db.View(func(tx *bolt.Tx) error { flat = flatLayout(tx); return nil }); err != nil {
		t.Fatal(err)
	}
	return flat
}

// spaceKeys lists the row keys physically present in one namespace. Both layouts
// keep that directory in dataBucket, which is exactly what the scan paths walk.
func spaceKeys(t *testing.T, s *Store, space string) []string {
	t.Helper()
	prefix, err := key(space, nil)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(dataBucket).Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			keys = append(keys, string(k[len(prefix):]))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	return keys
}

// physicalVersionCount counts the physical versions of one key in either layout,
// so a test can prove its fixture really contains unpublished versions.
func physicalVersionCount(t *testing.T, s *Store, space, rowKey string) int {
	t.Helper()
	logical, err := key(space, []byte(rowKey))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := s.db.View(func(tx *bolt.Tx) error {
		if flatLayout(tx) {
			prefix := flatPrefix(logical)
			c := tx.Bucket(flatVersionsBucket).Cursor()
			for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				if len(k) == len(prefix)+8 {
					count++
				}
			}
			return nil
		}
		rows := tx.Bucket(dataBucket).Bucket(logical)
		if rows == nil {
			return nil
		}
		return rows.ForEach(func(_, _ []byte) error { count++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func scanRows(t *testing.T, s *Store, scan func(yield func([]byte, []byte) error) error) map[string]string {
	t.Helper()
	rows := map[string]string{}
	if err := scan(func(k, v []byte) error {
		key := string(k)
		if _, duplicate := rows[key]; duplicate {
			t.Fatalf("scan returned %q twice", key)
		}
		rows[key] = string(v)
		return nil
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return rows
}

// readerPathRows runs every full-space reader path at one snapshot and returns
// path -> rows, plus the version each version-aware path resolved.
//
//   - visible            uncached point read
//   - visibilityReader   cached point read behind Store.Scan and rangePage
//   - flatVisible        flat-layout point lookup
//   - flatScanReader     flat forward-scan fast path, one fresh reader per key
//   - flatScanForward    one forward reader over the whole space, as rangePage drives it
//   - Store.Get          the public point API
//   - Store.Scan         paged unordered scan
//   - Store.ScanRange    paged forward range scan
//   - Store.ScanRangeReverse  paged reverse range scan
func readerPathRows(t *testing.T, s *Store, snapshot uint64) (map[string]map[string]string, map[string]map[string]uint64) {
	t.Helper()
	flat := storeIsFlat(t, s)
	keys := spaceKeys(t, s, visibilitySpace)
	prefix := prefixOf(t, visibilitySpace)
	rows := map[string]map[string]string{}
	versions := map[string]map[string]uint64{}

	record := func(path string, row map[string]string, version map[string]uint64) {
		rows[path] = row
		if version != nil {
			versions[path] = version
		}
	}

	if err := s.db.View(func(tx *bolt.Tx) error {
		uncachedRows, uncachedVersions := map[string]string{}, map[string]uint64{}
		cachedRows, cachedVersions := map[string]string{}, map[string]uint64{}
		// One reader for the whole space, exactly as a scan or a range page uses it.
		reader := newVisibilityReader(tx, snapshot)
		var fastRows map[string]string
		var fastVersionMap map[string]uint64
		var flatRows map[string]string
		var flatVersionMap map[string]uint64
		if flat {
			fastRows, fastVersionMap = map[string]string{}, map[string]uint64{}
			flatRows, flatVersionMap = map[string]string{}, map[string]uint64{}
		}
		for _, rowKey := range keys {
			logical, err := key(visibilitySpace, []byte(rowKey))
			if err != nil {
				return err
			}
			value, version, ok := visible(tx, logical, snapshot)
			uncachedVersions[rowKey] = version
			if ok {
				uncachedRows[rowKey] = string(value)
			}
			value, version, ok = reader.visible(logical)
			cachedVersions[rowKey] = version
			if ok {
				cachedRows[rowKey] = string(value)
			}
			if !flat {
				continue
			}
			value, version, ok = flatVisible(tx, logical, snapshot)
			flatVersionMap[rowKey] = version
			if ok {
				flatRows[rowKey] = string(value)
			}
			// A fresh fast-path reader per key exercises its accumulating walk and
			// its bounded-walk fallback independently for every key.
			fresh := newVisibilityReader(tx, snapshot)
			scanner := &flatScanReader{reader: &fresh, cursor: tx.Bucket(flatVersionsBucket).Cursor()}
			value, version, ok = scanner.visible(logical)
			fastVersionMap[rowKey] = version
			if ok {
				fastRows[rowKey] = string(value)
			}
		}
		record("visible", uncachedRows, uncachedVersions)
		record("visibilityReader", cachedRows, cachedVersions)
		if !flat {
			return nil
		}
		record("flatVisible", flatRows, flatVersionMap)
		record("flatScanReader", fastRows, fastVersionMap)

		// One forward reader over the whole space, driven exactly as rangePage
		// drives it: ascending directory keys, one shared reader.
		forwardRows, forwardVersions := map[string]string{}, map[string]uint64{}
		shared := newVisibilityReader(tx, snapshot)
		forward := &flatScanReader{reader: &shared, cursor: tx.Bucket(flatVersionsBucket).Cursor()}
		c := tx.Bucket(dataBucket).Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			rowKey := string(k[len(prefix):])
			value, version, ok := forward.visible(k)
			forwardVersions[rowKey] = version
			if ok {
				forwardRows[rowKey] = string(value)
			}
		}
		record("flatScanForward", forwardRows, forwardVersions)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	getRows, getVersions := map[string]string{}, map[string]uint64{}
	for _, key := range keys {
		value, version, ok, err := s.Get(snapshot, visibilitySpace, []byte(key))
		if err != nil {
			t.Fatal(err)
		}
		getVersions[key] = version
		if ok {
			getRows[key] = string(value)
		}
	}
	record("Store.Get", getRows, getVersions)
	record("Store.Scan", scanRows(t, s, func(yield func([]byte, []byte) error) error {
		return s.Scan(context.Background(), snapshot, visibilitySpace, yield)
	}), nil)
	record("Store.ScanRange", scanRows(t, s, func(yield func([]byte, []byte) error) error {
		return s.ScanRange(context.Background(), snapshot, visibilitySpace, KeyRange{}, yield)
	}), nil)
	record("Store.ScanRangeReverse", scanRows(t, s, func(yield func([]byte, []byte) error) error {
		return s.ScanRange(context.Background(), snapshot, visibilitySpace, KeyRange{Reverse: true}, yield)
	}), nil)
	return rows, versions
}

func prefixOf(t *testing.T, space string) []byte {
	t.Helper()
	prefix, err := key(space, nil)
	if err != nil {
		t.Fatal(err)
	}
	return prefix
}

func requireSameRows(t *testing.T, label string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d\n got: %s\nwant: %s", label, len(got), len(want), formatRows(got), formatRows(want))
	}
	for _, key := range sortedRowKeys(want) {
		value, present := got[key]
		if !present || value != want[key] {
			t.Fatalf("%s: %q = (%q, present=%v), want %q", label, key, value, present, want[key])
		}
	}
}

// requireSameVersions asserts the version each version-aware path resolved.
//
// A key that the model expects to be absent (version 0) may also be missing from
// a path's probe set: a nested row directory entry with no physical version has
// no flat counterpart at all after a layout export. What must hold everywhere is
// that every expected non-zero version is reported, and that no path invents a
// version or a key.
func requireSameVersions(t *testing.T, label string, got, want map[string]uint64) {
	t.Helper()
	for _, key := range sortedRowKeys(want) {
		wantVersion := want[key]
		gotVersion, present := got[key]
		if wantVersion == 0 {
			if present && gotVersion != 0 {
				t.Fatalf("%s: %q resolved version %d, want none", label, key, gotVersion)
			}
			continue
		}
		if !present || gotVersion != wantVersion {
			t.Fatalf("%s: %q resolved version %d (present=%v), want %d", label, key, gotVersion, present, wantVersion)
		}
	}
	for _, key := range sortedRowKeys(got) {
		if _, known := want[key]; !known {
			t.Fatalf("%s: version probe set contains unexpected key %q", label, key)
		}
	}
}

// requireAllPathsMatch asserts that every full-space reader path at the snapshot
// reports exactly the expected rows.
func requireAllPathsMatch(t *testing.T, label string, s *Store, snapshot uint64, want map[string]string) {
	t.Helper()
	rows, _ := readerPathRows(t, s, snapshot)
	for _, path := range sortedRowKeys(rows) {
		requireSameRows(t, fmt.Sprintf("%s/%s@%d", label, path, snapshot), rows[path], want)
	}
}

// requireTxPathsMatch asserts the transaction-level paths: Get, Scan and both
// range directions on one transaction.
func requireTxPathsMatch(t *testing.T, label string, tx *Tx, want map[string]string) {
	t.Helper()
	probes := map[string]bool{}
	for _, key := range spaceKeys(t, tx.store, visibilitySpace) {
		probes[key] = true
	}
	for key := range want {
		probes[key] = true
	}
	got := map[string]string{}
	ordered := make([]string, 0, len(probes))
	for key := range probes {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		value, ok, err := tx.Get(visibilitySpace, []byte(key))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			got[key] = string(value)
		}
	}
	requireSameRows(t, label+"/Tx.Get", got, want)
	requireSameRows(t, label+"/Tx.Scan", scanRows(t, tx.store, func(yield func([]byte, []byte) error) error {
		return tx.Scan(context.Background(), visibilitySpace, yield)
	}), want)
	requireSameRows(t, label+"/Tx.ScanRange", scanRows(t, tx.store, func(yield func([]byte, []byte) error) error {
		return tx.ScanRange(context.Background(), visibilitySpace, KeyRange{}, yield)
	}), want)
	requireSameRows(t, label+"/Tx.ScanRangeReverse", scanRows(t, tx.store, func(yield func([]byte, []byte) error) error {
		return tx.ScanRange(context.Background(), visibilitySpace, KeyRange{Reverse: true}, yield)
	}), want)
}

func sortedRowKeys[V any](rows map[string]V) []string {
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func formatRows(rows map[string]string) string {
	parts := make([]string, 0, len(rows))
	for _, key := range sortedRowKeys(rows) {
		parts = append(parts, fmt.Sprintf("%q=%q", key, rows[key]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// TestVisibilityContractAcrossReaderPaths is the core contract: for every
// snapshot and both layouts, all reader paths agree with the rule for the shared
// fixture. It covers snapshot cutoffs, repeatable read, tombstones, unpublished
// versions (below and above the head) and cross-path agreement at once.
func TestVisibilityContractAcrossReaderPaths(t *testing.T) {
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			s := buildVisibilityFixture(t, layout == "flat")

			// The fixture must really contain the unpublished versions and the
			// tombstone, otherwise "unpublished is invisible" would pass vacuously.
			for _, probe := range []struct {
				key  string
				want int
			}{{"c", 2}, {"d", 3}, {"e", 2}, {"b", 3}, {"g", 0}} {
				if count := physicalVersionCount(t, s, visibilitySpace, probe.key); count != probe.want {
					t.Fatalf("fixture key %q has %d physical versions, want %d", probe.key, count, probe.want)
				}
			}
			if err := s.db.View(func(tx *bolt.Tx) error {
				for _, unpublished := range []uint64{15, 25, 55} {
					if tx.Bucket(commitsBucket).Get(sequence(unpublished)) != nil {
						t.Fatalf("sequence %d unexpectedly has a publication marker", unpublished)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			for _, snapshot := range visibilitySnapshots {
				want := visibilityExpectation(snapshot)
				wantVersions := visibilityVersionExpectation(snapshot)
				rows, versions := readerPathRows(t, s, snapshot)
				for _, path := range sortedRowKeys(rows) {
					requireSameRows(t, fmt.Sprintf("layout=%s/%s@%d", layout, path, snapshot), rows[path], want)
				}
				for _, path := range sortedRowKeys(versions) {
					requireSameVersions(t, fmt.Sprintf("layout=%s/%s@%d", layout, path, snapshot), versions[path], wantVersions)
				}
				if len(rows) < 6 {
					t.Fatalf("only %d reader paths were exercised", len(rows))
				}
				// Every path must be present: a refactor that silently stops
				// exercising one of them must fail here rather than pass quietly.
				wantPaths := []string{"Store.Get", "Store.Scan", "Store.ScanRange", "Store.ScanRangeReverse", "visibilityReader", "visible"}
				if layout == "flat" {
					wantPaths = append(wantPaths, "flatScanForward", "flatScanReader", "flatVisible")
				}
				sort.Strings(wantPaths)
				gotPaths := sortedRowKeys(rows)
				if len(gotPaths) != len(wantPaths) {
					t.Fatalf("reader paths = %v, want %v", gotPaths, wantPaths)
				}
				for i := range gotPaths {
					if gotPaths[i] != wantPaths[i] {
						t.Fatalf("reader paths = %v, want %v", gotPaths, wantPaths)
					}
				}
			}
		})
	}
}

// TestVisibilityContractTransactionPathsAtHead checks the transaction paths
// against the same fixture at the head snapshot.
func TestVisibilityContractTransactionPathsAtHead(t *testing.T) {
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			s := buildVisibilityFixture(t, layout == "flat")
			tx := beginBaseline(t, s)
			if tx.Snapshot != visibilityHead {
				t.Fatalf("transaction snapshot = %d, want %d", tx.Snapshot, visibilityHead)
			}
			requireTxPathsMatch(t, "layout="+layout, tx, visibilityExpectation(visibilityHead))
		})
	}
}

// Area 2: a transaction keeps reading the versions of its own snapshot after
// another transaction commits, through every path.
func TestVisibilityContractRepeatableRead(t *testing.T) {
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			s := newContractStore(t, layout == "flat")
			seed := beginBaseline(t, s)
			putVisible(t, seed, "k", "v1")
			putVisible(t, seed, "gone", "old")
			mustCommit(t, seed)

			reader := beginBaseline(t, s)
			readTS := reader.Snapshot
			requireTxPathsMatch(t, "before", reader, map[string]string{"k": "v1", "gone": "old"})

			writer := beginBaseline(t, s)
			putVisible(t, writer, "k", "v2")
			deleteVisible(t, writer, "gone")
			mustCommit(t, writer)
			if head := mustHead(t, s); head <= readTS {
				t.Fatalf("head %d did not advance past the reader snapshot %d", head, readTS)
			}

			// The old snapshot is unchanged through every reader path, and the
			// committed delete is invisible at it.
			requireAllPathsMatch(t, "pinned", s, readTS, map[string]string{"k": "v1", "gone": "old"})
			requireTxPathsMatch(t, "pinned", reader, map[string]string{"k": "v1", "gone": "old"})
			// A transaction that begins later sees the new state.
			requireAllPathsMatch(t, "current", s, mustHead(t, s), map[string]string{"k": "v2"})
			fresh := beginBaseline(t, s)
			requireTxPathsMatch(t, "current", fresh, map[string]string{"k": "v2"})
			if reader.Snapshot != readTS {
				t.Fatalf("read timestamp moved: %d -> %d", readTS, reader.Snapshot)
			}
		})
	}
}

// Area 3: an uncommitted write set is invisible to every other reader, including
// the newest-revision read.
func TestVisibilityContractDirtyRead(t *testing.T) {
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			s := newContractStore(t, layout == "flat")
			seed := beginBaseline(t, s)
			putVisible(t, seed, "k", "v1")
			mustCommit(t, seed)
			head := mustHead(t, s)

			writer := beginBaseline(t, s)
			putVisible(t, writer, "k", "uncommitted")
			putVisible(t, writer, "fresh", "uncommitted")
			deleteVisible(t, writer, "k")
			// Restore a live uncommitted value so the case is "new value", not only
			// "uncommitted tombstone".
			putVisible(t, writer, "k", "uncommitted-2")

			want := map[string]string{"k": "v1"}
			requireAllPathsMatch(t, "dirty", s, head, want)
			// The newest-revision read must not see an unpublished version either.
			requireAllPathsMatch(t, "dirty-newest", s, ^uint64(0), want)
			// And a transaction that begins now must not see it.
			observer := beginBaseline(t, s)
			requireTxPathsMatch(t, "dirty", observer, want)

			if err := writer.Rollback(); err != nil {
				t.Fatal(err)
			}
			requireAllPathsMatch(t, "after-rollback", s, head, want)
		})
	}
}

// Area 4: a transaction reads its own writes through every transaction path,
// including replacement and its own tombstone.
func TestVisibilityContractReadYourWrites(t *testing.T) {
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			s := newContractStore(t, layout == "flat")
			tx := beginBaseline(t, s)
			requireTxPathsMatch(t, "empty", tx, map[string]string{})

			putVisible(t, tx, "a", "first")
			requireTxPathsMatch(t, "own-insert", tx, map[string]string{"a": "first"})

			putVisible(t, tx, "a", "second")
			requireTxPathsMatch(t, "own-replace", tx, map[string]string{"a": "second"})

			putVisible(t, tx, "b", "value")
			deleteVisible(t, tx, "b")
			requireTxPathsMatch(t, "own-tombstone", tx, map[string]string{"a": "second"})

			// A concurrent transaction sees none of it.
			observer := beginBaseline(t, s)
			requireTxPathsMatch(t, "observer", observer, map[string]string{})
		})
	}
}

// Area 5: child visibility in both directions, before and after merge, and after
// a parent rollback.
func TestVisibilityContractChildVisibility(t *testing.T) {
	ctx := context.Background()
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			t.Run("merge", func(t *testing.T) {
				s := newContractStore(t, layout == "flat")
				parent := beginBaseline(t, s)
				putVisible(t, parent, "p", "parent")

				child, err := parent.Child()
				if err != nil {
					t.Fatal(err)
				}
				defer child.Rollback()
				// The child sees the parent write.
				requireTxPathsMatch(t, "child-sees-parent", child, map[string]string{"p": "parent"})

				putVisible(t, child, "c", "child")
				putVisible(t, child, "p", "child-wins")
				// The child sees its own writes, which win over the parent's.
				requireTxPathsMatch(t, "child-own", child, map[string]string{"p": "child-wins", "c": "child"})
				// The parent does not see unmerged child writes, through any path.
				requireTxPathsMatch(t, "parent-unmerged", parent, map[string]string{"p": "parent"})
				requireAllPathsMatch(t, "store-unmerged", s, parent.Snapshot, map[string]string{})

				if _, err := child.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				// After the merge the parent sees the merged writes...
				requireTxPathsMatch(t, "parent-merged", parent, map[string]string{"p": "child-wins", "c": "child"})
				// ...while the store still holds nothing, because the parent has not
				// published anything yet.
				requireAllPathsMatch(t, "store-before-parent-commit", s, parent.Snapshot, map[string]string{})

				if _, err := parent.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				requireAllPathsMatch(t, "store-after-parent-commit", s, mustHead(t, s), map[string]string{"p": "child-wins", "c": "child"})
			})

			t.Run("parent-rollback", func(t *testing.T) {
				s := newContractStore(t, layout == "flat")
				parent := beginBaseline(t, s)
				putVisible(t, parent, "p", "parent")
				child, err := parent.Child()
				if err != nil {
					t.Fatal(err)
				}
				defer child.Rollback()
				putVisible(t, child, "c", "child")
				if _, err := child.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				requireTxPathsMatch(t, "merged", parent, map[string]string{"p": "parent", "c": "child"})

				if err := parent.Rollback(); err != nil {
					t.Fatal(err)
				}
				// Every merged child write disappears with the parent.
				requireAllPathsMatch(t, "parent-rolled-back", s, ^uint64(0), map[string]string{})
			})

			t.Run("child-rollback", func(t *testing.T) {
				s := newContractStore(t, layout == "flat")
				parent := beginBaseline(t, s)
				putVisible(t, parent, "p", "parent")
				child, err := parent.Child()
				if err != nil {
					t.Fatal(err)
				}
				putVisible(t, child, "c", "child")
				if err := child.Rollback(); err != nil {
					t.Fatal(err)
				}
				requireTxPathsMatch(t, "child-rolled-back", parent, map[string]string{"p": "parent"})
			})
		})
	}
}

// Area 7/8: unpublished versions must not count as a range dependency, and the
// range reader must respect the snapshot it was created with.
func TestVisibilityContractRangeDependency(t *testing.T) {
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			s := buildVisibilityFixture(t, layout == "flat")
			rangeVersion := func(bounds KeyRange, snapshot uint64) uint64 {
				t.Helper()
				dependency, err := json.Marshal(rangeDependency{Space: visibilitySpace, Bounds: bounds})
				if err != nil {
					t.Fatal(err)
				}
				guard, err := key(rangeGuardSpace, append([]byte{0, 1}, dependency...))
				if err != nil {
					t.Fatal(err)
				}
				version := uint64(0)
				if err := s.db.View(func(tx *bolt.Tx) error {
					reader := newVisibilityReader(tx, snapshot)
					_, version, _ = reader.visible(guard)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return version
			}

			// "d" and "e" hold unpublished versions at 25, but their only committed
			// change is at 10. A guard must not see the unpublished ones.
			unpublished := KeyRange{Lower: []byte("d"), Upper: []byte("e"), LowerInclusive: true, UpperInclusive: true}
			if version := rangeVersion(unpublished, ^uint64(0)); version != 10 {
				t.Fatalf("range dependency over unpublished versions = %d, want 10", version)
			}
			// The whole space resolves to the newest committed change.
			if version := rangeVersion(KeyRange{}, ^uint64(0)); version != visibilityHead {
				t.Fatalf("whole-space range dependency = %d, want %d", version, visibilityHead)
			}
			// The uncommitted "c" alone contributes nothing.
			if version := rangeVersion(KeyRange{Lower: []byte("c"), Upper: []byte("c"), LowerInclusive: true, UpperInclusive: true}, ^uint64(0)); version != 0 {
				t.Fatalf("range dependency over an unpublished-only key = %d, want 0", version)
			}
			// The reader's snapshot still bounds the answer, which is why every
			// commit path builds it with ^uint64(0).
			if version := rangeVersion(KeyRange{}, 10); version != 10 {
				t.Fatalf("range dependency at snapshot 10 = %d, want 10", version)
			}
			if version := rangeVersion(KeyRange{}, 5); version != 0 {
				t.Fatalf("range dependency at snapshot 5 = %d, want 0", version)
			}
		})
	}
}

// Area 8: the paged scan paths must agree with point reads across page
// boundaries, where the cursor advances by directory key.
func TestVisibilityContractScanPaging(t *testing.T) {
	ctx := context.Background()
	const rows = 600
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			s := newContractStore(t, layout == "flat")
			first := beginBaseline(t, s)
			original := map[string]string{}
			for i := 0; i < rows; i++ {
				key := fmt.Sprintf("k%05d", i)
				value := fmt.Sprintf("one-%05d", i)
				putVisible(t, first, key, value)
				original[key] = value
			}
			if _, err := first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			firstSnapshot := mustHead(t, s)

			second := beginBaseline(t, s)
			current := map[string]string{}
			for i := 0; i < rows; i++ {
				key := fmt.Sprintf("k%05d", i)
				switch {
				case i%3 == 0:
					deleteVisible(t, second, key)
				case i%5 == 0:
					value := fmt.Sprintf("two-%05d", i)
					putVisible(t, second, key, value)
					current[key] = value
				default:
					current[key] = original[key]
				}
			}
			if _, err := second.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			head := mustHead(t, s)

			requireAllPathsMatch(t, "paged-first", s, firstSnapshot, original)
			requireAllPathsMatch(t, "paged-head", s, head, current)
			// A reverse scan visits the same rows in the opposite direction.
			reverse := scanRows(t, s, func(yield func([]byte, []byte) error) error {
				return s.ScanRange(ctx, head, visibilitySpace, KeyRange{Reverse: true}, yield)
			})
			requireSameRows(t, "paged-reverse", reverse, current)

			// Point reads agree with the paged scans for a spread of keys.
			for i := 0; i < rows; i += 7 {
				key := fmt.Sprintf("k%05d", i)
				value, _, ok, err := s.Get(head, visibilitySpace, []byte(key))
				if err != nil {
					t.Fatal(err)
				}
				want, wantOK := current[key]
				if ok != wantOK || string(value) != want {
					t.Fatalf("point read %q = (%q, %v), want (%q, %v)", key, value, ok, want, wantOK)
				}
			}
		})
	}
}

// TestVisibilityFixtureIsDiscriminating checks the fixture table itself, so a
// broken fixture cannot make the contract tests pass vacuously. Every assertion
// here is written by hand rather than derived from the same rule the tests use.
func TestVisibilityFixtureIsDiscriminating(t *testing.T) {
	cases := []struct {
		snapshot uint64
		want     map[string]string
	}{
		{0, map[string]string{}},
		{5, map[string]string{}},
		{10, map[string]string{"a": "a10", "b": "b10", "d": "d10", "e": "e10", "empty": "", "f": "f10", "n\x00x": "n10", "pre": "pre10"}},
		{15, map[string]string{"a": "a10", "b": "b10", "d": "d10", "e": "e10", "empty": "", "f": "f10", "n\x00x": "n10", "pre": "pre10"}},
		{19, map[string]string{"a": "a10", "b": "b10", "d": "d10", "e": "e10", "empty": "", "f": "f10", "n\x00x": "n10", "pre": "pre10"}},
		// 20 deletes "b" and "n\x00x" and publishes a20, f20 and pre2.
		{20, map[string]string{"a": "a20", "d": "d10", "e": "e10", "empty": "", "f": "f20", "pre": "pre10", "pre2": "pre20"}},
		// 25 publishes nothing: it must not appear anywhere.
		{25, map[string]string{"a": "a20", "d": "d10", "e": "e10", "empty": "", "f": "f20", "pre": "pre10", "pre2": "pre20"}},
		// 30 revives "b" and "n\x00x".
		{30, map[string]string{"a": "a30", "b": "b30", "d": "d10", "e": "e10", "empty": "", "f": "f30", "n\x00x": "n30", "pre": "pre10", "pre2": "pre20"}},
		// 55 is above the head and unpublished: nothing changes.
		{55, map[string]string{"a": "a30", "b": "b30", "d": "d10", "e": "e10", "empty": "", "f": "f50", "n\x00x": "n30", "pre": "pre10", "pre2": "pre20"}},
		{^uint64(0), map[string]string{"a": "a30", "b": "b30", "d": "d10", "e": "e10", "empty": "", "f": "f50", "n\x00x": "n30", "pre": "pre10", "pre2": "pre20"}},
	}
	signatures := map[string]bool{}
	for _, c := range cases {
		requireSameRows(t, fmt.Sprintf("fixture@%d", c.snapshot), visibilityExpectation(c.snapshot), c.want)
	}
	for _, snapshot := range visibilitySnapshots {
		signatures[formatRows(visibilityExpectation(snapshot))] = true
	}
	// {}, after 10, after 20, after 30, after 40, after 50.
	if len(signatures) < 6 {
		t.Fatalf("the fixture only produces %d distinct states; it cannot discriminate reader paths", len(signatures))
	}
	// "c" holds only unpublished versions, so it is never part of any expectation.
	for _, snapshot := range visibilitySnapshots {
		if _, present := visibilityExpectation(snapshot)["c"]; present {
			t.Fatalf("unpublished-only key c is visible at snapshot %d", snapshot)
		}
	}
}

// TestVisibilityContractBoundedRangePaths covers the bounded range cursor, which
// is separate code from the unbounded scan, in both directions.
func TestVisibilityContractBoundedRangePaths(t *testing.T) {
	ctx := context.Background()
	ranges := []struct {
		name   string
		bounds KeyRange
	}{
		{"unbounded-lower", KeyRange{Upper: []byte("empty"), UpperInclusive: true}},
		{"unbounded-upper", KeyRange{Lower: []byte("empty"), LowerInclusive: true}},
		{"inclusive", KeyRange{Lower: []byte("c"), Upper: []byte("e"), LowerInclusive: true, UpperInclusive: true}},
		{"exclusive", KeyRange{Lower: []byte("c"), Upper: []byte("e")}},
		{"empty-upper", KeyRange{Upper: []byte{}, UpperInclusive: true}},
		{"prefix-pair", KeyRange{Lower: []byte("pre"), Upper: []byte("pre2"), LowerInclusive: true, UpperInclusive: true}},
		{"single-key", KeyRange{Lower: []byte("b"), Upper: []byte("b"), LowerInclusive: true, UpperInclusive: true}},
	}
	for _, layout := range visibilityLayouts {
		t.Run(layout, func(t *testing.T) {
			s := buildVisibilityFixture(t, layout == "flat")
			for _, snapshot := range visibilitySnapshots {
				full := visibilityExpectation(snapshot)
				for _, probe := range ranges {
					want := restrictRows(full, probe.bounds)
					forward := scanRows(t, s, func(yield func([]byte, []byte) error) error {
						return s.ScanRange(ctx, snapshot, visibilitySpace, probe.bounds, yield)
					})
					requireSameRows(t, fmt.Sprintf("%s@%d/forward", probe.name, snapshot), forward, want)
					reverse := probe.bounds
					reverse.Reverse = true
					backward := scanRows(t, s, func(yield func([]byte, []byte) error) error {
						return s.ScanRange(ctx, snapshot, visibilitySpace, reverse, yield)
					})
					requireSameRows(t, fmt.Sprintf("%s@%d/reverse", probe.name, snapshot), backward, want)
				}
			}
		})
	}
}

func restrictRows(rows map[string]string, bounds KeyRange) map[string]string {
	out := map[string]string{}
	for key, value := range rows {
		if bounds.Contains([]byte(key)) {
			out[key] = value
		}
	}
	return out
}

// TestVisibilityContractFlatMigrationPreservesSemantics: exporting the nested
// layout must preserve visibility for every path, including unpublished versions
// and tombstones.
func TestVisibilityContractFlatMigrationPreservesSemantics(t *testing.T) {
	nested := buildVisibilityFixture(t, false)
	target := filepath.Join(t.TempDir(), "flat")
	if err := nested.ExportLayout(context.Background(), target, "flat"); err != nil {
		t.Fatal(err)
	}
	flat, err := OpenWithOptions(target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer flat.Close()
	if !storeIsFlat(t, flat) {
		t.Fatal("exported store is not in the flat layout")
	}
	for _, snapshot := range visibilitySnapshots {
		want := visibilityExpectation(snapshot)
		wantVersions := visibilityVersionExpectation(snapshot)
		rows, versions := readerPathRows(t, flat, snapshot)
		for _, path := range sortedRowKeys(rows) {
			requireSameRows(t, fmt.Sprintf("migrated/%s@%d", path, snapshot), rows[path], want)
		}
		for _, path := range sortedRowKeys(versions) {
			requireSameVersions(t, fmt.Sprintf("migrated/%s@%d", path, snapshot), versions[path], wantVersions)
		}
	}
}
