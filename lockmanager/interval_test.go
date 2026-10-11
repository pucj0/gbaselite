package lockmanager

import (
	"testing"
)

// interval_test.go is T020: the interval containment and overlap rules.
//
// Every case is stated as a position that must be included or excluded, not as a comparison of
// bounds, because the property that matters is "does this gap protect this insertion position".
// A bounds comparison can be right for the wrong reason and still block the wrong insert.

// gap builds one Gap component over [lower, upper].
func gap(lower, upper string, lowerInclusive, upperInclusive bool) ResourceRef {
	ref := ResourceRef{Identity: Identity{
		Kind:           KindInterval,
		Mode:           ModeGap,
		Space:          "index/PRIMARY",
		TableID:        "t1",
		IndexID:        "PRIMARY",
		LowerInclusive: lowerInclusive,
		UpperInclusive: upperInclusive,
	}}
	if lower != infinity {
		ref.Lower = []byte(lower)
	}
	if upper != infinity {
		ref.Upper = []byte(upper)
	}
	return ref
}

// infinity marks an unbounded side so a test can write it explicitly.
const infinity = "\x00INFINITY\x00"

// ii builds one Insert Intention component at a key.
func ii(key string) ResourceRef {
	return ResourceRef{Identity: Identity{
		Kind:    KindInterval,
		Mode:    ModeInsertIntention,
		Space:   "index/PRIMARY",
		TableID: "t1",
		IndexID: "PRIMARY",
		Key:     []byte(key),
	}}
}

// TestB02GapContainsInsertionPosition covers the open/closed endpoint rules.
func TestB02GapContainsInsertionPosition(t *testing.T) {
	cases := []struct {
		name          string
		gap           ResourceRef
		position      string
		wantContained bool
		because       string
	}{
		{name: "inside-open-open", gap: gap("a", "z", false, false), position: "m", wantContained: true, because: "m is strictly inside"},
		{name: "at-lower-exclusive", gap: gap("a", "z", false, false), position: "a", wantContained: false, because: "the lower end is open"},
		{name: "at-lower-inclusive", gap: gap("a", "z", true, false), position: "a", wantContained: true, because: "the lower end is closed"},
		{name: "at-upper-exclusive", gap: gap("a", "z", false, false), position: "z", wantContained: false, because: "the upper end is open"},
		{name: "at-upper-inclusive", gap: gap("a", "z", false, true), position: "z", wantContained: true, because: "the upper end is closed"},
		{name: "below", gap: gap("m", "z", true, true), position: "a", wantContained: false, because: "a sorts before the lower bound"},
		{name: "above", gap: gap("a", "m", true, true), position: "z", wantContained: false, because: "z sorts after the upper bound"},
		{name: "zero-length-closed", gap: gap("p", "p", true, true), position: "p", wantContained: true, because: "[p,p] contains exactly p"},
		{name: "zero-length-half-open", gap: gap("p", "p", true, false), position: "p", wantContained: false, because: "[p,p) is empty"},
		{name: "unbounded-lower", gap: gap(infinity, "m", false, true), position: "a", wantContained: true, because: "a is below the upper bound with no lower bound"},
		{name: "unbounded-upper", gap: gap("m", infinity, true, false), position: "z", wantContained: true, because: "the terminal gap extends to infinity"},
		{name: "whole-domain", gap: gap(infinity, infinity, false, false), position: "anything", wantContained: true, because: "the whole domain contains every key"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := gapBlocksInsertion(testCase.gap, []byte(testCase.position))
			if got != testCase.wantContained {
				t.Fatalf("gapBlocksInsertion = %v, want %v: %s", got, testCase.wantContained, testCase.because)
			}
		})
	}
}

// TestB02InsertionOutsideAGapIsCompatible is the "different positions in one gap" rule.
//
// A gap protects the positions inside it. An insertion outside the gap is untouched by it, which is
// what lets two transactions that happen to hold neighbouring ranges both insert. Two insertions
// *inside* one gap are also compatible with each other: neither gap claims a key, so only the
// record reservation they take next can make them collide.
func TestB02InsertionOutsideAGapIsCompatible(t *testing.T) {
	outside := gap("a", "m", false, false)
	for _, position := range []string{"a", "n", "z"} {
		if componentsConflict(outside, ii(position)) {
			t.Fatalf("gap (a,m) blocked insertion at %q, which is outside it", position)
		}
		if componentsConflict(ii(position), outside) {
			t.Fatalf("insertion at %q was blocked by gap (a,m) in the other direction", position)
		}
	}
	// Two positions strictly inside one gap, checking against each other rather than the gap.
	held := gap("a", "z", false, false)
	for _, pair := range [][2]string{{"b", "m"}, {"m", "y"}, {"b", "y"}} {
		left, right := ii(pair[0]), ii(pair[1])
		if componentsConflict(left, right) {
			t.Fatalf("two insertions inside one gap (%q, %q) conflicted with each other", pair[0], pair[1])
		}
	}
	// But each of them IS blocked by the gap that contains it.
	for _, position := range []string{"b", "m", "y"} {
		if !componentsConflict(held, ii(position)) {
			t.Fatalf("gap (a,z) did not block insertion at %q, which it contains", position)
		}
	}
}

