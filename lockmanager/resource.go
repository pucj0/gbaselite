package lockmanager

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strconv"
	"sync"
	"time"
)

// This file holds the identity, interval and binding machinery that every later
// lock stage depends on. All of it is pure value logic: no locks, no waiting and no
// transaction interaction, so it can be tested without a manager or a clock.

// Validate reports why an identity cannot be used as a lock resource.
//
// Every rejection is fail closed: an identity that cannot be proved to address
// exactly one resource is refused rather than approximated, because approximating it
// would either over-lock unrelated keys or under-lock the requested one.
func (id Identity) Validate() error {
	if !id.Kind.Valid() {
		return ErrInvalidResource
	}
	if !id.Mode.Valid() {
		return ErrInvalidResource
	}
	if id.Space == "" {
		return ErrInvalidResource
	}
	if id.Space != canonicalSpace(id.Space) {
		// A non-canonical space would let one logical namespace be addressed by two
		// different identities, which would silently defeat conflict detection.
		return ErrInvalidResource
	}
	switch id.Kind {
	case KindTableIntent:
		if id.TableID == "" || id.Mode != ModeIS && id.Mode != ModeIX {
			return ErrInvalidResource
		}
		if len(id.Key) != 0 || len(id.Lower) != 0 || len(id.Upper) != 0 {
			return ErrInvalidResource
		}
	case KindRecord:
		if !id.Mode.Record() && id.Mode != ModeInsertIntention {
			return ErrInvalidResource
		}
		if id.Key == nil {
			return ErrInvalidResource
		}
		if len(id.Lower) != 0 || len(id.Upper) != 0 {
			return ErrInvalidResource
		}
	case KindInterval:
		if id.Mode != ModeGap {
			return ErrInvalidResource
		}
		if len(id.Key) != 0 {
			return ErrInvalidResource
		}
		if !intervalOrdered(id.Lower, id.Upper, id.LowerInclusive, id.UpperInclusive) {
			return ErrInvalidResource
		}
	}
	return nil
}

// canonicalSpace normalizes a namespace for comparison. Spaces come from the
// existing key layout, which never uses a slash or a NUL, so a space that needs
// normalization is a bug rather than an alternative spelling.
func canonicalSpace(space string) string { return space }

// intervalOrdered reports whether [lower, upper] is a legal interval under unsigned
// byte order. A nil bound is infinity, so a nil-nil interval is the whole domain and
// is always legal.
func intervalOrdered(lower, upper []byte, lowerInclusive, upperInclusive bool) bool {
	if lower == nil || upper == nil {
		return true
	}
	switch bytes.Compare(lower, upper) {
	case -1:
		return true
	case 0:
		return lowerInclusive && upperInclusive
	default:
		return false
	}
}

// Clone returns a deep copy that owns every byte slice, so the caller may keep or
// mutate its own copy afterwards without changing the resource.
func (id Identity) Clone() Identity {
	out := id
	out.Key = bytes.Clone(id.Key)
	out.Lower = bytes.Clone(id.Lower)
	out.Upper = bytes.Clone(id.Upper)
	return out
}

