package executor

import (
	"context"
	"fmt"

	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// locking_read.go serves a locking read (`SELECT ... FOR UPDATE`, `FOR SHARE`, `LOCK IN SHARE MODE`)
// from the current state of the rows it protects, instead of from the statement snapshot.
//
// The pipeline around it does not change. `bindPhysicalSelect` builds the same projection, WHERE,
// ORDER BY and LIMIT over a *different source*: the source here claims the rows it visits and reads
// their latest published version under the claim. That is what makes the WHERE clause re-evaluated on
// current data — the filter sits above this source, so it sees the re-read row — and what makes a row
// that was deleted or that no longer matches disappear from the result rather than being returned as
// the snapshot saw it. A plain `SELECT` never reaches this file.
//
// What it deliberately does not do: it never falls back to a snapshot read. A backend without the
// locking capability, an access path whose protection cannot be derived, and a statement shape whose
// locking semantics are not implemented all fail closed, because a caller that asked for a lock and
// silently received none would decide on data nothing protected.

// errLockedReadUnavailable reports a locking read that cannot be served with the protection it asked
// for. It is a refusal, not a degradation: the statement does not run at all.
var errLockedReadUnavailable = fmt.Errorf("%w: this locking read cannot be served safely", errLockingUnavailable)

// errLockedReadShape reports a locking read on a statement shape whose locking semantics are not
// implemented yet.
var errLockedReadShape = fmt.Errorf("%w: this statement shape does not support row locking yet", storageengine.ErrUnsupported)

// lockedReadWaitHook, when set, is consulted the moment a locking read discovers it has to wait for a
// conflicting claim. It mirrors the mutation path's request hook: it turns "this statement is
// genuinely queued on a lock" into an observed fact a test can synchronise on, instead of a timing
// guess. A nil hook means "wait normally", which is what production does.
var lockedReadWaitHook func(space string) bool

// lockedReadWaitDecision is the ShouldWait the locking read passes with its request.
func lockedReadWaitDecision(space string) func(context.Context) bool {
	if lockedReadWaitHook == nil {
		return nil
	}
	return func(context.Context) bool { return lockedReadWaitHook(space) }
}

// readSourceOperator returns the row source of one single-table SELECT.
//
// A plain read keeps the snapshot scan it always had; a locking read is served by the locked path,
// which either protects what it reads or returns an error.
func readSourceOperator(ctx context.Context, tx storageengine.Txn, session *Session, statement parser.Select, table versionedTable, plan sqlAccessPlan, decode func([]byte) (storage.Row, error)) (physical.Operator[storage.Row], error) {
	if session.EffectiveIsolation() == parser.LevelSerializable && (session.InTransaction() || session.AutocommitDisabled) {
		statement.Locking = true
		statement.LockMode = parser.LockModeShare
	}
	if !statement.Locking {
		return bindScan(tx, table, plan, decode), nil
	}
	return bindLockedRead(ctx, tx, session, statement, table, plan, decode)
}

// validateLockedReadShape refuses a locking read on a shape this release cannot protect.
//
// These are exactly the shapes whose row source is *not* one table scan: a joined, derived, CTE,
// view-backed or aggregated relation is materialized by other code that reads the snapshot, so a
// locking read over one would claim to protect rows it never claimed. Refusing is the only outcome
// that does not misrepresent the protection; teaching those paths to lock is separate work.
func validateLockedReadShape(statement parser.Select) error {
	switch {
	case statement.Table == "":
		return fmt.Errorf("%w: a locking read needs a table", errLockedReadShape)
	case len(statement.Joins) > 0:
		return fmt.Errorf("%w: joins", errLockedReadShape)
	case statement.Subquery != nil:
		return fmt.Errorf("%w: derived tables", errLockedReadShape)
	case len(statement.GroupBy) > 0, statement.Having != nil, statement.Distinct:
		return fmt.Errorf("%w: aggregation or DISTINCT", errLockedReadShape)
	case selectHasAggregate(statement.Items):
		return fmt.Errorf("%w: aggregation", errLockedReadShape)
	case expressionHasSubquery(statement.Where):
		return fmt.Errorf("%w: subqueries", errLockedReadShape)
	}
	return nil
}

// bindLockedRead builds the locked row source of one single-table SELECT.
func bindLockedRead(ctx context.Context, tx storageengine.Txn, session *Session, statement parser.Select, table versionedTable, plan sqlAccessPlan, decode func([]byte) (storage.Row, error)) (physical.Operator[storage.Row], error) {
	if err := validateLockedReadShape(statement); err != nil {
		return nil, err
	}
	if relation, ok := cteFor(session, statement.Table); ok {
		_ = relation
		return nil, fmt.Errorf("%w: common table expressions", errLockedReadShape)
	}
	if _, _, isView, err := viewRelation(ctx, tx, session, statement.Table, statement.TableAlias); err != nil {
		return nil, err
	} else if isView {
		return nil, fmt.Errorf("%w: views", errLockedReadShape)
	}
	mode := planSQLLockingMode(true, statement.LockMode)
	lockPlan, err := planSQLLocking(plan, mode)
	if err != nil {
		return nil, err
	}
	if err := lockedReadIndexGuard(plan); err != nil {
		return nil, err
	}
	// The planner leaves the namespace of a primary-key range implicit: the rows it reads live in the
	// rows namespace, which only the reader knows (the plan itself carries bounds and an index, not a
	// space). Its bounds are still proven, so resolving that here is what keeps a proven interval from
	// being downgraded into a whole-namespace fallback — a downgrade that would serialize every insert
	// of the table behind one range read.
	if lockPlan.kind == lockPlanWholeDomain {
		if bounds, proven := provenPrimaryInterval(plan); proven {
			lockPlan.kind = lockPlanRange
			lockPlan.bounds = bounds
			lockPlan.reason = "proven primary-key interval; interval and visited keys"
		}
	}
	switch lockPlan.kind {
	case lockPlanNone:
		// A locking statement always produces a plan; reaching here would mean the mode was lost.
		return nil, fmt.Errorf("%w: no lock plan for a locking read", ErrLockPlanUnavailable)
	case lockPlanRefused:
		return nil, fmt.Errorf("%w: %s", ErrLockPlanUnavailable, lockPlan.reason)
	case lockPlanRecordOnly, lockPlanInsertion:
		// The point path below proves the row's own identity, which is what a record-only plan means.
	case lockPlanRange, lockPlanWholeDomain:
		// The scan path below protects an interval and every key it visits.
	default:
		return nil, fmt.Errorf("%w: unknown lock plan %q", ErrLockPlanUnavailable, lockPlan.kind)
	}

	locking, err := storageengine.LockingTxnOf(tx)
	if err != nil {
		// A backend without a lock manager must never serve a locking read from its snapshot.
		return nil, errLockedReadUnavailable
	}
	node := &physical.PlanNode{Kind: scanKind(plan), Attributes: map[string]string{
		"table": table.CatalogName, "access": plan.kind, "index": plan.index,
		"locks": lockPlan.Annotation(true),
	}}
	return physical.Scan[storage.Row]{Plan: node, Open: func(openCtx context.Context) (storageengine.Iterator, error) {
		// The scope is opened here, together with the source that owns it, and closed on every failure
		// below. Opening it while *binding* the statement instead would leak it whenever the statement
		// fails before this operator is opened — a projection error, a refused plan, a closed
		// transaction, a LIMIT that never pulls a row — because nothing would be left to close it.
		//
		// The session's lock-wait budget travels as the scope's per-wait budget, so each claim the
		// statement makes has its own bound while the statement's context still caps all of them.
		scope, err := locking.BeginLockScopeBudget(openCtx, storageengine.LockScopeStatement, session.LockWaitTimeout())
		if err != nil {
			return nil, err
		}
		it, err := openLockedSource(openCtx, locking, scope, table, plan, lockPlan, mode)
		if err != nil {
			closeLockScope(locking, scope)
			return nil, err
		}
		return &lockedSource{Iterator: it, locks: locking, scope: scope}, nil
	}, Decode: func(_, v []byte) (storage.Row, error) { return decode(v) }}, nil
}

// closeLockScope closes a statement scope whose source never came into existence.
//
// It reports nothing: the caller is already returning the error that made the source fail, and a scope
// that cannot be closed is a lock-manager invariant violation, not a second failure for the statement.
func closeLockScope(locking storageengine.LockingTxn, scope uint64) {
	// The context is deliberately fresh: closing a scope has to work even when the statement's own
	// context was cancelled or already past its deadline, or a cancelled statement would keep its
	// pending requests alive.
	_ = locking.CloseLockScope(context.Background(), scope)
}

// openLockedSource opens the byte-level source of one locked read.
func openLockedSource(ctx context.Context, locking storageengine.LockingTxn, scope uint64, table versionedTable, plan sqlAccessPlan, lockPlan sqlLockPlan, mode storageengine.LockMode) (storageengine.Iterator, error) {
	space := lockedReadSpace(table, plan, lockPlan)
	switch lockPlan.kind {
	case lockPlanWholeDomain, lockPlanRange:
		bounds := lockPlan.bounds
		return locking.NewCurrentIterator(ctx, scope, storageengine.CurrentScanRequest{
			Space: space,
			Range: bounds,
			Mode:  mode,
			Plan: storageengine.LockPlan{
				Kind:   string(lockPlan.kind),
				Index:  plan.index,
				Proven: lockPlan.kind == lockPlanRange,
				Components: []storageengine.LockComponent{
					readTableIntent(table, mode),
					intervalComponent(space, table.ID, plan.index, bounds),
				},
			},
		})
	case lockPlanRecordOnly, lockPlanInsertion:
		// A unique index access reaches its row through its entry, which is a different two-step path
		// from a primary-identity point read; the plan's access kind is what decides, not its lock kind.
		if plan.kind == sqlAccessUnique {
			return openLockedIndexPoint(ctx, locking, scope, table, plan, mode)
		}
		return openLockedPoint(ctx, locking, scope, table, plan, lockPlan, mode)
	}
	return nil, fmt.Errorf("%w: unknown lock plan %q", ErrLockPlanUnavailable, lockPlan.kind)
}

// openLockedPoint serves the point path: one row, reached by its own identity.
//
// The order matters. The row's record claim is taken *before* the row is read, so the value the
// statement returns is the version it holds the lock on; if the row is absent, the same claim plus the
// interval that contains its insertion position is what stops another transaction from inserting it
// while this statement reports it missing.
func openLockedPoint(ctx context.Context, locking storageengine.LockingTxn, scope uint64, table versionedTable, plan sqlAccessPlan, lockPlan sqlLockPlan, mode storageengine.LockMode) (storageengine.Iterator, error) {
	space := lockedReadSpace(table, plan, lockPlan)
	key := lockPlan.exact
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: the point plan carries no key", ErrLockPlanUnavailable)
	}
	components := []storageengine.LockComponent{readTableIntent(table, mode)}
	components = append(components, recordComponents(space, table.ID, plan.index, key, mode, false)...)
	grant, err := locking.Acquire(ctx, scope, storageengine.LockRequest{Components: components, ShouldWait: lockedReadWaitDecision(space)})
	if err != nil {
		return nil, err
	}
	observation, err := locking.ReadCurrent(ctx, grant, space, key)
	if err != nil {
		return nil, err
	}
	if observation.Exists {
		return &lockedRow{value: observation.Value}, nil
	}
	// The row is absent. Reading it under the claim already proves it was absent *while the claim was
	// held*, which is what the insert path conflicts with; the gap around the insertion position
	// additionally covers a row that arrives through it.
	if err := protectInsertionPosition(ctx, locking, scope, table, space, plan.index, key, mode); err != nil {
		return nil, err
	}
	// Re-read under the wider claim: a row that arrived between the two claims must be returned
	// rather than reported missing.
	observation, err = locking.ReadCurrent(ctx, grant, space, key)
	if err != nil {
		return nil, err
	}
	if !observation.Exists {
		return &lockedRow{}, nil
	}
	return &lockedRow{value: observation.Value}, nil
}

