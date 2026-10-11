package executor

import (
	"fmt"
	"strings"

	"gbaselite/parser"
	"gbaselite/storageengine"
)

// lock_plan.go turns the access path a statement actually chose into the resources it must protect.
//
// It exists because a lock range may never be guessed from the text of a WHERE clause. The planner
// has already decided how the statement will reach its rows — a point lookup on the primary key, an
// equality probe on a unique or non-unique index, an ordered range, or a full scan — and that
// decision is the evidence for what has to be locked. Deriving the range from `WHERE a > 5` instead
// would be a guess: the predicate says nothing about whether an index can serve it, whether the
// encoding is ordered, or how far the scan will actually reach.
//
// Two rules follow from that and are the reason this file has a "refused" case:
//
//   - A statement whose access path cannot prove a range gets the whole domain, explicitly marked as
//     a fallback. That is conservative and it is visible in EXPLAIN.
//   - A statement whose locking plan cannot be derived at all is refused. Running it unprotected
//     while the plan says nothing would be the one outcome the contract forbids: silently degrading
//     to no lock at all.
//
// The plan is a pure value: it names the kind of protection, the index and the bounds, and it
// annotates itself for EXPLAIN. Building the neutral components from it is a separate step so a
// caller can report the plan without touching a lock manager.

// lockPlanKind classifies how a statement protects the rows it reads or writes.
type lockPlanKind string

const (
	// lockPlanNone means the statement takes no lock at all: a plain snapshot read, which is the
	// only case where taking nothing is correct.
	lockPlanNone lockPlanKind = "none"
	// lockPlanRecordOnly means every row is reached by a full equality on a unique identity, so the
	// rows themselves are the whole protection and no interval has to be protected.
	lockPlanRecordOnly lockPlanKind = "record-only"
	// lockPlanInsertion means the statement may create a key, so it claims the insertion position
	// (an insert intention paired with the record lock of that key). A miss is not the absence of a
	// lock: it is exactly the case where the interval around the insertion position matters.
	lockPlanInsertion lockPlanKind = "insertion"
	// lockPlanRange means the access path proves an interval: an ordered range, a non-unique index
	// probe or a partial composite prefix. The proven interval is locked together with the rows the
	// scan visits.
	lockPlanRange lockPlanKind = "range"
	// lockPlanWholeDomain is the explicit fallback: the access path cannot bound what it will touch,
	// so the whole namespace of the index (or of the rows) is protected.
	lockPlanWholeDomain lockPlanKind = "whole-domain"
	// lockPlanRefused means no safe plan could be derived. The caller must refuse the statement
	// rather than run it unprotected.
	lockPlanRefused lockPlanKind = "refused"
)

// ErrLockPlanUnavailable reports that a statement's locking plan cannot be derived safely.
//
// It is an error rather than a fallback because the only alternative would be to run the statement
// with no protection while its own plan claims protection is required.
var ErrLockPlanUnavailable = fmt.Errorf("cannot derive a safe lock plan for this access path")

// sqlLockPlan is the locking half of one access plan.
type sqlLockPlan struct {
	kind lockPlanKind
	// index is the index the plan protects, or the primary key's name. It is reported so a
	// diagnostic can name which access path produced the plan.
	index string
	// space is the namespace the interval belongs to. It is the index's own namespace for an index
	// range and the rows' namespace for a primary-key range, because that is where the write path
	// puts the keys the range has to protect.
	space string
	// bounds is the proven interval for a range plan. The zero value means unbounded, which is what
	// a whole-domain fallback uses.
	bounds storageengine.KeyRange
	// exact is the point the plan protects when the access path is an equality, so a caller can
	// build the record lock and the insertion claim without re-deriving the key.
	exact []byte
	// reason explains, in one clause, why the plan is what it is. It is what EXPLAIN shows, so a
	// fallback is never silent.
	reason string
}

// Annotation renders the plan for EXPLAIN's Extra column.
//
// The wording distinguishes a plan that is executed today from one that is only planned: locking
// reads (`FOR UPDATE`/`FOR SHARE`) are recognised by the parser and still execute as snapshot reads
// until the locked read path is wired, and an annotation that hid that would read as a promise the
// engine does not keep.
func (p sqlLockPlan) Annotation(executed bool) string {
	detail := p.reason
	if detail == "" {
		detail = string(p.kind)
	}
	if executed {
		return fmt.Sprintf("Locks: %s (%s)", p.kind, detail)
	}
	return fmt.Sprintf("Locks: %s (%s; not executed yet: the statement still reads its snapshot)", p.kind, detail)
}

// planSQLLockingMode maps the locking mode a statement asked for onto the neutral lock mode the
// plan protects with.
//
// A statement that asked for no row lock has no locking plan at all: that is the `lockPlanNone`
// case, and it is answered here rather than by an empty plan so "this statement takes nothing" is
// an explicit outcome. `FOR UPDATE` protects exclusively and `FOR SHARE`/`LOCK IN SHARE MODE`
// protects shared, which is the same distinction the parser preserved in the AST.
func planSQLLockingMode(locking bool, mode parser.LockMode) storageengine.LockMode {
	if !locking {
		return ""
	}
	if mode == parser.LockModeShare {
		return storageengine.LockModeS
	}
	return storageengine.LockModeX
}

