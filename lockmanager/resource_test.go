package lockmanager

import (
	"bytes"
	"testing"
	"time"
)

// resource_test.go pins the identity, interval and scope-tree rules the whole lock
// manager rests on. Every assertion here is about a *value* rule: no lock mode is
// granted and no owner waits, so this file cannot be mistaken for the US1 kernel.

// TestB02ResourceIdentityIsolation proves that generation, namespace, table and index
// each separate resources, so two different keys can never share one lock identity.
func TestB02ResourceIdentityIsolation(t *testing.T) {
	base := Identity{
		Generation: 1,
		Space:      "rows",
		TableID:    "t1",
		IndexID:    "PRIMARY",
		Kind:       KindRecord,
		Mode:       ModeX,
		Key:        []byte{0x00, 0x01},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("base identity: %v", err)
	}

	variants := map[string]Identity{
		"generation": {Generation: 2},
		"space":      {Space: "index/u1"},
		"table":      {TableID: "t2"},
		"index":      {IndexID: "u1"},
		"kind":       {Kind: KindInterval, Mode: ModeGap, Key: nil, Lower: []byte{0x00, 0x01}, Upper: []byte{0x00, 0x02}},
		"key":        {Key: []byte{0x00, 0x02}},
	}
	baseKey := base.ResourceKey()
	for name, patch := range variants {
		other := base
		other.Generation = patch.Generation
		if patch.Space != "" {
			other.Space = patch.Space
		}
		if patch.TableID != "" {
			other.TableID = patch.TableID
		}
		if patch.IndexID != "" {
			other.IndexID = patch.IndexID
		}
		if patch.Kind != "" {
			other.Kind = patch.Kind
			other.Mode = patch.Mode
			other.Key = patch.Key
			other.Lower = patch.Lower
			other.Upper = patch.Upper
		}
		if patch.Key != nil && patch.Kind == "" {
			other.Key = patch.Key
		}
		if err := other.Validate(); err != nil {
			t.Fatalf("%s variant: %v", name, err)
		}
		if other.ResourceKey() == baseKey {
			t.Fatalf("%s must produce a different resource identity", name)
		}
	}

	// Mode is a request, not part of the identity: S and X on one row are the same
	// resource, which is what makes them conflict at all.
	shared := base
	shared.Mode = ModeS
	if shared.ResourceKey() != baseKey {
		t.Fatal("mode must not be part of the resource identity")
	}
}

// TestB02ResourceKeyIsUnambiguous proves the canonical key is collision free by
// length-prefixing every variable field. Without that, field boundaries could slide
// and two distinct identities could collide, which would silently merge two locks.
func TestB02ResourceKeyIsUnambiguous(t *testing.T) {
	a := Identity{Generation: 1, Space: "ab", TableID: "c", Kind: KindRecord, Mode: ModeX, Key: []byte("k")}
	b := Identity{Generation: 1, Space: "a", TableID: "bc", Kind: KindRecord, Mode: ModeX, Key: []byte("k")}
	if a.ResourceKey() == b.ResourceKey() {
		t.Fatal("shifted field boundaries produced the same resource key")
	}
	c := Identity{Generation: 1, Space: "rows", TableID: "t", Kind: KindRecord, Mode: ModeX, Key: []byte("ab")}
	d := Identity{Generation: 1, Space: "rows", TableID: "t", Kind: KindRecord, Mode: ModeX, Key: []byte("a")}
	if c.ResourceKey() == d.ResourceKey() {
		t.Fatal("different keys produced the same resource key")
	}
	// The zero key is a real key, not "unset": it must be distinguishable from a nil
	// key, which is refused.
	zero := Identity{Generation: 1, Space: "rows", Kind: KindRecord, Mode: ModeS, Key: []byte{}}
	if err := zero.Validate(); err != nil {
		t.Fatalf("an empty non-nil key is a valid bound: %v", err)
	}
}

