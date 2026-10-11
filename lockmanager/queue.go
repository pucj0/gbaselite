package lockmanager

import (
	"sort"
)

// queue.go holds the lock table of one engine's lock manager: which owner holds which mode
// on which resource, and which requests are waiting for it.
//
// The rules it implements are the contract's:
//
//   - A resource's holders are recorded per owner together with every request that
//     contributed to that owner's mode, so a re-entrant request never conflicts with
//     itself *and* a cancelled or released request can retract exactly its own claim
//     without disturbing a stronger one the same owner still holds.
//   - A request is blocked by an incompatible holder or by an older incompatible waiter.
//     That second half is the fairness rule: a young shared request may not overtake an
//     older request it would block, while an unrelated resource keeps making progress.
//   - A request reaches exactly one terminal status, decided under the manager mutex.
//
// Nothing here waits, calls a callback or performs I/O: every function is called with the
// manager mutex held and returns immediately. That is what makes "no latch is held across a
// wait" structural rather than a rule the caller has to remember.

// lockClaim is one request's contribution to its owner's mode on one resource.
//
// A grant is not simply "owner -> mode". It is the strongest of the claims the owner holds
// there, and each claim belongs to one request. Keeping the claims is what makes these three
// cases correct at once:
//
//   - a re-entrant request adds a claim and changes nothing;
//   - a weaker claim must not downgrade a stronger mode the same owner already holds;
//   - a cancelled or released request retracts only its own claim, so releasing the shared
//     lock it took cannot drop the exclusive mode another request of the same owner holds.
type lockClaim struct {
	request *LockRequest
	mode    Mode
	// ref is the component this claim protects, including its interval bounds.
	//
	// The bounds live on the claim rather than being looked up from the request, because the
	// conflict rule for an interval needs them on every check: a gap blocks an insertion only
	// inside the gap, and a claim whose bounds were unavailable would have to answer
	// conservatively instead of correctly.
	ref ResourceRef
	// pending records that this claim has not been applied yet because its bundle is still
	// waiting on another resource. A pending claim still counts against other owners, so a
	// bundle's blocked components cannot be taken by someone else in the meantime.
	pending bool
}

// entryState records how far one bundle component has settled.
//
// The three states are needed because a bundle is granted as a whole while its components are
// held one resource at a time: a component can be applied, still waiting, or already covered
// by the owner's earlier claim. Collapsing "applied" and "never queued" into one boolean makes
// a partially granted bundle look complete, which reports a grant for a component that is not
// held.
type entryState uint8

const (
	// entryPending is a component that is not held yet and is waiting in its resource's queue.
	entryPending entryState = iota
	// entryHeld is a component whose claim has been applied to its owner's mode.
	entryHeld
	// entryAdopted is a component the owner already held through an earlier request, so this
	// request takes no claim for it and its release must not retract the adopted claim.
	entryAdopted
)

// lockRequest is one entry of the waiting footprint of a resource.
type lockRequest struct {
	req    *LockRequest
	ref    ResourceRef
	bundle *requestBundle
	// state records how far this component has settled. A cancelled or released request must
	// retract only the claims it really holds, and a component that was already granted when
	// the rest of the bundle blocked must not be mistaken for one that was never queued.
	state entryState
	// adopted is the earlier request whose claim already covers this component, when the
	// owner held it before this bundle was queued. An adopted component adds no claim, and
	// this request is then already complete for it.
	adopted *LockRequest
	// blocked records whether this component is held back by a holder or by an older waiter.
	blocked bool
	// dead marks a request whose owner ended or whose scope or epoch invalidated it. A dead
	// entry is removed from the queue and never granted.
	dead bool
}

// settled reports whether this component no longer needs a grant.
func (r *lockRequest) settled() bool { return r.state != entryPending }

