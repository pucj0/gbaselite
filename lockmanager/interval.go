package lockmanager

import "bytes"

// interval.go holds the interval containment and overlap rules of the B02 lock manager.
//
// The rules exist because a Gap lock is not a Record lock: it protects a *range of insert
// positions*, so whether it conflicts with an insertion depends on where that insertion lands.
// Comparing only modes would say "a gap always blocks every insertion intention on this resource",
// which would serialise two transactions inserting at different positions of one gap. That is both
// weaker than necessary and observably wrong against the contract's interval matrix.
//
// Every comparison uses the existing unsigned lexicographic byte order, matching the order the
// write path already uses for keys. A nil bound is infinity; an empty non-nil slice is a real
// bound, so 0-length is never confused with unbounded.

// containsPoint reports whether one interval contains one point.
//
// A nil bound means infinity. The inclusivity flags decide the endpoints, so a half-open gap
// (prev, key) excludes the key itself while a closed one includes it. An empty non-nil key is a
// legal point that sorts before every non-empty key, so it must be compared rather than treated as
// "no point".
func containsPoint(lower, upper []byte, lowerInclusive, upperInclusive bool, point []byte) bool {
	if point == nil {
		// A nil point is not a key: an insertion intention always names the key it will create, so
		// there is nothing to contain. Refusing is safer than guessing a bound.
		return false
	}
	if lower != nil {
		order := bytes.Compare(point, lower)
		if order < 0 || (order == 0 && !lowerInclusive) {
			return false
		}
	}
	if upper != nil {
		order := bytes.Compare(point, upper)
		if order > 0 || (order == 0 && !upperInclusive) {
			return false
		}
	}
	return true
}

// isEmptyInterval reports whether an interval contains no point at all.
//
// Only a zero-length half-open interval is empty: [p,p) and (p,p] and (p,p) contain nothing,
// while [p,p] contains exactly p. An unbounded interval is never empty.
func isEmptyInterval(lower, upper []byte, lowerInclusive, upperInclusive bool) bool {
	if lower == nil || upper == nil {
		return false
	}
	if bytes.Compare(lower, upper) != 0 {
		return false
	}
	return !(lowerInclusive && upperInclusive)
}

// gapBlocksInsertion reports whether a held Gap lock covers the insertion point of a request.
//
// The two directions of the interval matrix are the same question asked from either side, so both
// call sites use this one function and cannot drift apart.
func gapBlocksInsertion(gap ResourceRef, insertionKey []byte) bool {
	if isEmptyInterval(gap.Lower, gap.Upper, gap.LowerInclusive, gap.UpperInclusive) {
		// An interval with no point protects no insertion position. This cannot arise from a legal
		// Gap component, because ValidateComponent refuses a half-open zero-length interval, but
		// the rule is stated here so the answer does not depend on that check being the only caller.
		return false
	}
	return containsPoint(gap.Lower, gap.Upper, gap.LowerInclusive, gap.UpperInclusive, insertionKey)
}

// gapOverlapsGap reports whether two Gap locks protect a common insert position.
//
// Two gaps over disjoint ranges cannot both need to block the same insertion, so they are
// compatible. Two gaps that overlap are compatible as well: both say "no insert here", and neither
// is claiming a key. The function exists so a caller can ask the question explicitly rather than
// relying on the default case falling through to the record matrix.
func gapOverlapsGap(a, b ResourceRef) bool {
	// An interval with no point protects no position, so it cannot overlap anything. The check is
	// explicit here because intervalsOverlap is the general predicate and treats a zero-length
	// half-open interval by its bounds rather than by emptiness.
	if isEmptyInterval(a.Lower, a.Upper, a.LowerInclusive, a.UpperInclusive) ||
		isEmptyInterval(b.Lower, b.Upper, b.LowerInclusive, b.UpperInclusive) {
		return false
	}
	return intervalsOverlap(
		a.Lower, a.Upper, a.LowerInclusive, a.UpperInclusive,
		b.Lower, b.Upper, b.LowerInclusive, b.UpperInclusive,
	)
}

// intervalComponentsEqual reports whether two interval components describe the same range in the
// same namespace.
//
// It is used to tell "the same lock asked twice" from "two different ranges", so a repeated
// request is recognised as a claim the owner already holds rather than queued a second time.
func intervalComponentsEqual(a, b ResourceRef) bool {
	return a.Kind == b.Kind &&
		a.Mode == b.Mode &&
		a.Space == b.Space &&
		a.TableID == b.TableID &&
		a.IndexID == b.IndexID &&
		bytes.Equal(a.Lower, b.Lower) && bytes.Equal(a.Upper, b.Upper) &&
		a.LowerInclusive == b.LowerInclusive && a.UpperInclusive == b.UpperInclusive
}
