package lockmanager

import "sync"

// budget.go holds the single accounting ledger of one lock manager.
//
// Every lock resource the manager retains is charged exactly once, by the ledger,
// before any manager state changes. That ordering is what makes a rejected request
// side-effect free: nothing is granted, nothing is queued and nothing is charged.
//
// The ledger never releases a grant to stay inside a budget. When a budget is
// exhausted the request fails closed, because trading a held lock for a smaller
// bookkeeping footprint would silently reduce the protection the caller already
// holds.
//
// Accounting model. An owner pins every identity it holds, granted or reserved, in
// one map, so a resource is counted once no matter how many requests name it. A
// reservation remembers which identities it introduced, which is what lets a release
// remove exactly its own contribution.
//
//	ownerCharge.resources      every identity the owner pins
//	ownerCharge.resourceBytes  owned bytes of the identities that are still reserved
//	ownerCharge.fixedBytes     scope and summary state the owner retains
//	ownerCharge.grantedBytes   owned bytes of the identities that became grants
//	Reservation.newResources   the identities this reservation introduced
//
//	memory = Σ owner(ownerFixedBytes + resourceBytes + fixedBytes + grantedBytes
//	                 + grants*grantFixedBytes)
//	       + Σ live reservation(waiterFixedBytes)
//	       + engineFixed
//	       + edges*edgeFixedBytes
//
// recompute rebuilds exactly that formula, and the budget test asserts the
// incremental counter agrees with it and that a full release returns zero.

// Reservation is one live charge.
//
// It is either committed to its owner's granted set or released; committing or
// releasing twice is a no-op, so a double cleanup cannot underflow a counter. A
// committed reservation gives up its identities to the owner's granted set, which is
// why a release after a commit refunds nothing.
type Reservation struct {
	token     uint64
	ownerID   string
	waiter    bool
	resources []ResourceRef
	// newResources are the identities this reservation introduced into its owner's
	// map. Only these may be removed on release.
	newResources []ResourceRef
	// newResourceBytes is the owned size of newResources.
	newResourceBytes int64
	// reservedBytes is the bytes this reservation charged, which is the size of every
	// component it listed, whether the owner already pinned it or not.
	reservedBytes int64
	committed     bool
	released      bool
}

// Token reports the reservation identity.
func (r *Reservation) Token() uint64 { return r.token }

// Components reports how many components this reservation charged.
func (r *Reservation) Components() int64 { return int64(len(r.resources)) }

// Bytes reports how many resource bytes this reservation charged.
func (r *Reservation) Bytes() int64 { return r.reservedBytes }

// OwnerID reports the owner this reservation was charged to.
func (r *Reservation) OwnerID() string { return r.ownerID }

// Waiter reports whether this reservation is counted against the waiter budget.
func (r *Reservation) Waiter() bool { return r.waiter }

// Ledger is the bounded accounting of one lock manager.
//
// It is safe for concurrent use. Every method takes one short critical section and
// never calls back into a manager, a transaction or storage, so it can be used from
// a detection or diagnostics goroutine too.
type Ledger struct {
	mu     sync.RWMutex
	limits BudgetLimits
	next   uint64

	grants      int64
	edges       int64
	memory      int64
	engineFixed int64

	owners map[string]*ownerCharge
	live   map[uint64]*Reservation

	rejections int64
}

type ownerCharge struct {
	// resources holds every distinct identity the owner pins, granted or reserved.
	resources map[string]ResourceRef
	// resourceBytes is the incremental sum of owned bytes for the identities that are
	// still only reserved. Committing an identity moves its bytes to grantedBytes, so
	// the owner's total never counts the same identity twice.
	resourceBytes int64
	// fixedBytes is state the owner retains outside a resource list: one scope, one
	// recorded summary. It is refunded when the owner entry is released.
	fixedBytes   int64
	grantedSet   map[string]bool
	grantedBytes int64
	grants       int64
}

// trackedBytes reports everything the owner contributes to the memory accounting.
func (c *ownerCharge) trackedBytes() int64 {
	return ownerFixedBytes + c.resourceBytes + c.fixedBytes + c.grantedBytes + c.grants*grantFixedBytes
}