// ResourceKey is the canonical key of the resource's **conflict domain**: the set of claims that
// must be compared with each other.
//
// It is a table key, not the identity of one lock. The manager stores every claim of one domain in
// one entry so the bounds-aware rules can run over all of them, and it looks a request's domain up
// with this key. What the request actually protects is `ClaimKey`.
//
// Every variable-length field is length-prefixed, because joining fields with a plain
// separator would let two different identities produce the same key; two identities
// that share a key would then be treated as one resource and two different keys could
// be locked as if they were the same row.
//
// The kind is encoded as one fixed byte right after the generation, so the byte order
// of this key is also the canonical resource order and a table's intention always
// sorts before the rows it protects.
//
// TableID and IndexID are deliberately NOT part of the key. Record identity is
// (Generation, Space, Key) and interval identity is (Generation, Space): the space is the
// logical namespace — `row/<table>`, `index/<table>/<name>`, `secondary/<table>/<name>` —
// so it already separates one table's rows from another's and one index from another. A
// label is metadata that the SQL layer happens to have and a raw writer does not, and
// including it would make "the same row" two different resources depending on which layer
// asked: a raw `Put` could then bypass a row X held by SQL, which is exactly the bypass
// FR-026 forbids. The labels still travel on the identity for the table-intent
// prerequisite, the namespace bindings and diagnostics; they are labels, not identity.
//
// An interval-type identity — a Gap or an Insert Intention — has the whole domain
// (Generation, Space) as its key, with no bounds and no insertion key. Whether a gap blocks an
// insertion is a question about the actual bounds and the actual insertion point, so it cannot be
// answered by comparing keys: two components that have to meet (a gap over `(a,z)` and an
// intention to insert `m`) have completely different bounds, and putting them in different keys
// would put them in different tables, where the manager never compares them at all and the gap
// would block nothing. Keying the whole interval domain together is what makes
// componentsConflict's containment rule reachable. Two gaps of one namespace are therefore two
// *claims* in one domain — see ClaimKey — not one lock. Record identity stays exact: a record
// request is still (Generation, Record, Space, Key), so an interval domain and the rows beside it
// never collide.
//
// Mode is deliberately excluded too: a mode is a request, not part of the identity. That is what
// lets one entry hold both gap claims and insertion-intention claims and decide between them by
// containment.
func (id Identity) ResourceKey() string {
	var b bytes.Buffer
	b.Grow(len(id.Space) + len(id.Key) + len(id.Lower) + len(id.Upper) + 64)
	writeUint64(&b, id.Generation)
	if id.Mode.Interval() {
		// The interval domain: generation, the interval kind code and the namespace. No bounds, no
		// key.
		b.WriteByte(kindCode(KindInterval))
		writeField(&b, id.Space)
		return b.String()
	}
	b.WriteByte(kindCode(id.Kind))
	writeField(&b, id.Space)
	writeField(&b, string(id.Key))
	writeField(&b, string(id.Lower))
	writeField(&b, string(id.Upper))
	if id.LowerInclusive {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	if id.UpperInclusive {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	return b.String()
}

// ClaimKey is the exact identity of the lock one claim protects: the conflict domain *plus* what
// makes this claim different from its neighbours in that domain.
//
// The two questions are different and were once conflated, which made distinct locks look like one:
//
//   - ResourceKey answers "which claims must be compared with each other?" — a Gap and an Insert
//     Intention of one namespace must be, so they share a domain.
//   - ClaimKey answers "is this the same lock?" — two gaps with different bounds are two locks, two
//     insertion positions are two locks, and neither may satisfy, join, adopt or release the other.
//
// For a record or table-intent identity the domain key is already exact, so ClaimKey is the same
// string: record S and record X name one row, which is what makes "X satisfies a request for S" a
// resource-level answer with the mode compared separately by upgradeNeeded.
//
// For an interval-type identity the exact key adds the mode and the bounds or insertion key, because
// a Gap and an intention at the same position are different locks with different rules rather than
// one lock at two strengths. The mode is part of the key there for the same reason: a Gap(a,m) and
// an II(a) in one namespace must never collapse into each other, and neither may a Gap(a,m) and a
// Gap(n,z).
func (id Identity) ClaimKey() string {
	if !id.Mode.Interval() {
		return id.ResourceKey()
	}
	var b bytes.Buffer
	b.Grow(len(id.Space) + len(id.Key) + len(id.Lower) + len(id.Upper) + 64)
	writeUint64(&b, id.Generation)
	b.WriteByte(kindCode(KindInterval))
	writeField(&b, id.Space)
	writeField(&b, string(id.Mode))
	writeField(&b, string(id.Key))
	writeField(&b, string(id.Lower))
	writeField(&b, string(id.Upper))
	if id.LowerInclusive {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	if id.UpperInclusive {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	return b.String()
}

// SameClaim reports whether two components name the very same lock.
//
// It is the equality test every claim-level decision uses — re-entrancy, coverage, adoption,
// joining a queued request — because sharing a conflict domain is not sharing a lock: two gaps of
// one namespace are compared with each other, and must still be two claims.
func SameClaim(a, b ResourceRef) bool { return a.ClaimKey() == b.ClaimKey() }

// ClaimKey is the identity of the lock this component's claim protects.
func (r ResourceRef) ClaimKey() string { return r.Identity.ClaimKey() }

// kindCode is the stable one-byte code of a resource kind. The codes are ordered so
// the canonical key order lists an intention before the keys it protects.
func kindCode(kind ResourceKind) byte {
	switch kind {
	case KindTableIntent:
		return 1
	case KindInterval:
		return 2
	case KindRecord:
		return 3
	default:
		return 0
	}
}

// IntervalKey is the identity key of the interval domain, which ignores the mode.
// Two intervals share a domain exactly when they share generation and space, so
// overlap must be decided by interval intersection and never by comparing bounds.
func (id Identity) IntervalKey() string {
	return strconv.FormatUint(id.Generation, 10) + "\x00" + id.Space
}

// TableKey is the identity key of a table-intent domain.
func (id Identity) TableKey() string {
	return strconv.FormatUint(id.Generation, 10) + "\x00" + id.TableID
}

func writeUint64(b *bytes.Buffer, v uint64) {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], v)
	b.Write(raw[:])
}

func writeField(b *bytes.Buffer, s string) {
	writeUint64(b, uint64(len(s)))
	b.WriteString(s)
}

// ContainsKey reports whether an interval resource contains one exact insertion key.
//
// It is the only membership test used for insert intentions, so the open/closed
// boundary rule is stated in exactly one place. A row lock is not an interval and
// never contains a key; an insert intention as the *holder* is handled by the
// compatibility matrix, not here.
func (id Identity) ContainsKey(key []byte) bool {
	if id.Kind != KindInterval {
		return false
	}
	return intervalContains(id.Lower, id.Upper, id.LowerInclusive, id.UpperInclusive, key)
}

func intervalContains(lower, upper []byte, lowerInclusive, upperInclusive bool, key []byte) bool {
	if lower != nil {
		switch bytes.Compare(key, lower) {
		case -1:
			return false
		case 0:
			if !lowerInclusive {
				return false
			}
		}
	}
	if upper != nil {
		switch bytes.Compare(key, upper) {
		case 1:
			return false
		case 0:
			if !upperInclusive {
				return false
			}
		}
	}
	return true
}

// Overlaps reports whether two interval resources share at least one key.
//
// Two intervals in different generations or namespaces never overlap, even when
// their bounds are identical, because they address different byte domains. An
// inverted or illegal interval never overlaps anything: it is refused by Validate
// and must not silently become an empty or whole-domain lock here.
func (id Identity) Overlaps(other Identity) bool {
	if id.Kind != KindInterval || other.Kind != KindInterval {
		return false
	}
	if id.Generation != other.Generation || id.Space != other.Space {
		return false
	}
	if id.Validate() != nil || other.Validate() != nil {
		return false
	}
	return intervalsOverlap(
		id.Lower, id.Upper, id.LowerInclusive, id.UpperInclusive,
		other.Lower, other.Upper, other.LowerInclusive, other.UpperInclusive,
	)
}

// intervalsOverlap decides whether two legal intervals share at least one key.
//
// It is stated as two disjointness tests, one in each direction, rather than as a
// one-sided test after ordering the intervals. A one-sided test needs a real total
// order on intervals, and the obvious lower-bound order is not one: an interval can
// start later and still be the one that should be compared first. Two independent
// disjointness tests cannot make that mistake.
//
// The endpoint rule is exact: an interval ends *before* another begins when the
// bounds differ, and when they are equal the two intervals are disjoint exactly when
// at least one of the two touching ends excludes the point.
func intervalsOverlap(aLower, aUpper []byte, aLowerInclusive, aUpperInclusive bool,
	bLower, bUpper []byte, bLowerInclusive, bUpperInclusive bool) bool {
	if boundsDisjoint(aLower, aLowerInclusive, bUpper, bUpperInclusive) {
		return false
	}
	if boundsDisjoint(bLower, bLowerInclusive, aUpper, aUpperInclusive) {
		return false
	}
	return true
}

// boundsDisjoint reports whether one interval's lower bound lies past another
// interval's upper bound, so the two share no key. A nil bound is the corresponding
// infinity and never lies past anything.
func boundsDisjoint(lower []byte, lowerInclusive bool, upper []byte, upperInclusive bool) bool {
	if upper == nil || lower == nil {
		// One side is infinite, so this direction cannot prove disjointness.
		return false
	}
	switch bytes.Compare(lower, upper) {
	case -1:
		return false
	case 0:
		// The bounds meet at one point, which both intervals share only when both
		// facing ends include it.
		return !(lowerInclusive && upperInclusive)
	default:
		return true
	}
}

// CloneResourceRefs returns an independent copy of a component list. It is the
// single clone point for request accounting, so a caller that reuses its slice
// cannot change a granted bundle.
func CloneResourceRefs(components []ResourceRef) []ResourceRef {
	if len(components) == 0 {
		return nil
	}
	out := make([]ResourceRef, len(components))
	for i, component := range components {
		out[i] = ResourceRef{Identity: component.Identity.Clone()}
	}
	return out
}

// RefBytes reports the accounted size of a component list.
func RefBytes(components []ResourceRef) int64 {
	var total int64
	for _, component := range components {
		total += int64(component.OwnedBytes())
	}
	return total
}

// SortResourceRefs orders a component list canonically.
//
// The order is the byte order of the canonical resource key, which is what makes it a
// real total order on identities and lets a caller compare two bundles' orders
// directly. The kind comes first inside the key, so a bundle always lists a table's
// intention before the rows it protects. A mode is only a tiebreaker, because it is
// not part of the identity.
func SortResourceRefs(components []ResourceRef) {
	sort.SliceStable(components, func(i, j int) bool {
		a, b := components[i].Identity, components[j].Identity
		if left, right := a.ClaimKey(), b.ClaimKey(); left != right {
			return left < right
		}
		return a.Mode < b.Mode
	})
}

// RefsInCanonicalOrder reports whether a component list is already in canonical
// order. It is the sortedness check the identity test asserts.
func RefsInCanonicalOrder(components []ResourceRef) bool {
	for i := 1; i < len(components); i++ {
		if components[i-1].ClaimKey() > components[i].ClaimKey() {
			return false
		}
	}
	return true
}

// DedupeResourceRefs collapses repeated identities while keeping the strongest
// requested mode for each. It returns the canonical-ordered result.
//
// It exists so a bundle that names the same row twice is charged once, and so the
// caller's bundle size is not the accounting unit: a component count is not a
// bundle count.
//
// Components are collapsed by ClaimKey, never by the conflict domain: two gaps with different
// bounds are two locks, and an intention at one position never stands in for an intention at
// another. Collapsing by the domain would silently drop a protection the caller asked for — and
// because the mode rank orders an intention above a gap, a bundle carrying both would lose the
// gap entirely while still validating.
func DedupeResourceRefs(components []ResourceRef) []ResourceRef {
	if len(components) == 0 {
		return nil
	}
	byKey := make(map[string]int, len(components))
	out := make([]ResourceRef, 0, len(components))
	for _, component := range components {
		if err := component.Validate(); err != nil {
			// An invalid component is dropped here so it cannot be merged with a
			// valid one; the request validation step rejects the whole bundle.
			out = append(out, ResourceRef{Identity: component.Identity.Clone()})
			continue
		}
		key := component.ClaimKey()
		if index, ok := byKey[key]; ok {
			if modeRank(component.Mode) > modeRank(out[index].Mode) {
				out[index].Mode = component.Mode
			}
			continue
		}
		byKey[key] = len(out)
		out = append(out, ResourceRef{Identity: component.Identity.Clone()})
	}
	SortResourceRefs(out)
	return out
}

// modeRank orders modes by strength for deduplication. It only has to be a total
// order; compatibility is decided by the published matrix and never by this rank.
func modeRank(m Mode) int {
	switch m {
	case ModeIS:
		return 1
	case ModeGap:
		return 2
	case ModeInsertIntention:
		return 3
	case ModeS:
		return 4
	case ModeIX:
		return 5
	case ModeX:
		return 6
	default:
		return 0
	}
}

// NamespaceBinding binds one raw byte namespace to the immutable SQL metadata label
// of the row or index that owns it.
//
// The backend adapter publishes a binding once a namespace is known. The lock
// manager only carries the label: it never parses a key and never re-encodes one, so
// a raw write and a SQL write to the same row always meet on the same resource.
//
// Node distinguishes the physical relation a SQL index label belongs to, because
// two tables may both have an index named "PRIMARY". It is an opaque tag.
type NamespaceBinding struct {
	Namespace string
	TableID   string
	IndexID   string
	Node      string
	Bound     uint64
}

// Validate reports whether a binding carries a usable label.
func (b NamespaceBinding) Validate() error {
	if b.Namespace == "" || b.TableID == "" {
		return ErrInvalidResource
	}
	return nil
}

// Key is the binding identity, which is the namespace alone: one namespace has
// exactly one owner, and re-binding it to a different label is a conflict.
func (b NamespaceBinding) Key() string { return b.Namespace }

// Bindings is the generation-scoped namespace-to-label registry of one engine.
//
// A conflicting re-bind is refused rather than replaced: silently replacing it would
// let two SQL tables share one lock identity. A generation change invalidates the
// whole registry.
type Bindings struct {
	mu         sync.RWMutex
	generation uint64
	entries    map[string]NamespaceBinding
	bytes      int64
}

// NewBindings returns an empty registry for one engine generation.
func NewBindings(generation uint64) *Bindings {
	return &Bindings{generation: generation, entries: make(map[string]NamespaceBinding)}
}

// Bind records one binding. Re-binding the same namespace to the same label is
// idempotent; a different label is a conflict.
func (b *Bindings) Bind(binding NamespaceBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, ok := b.entries[binding.Key()]; ok {
		if existing.TableID != binding.TableID || existing.IndexID != binding.IndexID || existing.Node != binding.Node {
			return ErrInvalidResource
		}
		return nil
	}
	if b.entries == nil {
		b.entries = make(map[string]NamespaceBinding)
	}
	b.entries[binding.Key()] = binding
	b.bytes += int64(len(binding.Namespace) + len(binding.TableID) + len(binding.IndexID) + len(binding.Node) + resourceFixedBytes)
	return nil
}

// Lookup returns the binding of one namespace.
func (b *Bindings) Lookup(namespace string) (NamespaceBinding, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	binding, ok := b.entries[namespace]
	return binding, ok
}

// Unbind removes one namespace binding.
func (b *Bindings) Unbind(namespace string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if binding, ok := b.entries[namespace]; ok {
		b.bytes -= int64(len(binding.Namespace) + len(binding.TableID) + len(binding.IndexID) + len(binding.Node) + resourceFixedBytes)
		delete(b.entries, namespace)
	}
}

// Reset drops every binding and moves to a new generation. It is the generation
// change hook: bindings from a replaced database must not survive into the new one.
func (b *Bindings) Reset(generation uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.generation = generation
	b.entries = make(map[string]NamespaceBinding)
	b.bytes = 0
}

// Generation reports the generation this registry belongs to.
func (b *Bindings) Generation() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.generation
}

// Bytes reports the accounted size of the binding registry.
func (b *Bindings) Bytes() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.bytes
}