// planSQLLocking derives the locking plan of one SELECT-shaped access path.
//
// It reads the planner's own decision (`sqlAccessPlan`) and nothing else: the kind of access, the
// index it chose, the bounds it proved and whether it is covering. mode is the lock mode the
// statement asked for; a plain snapshot read has no locking plan at all, which is reported as
// lockPlanNone rather than as a plan that happens to be empty.
func planSQLLocking(p sqlAccessPlan, mode storageengine.LockMode) (sqlLockPlan, error) {
	if mode == "" {
		return sqlLockPlan{kind: lockPlanNone, index: p.index, space: p.space, reason: "plain snapshot read"}, nil
	}
	plan := sqlLockPlan{index: p.index, space: p.space, exact: p.key, bounds: p.bounds}
	switch p.kind {
	case sqlAccessPoint:
		// A full equality on a unique identity reaches exactly one row, so the row lock is the whole
		// protection. The same shape is what an INSERT uses when the row is absent, which is why the
		// annotation says so: existence is a property of the data, not of the plan.
		plan.kind = lockPlanRecordOnly
		plan.reason = "full equality on a unique identity; an absent target additionally claims its insertion position"
		return plan, nil
	case sqlAccessUnique:
		// The unique index is the access path, so both the index entry and the common row are
		// protected: two statements that reach one row through different indexes still meet on it.
		plan.kind = lockPlanRecordOnly
		if p.covering {
			plan.reason = "unique equality; covering index, the common row is still locked"
		} else {
			plan.reason = "unique equality; index entry and common row"
		}
		return plan, nil
	case sqlAccessRange, sqlAccessOrdered, "ref":
		// The planner proved a bounded interval. A reverse scan walks the same interval, so the
		// direction never changes what is protected.
		if !rangeIsProven(p) {
			plan.kind = lockPlanWholeDomain
			plan.bounds = storageengine.KeyRange{}
			plan.reason = "ordered access without provable bounds; whole namespace protected"
			return plan, nil
		}
		plan.kind = lockPlanRange
		plan.reason = fmt.Sprintf("proven interval on %s; interval and visited keys", plan.index)
		return plan, nil
	case sqlAccessAll:
		plan.kind = lockPlanWholeDomain
		plan.bounds = storageengine.KeyRange{}
		plan.reason = "no usable access path (full scan); whole namespace protected"
		return plan, nil
	default:
		// An access kind this file does not know cannot be classified. Refusing is the only
		// alternative to protecting nothing, so it is not softened into a fallback.
		return sqlLockPlan{kind: lockPlanRefused, index: p.index, space: p.space}, fmt.Errorf("%w: unknown access kind %q", ErrLockPlanUnavailable, p.kind)
	}
}

// rangeIsProven reports whether an ordered access really proved an interval.
//
// An ordered plan with neither bound would read the whole index while claiming to be a range, which
// is a fallback and must be reported as one. A plan whose index or space is empty cannot name the
// namespace it would protect, so it is not provable either.
func rangeIsProven(p sqlAccessPlan) bool {
	if p.space == "" || (strings.TrimSpace(p.index) == "" && p.kind != sqlAccessRange) {
		return false
	}
	if len(p.bounds.Lower) == 0 && len(p.bounds.Upper) == 0 {
		// Unbounded on both sides: the plan is reading the whole namespace, whatever kind it
		// reported. That is a fallback, not a proven interval.
		return false
	}
	return true
}

// recordComponents builds the components that protect one exact key: the record lock, plus the
// insertion claim when the caller is creating it.
//
// The insertion claim is always paired with its record lock, because that is what makes it legal
// (`MissingRowClaims` refuses an unpaired intention) and what gives an insertion its two answers:
// the intention is what a covering Gap blocks, and the record lock is what decides contention on
// the key itself.
func recordComponents(space, tableID, indexID string, key []byte, mode storageengine.LockMode, inserting bool) []storageengine.LockComponent {
	if len(key) == 0 {
		return nil
	}
	recordMode := mode
	if recordMode != storageengine.LockModeS {
		recordMode = storageengine.LockModeX
	}
	components := make([]storageengine.LockComponent, 0, 2)
	if inserting {
		components = append(components, storageengine.LockComponent{
			Mode:    storageengine.LockModeInsertIntention,
			Kind:    storageengine.LockComponentRecord,
			Space:   space,
			TableID: tableID,
			IndexID: indexID,
			Key:     key,
		})
	}
	components = append(components, storageengine.LockComponent{
		Mode:    recordMode,
		Kind:    storageengine.LockComponentRecord,
		Space:   space,
		TableID: tableID,
		IndexID: indexID,
		Key:     key,
	})
	return components
}

// intervalComponent builds the interval component of a range plan.
//
// An unbounded range is legal and means the whole namespace: that is the fallback's shape, and it
// is stated here rather than by omitting the component, so "protect everything" can never be
// mistaken for "protect nothing".
func intervalComponent(space, tableID, indexID string, bounds storageengine.KeyRange) storageengine.LockComponent {
	return storageengine.LockComponent{
		Mode:           storageengine.LockModeGap,
		Kind:           storageengine.LockComponentInterval,
		Space:          space,
		TableID:        tableID,
		IndexID:        indexID,
		Lower:          bounds.Lower,
		Upper:          bounds.Upper,
		LowerInclusive: bounds.LowerInclusive,
		UpperInclusive: bounds.UpperInclusive,
	}
}

// rangeComponents builds the interval plus a record component for every key the scan visits.
//
// The interval is what protects positions the scan did not visit, and the visited keys are the rows
// the statement may change; a range plan that locked only its bounds would leave every existing row
// inside the range unprotected.
func rangeComponents(plan sqlLockPlan, tableID, indexID string, mode storageengine.LockMode, visited [][]byte) []storageengine.LockComponent {
	components := make([]storageengine.LockComponent, 0, len(visited)+1)
	components = append(components, intervalComponent(plan.space, tableID, indexID, plan.bounds))
	for _, key := range visited {
		components = append(components, storageengine.LockComponent{
			Mode:    mode,
			Kind:    storageengine.LockComponentRecord,
			Space:   plan.space,
			TableID: tableID,
			IndexID: indexID,
			Key:     key,
		})
	}
	return components
}
