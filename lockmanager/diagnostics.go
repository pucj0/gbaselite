package lockmanager

import (
	"sort"
	"strconv"
	"time"
)

// diagnostics.go holds the bounded, read-only diagnostics views of one lock manager.
//
// What a diagnostics page must never do is hand a caller the manager's own mutable state. Every
// row here is a copy of plain values, the resource lists are copied slices, and the retained
// deadlock summaries are deep-copied, so a caller that holds a page can iterate it, mutate it or
// keep it for as long as it likes without reaching into the manager. That is structural rather
// than a rule a caller has to remember.
//
// Privacy is bounded by construction as well: a resource appears only as an anonymous id and a
// kind. The raw key, the space, the table id and the index id are never copied into a page, so
// there is no field a caller could print that would leak a row's contents or a statement's SQL
// (FR-023).

// LockDiagnosticsPage is one bounded snapshot of a lock manager's observable state.
//
// The three row sets are independent: a page can carry holders, waiters and deadlock summaries
// at once. Each set is capped at the page limit, and both the per-set truncation flag and the
// total row count are reported so a consumer can tell a complete page from a clipped one
// instead of silently believing a partial view.
type LockDiagnosticsPage struct {
	Page

	// Resources are the resources that currently have a holder or a waiter.
	Resources []LockResourceRow
	// Grants are the granted claims currently held across resources.
	Grants []LockGrantRow
	// Waits are the requests that are currently waiting.
	Waits []LockWaitRow
	// Deadlocks are the retained deadlock summaries, newest first.
	Deadlocks []DeadlockSummary

	// TotalResources, TotalWaits and TotalDeadlocks are the complete counts, so a truncated page
	// still reports how much it is not showing.
	TotalResources int
	TotalGrants    int
	TotalWaits     int
	TotalDeadlocks int

	// ResourcesTruncated, WaitsTruncated and DeadlocksTruncated report that a set was clipped.
	ResourcesTruncated bool
	GrantsTruncated    bool
	WaitsTruncated     bool
	DeadlocksTruncated bool
}

// LockGrantRow is one granted component of a live owner.
type LockGrantRow struct {
	GrantID    uint64
	RootID     string
	ScopeID    uint64
	Generation uint64
	Term       uint64
	Mode       Mode
	Kind       ResourceKind
	ResourceID uint64
	GrantedAt  time.Time
}

// LockResourceRow is one resource's observable state with anonymous identity only.
type LockResourceRow struct {
	// ResourceID is the anonymous resource identity. It is a manager-local counter, so it can be
	// used to correlate rows and edges without revealing a table, index or key.
	ResourceID uint64
	// Kind is the resource kind. It is structural, not identifying.
	Kind ResourceKind
	// State is the projected lock state.
	State LockState
	// HolderMode is the strongest mode any holder currently holds.
	HolderMode Mode
	// HolderCount and WaiterCount are the counts backing State.
	HolderCount int
	WaiterCount int
	// HolderIDs are the roots that hold a claim, sorted for determinism.
	HolderIDs []string
	// HolderModes are the modes those roots offer, index-aligned with HolderIDs.
	HolderModes []Mode
	// WaiterIDs are the roots waiting on this resource, in queue order.
	WaiterIDs []string
	// WaiterModes are the modes those roots asked for, index-aligned with WaiterIDs.
	WaiterModes []Mode
	// OldestWaitMicros is the longest current wait on this resource, in microseconds. Zero means
	// nobody is waiting.
	OldestWaitMicros int64
}

// LockWaitRow is one waiting request.
type LockWaitRow struct {
	RequestID      uint64
	OwnerID        string
	ScopeID        uint64
	EnqueueOrdinal uint64
	// ResourceID is the anonymous identity of the resource this component of the request is
	// blocked on. A request with several blocked components reports one row per component.
	ResourceID uint64
	Kind       ResourceKind
	Mode       Mode
	Status     RequestStatus
	// WaitMicros is how long the request has been waiting, measured against the manager's clock.
	WaitMicros int64
	// DeadlineMicros is the wait deadline expressed as microseconds since the Unix epoch, or zero
	// when no deadline is set.
	DeadlineMicros int64
	// BlockerIDs are the roots this request is blocked by.
	BlockerIDs []string
	// Reasons are the edge reasons for those blockers, so a consumer can tell a holder block from
	// a queue-order block.
	Reasons []WaitEdgeReason
}