// TestB02ResourceValidationFailsClosed covers the invalid identities. Each one must be
// refused rather than approximated: approximating an identity would either over-lock
// unrelated rows or under-lock the requested one.
func TestB02ResourceValidationFailsClosed(t *testing.T) {
	cases := map[string]Identity{
		"unknown kind":             {Kind: ResourceKind("PAGE"), Mode: ModeX, Space: "rows", Key: []byte("k")},
		"unknown mode":             {Kind: KindRecord, Mode: Mode("SIX"), Space: "rows", Key: []byte("k")},
		"empty space":              {Kind: KindRecord, Mode: ModeX, Key: []byte("k")},
		"table intent no table":    {Kind: KindTableIntent, Mode: ModeIX, Space: "table"},
		"table intent with key":    {Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: "t", Key: []byte("k")},
		"table intent record mode": {Kind: KindTableIntent, Mode: ModeX, Space: "table", TableID: "t"},
		"record nil key":           {Kind: KindRecord, Mode: ModeX, Space: "rows"},
		"record with bounds":       {Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k"), Lower: []byte("a")},
		"record gap mode":          {Kind: KindRecord, Mode: ModeGap, Space: "rows", Key: []byte("k")},
		"interval with key":        {Kind: KindInterval, Mode: ModeGap, Space: "rows", Key: []byte("k")},
		"interval record mode":     {Kind: KindInterval, Mode: ModeS, Space: "rows"},
		"inverted closed":          {Kind: KindInterval, Mode: ModeGap, Space: "rows", Lower: []byte("b"), Upper: []byte("a")},
		"equal exclusive":          {Kind: KindInterval, Mode: ModeGap, Space: "rows", Lower: []byte("a"), Upper: []byte("a")},
	}
	for name, identity := range cases {
		if err := identity.Validate(); err == nil {
			t.Fatalf("%s: expected a validation error", name)
		}
	}

	legal := map[string]Identity{
		"whole domain":      {Kind: KindInterval, Mode: ModeGap, Space: "rows"},
		"equal inclusive":   {Kind: KindInterval, Mode: ModeGap, Space: "rows", Lower: []byte("a"), Upper: []byte("a"), LowerInclusive: true, UpperInclusive: true},
		"empty lower bound": {Kind: KindInterval, Mode: ModeGap, Space: "rows", Lower: []byte{}, Upper: []byte("m")},
		"empty record key":  {Kind: KindRecord, Mode: ModeS, Space: "rows", Key: []byte{}},
		"table intent IS":   {Kind: KindTableIntent, Mode: ModeIS, Space: "table", TableID: "t"},
		"insert intention":  {Kind: KindRecord, Mode: ModeInsertIntention, Space: "rows", Key: []byte("k")},
	}
	for name, identity := range legal {
		if err := identity.Validate(); err != nil {
			t.Fatalf("%s must be legal: %v", name, err)
		}
	}
}

// TestB02ResourceCloneOwnsItsBytes proves a cloned resource never aliases caller
// bytes: a caller that reuses a key buffer must not be able to change a stored lock.
func TestB02ResourceCloneOwnsItsBytes(t *testing.T) {
	key := []byte("key")
	lower, upper := []byte("a"), []byte("z")
	original := Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: key}
	clone := original.Clone()
	key[0] = 'X'
	if bytes.Equal(clone.Key, key) {
		t.Fatal("Clone borrowed the caller's key slice")
	}

	interval := Identity{Kind: KindInterval, Mode: ModeGap, Space: "rows", Lower: lower, Upper: upper}
	intervalClone := interval.Clone()
	lower[0], upper[0] = 'A', 'Z'
	if bytes.Equal(intervalClone.Lower, lower) || bytes.Equal(intervalClone.Upper, upper) {
		t.Fatal("Clone borrowed an interval bound")
	}

	// A nil bound must stay nil, because nil is infinity and must never be turned
	// into the empty slice, which is a real bound.
	whole := Identity{Kind: KindInterval, Mode: ModeGap, Space: "rows"}
	if got := whole.Clone(); got.Lower != nil || got.Upper != nil {
		t.Fatalf("Clone turned an infinite bound into %#v/%#v", got.Lower, got.Upper)
	}

	refs := []ResourceRef{{Identity: original}}
	copied := CloneResourceRefs(refs)
	refs[0].Key[0] = 'Y'
	if bytes.Equal(copied[0].Key, refs[0].Key) {
		t.Fatal("CloneResourceRefs borrowed a caller slice")
	}
	if CloneResourceRefs(nil) != nil {
		t.Fatal("CloneResourceRefs(nil) must stay nil")
	}
}