// TestB02GapBlocksInsertionInsideIt is the core gap guarantee.
func TestB02GapBlocksInsertionInsideIt(t *testing.T) {
	held := gap("a", "z", true, true)
	for _, position := range []string{"a", "m", "z"} {
		if !componentsConflict(held, ii(position)) {
			t.Fatalf("gap [a,z] did not block insertion at %q", position)
		}
		if !componentsConflict(ii(position), held) {
			t.Fatalf("insertion at %q did not conflict with gap [a,z] in the other direction", position)
		}
	}
	// A terminal gap blocks everything above its lower bound.
	terminal := gap("m", infinity, false, false)
	for _, position := range []string{"n", "z", "\xff\xff"} {
		if !componentsConflict(terminal, ii(position)) {
			t.Fatalf("terminal gap (m,+inf) did not block insertion at %q", position)
		}
	}
}

// TestB02GapAgainstGapIsCompatible covers the diagonal of the interval matrix.
//
// Two gaps say "no insert here". Neither claims a key, so they never exclude each other, whether
// they are adjacent, overlapping or identical.
func TestB02GapAgainstGapIsCompatible(t *testing.T) {
	cases := []struct {
		name  string
		left  ResourceRef
		right ResourceRef
	}{
		{name: "identical", left: gap("a", "m", false, false), right: gap("a", "m", false, false)},
		{name: "overlapping", left: gap("a", "m", false, false), right: gap("g", "z", false, false)},
		{name: "adjacent-touching", left: gap("a", "m", false, true), right: gap("m", "z", true, false)},
		{name: "nested", left: gap("a", "z", false, false), right: gap("g", "m", false, false)},
		{name: "disjoint", left: gap("a", "m", false, false), right: gap("p", "z", false, false)},
		{name: "unbounded", left: gap("a", infinity, false, false), right: gap("m", infinity, false, false)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if componentsConflict(testCase.left, testCase.right) {
				t.Fatal("two gaps conflicted")
			}
			if componentsConflict(testCase.right, testCase.left) {
				t.Fatal("two gaps conflicted in the other direction")
			}
			// The explicit overlap question agrees with the compatibility answer.
			if !gapOverlapsGap(testCase.left, testCase.right) && testCase.name != "disjoint" {
				t.Fatalf("%s: gapOverlapsGap reported no overlap", testCase.name)
			}
		})
	}
	if gapOverlapsGap(gap("a", "m", false, false), gap("p", "z", false, false)) {
		t.Fatal("two disjoint gaps reported an overlap")
	}
}

// TestB02InsertIntentionAgainstInsertIntentionIsCompatible covers the II/II diagonal.
//
// Two transactions may both intend to insert the same key; the actual reservation is the record X
// they take next, which is where they serialise.
func TestB02InsertIntentionAgainstInsertIntentionIsCompatible(t *testing.T) {
	for _, pair := range [][2]string{{"k1", "k1"}, {"k1", "k2"}} {
		if componentsConflict(ii(pair[0]), ii(pair[1])) {
			t.Fatalf("insert intentions at %q and %q conflicted", pair[0], pair[1])
		}
		if componentsConflict(ii(pair[1]), ii(pair[0])) {
			t.Fatalf("insert intentions at %q and %q conflicted in the other direction", pair[1], pair[0])
		}
	}
}

// TestB02AdjacentAndOverlappingIntervals covers the overlap predicate on the boundaries that are
// easy to get wrong: intervals that meet at one point with different endpoint flags.
func TestB02AdjacentAndOverlappingIntervals(t *testing.T) {
	cases := []struct {
		name      string
		a, b      ResourceRef
		wantTouch bool
	}{
		{name: "meet-both-closed", a: gap("a", "m", true, true), b: gap("m", "z", true, true), wantTouch: true},
		{name: "meet-left-open", a: gap("a", "m", true, false), b: gap("m", "z", true, true), wantTouch: false},
		{name: "meet-right-open", a: gap("a", "m", true, true), b: gap("m", "z", false, true), wantTouch: false},
		{name: "meet-both-open", a: gap("a", "m", true, false), b: gap("m", "z", false, true), wantTouch: false},
		{name: "gap-between", a: gap("a", "m", true, true), b: gap("n", "z", true, true), wantTouch: false},
		{name: "overlap-by-one-point", a: gap("a", "m", true, true), b: gap("m", "m", true, true), wantTouch: true},
		{name: "same-range", a: gap("a", "m", true, true), b: gap("a", "m", true, true), wantTouch: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := gapOverlapsGap(testCase.a, testCase.b); got != testCase.wantTouch {
				t.Fatalf("overlap = %v, want %v", got, testCase.wantTouch)
			}
			if got := gapOverlapsGap(testCase.b, testCase.a); got != testCase.wantTouch {
				t.Fatalf("overlap is not symmetric: %v vs %v", got, testCase.wantTouch)
			}
		})
	}
}

