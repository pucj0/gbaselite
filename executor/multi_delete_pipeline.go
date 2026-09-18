package executor

import (
	"context"
	"errors"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"sort"
	"strings"
)

// errMultiDeleteNoIdentity reports a multi-table DELETE whose join produced no inputs. A resolved
// target always implies at least one input, so this is a binding guard rather than an expected
// runtime condition.
var errMultiDeleteNoIdentity = errors.New("multi-table DELETE has no join input")

// This file owns the multi-table DELETE pipeline:
//
//	JOIN
//	 -> WHERE
//	 -> project self-contained DeleteCandidates
//	 -> TargetRowDedup
//	 -> bounded/spillable staging
//	 -> multiDeleteOrder
//	 -> DeleteOperator
//
// Two properties make that structure possible.
//
// First, a candidate is *self-contained*: it carries the target table, the physical storage key,
// the old row and the target's write metadata, all captured while the joined row is in hand.
// Nothing after dedup re-runs the join, replays an ordinal, or consults a mutable identity ledger.
//
// Second, TargetRowDedup returns the winner's full payload, so the deduplicated stream is already
// the set of rows to delete; staging only holds those winners so they can be ordered children-first
// before the write.

// multiDeleteCandidate is one joined row observed as a delete target for one requested target table.
// It is self-contained by construction: everything the delete needs is captured here, while the
// joined row is still in hand.
type multiDeleteCandidate struct {
	// target indexes the statement's resolved target list, which owns the table definition and the
	// window arithmetic. It is a statement-local index, not a re-resolvable identity.
	target int
	// Identity is the target's physical row identity: target table ID plus the storage key the scan
	// observed for exactly this row.
	Identity RowIdentity
	// OldRow holds the target table's own columns, with the hidden provenance columns stripped.
	OldRow storage.Row
}

// multiDeletePlan is the join stage of the multi-table DELETE pipeline.
type multiDeletePlan struct {
	inputs   []joinInput
	combined *storage.Table
	where    parser.Expr
	session  *Session
	rows     physical.Operator[storage.Row]
	// targets holds the resolved target metadata, indexed by multiDeleteCandidate.target.
	targets []multiDeleteTarget
}

// bindMultiDeletePlan builds the join stage from an already bound identified join.
//
// The caller binds once and passes the result in, because the resolved targets and the plan must
// agree on the join layout: target windows and provenance column positions are derived from these
// very inputs.
func bindMultiDeletePlan(read storageengine.Txn, session *Session, statement parser.Delete, inputs []joinInput, targets []multiDeleteTarget) (multiDeletePlan, error) {
	plan := multiDeletePlan{session: session, inputs: inputs, targets: targets}
	if len(inputs) == 0 {
		return plan, errMultiDeleteNoIdentity
	}
	plan.combined = inputs[len(inputs)-1].combined
	plan.where = statement.Where
	if err := bindSQLExplainExprSession(statement.Where, plan.combined, session); err != nil {
		return plan, err
	}
	var driving physical.Operator[storage.Row]
	switch {
	case inputs[0].rows != nil:
		driving = derivedRows(inputs[0].rows)
	case inputs[0].identityColumn != "":
		driving = identityScan(read, &inputs[0], sqlAccessPlan{kind: sqlAccessAll}, session)
	default:
		driving = bindScan(read, inputs[0].definition, sqlAccessPlan{kind: sqlAccessAll}, func(v []byte) (storage.Row, error) {
			return decodeSQLRow(inputs[0].definition, v)
		})
	}
	// The join keeps its full SQL semantics, so a RIGHT JOIN still visits unmatched right rows and a
	// LEFT JOIN still null-extends its right side. Driving from the join rather than from a target is
	// what lets an outer join's absent side be recognised instead of silently dropped.
	plan.rows = chainJoinInputs(read, session, inputs, driving, false)
	return plan, nil
}

// matched applies the statement WHERE to the joined rows.
func (p multiDeletePlan) matched() physical.Operator[storage.Row] {
	if p.where == nil {
		return p.rows
	}
	return physical.Filter[storage.Row]{Input: p.rows, Predicate: func(row storage.Row) (bool, error) {
		value, err := evaluateExprWithContext(p.where, p.combined, row, p.session, nil)
		return truthy(value), err
	}}
}