// TestB02IntervalContainsKey covers the endpoint set, including the two infinities and
// the empty non-nil bound.
func TestB02IntervalContainsKey(t *testing.T) {
	cases := []struct {
		name               string
		lower, upper       []byte
		lowerInc, upperInc bool
		key                []byte
		want               bool
	}{
		{"open open inside", []byte("b"), []byte("y"), false, false, []byte("m"), true},
		{"open open at lower", []byte("b"), []byte("y"), false, false, []byte("b"), false},
		{"open open at upper", []byte("b"), []byte("y"), false, false, []byte("y"), false},
		{"closed closed at lower", []byte("b"), []byte("y"), true, true, []byte("b"), true},
		{"closed closed at upper", []byte("b"), []byte("y"), true, true, []byte("y"), true},
		{"below lower", []byte("b"), []byte("y"), true, true, []byte("a"), false},
		{"above upper", []byte("b"), []byte("y"), true, true, []byte("z"), false},
		{"minus infinity", nil, []byte("m"), false, false, []byte{0x00}, true},
		{"plus infinity", []byte("m"), nil, false, false, []byte{0xff}, true},
		{"whole domain", nil, nil, false, false, []byte("anything"), true},
		{"empty non-nil lower", []byte{}, []byte("m"), true, false, []byte{}, true},
		{"empty non-nil lower excluded", []byte{}, []byte("m"), false, false, []byte{}, false},
		{"terminal gap excludes last key", []byte("m"), nil, false, false, []byte("m"), false},
		{"terminal gap includes after last", []byte("m"), nil, false, false, []byte("m\x00"), true},
	}
	for _, tc := range cases {
		identity := Identity{
			Kind: KindInterval, Mode: ModeGap, Space: "rows",
			Lower: tc.lower, Upper: tc.upper,
			LowerInclusive: tc.lowerInc, UpperInclusive: tc.upperInc,
		}
		if err := identity.Validate(); err != nil {
			t.Fatalf("%s: interval is not legal: %v", tc.name, err)
		}
		if got := identity.ContainsKey(tc.key); got != tc.want {
			t.Fatalf("%s: ContainsKey(%q) = %v, want %v", tc.name, tc.key, got, tc.want)
		}
	}

	// A record never "contains" a key, and an insert intention is not a container.
	record := Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("m")}
	if record.ContainsKey([]byte("m")) {
		t.Fatal("a record must not report interval membership")
	}
	intention := Identity{Kind: KindRecord, Mode: ModeInsertIntention, Space: "rows", Key: []byte("m")}
	if intention.ContainsKey([]byte("m")) {
		t.Fatal("an insert intention must not report interval membership")
	}
}

// TestB02IntervalOverlap covers different endpoints, both infinities, cross-domain and
// the invalid-interval cases. Overlap must never be decided by comparing bounds for
// equality: two intervals with different endpoints can still overlap.
func TestB02IntervalOverlap(t *testing.T) {
	mk := func(lower, upper []byte, lowerInc, upperInc bool) Identity {
		return Identity{Kind: KindInterval, Mode: ModeGap, Space: "rows", Lower: lower, Upper: upper, LowerInclusive: lowerInc, UpperInclusive: upperInc}
	}
	cases := []struct {
		name string
		a, b Identity
		want bool
	}{
		{"identical open", mk([]byte("b"), []byte("y"), false, false), mk([]byte("b"), []byte("y"), false, false), true},
		{"different endpoints overlapping", mk([]byte("b"), []byte("m"), false, false), mk([]byte("k"), []byte("y"), false, false), true},
		{"touching open at point", mk([]byte("b"), []byte("m"), false, false), mk([]byte("m"), []byte("y"), false, false), false},
		{"touching closed at point", mk([]byte("b"), []byte("m"), false, true), mk([]byte("m"), []byte("y"), true, false), true},
		{"disjoint", mk([]byte("b"), []byte("c"), true, true), mk([]byte("x"), []byte("y"), true, true), false},
		{"contained", mk([]byte("b"), []byte("y"), true, true), mk([]byte("m"), []byte("n"), true, true), true},
		{"whole domain", mk(nil, nil, false, false), mk([]byte("m"), []byte("n"), true, true), true},
		{"both unbounded one side", mk(nil, []byte("m"), false, false), mk([]byte("a"), nil, false, false), true},
		{"terminal gaps", mk([]byte("m"), nil, false, false), mk([]byte("y"), nil, false, false), true},
		{"different namespace", Identity{Kind: KindInterval, Mode: ModeGap, Space: "index/u1", Lower: []byte("b"), Upper: []byte("y")}, mk([]byte("b"), []byte("y"), false, false), false},
		{"different generation", Identity{Kind: KindInterval, Mode: ModeGap, Space: "rows", Generation: 2, Lower: []byte("b"), Upper: []byte("y")}, mk([]byte("b"), []byte("y"), false, false), false},
	}
	for _, tc := range cases {
		if got := tc.a.Overlaps(tc.b); got != tc.want {
			t.Fatalf("%s: Overlaps = %v, want %v", tc.name, got, tc.want)
		}
		if got := tc.b.Overlaps(tc.a); got != tc.want {
			t.Fatalf("%s: Overlaps is not symmetric", tc.name)
		}
	}

	// A record is never an interval and never overlaps one.
	record := Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("m")}
	if record.Overlaps(mk(nil, nil, false, false)) {
		t.Fatal("a record must not overlap an interval")
	}
	// An illegal interval must not silently become an empty or whole-domain lock.
	inverted := Identity{Kind: KindInterval, Mode: ModeGap, Space: "rows", Lower: []byte("z"), Upper: []byte("a")}
	if inverted.Overlaps(mk(nil, nil, false, false)) {
		t.Fatal("an inverted interval must never overlap anything")
	}
}