// NewLedger returns a ledger with resolved limits. A zero limit means its default.
func NewLedger(limits BudgetLimits) (*Ledger, error) {
	resolved, err := limits.Resolve()
	if err != nil {
		return nil, err
	}
	return &Ledger{limits: resolved, owners: make(map[string]*ownerCharge), live: make(map[uint64]*Reservation)}, nil
}

// Limits returns the resolved limits.
func (l *Ledger) Limits() BudgetLimits { return l.limits }

// Reserve charges one atomic component bundle for an owner.
//
// The check is all-or-nothing: the memory, per-root and waiter budgets are evaluated
// together, and a failure charges nothing. waiter asks for the pending-request
// accounting as well, which is what the engine waiter budget bounds.
func (l *Ledger) Reserve(ownerID string, components []ResourceRef, waiter bool) (*Reservation, error) {
	if ownerID == "" {
		return nil, ErrOwnerUnknown
	}
	reservedBytes := RefBytes(components)
	count := int64(len(components))
	waiterBytes := int64(0)
	if waiter {
		waiterBytes = waiterFixedBytes
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.next == ^uint64(0) {
		l.rejections++
		return nil, ErrOrdinalExhausted
	}
	charge, exists := l.owners[ownerID]
	if charge == nil {
		charge = newOwnerCharge()
	}
	// The identities this reservation introduces; the rest of the bundle is already
	// pinned by the owner and must not be charged to memory twice.
	var introduced []ResourceRef
	var introducedBytes int64
	seen := make(map[string]bool, len(components))
	for _, component := range components {
		identity := component.Identity
		key := identity.ClaimKey()
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, exists := charge.resources[key]; exists {
			continue
		}
		introduced = append(introduced, ResourceRef{Identity: identity.Clone()})
		introducedBytes += component.OwnedBytes64()
	}
	delta := introducedBytes + waiterBytes
	if !exists {
		delta += ownerFixedBytes
	}
	if l.memoryUsage()+delta > l.limits.MemoryBytes {
		l.rejections++
		return nil, ErrBudgetMemory
	}
	if l.ownerComponentsLocked(ownerID)+count > l.limits.RootRequests {
		l.rejections++
		return nil, ErrBudgetRootRequests
	}
	if waiter && l.pendingLocked()+1 > l.limits.Waiters {
		l.rejections++
		return nil, ErrBudgetWaiters
	}
	l.next++
	reservation := &Reservation{
		token:            l.next,
		ownerID:          ownerID,
		waiter:           waiter,
		resources:        CloneResourceRefs(components),
		newResources:     introduced,
		newResourceBytes: introducedBytes,
		reservedBytes:    reservedBytes,
	}
	if len(introduced) == 0 && !waiter {
		// The owner already pins every component of this bundle and nothing has to
		// wait, so this request is satisfied by the existing protection: it holds no
		// new state and must be charged nothing. Charging it would make re-entrant
		// requests from one root shrink the budget for no reason.
		reservation.committed = true
		return reservation, nil
	}
	for _, component := range introduced {
		charge.resources[component.ClaimKey()] = component
	}
	charge.resourceBytes += introducedBytes
	l.owners[ownerID] = charge
	l.live[reservation.token] = reservation
	l.recompute()
	return reservation, nil
}

// Commit turns a pending reservation into granted accounting.
//
// The transition moves a charge from "reserved" to "granted": the identity's bytes were
// already charged at reservation time, so only the one-time grant overhead is new. A
// committed reservation gives up its identities, so a later Release refunds nothing.
func (l *Ledger) Commit(reservation *Reservation) error {
	if reservation == nil {
		return ErrOwnerUnknown
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if reservation.released || reservation.committed {
		return nil
	}
	charge := l.owners[reservation.ownerID]
	if charge == nil {
		// The owner was released while the reservation was pending, so committing it
		// must not silently resurrect an owner entry the caller already ended.
		reservation.committed = true
		delete(l.live, reservation.token)
		l.recompute()
		return nil
	}
	if l.memoryUsage()+grantFixedBytes > l.limits.MemoryBytes {
		// Fail closed and leave the reservation pending, so the caller still owns a
		// charge it can release.
		l.rejections++
		return ErrBudgetMemory
	}
	beforeGranted := charge.grantedBytes
	for _, component := range reservation.newResources {
		key := component.ClaimKey()
		if charge.grantedSet[key] {
			continue
		}
		charge.grantedSet[key] = true
		charge.grantedBytes += component.OwnedBytes64()
	}
	// The identity's bytes move from the reserved pool to the granted pool, so the total
	// is unchanged and only the one-time grant overhead is new. Taking the difference
	// rather than adding grantedDelta keeps a reservation that repeated an already-pinned
	// identity from charging its bytes twice.
	if grantedDelta := charge.grantedBytes - beforeGranted; grantedDelta != 0 {
		charge.resourceBytes -= grantedDelta
	}
	charge.grants++
	if reservation.waiter {
		reservation.waiter = false
	}
	reservation.committed = true
	delete(l.live, reservation.token)
	l.recompute()
	return nil
}

// Release refunds a reservation that never became a grant.
func (l *Ledger) Release(reservation *Reservation) {
	if reservation == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if reservation.released || reservation.committed {
		return
	}
	reservation.released = true
	delete(l.live, reservation.token)
	charge := l.owners[reservation.ownerID]
	if charge != nil {
		for _, component := range reservation.newResources {
			key := component.ClaimKey()
			if charge.grantedSet[key] {
				// A committed identity is owned by the granted set, which a release of
				// an uncommitted reservation can never hold.
				continue
			}
			if _, exists := charge.resources[key]; exists {
				delete(charge.resources, key)
				charge.resourceBytes -= component.OwnedBytes64()
			}
		}
	}
	if reservation.waiter {
		reservation.waiter = false
	}
	l.recompute()
}

// ReleaseOwner refunds every charge of one owner and forgets its resources. It is
// idempotent, which is what makes a double root cleanup harmless.
func (l *Ledger) ReleaseOwner(ownerID string) BudgetUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	if charge := l.owners[ownerID]; charge != nil {
		l.grants -= charge.grants
		delete(l.owners, ownerID)
	}
	for token, reservation := range l.live {
		if reservation.ownerID != ownerID {
			continue
		}
		delete(l.live, token)
		reservation.released = true
	}
	l.recompute()
	return l.totalsLocked()
}

// ClaimEdge charges one wait-graph edge.
func (l *Ledger) ClaimEdge() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.edges+1 > l.limits.GraphWork {
		l.rejections++
		return ErrBudgetGraph
	}
	if l.memoryUsage()+edgeFixedBytes > l.limits.MemoryBytes {
		l.rejections++
		return ErrBudgetMemory
	}
	l.edges++
	l.recompute()
	return nil
}