// requestBundle is one atomic request: its identity, its per-component queue entries, its
// budget reservation and the count of components already held.
type requestBundle struct {
	req         *LockRequest
	reservation *Reservation
	entries     []*lockRequest
	// held counts the entries whose claim is applied. The bundle is complete exactly when it
	// reaches len(entries), and completion is what commits the reservation, records the grant
	// and notifies the caller once.
	held    int
	grantID uint64
	// blockers is the initial blocker footprint, one entry per distinct blocking root.
	blockers []string
	// blockedByOlderWaiter records that at least one component was held back by an older
	// waiter rather than by a holder, so diagnostics can name the reason.
	blockedByOlderWaiter bool
	// decision is the shared, single terminal status a caller reads through its handle.
	decision *requestState
}

// complete reports whether every component of the bundle is held.
func (b *requestBundle) complete() bool { return b.held >= len(b.entries) }

// setStatus records the single terminal outcome and notifies the waiting caller once.
//
// It is the only writer of the terminal status, so "granted" and "cancelled" can never both
// be observed, and reading the status is idempotent because it is stored before the
// notification is sent. The stored value is the synchronized decision's own winner, so a
// later attempt (a cancel that lost to a grant) delivers the winner instead of its own
// status, and LockRequest.Status — which is only ever written here and only ever read under
// the manager mutex — is kept equal to the decision.
func (b *requestBundle) setStatus(status RequestStatus) {
	recorded := status
	if b.decision != nil {
		recorded = b.decision.record(status)
	}
	b.req.Status = recorded
	b.req.Notify(recorded)
}

// untrack removes every entry of this bundle from a per-root pending index.
func (b *requestBundle) untrack(index map[string]map[*lockRequest]bool) {
	requests := index[b.req.OwnerID]
	if requests == nil {
		return
	}
	for _, entry := range b.entries {
		delete(requests, entry)
	}
	if len(requests) == 0 {
		delete(index, b.req.OwnerID)
	}
}

// components returns the bundle's components in canonical order.
func (b *requestBundle) components() []ResourceRef {
	out := make([]ResourceRef, 0, len(b.entries))
	for _, entry := range b.entries {
		out = append(out, entry.ref)
	}
	return out
}