// TestB02DedupeUsesStrongestModeAndCanonicalOrder proves a bundle that names the same
// row twice is charged once, and that the canonical order is stable.
func TestB02DedupeUsesStrongestModeAndCanonicalOrder(t *testing.T) {
	components := []ResourceRef{
		{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: []byte("b")}},
		{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("b")}},
		{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: []byte("a")}},
		{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: "t1"}},
	}
	deduped := DedupeResourceRefs(components)
	if len(deduped) != 3 {
		t.Fatalf("dedupe kept %d components, want 3: %+v", len(deduped), deduped)
	}
	var strongest Mode
	for _, component := range deduped {
		if bytes.Equal(component.Key, []byte("b")) {
			strongest = component.Mode
		}
	}
	if strongest != ModeX {
		t.Fatalf("duplicate key kept mode %q, want X", strongest)
	}
	// The canonical order is the byte order of the canonical resource key, and the
	// kind comes first inside that key, so the table intent sorts before the row keys.
	if deduped[0].Kind != KindTableIntent {
		t.Fatalf("canonical order put %q first", deduped[0].Kind)
	}
	if !RefsInCanonicalOrder(deduped) {
		t.Fatal("canonical order is not sorted")
	}

	// An invalid component is preserved so the request validator can reject it,
	// rather than being silently merged away.
	withInvalid := DedupeResourceRefs([]ResourceRef{{Identity: Identity{Kind: KindRecord, Mode: Mode("SIX"), Space: "rows", Key: []byte("k")}}})
	if len(withInvalid) != 1 {
		t.Fatalf("an invalid component was dropped by dedupe: %+v", withInvalid)
	}
	if DedupeResourceRefs(nil) != nil {
		t.Fatal("DedupeResourceRefs(nil) must stay nil")
	}
}

// TestB02RefBytesAccountingIsExact pins the accounting unit: copied identity bytes plus
// one fixed constant per reference.
func TestB02RefBytesAccountingIsExact(t *testing.T) {
	identity := Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", TableID: "t", IndexID: "PRIMARY", Key: []byte("abc")}
	ref := ResourceRef{Identity: identity}
	want := int64(len("rows")+len("t")+len("PRIMARY")+len("abc")) + resourceFixedBytes
	if got := ref.OwnedBytes64(); got != want {
		t.Fatalf("OwnedBytes = %d, want %d", got, want)
	}
	if got := RefBytes([]ResourceRef{ref, ref}); got != 2*want {
		t.Fatalf("RefBytes of two refs = %d, want %d", got, 2*want)
	}
	if RefBytes(nil) != 0 {
		t.Fatal("RefBytes(nil) must be zero")
	}
}

