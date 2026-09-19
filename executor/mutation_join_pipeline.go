package executor

import (
	"context"
	"errors"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// This file owns the UPDATE JOIN mutation:
//
//	JOIN
//	 -> WHERE
//	 -> self-contained UpdateCandidate
//	 -> TargetRowDedup (ledger and payload both spillable)
//	 -> LIMIT
//	 -> UpdateOperator
//
// The join runs exactly once. Each joined row that satisfies WHERE is turned into a complete
// UpdateCandidate while the row is in hand — target identity, the target's own old row, and the
// full joined evaluation row — and that candidate then travels through dedup and the write path
// itself. Nothing re-runs the join, replays an ordinal, or looks a winner up in a ledger, so
// there is no second pass that could observe different rows than the first.
//
// Two details of the pipeline are worth stating explicitly.
//
// First, LIMIT sits *after* dedup: LIMIT counts the distinct target rows the statement mutates,
// not the number of joined rows the source produced, so a target matched by three source rows
// consumes one unit of the limit.
//
// Second, dedup carries payloads. A target matched by a thousand join combinations occupies one
// entry in the sorter, and if that entry spills to a run file the candidate is decoded back from
// disk — there is no in-memory table of winners keyed on target identity, so neither the ledger
// nor the staging grows with the join's cardinality.
//
// The target stays the driving side of the join (chainJoinInputs' targetDriven mode), so a target
// matched by several source rows is visited once per matching join row and never once per target.

// updateJoinCandidate is one joined row proposed as an UPDATE target.
//
// It is self-contained: everything the write needs is captured here, while the joined row is
// still in hand. Identity is the target's *old* physical identity, OldRow is the target's own old
// row, and EvalRow is the whole joined row, which is what a SET expression is evaluated against.
type updateJoinCandidate struct {
	// Ordinal is the candidate's position in the join output. Dedup uses it to keep the first
	// occurrence and to restore source order.
	Ordinal uint64
	// Identity is the target's old physical identity: table identifier plus the storage key the
	// scan observed for exactly this row.
	Identity RowIdentity
	// OldRow holds the target table's own columns.
	OldRow storage.Row
	// EvalRow holds the full joined row, so an assignment may read both the target and the
	// joined source.
	EvalRow storage.Row
}

// updateJoinPlan is the join stage of the UPDATE JOIN pipeline.
type updateJoinPlan struct {
	inputs     []joinInput
	combined   *storage.Table
	where      parser.Expr
	session    *Session
	table      versionedTable
	rows       physical.Operator[storage.Row]
	identityAt int
	// provenanceAt is the position of the hidden storage-key column inside the joined row.
	provenanceAt int
	// targetCount is the number of the target's own columns, which is the prefix of the joined row.
	targetCount int
	// limit is the statement's LIMIT, or -1 for unlimited. It is applied after dedup.
	limit int
	// budget is the one sort-memory pool every sorter of this statement draws from, so the
	// statement's peak retained sort memory is the configured budget rather than a multiple.
	budget *sorterBudget
}

// errUpdateJoinNoIdentity reports a target whose physical identity the join could not observe. A
// base-table target always has one, so this is a binding guard rather than an expected runtime
// condition.
var errUpdateJoinNoIdentity = errors.New("UPDATE JOIN target has no physical identity")

// updateJoinBuildHook, when non-nil, observes every UPDATE JOIN pipeline binding. The pipeline binds
// and runs the join exactly once per statement, so a test can assert that the join is not replayed
// to recover candidate payloads. Nil in production, so it costs nothing there.
var updateJoinBuildHook func(plan *updateJoinPlan)

// bindUpdateJoinPlan binds the join chain of an UPDATE ... JOIN statement.
//
// It uses the identified binding, so every joined base table carries the hidden storage row key
// its scan observed. That is the same provenance a multi-table DELETE relies on, and it is what
// gives the target a physical identity that also works for a target without a primary key.
func bindUpdateJoinPlan(ctx context.Context, read storageengine.Txn, session *Session, statement parser.Update, table versionedTable, schema *storage.Table, limit int) (updateJoinPlan, []updateAssignment, error) {
	plan := updateJoinPlan{session: session, table: table, limit: limit}
	inputs, err := bindJoinsIdentified(read, session, parser.Select{Table: statement.Table, TableAlias: statement.TableAlias, Joins: statement.Joins})
	if err != nil {
		return plan, nil, err
	}
	// identityKeys is populated while the join scans, so only the identity column's presence is
	// checked here; the target's keys are resolved per row.
	if len(inputs) == 0 || inputs[0].identityColumn == "" {
		return plan, nil, errUpdateJoinNoIdentity
	}
	plan.inputs = inputs
	plan.combined = inputs[len(inputs)-1].combined
	plan.where = statement.Where
	if err = bindSQLExplainExprSession(statement.Where, plan.combined, session); err != nil {
		return plan, nil, err
	}
	targetColumns := schema.ColumnsView()
	if len(inputs[0].schema.ColumnsView()) < len(targetColumns) {
		return plan, nil, errUpdateJoinNoIdentity
	}
	// The target's own columns are the prefix of the joined row, so the target row width is the
	// target authority's width. The inputs' own schemas also carry the hidden provenance columns, so
	// they cannot be used to derive it.
	plan.targetCount = len(targetColumns)
	// The target is the driving input, so its columns are the prefix of the joined row and the two
	// hidden provenance columns follow them: the identity marker and then the physical storage
	// key. This is the same layout arithmetic the multi-table DELETE target resolution uses.
	plan.identityAt = provenanceIdentityAt(&inputs[0])
	plan.provenanceAt = provenanceKeyAt(&inputs[0])
	if plan.provenanceAt < 0 {
		return plan, nil, errUpdateJoinNoIdentity
	}
	qualifier := mutationQualifier(statement.Table, statement.TableAlias)
	_, targetTable := splitTableName(statement.Table)
	assignments, err := resolveUpdateAssignments(statement, table, schema, qualifier, targetTable, qualifier, plan.combined)
	if err != nil {
		return plan, nil, err
	}
	// The driver reuses the statement's access plan, so an indexed UPDATE JOIN still probes an
	// index instead of scanning the target table. It is an identity scan, so the hidden identity
	// column the identified binding defined for the target is part of the joined row and every
	// target row carries the storage key its scan observed.
	driver := identityScan(read, &inputs[0], planSQLAccess(parser.Select{Where: statement.Where}, table, schema, session), session)
	plan.rows = chainJoinInputs(read, session, inputs, driver, true)
	if session != nil && session.query != nil {
		plan.budget = newSorterBudget(session.query.options.SortMemoryBytes)
	}
	if updateJoinBuildHook != nil {
		updateJoinBuildHook(&plan)
	}
	return plan, assignments, nil
}

// matched applies the statement WHERE to the joined rows.
func (p updateJoinPlan) matched() physical.Operator[storage.Row] {
	if p.where == nil {
		return p.rows
	}
	return physical.Filter[storage.Row]{Input: p.rows, Predicate: func(row storage.Row) (bool, error) {
		value, err := evaluateExprWithContext(p.where, p.combined, row, p.session, nil)
		return truthy(value), err
	}}
}

// identity resolves the physical identity of the joined row's target.
//
// The provenance column is preferred: it carries the storage key inside the row, so it stays
// correct even when the driving input is re-scanned by the nested join loop. A row with no
// provenance column falls back to the identity marker plus the ledger, and a null marker (an
// outer join null-extension) yields an invalid identity that TargetRowDedup drops.
func (p updateJoinPlan) identity(row storage.Row) RowIdentity {
	if key, ok := ProvenanceKey(row, p.provenanceAt); ok {
		return RowIdentity{TableID: p.table.ID, Key: key, Valid: true}
	}
	if p.identityAt < 0 || p.identityAt >= len(row) {
		return RowIdentity{}
	}
	value := row[p.identityAt]
	if value.Null {
		return RowIdentity{}
	}
	key, ok := p.inputs[0].identityKeys[value.Int64]
	if !ok {
		return RowIdentity{}
	}
	return RowIdentity{TableID: p.table.ID, Key: key, Valid: true}
}

// candidates turns every joined row that satisfies WHERE into a complete UpdateCandidate.
//
// The candidate owns its bytes: OldRow is cloned out of the joined row and EvalRow is cloned for
// the same reason, because the sorter retains the candidate past the scan that produced it.
func (p updateJoinPlan) candidates(ordinal *uint64) physical.Operator[updateJoinCandidate] {
	return physical.Projection[storage.Row, updateJoinCandidate]{Input: p.matched(), Project: func(row storage.Row) (updateJoinCandidate, error) {
		position := *ordinal
		*ordinal = position + 1
		if len(row) < p.targetCount {
			return updateJoinCandidate{Ordinal: position}, nil
		}
		owned := append(storage.Row(nil), row...)
		return updateJoinCandidate{
			Ordinal:  position,
			Identity: p.identity(row),
			OldRow:   append(storage.Row(nil), owned[:p.targetCount]...),
			EvalRow:  owned,
		}, nil
	}}
}

// deduped runs the join once and keeps the first occurrence of every target identity. The ledger
// and the candidate payload both spill, so a heavily matched target costs one sorter entry rather
// than one per join combination.
func (p updateJoinPlan) deduped(ordinal *uint64) *physical.TargetRowDedup[updateJoinCandidate] {
	codec := updateJoinCodec{}
	return &physical.TargetRowDedup[updateJoinCandidate]{
		Input: p.candidates(ordinal),
		Identity: func(candidate updateJoinCandidate) (RowIdentity, bool) {
			return candidate.Identity, true
		},
		NewSort: func(byKey bool) (physical.Sorter[physical.SortRow[updateJoinCandidate]], error) {
			return newCandidateSorter[updateJoinCandidate](p.session.query, p.budget, codec, byKey)
		},
	}
}

// run executes the whole pipeline and reports how many rows the statement updated.
//
// The candidate the write path receives is the one dedup selected, so the source row that produced
// the identity is also the one that supplies the assignment values: first match wins.
func (p updateJoinPlan) run(ctx context.Context, target versionedTable, targetColumns []storage.Column, assignments []updateAssignment, schema *storage.Table, write storageengine.Txn) (uint64, error) {
	var ordinal uint64
	// The write path consumes the deduplicated candidates directly. A candidate only has to be
	// reshaped so OldRow is exactly the target's own columns; the joined evaluation row and the old
	// physical identity come straight from the winner, whether it was retained in memory or decoded
	// from a spill run.
	operator := &UpdateOperator{
		Input: physical.Projection[updateJoinCandidate, UpdateCandidate]{
			Input: physical.Limit[updateJoinCandidate]{
				Input: p.deduped(&ordinal),
				Count: p.limit,
			},
			Project: func(candidate updateJoinCandidate) (UpdateCandidate, error) {
				return candidate.rebuild(p.targetCount), nil
			},
		},
		// Only the target's own columns are written; assignments evaluate against the whole joined
		// row, which is why EvalSchema is the joined schema.
		Target:      target,
		Schema:      schema,
		EvalSchema:  p.combined,
		Assignments: assignments,
		Write:       write,
		Session:     p.session,
	}
	if err := operator.Run(ctx); err != nil {
		return 0, err
	}
	return operator.Result.AffectedRows, nil
}