// lockedReadIndexGuard refuses the access paths whose protection is only half implemented.
//
// A secondary index access reaches its rows through an entry set that can hold several entries per
// value, and protecting it means enumerating that set from the latest published state and claiming
// every visited entry together with its common primary row. That path is not wired yet, and half of it
// is not an option: the contract requires a locking read to protect the index entry *and* the common
// row, so an unclaimed entry set would report rows nothing protected.
//
// A *unique* index access has exactly one entry per value, so it is served by openLockedIndexPoint
// below, which claims the value's reservation (the same resource the DML path reserves) and then the
// common primary row.
func lockedReadIndexGuard(plan sqlAccessPlan) error {
	if plan.kind == "ref" {
		return fmt.Errorf("%w: a locking read through a secondary index", errLockedReadShape)
	}
	return nil
}

// openLockedIndexPoint serves a locking read that reaches its row through a unique index.
//
// It mirrors the snapshot path's two steps (read the entry, then the row it owns) but claims each
// resource *before* reading it, and re-reads both under those claims:
//
//  1. the value's reservation — the record the write path takes to own that unique value, so the
//     read and the writer that would take the value away meet on exactly one resource;
//  2. the common primary row the entry names — `primaryRowComponent`'s resource, so two statements
//     that reach one row through different indexes still meet on the row.
//
// The entry's value is the owning primary key, which is the writer's own encoding; a row that was
// deleted between the two steps is reported as absent rather than returned from the entry's stale
// pointer.
func openLockedIndexPoint(ctx context.Context, locking storageengine.LockingTxn, scope uint64, table versionedTable, plan sqlAccessPlan, mode storageengine.LockMode) (storageengine.Iterator, error) {
	entrySpace, entryKey := plan.space, plan.key
	if entrySpace == "" || len(entryKey) == 0 {
		return nil, fmt.Errorf("%w: the unique index plan carries no entry", ErrLockPlanUnavailable)
	}
	entryGrant, err := openEntryClaim(ctx, locking, scope, table, entrySpace, plan.index, entryKey, mode)
	if err != nil {
		return nil, err
	}
	entry, err := locking.ReadCurrent(ctx, entryGrant, entrySpace, entryKey)
	if err != nil {
		return nil, err
	}
	if !entry.Exists {
		// No row owns the value. The reservation above already excludes a writer taking it while this
		// statement reports the value missing; the interval additionally covers the entry's position.
		if err := protectInsertionPosition(ctx, locking, scope, table, entrySpace, plan.index, entryKey, mode); err != nil {
			return nil, err
		}
		recheck, err := locking.ReadCurrent(ctx, entryGrant, entrySpace, entryKey)
		if err != nil {
			return nil, err
		}
		if !recheck.Exists {
			return &lockedRow{}, nil
		}
		entry = recheck
	}
	rowKey := append([]byte(nil), entry.Value...)
	rowSpace := sqllayout.Rows(table.ID)
	rowGrant, err := openEntryClaim(ctx, locking, scope, table, rowSpace, "", rowKey, mode)
	if err != nil {
		return nil, err
	}
	row, err := locking.ReadCurrent(ctx, rowGrant, rowSpace, rowKey)
	if err != nil {
		return nil, err
	}
	if !row.Exists {
		// The entry pointed at a row that is gone: report the row as absent rather than returning the
		// entry's stale pointer as data.
		return &lockedRow{}, nil
	}
	return &lockedRow{value: row.Value}, nil
}