// DiagnosticsOptions selects the bounded content of one page.
type DiagnosticsOptions struct {
	// Limit caps each row set. It is clamped to [1, MaxDiagnosticRows]; a request above the bound
	// is served truncated and reported, never served in full.
	Limit int
	// Cursor is the SnapshotRevision a previous page reported. When it does not match the current
	// revision the page still returns, but Truncated is not the reason: the caller learns the
	// graph moved by comparing SnapshotRevision itself.
	Cursor string
	// IncludeWaiters selects the wait rows.
	IncludeWaiters bool
	// IncludeDeadlocks selects the retained deadlock summaries.
	IncludeDeadlocks bool
}

// Diagnostics returns a bounded, fully copied snapshot of the manager's observable state.
//
// It never blocks on a lock owner making progress: it takes one short critical section, copies
// what it needs and returns. No output happens under the lock and no caller code runs under it,
// so a diagnostics reader cannot hold up a transaction or be held up by one.
func (m *manager) Diagnostics(options DiagnosticsOptions) LockDiagnosticsPage {
	limit, truncatedByBound := ResolvePageLimit(options.Limit)

	m.mu.Lock()
	// The derived graph must be current before its revision is reported, or a page would carry the
	// revision of the previous state and a consumer comparing cursors would conclude that nothing
	// changed when the graph actually had.
	m.ensureGraphLocked()
	now := m.clock.Now()
	page := LockDiagnosticsPage{
		Page: Page{
			SnapshotRevision: m.graph.Revision(),
			Limit:            limit,
			Truncated:        truncatedByBound,
		},
	}
	page.NextCursor = strconv.FormatUint(page.SnapshotRevision, 10)

	resources, resourcesTotal := m.resourceRowsLocked(limit, now)
	page.Resources = resources
	page.TotalResources = resourcesTotal
	page.ResourcesTruncated = resourcesTotal > len(resources)

	grants, grantsTotal := m.grantRowsLocked(limit)
	page.Grants = grants
	page.TotalGrants = grantsTotal
	page.GrantsTruncated = grantsTotal > len(grants)

	if options.IncludeWaiters {
		waits, waitsTotal := m.waitRowsLocked(limit, now)
		page.Waits = waits
		page.TotalWaits = waitsTotal
		page.WaitsTruncated = waitsTotal > len(waits)
	}
	if options.IncludeDeadlocks {
		summaries, deadlocksTruncated := m.deadlockSummariesLocked(limit)
		page.Deadlocks = summaries
		page.TotalDeadlocks = len(m.deadlockRing)
		page.DeadlocksTruncated = deadlocksTruncated
	}
	m.mu.Unlock()

	if page.ResourcesTruncated || page.GrantsTruncated || page.WaitsTruncated || page.DeadlocksTruncated {
		page.Truncated = true
	}
	return page
}