// resourceKeys returns the canonical keys of the bundle's components, deduplicated and
// sorted.
//
// The order matters because a bundle can touch several resources and the manager must
// advance them in one stable order, so the outcome cannot depend on map iteration order.
func (b *requestBundle) resourceKeys() []string {
	keys := make([]string, 0, len(b.entries))
	seen := make(map[string]bool, len(b.entries))
	for _, entry := range b.entries {
		key := entry.ref.ResourceKey()
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// lockEntry is one resource's lock state: the claims by owner, and the FIFO queue of
// requests waiting for it.
type lockEntry struct {
	ref ResourceRef
	// anonID is this resource's anonymous identity in the wait graph and in diagnostics. It is
	// assigned on demand from a manager-local counter and carries no information about the
	// table, index or key the entry stands for.
	anonID uint64
	// claims maps an owner to the claims its requests hold on this resource.
	claims map[string][]lockClaim
	// claimedModes indexes the claims by mode, so the conflict scan is over the small, fixed set
	// of modes in use rather than over every claim. A resource with thousands of queued waiters
	// would otherwise make each conflict check cost as much as the whole queue, which turns one
	// acquisition into a quadratic pass over the queue.
	claimedModes map[Mode]map[string]bool
	// waiting is the FIFO queue of requests that have not been granted their claim yet.
	waiting []*lockRequest
	// grantID identifies this entry for diagnostics and wait-edge reporting.
	grantID uint64
}

func newLockEntry(ref ResourceRef) *lockEntry {
	return &lockEntry{ref: ref, claims: make(map[string][]lockClaim)}
}

// effectiveClaim reduces one owner's claims on this resource to the strongest mode, with the
// caller choosing whether the claims of requests that are still waiting count.
//
// The two views answer two different questions, and keeping them apart is what makes a
// waiting upgrade correct:
//
//   - What the owner *holds* counts applied claims only, so a transaction waiting to upgrade
//     still holds the shared lock it started with.
//   - What the owner *offers* to everyone else counts pending claims too, so a transaction
//     waiting to upgrade already blocks a new shared request from another owner.
func (e *lockEntry) effectiveClaim(ownerID string, includePending bool) (Mode, bool) {
	claims := e.claims[ownerID]
	if len(claims) == 0 {
		return "", false
	}
	modes := make([]Mode, 0, len(claims))
	for _, claim := range claims {
		if claim.pending && !includePending {
			continue
		}
		modes = append(modes, claim.mode)
	}
	if len(modes) == 0 {
		return "", false
	}
	return effectiveMode(modes...), true
}

// heldMode reports what the owner holds, which is the applied claims only.
func (e *lockEntry) heldMode(ownerID string) (Mode, bool) { return e.effectiveClaim(ownerID, false) }

// offeredMode reports the mode the owner offers other owners, which includes the claims of
// its waiting requests.
func (e *lockEntry) offeredMode(ownerID string) (Mode, bool) { return e.effectiveClaim(ownerID, true) }

// hasAppliedClaim reports whether this owner already holds an applied claim whose mode
// covers want, which is the re-entrancy case.
func (e *lockEntry) hasAppliedClaim(ownerID string, want Mode) bool {
	for _, claim := range e.claims[ownerID] {
		if claim.pending {
			continue
		}
		if !upgradeNeeded(want, claim.mode) {
			return true
		}
	}
	return false
}

// addClaim records one request's claim on this resource.
//
// A claim is stored even when it is already covered, so the request can retract exactly its
// own contribution later; retracting nothing because the mode was already covered would let
// a release drop a mode the owner still needs.
func (e *lockEntry) addClaim(request *LockRequest, ref ResourceRef, pending bool) {
	e.claims[request.OwnerID] = append(e.claims[request.OwnerID], lockClaim{request: request, mode: ref.Mode, ref: ref, pending: pending})
	e.indexClaim(request.OwnerID, ref.Mode)
}

// indexClaim records that one owner has a claim at one mode.
func (e *lockEntry) indexClaim(ownerID string, mode Mode) {
	if e.claimedModes == nil {
		e.claimedModes = make(map[Mode]map[string]bool, 4)
	}
	owners := e.claimedModes[mode]
	if owners == nil {
		owners = make(map[string]bool, 4)
		e.claimedModes[mode] = owners
	}
	owners[ownerID] = true
}

// deindexClaim drops one owner from one mode's index, removing an emptied mode bucket.
func (e *lockEntry) deindexClaim(ownerID string, mode Mode) {
	owners := e.claimedModes[mode]
	if owners == nil {
		return
	}
	delete(owners, ownerID)
	if len(owners) == 0 {
		delete(e.claimedModes, mode)
	}
}

// applyClaim marks the claim of one request as applied. It reports whether a claim was found.
func (e *lockEntry) applyClaim(request *LockRequest) bool {
	claims := e.claims[request.OwnerID]
	for i := range claims {
		if claims[i].request != request {
			continue
		}
		claims[i].pending = false
		return true
	}
	return false
}

// dropClaim retracts one request's claim. It reports whether a claim was removed.
func (e *lockEntry) dropClaim(request *LockRequest) bool {
	claims := e.claims[request.OwnerID]
	for i := range claims {
		if claims[i].request != request {
			continue
		}
		mode := claims[i].mode
		claims[i] = claims[len(claims)-1]
		claims[len(claims)-1] = lockClaim{}
		claims = claims[:len(claims)-1]
		if len(claims) == 0 {
			delete(e.claims, request.OwnerID)
		} else {
			e.claims[request.OwnerID] = claims
		}
		// The mode index is maintained alongside the claim list, so a mode bucket never outlives
		// the last claim at that mode.
		if !e.ownerHoldsMode(request.OwnerID, mode) {
			e.deindexClaim(request.OwnerID, mode)
		}
		return true
	}
	return false
}

// ownerHoldsMode reports whether one owner still has a claim at one mode.
func (e *lockEntry) ownerHoldsMode(ownerID string, mode Mode) bool {
	for _, claim := range e.claims[ownerID] {
		if claim.mode == mode {
			return true
		}
	}
	return false
}

// dropAllClaims retracts every claim of one owner on this resource. It is the owner-release
// path: a root that ends drops everything it held.
func (e *lockEntry) dropAllClaims(ownerID string) bool {
	claims, ok := e.claims[ownerID]
	if !ok {
		return false
	}
	delete(e.claims, ownerID)
	// Every mode this owner contributed must leave the index with it, or a released owner would
	// keep blocking others through a stale mode bucket.
	seen := make(map[Mode]bool, len(claims))
	for _, claim := range claims {
		if seen[claim.mode] {
			continue
		}
		seen[claim.mode] = true
		if !e.ownerHoldsMode(ownerID, claim.mode) {
			e.deindexClaim(ownerID, claim.mode)
		}
	}
	return true
}

// dropClaimForOwner retracts one claim of one owner on this resource and reports whether a
// claim was removed.
//
// The caller supplies the owner rather than the request because the public release path
// releases a *resource*, not a specific request: an owner that acquired one lock through
// several re-entrant requests releases it once, and exactly one of its claims is dropped so
// the owner's mode is reduced by one request's worth rather than cleared outright.
func (e *lockEntry) dropClaimForOwner(ownerID string) bool {
	claims := e.claims[ownerID]
	if len(claims) == 0 {
		return false
	}
	// Prefer to retract an applied, weakest claim: a pending claim belongs to a request that
	// is still waiting, and retracting it would silently change what that request asked for.
	index := -1
	for i, claim := range claims {
		if claim.pending {
			continue
		}
		if index == -1 || modeStrength(claim.mode) < modeStrength(claims[index].mode) {
			index = i
		}
	}
	if index == -1 {
		index = 0
	}
	mode := claims[index].mode
	claims[index] = claims[len(claims)-1]
	claims[len(claims)-1] = lockClaim{}
	claims = claims[:len(claims)-1]
	if len(claims) == 0 {
		delete(e.claims, ownerID)
	} else {
		e.claims[ownerID] = claims
	}
	if !e.ownerHoldsMode(ownerID, mode) {
		e.deindexClaim(ownerID, mode)
	}
	return true
}

// heldModes returns the modes held by owners other than skip.
func (e *lockEntry) heldModes(skip string) []Mode {
	if len(e.claims) == 0 {
		return nil
	}
	modes := make([]Mode, 0, len(e.claims))
	for ownerID := range e.claims {
		if ownerID == skip {
			continue
		}
		if mode, ok := e.offeredMode(ownerID); ok {
			modes = append(modes, mode)
		}
	}
	return modes
}

// heldBy returns the mode one owner holds on this resource, if any.
func (e *lockEntry) heldBy(ownerID string) (Mode, bool) { return e.heldMode(ownerID) }

// holderModesByOwner reports every owner currently holding a claim on this resource, with the
// mode it offers.
//
// It counts offers rather than applied claims, so an owner whose upgrade is still queued is
// listed with the stronger mode it already protects the resource with. That is what a holder
// list must show, because it is the mode other owners collide with.
func (e *lockEntry) holderModesByOwner() map[string]Mode {
	out := make(map[string]Mode, len(e.claims))
	for ownerID := range e.claims {
		if mode, ok := e.offeredMode(ownerID); ok {
			out[ownerID] = mode
		}
	}
	return out
}

// holderMode reports the strongest mode currently granted on this resource.
func (e *lockEntry) holderMode() Mode {
	modes := make([]Mode, 0, len(e.claims))
	for ownerID := range e.claims {
		if mode, ok := e.offeredMode(ownerID); ok {
			modes = append(modes, mode)
		}
	}
	return effectiveMode(modes...)
}

// holderCount reports how many owners hold this resource.
func (e *lockEntry) holderCount() int { return len(e.claims) }

// waiterCount reports how many requests are queued for this resource.
func (e *lockEntry) waiterCount() int { return len(e.waiting) }

// state projects holders and waiters onto one observable state.
func (e *lockEntry) state() LockState {
	return Describe(len(e.claims), e.holderMode(), len(e.waiting))
}

// conflictingHolders reports the owners holding a mode incompatible with one component, ignoring
// one owner entirely.
//
// It is for the acquire path, where the requesting owner must not conflict with itself. The
// component rather than its mode is passed in, because an interval conflict depends on the actual
// bounds: a gap blocks an insertion only inside the gap.
func (e *lockEntry) conflictingHolders(request ResourceRef, skip string) []string {
	var owners []string
	for ownerID := range e.claims {
		if ownerID == skip {
			continue
		}
		if e.ownerConflicts(ownerID, request) {
			owners = append(owners, ownerID)
		}
	}
	sort.Strings(owners)
	return owners
}

// ownerConflicts reports whether anything one owner holds on this resource conflicts with a
// component.
//
// Every claim of that owner is consulted with its own bounds, so two gaps over disjoint ranges
// are recognised as compatible instead of both being collapsed into one unbounded gap.
func (e *lockEntry) ownerConflicts(ownerID string, request ResourceRef) bool {
	for _, claim := range e.claims[ownerID] {
		held := e.claimComponent(claim, request)
		if componentsConflict(request, held) {
			return true
		}
	}
	return false
}

// claimComponent reconstructs the component one claim holds.
//
// The bounds of a claim live on the request that made it, so a claim whose request no longer
// exists falls back to a component without bounds. That is the conservative direction: a gap with
// unknown bounds blocks every insertion rather than none.
func (e *lockEntry) claimComponent(claim lockClaim, request ResourceRef) ResourceRef {
	held := claim.ref
	if held.Kind == "" {
		// A claim recorded before the component was stored falls back to the entry's own identity
		// plus the claim's mode, which is enough to answer a Record or table conflict and answers
		// the interval question conservatively.
		held = ResourceRef{Identity: Identity{
			Kind:    request.Kind,
			Mode:    claim.mode,
			Space:   request.Space,
			TableID: request.TableID,
			IndexID: request.IndexID,
			Key:     request.Key,
		}}
		return held
	}
	// The mode is the claim's, not the component's: the same component can be claimed by a request
	// that later upgraded, and the conflict must be evaluated against what the owner decided to
	// take, which is what the claim records.
	if held.Mode != claim.mode {
		held.Mode = claim.mode
	}
	return held
}

// blockingClaims reports the *other owners* whose claims conflict with one queued entry.
//
// Unlike conflictingHolders it works from a specific entry and skips that entry's own
// pending claim. A queued request has already recorded its claim as pending, so without this
// check the bundle's own claim would appear to block the bundle that made it, and the fair
// pass would never grant it.
func (e *lockEntry) blockingClaims(entry *lockRequest) []string {
	return e.appendBlockingClaims(nil, entry)
}

// appendBlockingClaims appends the blocking owners to a caller-provided buffer.
//
// The graph rebuild walks every queued component, so it supplies its own buffer and reuses it
// across components. Allocating one slice per component instead made a rebuild allocation-bound
// and turned the queue's cost into garbage rather than work.
func (e *lockEntry) appendBlockingClaims(dst []string, entry *lockRequest) []string {
	owners := dst
	// The scan is over the modes actually in use on this resource — a fixed, small set — rather
	// than over every claim. With thousands of queued waiters the per-claim scan would make one
	// acquisition cost as much as the whole queue, so this is what keeps the queue linear.
	for mode, modeOwners := range e.claimedModes {
		for ownerID := range modeOwners {
			if ownerID == entry.req.OwnerID {
				// The requesting owner never blocks itself. Its own granted claims are what it
				// already holds, and an upgrade converts one of them rather than conflicting with
				// it; an earlier waiting request of the same owner is handled by waitsBehindOwner.
				continue
			}
			if e.ownerBlocksWith(ownerID, mode, entry) {
				owners = append(owners, ownerID)
			}
		}
	}
	if len(owners) > len(dst) {
		// Only the newly appended range is unordered, so only it is sorted; the caller's earlier
		// entries keep their order and the function stays usable as a pure append.
		sort.Strings(owners[len(dst):])
	}
	return owners
}

// hasBlockingClaims reports whether any other owner's claim conflicts with an entry.
//
// It is the fair pass's question, and it stops at the first blocking owner instead of collecting
// and sorting them. The fair pass asks it for every queued candidate, so building the full owner
// list — which means a sort with string comparisons — would dominate an acquisition once a queue
// is long. The reporting form is only needed by diagnostics, which runs rarely.
func (e *lockEntry) hasBlockingClaims(entry *lockRequest) bool {
	for mode, modeOwners := range e.claimedModes {
		for ownerID := range modeOwners {
			if ownerID == entry.req.OwnerID {
				continue
			}
			if e.ownerBlocksWith(ownerID, mode, entry) {
				return true
			}
		}
	}
	return false
}

// ownerBlocksWith reports whether one owner's claim at one mode actually blocks an entry.
//
// The mode index answers "does anyone hold an incompatible mode", and this answers the two
// per-claim questions that the index deliberately does not carry: whether the only incompatible
// claim belongs to the entry's own request, and whether it is a younger pending claim that the
// FIFO rule says must not block an older request.
func (e *lockEntry) ownerBlocksWith(ownerID string, mode Mode, entry *lockRequest) bool {
	for _, claim := range e.claims[ownerID] {
		if claim.mode != mode {
			continue
		}
		if claim.request == entry.req {
			continue
		}
		if claim.pending && claim.request != nil && claim.request.EnqueueOrdinal > entry.req.EnqueueOrdinal {
			// A *younger* pending claim arrived after this request. The queue is FIFO, so it must
			// not block an older request: doing so would let every later arrival hold back an
			// earlier one for ever.
			continue
		}
		// The conflict decision needs the held component's own bounds, because a Gap blocks an
		// insertion only inside the gap. Evaluating the mode ratio alone would make every gap
		// block every insertion on the resource.
		if componentsConflict(entry.ref, e.claimComponent(claim, entry.ref)) {
			return true
		}
	}
	return false
}

// olderBlockers reports the older waiters on this resource whose mode would be hindered by the
// candidate's request.
//
// It is waitersAhead with the blockers themselves instead of a yes/no answer, so the wait graph
// can name them. The order is the queue order, which is what keeps the fairness rule and its
// graph edges the same rule stated twice rather than two rules that can drift apart.
func (e *lockEntry) olderBlockers(self *lockRequest, request ResourceRef) []*lockRequest {
	var older []*lockRequest
	for _, candidate := range e.waiting {
		if candidate == self {
			return older
		}
		if candidate == nil || candidate.dead {
			continue
		}
		if componentsConflict(request, candidate.ref) {
			older = append(older, candidate)
		}
	}
	return older
}

// immediateConflict reports whether a *newly arrived* request conflicts with something other
// than its own owner: another owner's claim, or an older waiter that it would hinder.
//
// The requesting owner is skipped entirely, which is the re-entrancy rule: an upgrade must not
// be blocked by the shared claim it is converting. Ordering behind the owner's own earlier
// queued request is a queue concern rather than an arrival concern; waitersAhead covers the
// case where that earlier request is already in the queue.
func (e *lockEntry) immediateConflict(candidate *lockRequest) bool {
	return len(e.conflictingHolders(candidate.ref, candidate.req.OwnerID)) > 0 ||
		e.waitersAhead(candidate, candidate.ref)
}

// waitsBehindOwner reports whether the requesting owner already has an earlier waiting
// request with an incompatible claim here.
//
// A repeated request must wait behind the owner's own queued request rather than be granted
// alongside it: granting it would let one transaction hold two claims for one protection and
// would report success while the earlier request is still blocked. It is checked by the fair
// pass, which is where a request that has already recorded its own claim is evaluated.
func (e *lockEntry) waitsBehindOwner(entry *lockRequest) bool {
	for _, earlier := range e.waiting {
		if earlier == nil || earlier.dead || earlier == entry {
			continue
		}
		if earlier.req.OwnerID != entry.req.OwnerID {
			continue
		}
		if earlier.ref.Mode == entry.ref.Mode {
			// The same protection is already queued: this request is a duplicate of one the
			// owner is already waiting for, so it is adopted rather than taken again.
			entry.state = entryAdopted
			return true
		}
	}
	return false
}

// enqueue appends one request to the resource's FIFO queue.
func (e *lockEntry) enqueue(entry *lockRequest) { e.waiting = append(e.waiting, entry) }

// remove drops one request from the queue, keeping the order of the rest.
func (e *lockEntry) remove(target *lockRequest) bool {
	for i, entry := range e.waiting {
		if entry != target {
			continue
		}
		copy(e.waiting[i:], e.waiting[i+1:])
		e.waiting[len(e.waiting)-1] = nil
		e.waiting = e.waiting[:len(e.waiting)-1]
		return true
	}
	return false
}

// waitersAhead reports whether an older waiter would be blocked by this request.
//
// It is the fairness rule of the contract: "a young request may only be granted alongside an
// older waiter when it does not hinder it". Only waiters that appear earlier in the queue
// count, so a request is never blocked by a request behind it.
func (e *lockEntry) waitersAhead(self *lockRequest, request ResourceRef) bool {
	// It is olderBlockers as a yes/no answer, and it returns at the first blocking waiter rather
	// than collecting them all. That matters: the fair pass asks this question for every queued
	// candidate, so building the full list each time would make one acquisition walk the whole
	// queue once per queued request.
	for _, candidate := range e.waiting {
		if candidate == self {
			return false
		}
		if candidate == nil || candidate.dead {
			continue
		}
		if componentsConflict(request, candidate.ref) {
			return true
		}
	}
	return false
}

// grantable reports whether one queued component entry may be granted now, recording the
// reason when it may not.
//
// The two rules are separate on purpose. A holder of another owner with an incompatible mode
// is a HOLDER block. An older waiter that this request would hinder is an OLDER_WAITER
// block, and it is the reason a young shared request cannot join the holders in front of an
// old exclusive waiter.
func (e *lockEntry) grantable(candidate *lockRequest) bool {
	// The check is per claim rather than per owner: the candidate's own pending claim must not
	// count against it, while a claim of another request from the same owner (an upgrade the
	// owner still holds) must.
	if e.hasBlockingClaims(candidate) {
		return false
	}
	if e.waitersAhead(candidate, candidate.ref) {
		candidate.bundle.blockedByOlderWaiter = true
		return false
	}
	if e.waitsBehindOwner(candidate) {
		return false
	}
	return true
}

// lockTable maps a resource identity to its lock state.
//
// Addresses are stable: an entry is created on first use and removed as soon as it has
// neither claim nor waiter, so an unrelated row never sees a lock operation on another row.
// That per-resource isolation is what keeps the manager from becoming a global transaction
// gate.
type lockTable struct {
	entries map[string]*lockEntry
}

func newLockTable() *lockTable { return &lockTable{entries: make(map[string]*lockEntry)} }

func (t *lockTable) get(key string) (*lockEntry, bool) {
	entry, ok := t.entries[key]
	return entry, ok
}

// entryFor returns the entry for one resource, creating it when absent.
func (t *lockTable) entryFor(ref ResourceRef) *lockEntry {
	key := ref.ResourceKey()
	if entry, ok := t.entries[key]; ok {
		return entry
	}
	entry := newLockEntry(ref)
	t.entries[key] = entry
	return entry
}

// release removes an entry that has no claim and no waiter.
//
// Dropping the empty entry is the cleanup half of the memory budget: without it a table
// would grow with every key ever touched, which is exactly the unbounded growth the
// contract's budget forbids.
func (t *lockTable) release(key string, entry *lockEntry) {
	if entry == nil {
		return
	}
	if len(entry.claims) == 0 && len(entry.waiting) == 0 {
		delete(t.entries, key)
	}
}