// Len reports how many namespaces are bound.
func (b *Bindings) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries)
}

// ScopeTree is the ancestry model of one owner's scopes.
//
// Closing a scope cancels that scope's own pending requests and nothing else. A
// scope is never a grant owner, so no close path in this type releases a grant:
// keeping that rule structural is what makes "child rollback and savepoint rollback
// keep the root's granted locks" impossible to get wrong by accident.
type ScopeTree struct {
	scopes map[uint64]*Scope
	nextID uint64
}

// NewScopeTree returns an empty tree.
func NewScopeTree() *ScopeTree { return &ScopeTree{scopes: make(map[uint64]*Scope)} }

// Open registers one scope. A non-zero parent must already exist and belong to the
// same owner; a child scope can therefore never be attached to another owner's tree.
//
// deadline is the context deadline of the scope: its statement or query bound. The zero time means
// "no context deadline". The session's per-wait budget is opened separately, by OpenWithBudget.
func (t *ScopeTree) Open(ownerID, originTxnID string, parentID uint64, kind ScopeKind, openedAt time.Time, deadline time.Time) (*Scope, error) {
	return t.OpenWithBudget(ownerID, originTxnID, parentID, kind, openedAt, deadline, 0)
}

// OpenWithBudget opens a scope whose waits each carry one budget.
//
// waitBudget is the session's lock-wait budget as a duration; zero means the manager's default. It is
// kept separate from the context deadline so that each wait is measured from the moment it starts
// waiting, while the context deadline still caps the whole scope.
func (t *ScopeTree) OpenWithBudget(ownerID, originTxnID string, parentID uint64, kind ScopeKind, openedAt time.Time, deadline time.Time, waitBudget time.Duration) (*Scope, error) {
	if ownerID == "" || !kind.Valid() {
		return nil, ErrScopeUnknown
	}
	if parentID != 0 {
		parent, ok := t.scopes[parentID]
		if !ok || parent.OwnerID != ownerID {
			return nil, ErrScopeOwnerMismatch
		}
	}
	if t.nextID == ^uint64(0) {
		return nil, ErrOrdinalExhausted
	}
	t.nextID++
	scope := &Scope{
		ID:              t.nextID,
		OwnerID:         ownerID,
		OriginTxnID:     originTxnID,
		ParentScopeID:   parentID,
		Kind:            kind,
		State:           ScopeOpen,
		OpenedAt:        openedAt,
		ContextDeadline: deadline,
		WaitBudget:      waitBudget,
	}
	t.scopes[scope.ID] = scope
	return scope, nil
}