// ReleaseEdge refunds one wait-graph edge. It is a no-op at zero, so a repeated
// release cannot underflow the counter.
func (l *Ledger) ReleaseEdge() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.edges <= 0 {
		return
	}
	l.edges--
	l.recompute()
}

// ChargeFixed adds one fixed component to the memory accounting, for state the
// manager retains outside a resource list (a scope, a recorded summary). It returns
// ErrBudgetMemory and changes nothing when the budget is full.
//
// A charge for a named owner is tracked as that owner's fixed bytes, so releasing the
// owner refunds it together with the owner's own overhead; a charge with an empty
// ownerID is engine-wide state that belongs to no owner and is refunded explicitly by
// ReleaseFixed.
func (l *Ledger) ChargeFixed(bytes int64, ownerID string) error {
	if bytes <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delta := bytes
	if ownerID != "" && l.owners[ownerID] == nil {
		// The owner entry does not exist yet, so this charge creates it and its own
		// overhead is charged here exactly once.
		delta += ownerFixedBytes
	}
	if l.memoryUsage()+delta > l.limits.MemoryBytes {
		l.rejections++
		return ErrBudgetMemory
	}
	if ownerID == "" {
		l.engineFixed += bytes
		l.recompute()
		return nil
	}
	charge := l.owners[ownerID]
	if charge == nil {
		charge = newOwnerCharge()
		l.owners[ownerID] = charge
	}
	charge.fixedBytes += bytes
	l.recompute()
	return nil
}