// TestB02ScopeTreeOwnershipAndAncestry proves every scope belongs to exactly one root,
// that a sibling is never an ancestor, and that closing a scope never releases a grant
// (there is no path in this type that could).
func TestB02ScopeTreeOwnershipAndAncestry(t *testing.T) {
	tree := NewScopeTree()
	root, err := tree.Open("root-1", "txn-1", 0, ScopeStatement, testEpoch, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := tree.Open("root-1", "txn-2", root.ID, ScopeChild, testEpoch, testEpoch.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := tree.Open("root-1", "txn-3", root.ID, ScopeSavepoint, testEpoch, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// A scope of another owner may not be attached here, which is what keeps a grant
	// from being attributed to the wrong root.
	if _, err := tree.Open("root-2", "txn-4", root.ID, ScopeChild, testEpoch, time.Time{}); err != ErrScopeOwnerMismatch {
		t.Fatalf("cross-owner parent = %v, want ErrScopeOwnerMismatch", err)
	}
	if _, err := tree.Open("", "txn", 0, ScopeChild, testEpoch, time.Time{}); err != ErrScopeUnknown {
		t.Fatalf("empty owner = %v, want ErrScopeUnknown", err)
	}
	if _, err := tree.Open("root-1", "txn", 0, ScopeKind("weird"), testEpoch, time.Time{}); err != ErrScopeUnknown {
		t.Fatalf("unknown kind = %v, want ErrScopeUnknown", err)
	}

	if !tree.IsAncestor(root.ID, child.ID) || !tree.IsAncestor(root.ID, root.ID) {
		t.Fatal("ancestry must include the scope itself and its root")
	}
	if tree.IsAncestor(child.ID, sibling.ID) {
		t.Fatal("one savepoint branch must not be an ancestor of a sibling branch")
	}
	if tree.IsAncestor(0, child.ID) || tree.IsAncestor(root.ID, 0) {
		t.Fatal("a zero scope id is never an ancestor")
	}
	chain := tree.Ancestors(child.ID)
	if len(chain) != 2 || chain[0].ID != child.ID || chain[1].ID != root.ID {
		t.Fatalf("ancestor chain = %+v", chain)
	}

	// Closing the root closes the whole subtree, deepest first, and leaves the
	// identities readable for diagnostics.
	closed := tree.CloseSubtree(root.ID)
	if len(closed) != 3 {
		t.Fatalf("CloseSubtree closed %d scopes, want 3: %v", len(closed), closed)
	}
	if closed[0] != child.ID && closed[0] != sibling.ID {
		t.Fatalf("CloseSubtree did not report a leaf first: %v", closed)
	}
	for _, id := range []uint64{root.ID, child.ID, sibling.ID} {
		scope, ok := tree.Get(id)
		if !ok {
			t.Fatalf("scope %d disappeared from the tree", id)
		}
		if scope.State != ScopeClosed {
			t.Fatalf("scope %d state = %q, want CLOSED", id, scope.State)
		}
		if scope.CanRequest() {
			t.Fatalf("closed scope %d still accepts requests", id)
		}
	}
	if tree.OpenScopes() != 0 {
		t.Fatalf("open scopes = %d after closing the root", tree.OpenScopes())
	}
	if len(tree.CloseSubtree(999)) != 0 {
		t.Fatal("closing an unknown scope must be a no-op")
	}
	if _, err := tree.BeginClose(999); err != ErrScopeUnknown {
		t.Fatalf("BeginClose(unknown) = %v, want ErrScopeUnknown", err)
	}
}

// TestB02BindingsRejectConflictingLabels proves a namespace has exactly one owner, so
// two SQL tables can never share one lock identity.
func TestB02BindingsRejectConflictingLabels(t *testing.T) {
	bindings := NewBindings(1)
	good := NamespaceBinding{Namespace: "index/u1", TableID: "t1", IndexID: "u1", Node: "t1"}
	if err := bindings.Bind(good); err != nil {
		t.Fatal(err)
	}
	if err := bindings.Bind(good); err != nil {
		t.Fatalf("re-binding the same label must be idempotent: %v", err)
	}
	if got, ok := bindings.Lookup("index/u1"); !ok || got.TableID != "t1" {
		t.Fatalf("lookup = %+v ok=%v", got, ok)
	}
	conflict := good
	conflict.TableID = "t2"
	if err := bindings.Bind(conflict); err != ErrInvalidResource {
		t.Fatalf("conflicting table label = %v, want ErrInvalidResource", err)
	}
	if err := bindings.Bind(NamespaceBinding{Namespace: "rows", Node: "t1"}); err != ErrInvalidResource {
		t.Fatalf("binding without a table id = %v, want ErrInvalidResource", err)
	}
	if bindings.Len() != 1 {
		t.Fatalf("bindings = %d, want 1", bindings.Len())
	}
	if bindings.Bytes() <= 0 {
		t.Fatal("binding accounting must be positive while a binding exists")
	}
	// A generation change drops every binding: an old namespace label must not
	// survive into the replaced database.
	bindings.Reset(2)
	if bindings.Len() != 0 || bindings.Bytes() != 0 || bindings.Generation() != 2 {
		t.Fatalf("after Reset: len=%d bytes=%d generation=%d", bindings.Len(), bindings.Bytes(), bindings.Generation())
	}
	// Re-binding after the reset is allowed even with a different label, because the
	// previous identity no longer addresses anything.
	if err := bindings.Bind(conflict); err != nil {
		t.Fatalf("re-binding after a generation change: %v", err)
	}
	bindings.Unbind("index/u1")
	if bindings.Len() != 0 || bindings.Bytes() != 0 {
		t.Fatalf("after Unbind: len=%d bytes=%d", bindings.Len(), bindings.Bytes())
	}
}