// Get returns one scope, whether open or closed.
func (t *ScopeTree) Get(id uint64) (Scope, bool) {
	scope, ok := t.scopes[id]
	if !ok {
		return Scope{}, false
	}
	return *scope, true
}

// BeginClose marks one open scope as closing and returns it.
func (t *ScopeTree) BeginClose(id uint64) (Scope, error) {
	scope, ok := t.scopes[id]
	if !ok {
		return Scope{}, ErrScopeUnknown
	}
	if scope.State == ScopeOpen {
		scope.State = ScopeClosing
	}
	return *scope, nil
}

// Close marks one scope closed. It is idempotent and keeps the identity, so a
// diagnostics page can still name the historical origin scope of a grant.
func (t *ScopeTree) Close(id uint64) {
	if scope, ok := t.scopes[id]; ok {
		scope.State = ScopeClosed
	}
}

// Discard removes one scope from the tree entirely.
//
// It exists for the failure path of Open: a scope that was never successfully created
// must not remain as a closed entry, because a closed entry still counts as a
// registered scope. Only Open's own rollback may call it.
func (t *ScopeTree) Discard(id uint64) {
	delete(t.scopes, id)
}

// CloseSubtree marks one scope and every descendant closed, deepest first, and
// returns the ids it closed in that order.
//
// A discarded savepoint layer takes its nested scopes with it, but the returned ids
// are for cancelling their pending requests only: the caller must not release any
// grant because of them.
func (t *ScopeTree) CloseSubtree(id uint64) []uint64 {
	if _, ok := t.scopes[id]; !ok {
		return nil
	}
	// Build the depth order with one pass per level so a deep chain cannot recurse.
	order := []uint64{id}
	for level := 0; level < len(order); level++ {
		for _, candidate := range t.scopes {
			if candidate.ParentScopeID == order[level] && candidate.State != ScopeClosed {
				order = append(order, candidate.ID)
			}
		}
	}
	// Deepest first: reverse the breadth order.
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	for _, scopeID := range order {
		t.Close(scopeID)
	}
	return order
}

