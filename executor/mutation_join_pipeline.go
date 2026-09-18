package executor

import (
	"context"
	"errors"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// This file owns the UPDATE JOIN mutation. It replaces the previous per-target nested
// join, which re-ran a join for every candidate target and wrote the physical row
// itself, with one pipeline:
//
//	JOIN
//	 -> WHERE
//	 -> UpdateCandidate
//	 -> TargetRowDedup
//	 -> LIMIT
//	 -> UpdateOperator
//
// Three details of that pipeline are worth stating explicitly.
//
// First, LIMIT sits *after* dedup: LIMIT counts the distinct target rows the statement
// mutates, not the number of joined rows the source produced, so a target matched by
// three source rows consumes one unit of the limit.
//
// Second, dedup runs over identities alone. A candidate stages its target identity plus
// its position in the join output, so TargetRowDedup's bounded/spillable ledger never has
// to encode a storage row. The join is then re-run and the winning positions select the
// rows to update. Both passes read the same parent statement snapshot, so they observe
// identical rows in identical order.
//
// Third, the target stays the driving side of the join (chainJoinInputs' targetDriven
// mode), so a target matched by several source rows is visited once per matching join row
// and never once per target.

// updateJoinCandidate is one joined row proposed as an UPDATE target.
//
// It carries only what dedup needs: the target's physical identity and the row's position
// in the join output. The joined row itself is re-read in the second pass rather than
// retained, which is also what keeps the dedup ledger free of row payloads.
type updateJoinCandidate struct {
	identity RowIdentity
	ordinal  uint64
}

// updateJoinPlan is the reusable join stage of the UPDATE JOIN pipeline. It is built once
// and run twice: first to choose which target rows the statement updates, then to
// materialise the chosen rows for the write.
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
}

// errUpdateJoinNoIdentity reports a target whose physical identity the join could not
// observe. A base-table target always has one, so this is a binding guard rather than an
// expected runtime condition.
var errUpdateJoinNoIdentity = errors.New("UPDATE JOIN target has no physical identity")