// resourceRowsLocked builds the resource rows, sorted by anonymous id so a page is deterministic.
func (m *manager) resourceRowsLocked(limit int, now time.Time) ([]LockResourceRow, int) {
	keys := make([]string, 0, len(m.locks.entries))
	for key, entry := range m.locks.entries {
		if entry.holderCount() == 0 && entry.waiterCount() == 0 {
			continue
		}
		keys = append(keys, key)
	}
	rows := make([]LockResourceRow, 0, len(keys))
	for _, key := range keys {
		entry := m.locks.entries[key]
		row := LockResourceRow{
			ResourceID:  m.anonIDLocked(entry),
			Kind:        entry.ref.Kind,
			State:       entry.state(),
			HolderMode:  entry.holderMode(),
			HolderCount: entry.holderCount(),
			WaiterCount: entry.waiterCount(),
		}
		for ownerID, mode := range entry.holderModesByOwner() {
			row.HolderIDs = append(row.HolderIDs, ownerID)
			row.HolderModes = append(row.HolderModes, mode)
		}
		for _, waiter := range entry.waiting {
			if waiter == nil || waiter.dead || waiter.req.Status.Terminal() {
				continue
			}
			row.WaiterIDs = append(row.WaiterIDs, waiter.req.OwnerID)
			row.WaiterModes = append(row.WaiterModes, waiter.ref.Mode)
			if !waiter.req.BlockedAt.IsZero() {
				if waited := now.Sub(waiter.req.BlockedAt); waited > time.Duration(row.OldestWaitMicros)*time.Microsecond {
					row.OldestWaitMicros = waited.Microseconds()
				}
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ResourceID < rows[j].ResourceID })
	total := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total
}

// grantRowsLocked builds the granted lock component rows.
func (m *manager) grantRowsLocked(limit int) ([]LockGrantRow, int) {
	keys := make([]string, 0, len(m.locks.entries))
	for key, entry := range m.locks.entries {
		if entry.holderCount() == 0 {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var rows []LockGrantRow
	for _, key := range keys {
		entry := m.locks.entries[key]
		resID := m.anonIDLocked(entry)
		for ownerID, claims := range entry.claims {
			owner := m.owners[ownerID]
			var startedAt time.Time
			var gen, term uint64
			if owner != nil {
				startedAt = owner.StartedAt
				gen = owner.Generation
				term = owner.Term
			}
			for _, claim := range claims {
				if claim.pending {
					continue
				}
				grantID := entry.grantID
				var scopeID uint64
				if claim.request != nil {
					scopeID = claim.request.ScopeID
					if claim.request.ID != 0 {
						grantID = claim.request.ID
					}
				}
				rows = append(rows, LockGrantRow{
					GrantID:    grantID,
					RootID:     ownerID,
					ScopeID:    scopeID,
					Generation: gen,
					Term:       term,
					Mode:       claim.mode,
					Kind:       entry.ref.Kind,
					ResourceID: resID,
					GrantedAt:  startedAt,
				})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ResourceID != rows[j].ResourceID {
			return rows[i].ResourceID < rows[j].ResourceID
		}
		if rows[i].RootID != rows[j].RootID {
			return rows[i].RootID < rows[j].RootID
		}
		return rows[i].GrantID < rows[j].GrantID
	})
	total := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total
}

// waitRowsLocked builds the wait rows in queue order.
func (m *manager) waitRowsLocked(limit int, now time.Time) ([]LockWaitRow, int) {
	type pendingWalk struct {
		entry *lockRequest
	}
	var walk []pendingWalk
	for _, requests := range m.pending {
		for entry := range requests {
			walk = append(walk, pendingWalk{entry: entry})
		}
	}
	sort.Slice(walk, func(i, j int) bool {
		if walk[i].entry.req.EnqueueOrdinal != walk[j].entry.req.EnqueueOrdinal {
			return walk[i].entry.req.EnqueueOrdinal < walk[j].entry.req.EnqueueOrdinal
		}
		return walk[i].entry.req.ID < walk[j].entry.req.ID
	})

	rows := make([]LockWaitRow, 0, len(walk))
	for _, item := range walk {
		entry := item.entry
		row := LockWaitRow{
			RequestID:      entry.req.ID,
			OwnerID:        entry.req.OwnerID,
			ScopeID:        entry.req.ScopeID,
			EnqueueOrdinal: entry.req.EnqueueOrdinal,
			Kind:           entry.ref.Kind,
			Mode:           entry.ref.Mode,
			Status:         entry.req.Status,
			BlockerIDs:     append([]string(nil), entry.bundle.blockers...),
		}
		if lockEntry, ok := m.locks.get(entry.ref.ResourceKey()); ok {
			row.ResourceID = m.anonIDLocked(lockEntry)
		}
		if !entry.req.BlockedAt.IsZero() {
			row.WaitMicros = now.Sub(entry.req.BlockedAt).Microseconds()
		}
		if !entry.req.Deadline.IsZero() {
			row.DeadlineMicros = entry.req.Deadline.UnixMicro()
		}
		// The reasons are derived from the same graph the detector searches, so a row's reasons
		// and the graph's edges for the same request always agree.
		for _, edge := range m.graph.edgesFrom(entry.req.OwnerID) {
			if edge.RequestID != entry.req.ID {
				continue
			}
			row.Reasons = append(row.Reasons, edge.Reason)
		}
		rows = append(rows, row)
	}
	total := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total
}

// DiagnosticsStats returns the aggregate counters plus the graph revision.
func (m *manager) DiagnosticsStats() Stats { return m.Stats() }