// ReleaseFixed refunds one engine-wide fixed charge, bounded at zero. A charge that
// belongs to an owner is refunded by releasing that owner.
func (l *Ledger) ReleaseFixed(bytes int64, ownerID string) {
	if bytes <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if ownerID == "" {
		l.engineFixed -= bytes
		l.recompute()
	}
}

// Resources returns a copy of the identities an owner currently holds, granted plus
// reserved, keyed by canonical resource key. The copy owns its bytes, so a caller
// cannot mutate the ledger through it.
func (l *Ledger) Resources(ownerID string) map[string]ResourceRef {
	l.mu.RLock()
	defer l.mu.RUnlock()
	charge := l.owners[ownerID]
	if charge == nil {
		return nil
	}
	out := make(map[string]ResourceRef, len(charge.resources))
	for key, resource := range charge.resources {
		out[key] = ResourceRef{Identity: resource.Identity.Clone()}
	}
	return out
}

// GrantedResources returns a copy of the identities one owner was actually granted.
//
// It is the re-entrancy view: a request whose whole bundle already appears here needs no
// new protection and no new charge, and a request for a resource that is only *reserved*
// (by a pending request of the same owner) must not be treated as already held.
func (l *Ledger) GrantedResources(ownerID string) map[string]ResourceRef {
	l.mu.RLock()
	defer l.mu.RUnlock()
	charge := l.owners[ownerID]
	if charge == nil {
		return nil
	}
	out := make(map[string]ResourceRef, len(charge.grantedSet))
	for key := range charge.grantedSet {
		resource, ok := charge.resources[key]
		if !ok {
			continue
		}
		out[key] = ResourceRef{Identity: resource.Identity.Clone()}
	}
	return out
}

// ReleaseGrant refunds one owner's grant on one resource.
//
// It is the single-lock release path, and it is deliberately narrower than Release: it
// removes one identity from the owner's granted set and from the owner's pinned set, so
// releasing a lock cannot refund a reservation the owner is still waiting on for another
// resource. A repeated release, or a release of an identity the owner does not hold, is
// a no-op.
func (l *Ledger) ReleaseGrant(ownerID string, resourceKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	charge := l.owners[ownerID]
	if charge == nil {
		return
	}
	if !charge.grantedSet[resourceKey] {
		return
	}
	delete(charge.grantedSet, resourceKey)
	if resource, ok := charge.resources[resourceKey]; ok {
		delete(charge.resources, resourceKey)
		bytes := resource.OwnedBytes64()
		if charge.grantedBytes >= bytes {
			charge.grantedBytes -= bytes
		} else {
			charge.grantedBytes = 0
		}
	}
	if charge.grants > 0 {
		charge.grants--
	}
	l.recompute()
}

// Usage returns the accounting of one owner plus the ledger-wide total.
func (l *Ledger) Usage(ownerID string) (BudgetUsage, BudgetUsage) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.usageLocked(ownerID), l.totalsLocked()
}

func (l *Ledger) usageLocked(ownerID string) BudgetUsage {
	charge := l.owners[ownerID]
	if charge == nil {
		return BudgetUsage{}
	}
	return BudgetUsage{
		Components:  l.ownerComponentsLocked(ownerID),
		MemoryBytes: charge.trackedBytes(),
	}
}

// Totals returns the ledger-wide accounting.
func (l *Ledger) Totals() BudgetUsage {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.totalsLocked()
}

func (l *Ledger) totalsLocked() BudgetUsage {
	total := BudgetUsage{MemoryBytes: l.memory, Waiters: l.pendingLocked(), GraphEdges: l.edges}
	for ownerID := range l.owners {
		total.Components += l.ownerComponentsLocked(ownerID)
	}
	return total
}