// provenPrimaryInterval reports the interval a primary-key scan proved, if the planner proved one.
//
// A range or ordered primary-key access carries the bounds the planner derived from the predicate and
// the index it walks; the namespace is implicit because such a scan reads the rows namespace. The
// interval is only treated as proven when the access is one of those two kinds, the plan names an
// index to walk, and at least one bound exists: an unbounded walk is a whole-namespace read and must
// stay reported as a fallback.
func provenPrimaryInterval(plan sqlAccessPlan) (storageengine.KeyRange, bool) {
	if plan.kind != sqlAccessRange && plan.kind != sqlAccessOrdered {
		return storageengine.KeyRange{}, false
	}
	if plan.index == "" || (plan.bounds.Lower == nil && plan.bounds.Upper == nil) {
		return storageengine.KeyRange{}, false
	}
	return plan.bounds, true
}

// openEntryClaim claims one record identity and returns its grant.
func openEntryClaim(ctx context.Context, locking storageengine.LockingTxn, scope uint64, table versionedTable, space, indexID string, key []byte, mode storageengine.LockMode) (storageengine.LockGrant, error) {
	components := []storageengine.LockComponent{readTableIntent(table, mode)}
	components = append(components, recordComponents(space, table.ID, indexID, key, mode, false)...)
	return locking.Acquire(ctx, scope, storageengine.LockRequest{Components: components, ShouldWait: lockedReadWaitDecision(space)})
}