// bindUpdateJoinPlan binds the join chain of an UPDATE ... JOIN statement.
//
// It uses the identified binding, so every joined base table carries the hidden storage
// row key its scan observed. That is the same provenance a multi-table DELETE relies on,
// and it is what gives the target a physical identity that also works for a target
// without a primary key.
func bindUpdateJoinPlan(ctx context.Context, read storageengine.Txn, session *Session, statement parser.Update, table versionedTable, schema *storage.Table, limit int) (updateJoinPlan, []updateAssignment, error) {
	plan := updateJoinPlan{session: session, table: table}
	inputs, err := bindJoinsIdentified(read, session, parser.Select{Table: statement.Table, TableAlias: statement.TableAlias, Joins: statement.Joins})
	if err != nil {
		return plan, nil, err
	}
	// identityKeys is populated while the join scans, so only the identity column's
	// presence is checked here; the target's keys are resolved per row during the passes.
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
	// The driver reuses the statement's access plan, so an indexed UPDATE JOIN still
	// probes an index instead of scanning the target table. It is an identity scan, so the
	// hidden identity column the identified binding defined for the target is part of the
	// joined row and every target row carries the storage key its scan observed.
	driver := identityScan(read, &inputs[0], planSQLAccess(parser.Select{Where: statement.Where}, table, schema, session), session)
	plan.rows = chainJoinInputs(read, session, inputs, driver, true)
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

// winners runs the join once, dedups target identities with TargetRowDedup (bounded and
// spillable, never an unbounded seen map) and applies LIMIT after dedup.
//
// The result is the set of join-output positions the statement updates. First occurrence
// wins, so a target matched by several source rows is updated once, with the first
// matching source row supplying the assignment values.
func (p updateJoinPlan) winners(ctx context.Context, limit int) (map[uint64]struct{}, error) {
	var ordinal uint64
	dedup := physical.TargetRowDedup[updateJoinCandidate]{
		Input: physical.Projection[storage.Row, updateJoinCandidate]{Input: p.matched(), Project: func(row storage.Row) (updateJoinCandidate, error) {
			position := ordinal
			ordinal++
			return updateJoinCandidate{identity: p.identity(row), ordinal: position}, nil
		}},
		Identity: func(candidate updateJoinCandidate) (RowIdentity, bool) {
			return candidate.identity, true
		},
		NewSort: func(byKey bool) (physical.Sorter[physical.TargetDedupRow[updateJoinCandidate]], error) {
			return newUpdateJoinSorter(p.session.query, byKey)
		},
	}
	selected := make(map[uint64]struct{})
	count := 0
	err := dedup.Run(ctx, func(candidate updateJoinCandidate) error {
		if limit >= 0 && count >= limit {
			return nil
		}
		selected[candidate.ordinal] = struct{}{}
		count++
		return nil
	})
	if err != nil {
		return nil, err
	}
	return selected, nil
}

// apply re-runs the join and mutates the winning rows through the shared UpdateOperator.
//
// OldRow is the target's own columns and EvalRow is the whole joined row, so a SET
// expression may read both the target and the joined source, and the source row that
// produced the identity is the one that supplies the values.
func (p updateJoinPlan) apply(ctx context.Context, selected map[uint64]struct{}, target versionedTable, targetColumns []storage.Column, assignments []updateAssignment, schema *storage.Table, write storageengine.Txn, session *Session) (uint64, error) {
	// Pass two walks the same rows in the same order, so the ordinal sequence lines up
	// with the one that produced the winners.
	ordinal := uint64(0)
	filtered := physical.Filter[storage.Row]{Input: p.matched(), Predicate: func(storage.Row) (bool, error) {
		position := ordinal
		ordinal++
		_, ok := selected[position]
		return ok, nil
	}}
	candidates := physical.Projection[storage.Row, UpdateCandidate]{Input: filtered, Project: func(row storage.Row) (UpdateCandidate, error) {
		if len(row) < len(targetColumns) {
			return UpdateCandidate{}, nil
		}
		return UpdateCandidate{
			Identity: p.identity(row),
			OldRow:   append(storage.Row(nil), row[:len(targetColumns)]...),
			EvalRow:  row,
		}, nil
	}}
	operator := &UpdateOperator{
		Input: candidates,
		// Only the target's own columns are written; assignments evaluate against the whole
		// joined row, which is why EvalSchema is the joined schema.
		Target:      target,
		Schema:      schema,
		EvalSchema:  p.combined,
		Assignments: assignments,
		Write:       write,
		Session:     session,
	}
	if err := operator.Run(ctx); err != nil {
		return 0, err
	}
	return operator.Result.AffectedRows, nil
}

// newUpdateJoinSorter adapts the shared spillable sorter to dedup candidates. The ledger
// row holds primitives only: the identity ledger key and the join position, ordered
// numerically through its big-endian string form.
func newUpdateJoinSorter(control *queryControl, byKey bool) (physical.Sorter[physical.TargetDedupRow[updateJoinCandidate]], error) {
	compare := func(a, b []any) int {
		if byKey {
			if c := strings.Compare(a[0].(string), b[0].(string)); c != 0 {
				return c
			}
		}
		return strings.Compare(a[1].(string), b[1].(string))
	}
	sorter, err := newExternalRowSorter(control, compare)
	if err != nil {
		return nil, err
	}
	return updateJoinSorter{sorter: sorter}, nil
}

type updateJoinSorter struct {
	sorter *externalRowSorter
}

func (s updateJoinSorter) Add(row physical.TargetDedupRow[updateJoinCandidate]) error {
	return s.sorter.Add([]any{row.Key, ordinalLedgerKey(row.Ordinal)})
}

func (s updateJoinSorter) Finish(yield func(physical.TargetDedupRow[updateJoinCandidate]) error) error {
	return s.sorter.Finish(func(row []any) error {
		ordinal := ordinalFromLedgerKey(row[1].(string))
		return yield(physical.TargetDedupRow[updateJoinCandidate]{
			Key:     row[0].(string),
			Ordinal: ordinal,
			Row:     updateJoinCandidate{ordinal: ordinal},
		})
	})
}

func (s updateJoinSorter) Close() error { return s.sorter.Close() }

// ordinalLedgerKey renders an ordinal so a bytewise comparison orders it numerically,
// which the shared sorter needs because it compares sort values in order.
func ordinalLedgerKey(ordinal uint64) string {
	var scratch [8]byte
	for i := range scratch {
		scratch[7-i] = byte(ordinal >> (8 * i))
	}
	return string(scratch[:])
}

func ordinalFromLedgerKey(key string) uint64 {
	var ordinal uint64
	for i := 0; i < len(key) && i < 8; i++ {
		ordinal = ordinal<<8 | uint64(key[i])
	}
	return ordinal
}