// ownerComponentsLocked counts the components one owner holds: the distinct
// identities it pins plus the extra components its live reservations named. A pending
// request that repeats an already-pinned identity therefore charges one more
// component, which is what makes the per-root budget bound components rather than
// distinct keys.
func (l *Ledger) ownerComponentsLocked(ownerID string) int64 {
	charge := l.owners[ownerID]
	if charge == nil {
		return 0
	}
	return int64(len(charge.resources)) + l.pendingExtraForLocked(ownerID)
}

// pendingExtraForLocked counts the components of an owner's live reservations that the
// owner's resource map does not already contain. It is zero by construction after
// Reserve, and it exists so the accounting can prove that.
func (l *Ledger) pendingExtraForLocked(ownerID string) int64 {
	charge := l.owners[ownerID]
	if charge == nil {
		return 0
	}
	var extra int64
	seen := make(map[string]bool, len(charge.resources))
	for key := range charge.resources {
		seen[key] = true
	}
	for _, reservation := range l.live {
		if reservation.ownerID != ownerID {
			continue
		}
		for _, component := range reservation.resources {
			key := component.ClaimKey()
			if seen[key] {
				continue
			}
			seen[key] = true
			extra++
		}
	}
	return extra
}

func (l *Ledger) pendingLocked() int64 {
	var count int64
	for _, reservation := range l.live {
		if reservation.waiter {
			count++
		}
	}
	return count
}

// GrantedComponents reports how many distinct identities one owner was granted.
func (l *Ledger) GrantedComponents(ownerID string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	charge := l.owners[ownerID]
	if charge == nil {
		return 0
	}
	return int64(len(charge.grantedSet))
}

// Waiters reports the current pending-request count.
func (l *Ledger) Waiters() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.pendingLocked()
}

// Edges reports the current wait-edge count.
func (l *Ledger) Edges() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.edges
}

// Grants reports the current granted-bundle count.
func (l *Ledger) Grants() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.grants
}

// Rejections reports how many requests a budget refused.
func (l *Ledger) Rejections() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.rejections
}

// recompute is the single writer of the memory and grant totals.
//
// There is deliberately no parallel incremental counter to drift away from it: the
// total is derived from the tracked collections after every mutation, so "the budget
// check" and "the reported usage" can never disagree. The collections are bounded by
// the budgets themselves, which keeps the derivation cheap.
//
// It also prunes owner entries that no longer contribute anything, so a released owner
// cannot keep charging its own overhead, and it clamps every counter at zero, so a
// repeated release can never surface as a negative usage.
func (l *Ledger) recompute() {
	var memory int64
	var grants int64
	for ownerID, charge := range l.owners {
		if len(charge.resources) == 0 && charge.grants == 0 && charge.fixedBytes == 0 {
			delete(l.owners, ownerID)
			continue
		}
		memory += charge.trackedBytes()
		grants += charge.grants
	}
	for _, reservation := range l.live {
		if reservation.released || reservation.committed {
			continue
		}
		// A reservation's identities are already tracked by its owner's resourceBytes,
		// so only the pending-request overhead is charged here.
		memory += waiterFixedBytes
	}
	memory += l.engineFixed
	memory += l.edges * edgeFixedBytes
	if memory < 0 {
		memory = 0
	}
	l.memory = memory
	l.grants = grants
}

// memoryUsage reports the current tracked memory.
//
// Callers must hold the ledger lock. It reads the value recompute produced.
func (l *Ledger) memoryUsage() int64 { return l.memory }

// ReleaseAll refunds every charge of every owner. It is the closed-manager path, and
// it exists so a closed manager reports exactly zero usage.
func (l *Ledger) ReleaseAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owners = make(map[string]*ownerCharge)
	l.live = make(map[uint64]*Reservation)
	l.grants = 0
	l.edges = 0
	l.memory = 0
	l.engineFixed = 0
}

func newOwnerCharge() *ownerCharge {
	return &ownerCharge{resources: make(map[string]ResourceRef), grantedSet: make(map[string]bool)}
}