// protectInsertionPosition claims the gap that contains one absent key's insertion position.
//
// The gap is bounded by the key's two published neighbours, which is what makes the protection the
// neighbourhood of the absent key rather than everything after it: with 10 and 100 in the table, a
// read of an absent 42 protects the positions strictly between them. A missing neighbour on either side
// means the gap reaches that end of the index, which is the same shape a next-key gap lock has.
func protectInsertionPosition(ctx context.Context, locking storageengine.LockingTxn, scope uint64, table versionedTable, space, indexID string, key []byte, mode storageengine.LockMode) error {
	previous, next, err := locking.KeysAround(ctx, space, key)
	if err != nil {
		return err
	}
	bounds := storageengine.KeyRange{Lower: previous, LowerInclusive: false, Upper: next, UpperInclusive: false}
	if previous == nil {
		// No key below: the gap starts at the beginning of the namespace. The lower bound stays nil so
		// the interval really is unbounded there rather than starting at the absent key.
		bounds.Lower = nil
	}
	_, err = locking.Acquire(ctx, scope, storageengine.LockRequest{
		Components: []storageengine.LockComponent{readTableIntent(table, mode), intervalComponent(space, table.ID, indexID, bounds)},
		ShouldWait: lockedReadWaitDecision(space),
	})
	return err
}