// Ancestors returns the scope chain from one scope outward to its root scope,
// starting with the scope itself. The result lets a read layer decide which
// ancestors' overlays it may observe.
func (t *ScopeTree) Ancestors(id uint64) []Scope {
	var chain []Scope
	seen := make(map[uint64]bool, 4)
	current, ok := t.scopes[id]
	for ok && !seen[current.ID] {
		seen[current.ID] = true
		chain = append(chain, *current)
		if current.ParentScopeID == 0 {
			break
		}
		current, ok = t.scopes[current.ParentScopeID]
	}
	return chain
}

// IsAncestor reports whether ancestor could be an ancestor of descendant. It is the
// read-layer overlay rule: a scope observes its own writes and its ancestors' writes,
// and never a sibling's.
func (t *ScopeTree) IsAncestor(ancestor, descendant uint64) bool {
	if ancestor == 0 || descendant == 0 {
		return false
	}
	for _, scope := range t.Ancestors(descendant) {
		if scope.ID == ancestor {
			return true
		}
	}
	return false
}

// OpenScopes returns how many scopes of this tree are not closed. It is the
// bounded-resource view of the tree.
func (t *ScopeTree) OpenScopes() int {
	count := 0
	for _, scope := range t.scopes {
		if scope.State != ScopeClosed {
			count++
		}
	}
	return count
}

// Len returns the number of scopes this tree ever created, including closed ones.
func (t *ScopeTree) Len() int { return len(t.scopes) }