// candidates projects every joined row that survives WHERE into one self-contained candidate per
// requested target.
//
// A target whose provenance is absent (an outer join null-extended it because the other side had no
// match) produces no candidate. That decision comes from physical provenance, never from whether the
// target's own columns happen to be NULL, so a real row holding NULL values stays deletable.
func (p multiDeletePlan) candidates() physical.Operator[multiDeleteCandidate] {
	rows := p.matched()
	return physical.Source[multiDeleteCandidate](func(ctx context.Context, yield physical.Yield[multiDeleteCandidate]) error {
		return rows.Run(ctx, func(row storage.Row) error {
			for index := range p.targets {
				target := p.targets[index]
				if target.start+target.count > len(row) {
					continue
				}
				// key() reads the joined row: the provenance columns sit just past the window.
				key, ok := target.key(row)
				if !ok {
					continue
				}
				candidate := multiDeleteCandidate{
					target:   index,
					Identity: RowIdentity{TableID: target.definition.ID, Key: append([]byte(nil), key...), Valid: true},
					OldRow:   append(storage.Row(nil), row[target.start:target.start+target.count]...),
				}
				if err := yield(candidate); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// selection runs the join once and deduplicates it by target identity, returning the winners.
//
// Deduplication is TargetRowDedup: the same physical target can appear in many JOIN combinations,
// and only its first occurrence may reach the write path. The operator's ledger is bounded and
// spillable, so a heavily fanned-out join needs no unbounded seen map, and it returns the winner's
// candidate unchanged — exactly the rows the join produced are the rows deleted.
func (p multiDeletePlan) selection(ctx context.Context) ([]multiDeleteCandidate, error) {
	// A cyclic target dependency has no valid single-statement order; reject it before any write.
	if _, err := multiDeleteOrder(p.targets); err != nil {
		return nil, err
	}
	dedup := physical.TargetRowDedup[multiDeleteCandidate]{
		Input: p.candidates(),
		Identity: func(candidate multiDeleteCandidate) (RowIdentity, bool) {
			return candidate.Identity, true
		},
		NewSort: func(byKey bool) (physical.Sorter[physical.TargetDedupRow[multiDeleteCandidate]], error) {
			return newMultiDeleteSorter(p.session.query, byKey)
		},
	}
	// Staging holds the deduplicated winners. It exists so the write can be ordered children-first;
	// it never re-derives identity, and every entry is already a survivor.
	staged := make([]multiDeleteCandidate, 0, len(p.targets))
	if err := dedup.Run(ctx, func(candidate multiDeleteCandidate) error {
		staged = append(staged, candidate)
		return nil
	}); err != nil {
		return nil, err
	}
	return staged, nil
}

// delete runs the staged winners through the shared DeleteOperator, one operator per target in
// dependency order, so a child table is always deleted before the parent that references it.
//
// Each operator owns its target's write metadata and writes through the statement child
// transaction, while the join above read the parent statement snapshot.
func (p multiDeletePlan) delete(ctx context.Context, staged []multiDeleteCandidate, write storageengine.Txn, session *Session) (uint64, error) {
	order, err := multiDeleteOrder(p.targets)
	if err != nil {
		return 0, err
	}
	var affected uint64
	for _, index := range order {
		target := p.targets[index]
		rows := make([]multiDeleteCandidate, 0, len(staged))
		for _, candidate := range staged {
			if candidate.target == index {
				rows = append(rows, candidate)
			}
		}
		if len(rows) == 0 {
			continue
		}
		// A deterministic per-target order keeps the statement reproducible; the dependency order
		// across targets is what the FK semantics require.
		sort.SliceStable(rows, func(i, j int) bool {
			return string(rows[i].Identity.Key) < string(rows[j].Identity.Key)
		})
		operator := &DeleteOperator{
			Input: physical.Source[DeleteCandidate](func(_ context.Context, yield physical.Yield[DeleteCandidate]) error {
				for _, row := range rows {
					if err := yield(DeleteCandidate{Identity: row.Identity, OldRow: row.OldRow}); err != nil {
						return err
					}
				}
				return nil
			}),
			Target:  target.definition,
			Write:   write,
			Session: session,
		}
		if err := operator.Run(ctx); err != nil {
			return affected, err
		}
		affected += operator.Result.AffectedRows
	}
	return affected, nil
}

// newMultiDeleteSorter adapts the shared spillable sorter to multi-delete candidates.
//
// The ledger row holds primitives only, because the shared sorter compares values in order: the dedup
// ledger key and the input ordinal. The candidate payload never travels through the sorter —
// TargetRowDedup returns it — so a spilled ledger cannot lose a row.
func newMultiDeleteSorter(control *queryControl, byKey bool) (physical.Sorter[physical.TargetDedupRow[multiDeleteCandidate]], error) {
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
	return multiDeleteSorter{sorter: sorter}, nil
}

type multiDeleteSorter struct {
	sorter *externalRowSorter
}

func (s multiDeleteSorter) Add(row physical.TargetDedupRow[multiDeleteCandidate]) error {
	return s.sorter.Add([]any{row.Key, ordinalLedgerKey(row.Ordinal)})
}

func (s multiDeleteSorter) Finish(yield func(physical.TargetDedupRow[multiDeleteCandidate]) error) error {
	return s.sorter.Finish(func(row []any) error {
		return yield(physical.TargetDedupRow[multiDeleteCandidate]{
			Key:     row[0].(string),
			Ordinal: ordinalFromLedgerKey(row[1].(string)),
		})
	})
}

func (s multiDeleteSorter) Close() error { return s.sorter.Close() }