// lockedReadSpace names the namespace the row-based locked paths protect.
//
// They all reach rows through their primary identity, which lives in the rows namespace — the same
// namespace the write path locks for a row, so the read and the writer meet on one resource. The
// index-entry paths name their own namespace explicitly instead of going through this helper.
func lockedReadSpace(table versionedTable, plan sqlAccessPlan, lockPlan sqlLockPlan) string {
	return sqllayout.Rows(table.ID)
}

// readTableIntent is the table intention that belongs with one read's index locks: IS while the
// statement only reads the rows, IX when it intends to change them. A shared read that declared IX
// would exclude other writers it does not conflict with, and an exclusive read that declared IS would
// be refused the intention it needs.
func readTableIntent(table versionedTable, mode storageengine.LockMode) storageengine.LockComponent {
	intent := storageengine.LockModeIS
	if mode != storageengine.LockModeS {
		intent = storageengine.LockModeIX
	}
	return storageengine.LockComponent{
		Mode:    intent,
		Kind:    storageengine.LockComponentTableIntent,
		Space:   sqllayout.Rows(table.ID),
		TableID: table.ID,
	}
}

// lockedSource closes the statement scope when its rows have been produced or the source is closed.
//
// Closing the scope withdraws requests that were still waiting and keeps every grant, which is exactly
// the lifecycle the contract asks for: a statement that failed hands nothing back, and a statement
// that succeeded leaves its locks with the transaction that took them.
type lockedSource struct {
	storageengine.Iterator
	locks storageengine.LockingTxn
	scope uint64
	done  bool
}

func (s *lockedSource) Next() bool {
	next := s.Iterator.Next()
	if !next {
		s.finish()
	}
	return next
}

func (s *lockedSource) Close() error {
	err := s.Iterator.Close()
	s.finish()
	return err
}

func (s *lockedSource) finish() {
	if s.done {
		return
	}
	s.done = true
	// Closing the scope ends the statement's request origin and keeps every grant the statement took:
	// in an explicit transaction those locks live until COMMIT/ROLLBACK, and in autocommit until the
	// root that owns them ends once this result has been consumed. It must never release a grant, or a
	// statement that succeeded would silently drop the protection its transaction still relies on.
	closeLockScope(s.locks, s.scope)
}

// lockedRow is the single-row source of the point path: zero rows when the row is absent, one row
// when it exists. It reports the latest value observed under the claim, so the WHERE filter above it
// re-evaluates the predicate on current data.
type lockedRow struct {
	value []byte
	done  bool
	err   error
}

func (r *lockedRow) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return r.value != nil
}
func (r *lockedRow) Key() []byte   { return nil }
func (r *lockedRow) Value() []byte { return r.value }
func (r *lockedRow) Err() error    { return r.err }
func (r *lockedRow) Close() error  { r.value = nil; return nil }