// TestB02EmptyAndZeroLengthIntervals proves the empty-interval rule.
func TestB02EmptyAndZeroLengthIntervals(t *testing.T) {
	empty := gap("p", "p", true, false)
	if !isEmptyInterval(empty.Lower, empty.Upper, empty.LowerInclusive, empty.UpperInclusive) {
		t.Fatal("[p,p) was not reported as empty")
	}
	if gapBlocksInsertion(empty, []byte("p")) {
		t.Fatal("an empty interval blocked an insertion")
	}
	if gapOverlapsGap(empty, gap("a", "z", true, true)) {
		t.Fatal("an empty interval overlapped a real one")
	}
	// A one-point closed interval is not empty and does block that point.
	point := gap("p", "p", true, true)
	if isEmptyInterval(point.Lower, point.Upper, point.LowerInclusive, point.UpperInclusive) {
		t.Fatal("[p,p] was reported as empty")
	}
	if !gapBlocksInsertion(point, []byte("p")) {
		t.Fatal("[p,p] did not block an insertion at p")
	}
}

// TestB02UnboundedAndSupremumBoundaries covers the first and last key of an index.
func TestB02UnboundedAndSupremumBoundaries(t *testing.T) {
	// Before the smallest key: the head gap.
	head := gap(infinity, "a", false, false)
	if !gapBlocksInsertion(head, []byte("A")) && []byte("A")[0] < 'a' {
		// "A" sorts before "a", so the head gap must contain it.
		t.Fatal("the head gap did not block an insertion before the first key")
	}
	// After the largest key: the terminal gap.
	terminal := gap("z", infinity, false, false)
	for _, position := range []string{"z\x00", "\xff"} {
		if !gapBlocksInsertion(terminal, []byte(position)) {
			t.Fatalf("the terminal gap did not block insertion at %q", position)
		}
	}
	// The whole domain.
	whole := gap(infinity, infinity, false, false)
	for _, position := range []string{"", "\x00", "a", "\xff\xff\xff"} {
		if !gapBlocksInsertion(whole, []byte(position)) {
			t.Fatalf("the whole-domain gap did not block insertion at %q", position)
		}
	}
	// An empty non-nil key is a real bound that sorts before every non-empty key.
	if !gapBlocksInsertion(whole, []byte{}) {
		t.Fatal("the whole-domain gap did not block the empty key")
	}
	// A nil key names no position, so no gap can be said to contain it.
	if gapBlocksInsertion(whole, nil) {
		t.Fatal("a nil insertion key was reported as contained")
	}
}

// TestB02IntervalIdentityIsPerNamespaceAndIndex proves two identically shaped ranges in different
// spaces are different resources, so one index's gap cannot block another index's insert.
//
// The namespace carries the separation: an interval's identity is (Generation, Space), so a
// different index or table gets a different space — the SQL layout builds `index/<table>/<name>`
// for exactly that reason. The table and index labels are metadata and must not be what separates
// two resources, or a caller that does not know a label would address a different resource than the
// writer it must exclude.
func TestB02IntervalIdentityIsPerNamespaceAndIndex(t *testing.T) {
	primary := gap("a", "z", false, false)
	secondary := primary
	secondary.IndexID = "idx_v"
	secondary.Space = "index/t1/idx_v"
	if primary.ResourceKey() == secondary.ResourceKey() {
		t.Fatal("a gap in the primary and in a secondary index share one resource identity")
	}
	otherTable := primary
	otherTable.TableID = "t2"
	otherTable.Space = "index/t2/PRIMARY"
	if primary.ResourceKey() == otherTable.ResourceKey() {
		t.Fatal("a gap in t1 and in t2 share one resource identity")
	}
	// The label alone does not separate: the space does.
	relabelled := primary
	relabelled.TableID = "t2"
	relabelled.IndexID = "other"
	if primary.ResourceKey() != relabelled.ResourceKey() {
		t.Fatal("a table or index label changed the interval identity; only the space may separate intervals")
	}
	if !intervalComponentsEqual(primary, gap("a", "z", false, false)) {
		t.Fatal("two identical interval components were reported as different")
	}
	if intervalComponentsEqual(primary, gap("a", "z", false, true)) {
		t.Fatal("intervals differing only in an endpoint flag were reported as equal")
	}
}
