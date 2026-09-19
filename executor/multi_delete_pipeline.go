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

// errMultiDeleteNoIdentity reports a multi-table DELETE whose join produced no inputs. A resolved
// target always implies at least one input, so this is a binding guard rather than an expected
// runtime condition.
var errMultiDeleteNoIdentity = errors.New("multi-table DELETE has no join input")

// errMultiDeleteStagingPayload reports a staging run whose payload column is not the owned bytes
// this package wrote.
var errMultiDeleteStagingPayload = errors.New("multi-table DELETE staging payload is not owned bytes")

// This file owns the multi-table DELETE pipeline:
//
//	JOIN
//	 -> WHERE
//	 -> self-contained DeleteCandidate
//	 -> TargetRowDedup (ledger and payload both spillable)
//	 -> spillable dependency-order staging
//	 -> multiDeleteOrder
//	 -> DeleteOperator
//
// Three properties make that structure possible.
//
// First, a candidate is *self-contained*: it carries the target table, the physical storage key,
// the old row and the target's index in the statement's resolved target list, all captured while
// the joined row is in hand. Nothing after dedup re-runs the join, replays an ordinal, or consults
// a mutable identity ledger.
//
// Second, TargetRowDedup returns the winner's full payload, and the staging that reorders those
// winners children-first is itself a bounded sorter. Neither the dedup ledger nor the staging holds
// one entry per distinct target in memory: a winner whose payload spilled is decoded back from a
// run file.
//
// Third, staging is *materialised* — it writes the ordered winners to run files and then reads them
// back once per target. That is what lets the write phase stay phase-ordered (every child row
// deleted before any parent row) without keeping the rows themselves in memory.

// multiDeleteCandidate is one joined row observed as a delete target for one requested target
// table. It is self-contained by construction: everything the delete needs is captured here, while
// the joined row is still in hand.
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
	// budget is the one sort-memory pool every sorter of this statement draws from, so the
	// statement's peak retained sort memory is the configured budget rather than a multiple.
	budget *sorterBudget
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
	// LEFT JOIN still null-extends its right side. Driving from the join rather than from a target
	// is what lets an outer join's absent side be recognised instead of silently dropped.
	plan.rows = chainJoinInputs(read, session, inputs, driving, false)
	if session != nil && session.query != nil {
		plan.budget = newSorterBudget(session.query.options.SortMemoryBytes)
	}
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
// match) produces no candidate. That decision comes from physical provenance, never from whether
// the target's own columns happen to be NULL, so a real row holding NULL values stays deletable.
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

// deduped runs the join once and keeps the first occurrence of every target identity.
//
// The same physical target can appear in many JOIN combinations, and only its first occurrence may
// reach the write path. The ledger and the candidate payload both spill, so a heavily fanned-out
// join needs neither an unbounded seen map nor an in-memory table of winners.
func (p multiDeletePlan) deduped() *physical.TargetRowDedup[multiDeleteCandidate] {
	codec := multiDeleteCodec{}
	return &physical.TargetRowDedup[multiDeleteCandidate]{
		Input: p.candidates(),
		Identity: func(candidate multiDeleteCandidate) (RowIdentity, bool) {
			return candidate.Identity, true
		},
		NewSort: func(byKey bool) (physical.Sorter[physical.SortRow[multiDeleteCandidate]], error) {
			return newCandidateSorter[multiDeleteCandidate](p.session.query, p.budget, codec, byKey)
		},
	}
}

// dependencyRanks maps every target index to its position in the children-first dependency order.
//
// The rank is small — one entry per target the statement names — so it is computed once and indexed
// by multiDeleteCandidate.target. multiDeleteOrder also rejects a cyclic target dependency here,
// before anything is written.
func (p multiDeletePlan) dependencyRanks() ([]string, error) {
	order, err := multiDeleteOrder(p.targets)
	if err != nil {
		return nil, err
	}
	rank := make([]string, len(p.targets))
	for position, index := range order {
		rank[index] = ordinalLedgerKey(uint64(position))
	}
	return rank, nil
}

// stage reorders the deduplicated winners children-first into a spillable staging run, then hands
// that materialised run to emit. The staging sorter owns the run for the whole call and is closed on
// every path, including a dedup failure and a downstream write failure.
//
// The staging sorter sorts on exactly two columns: the target's rank in the dependency order, and
// then the target row's own storage key. So a child target's rows all precede a parent target's
// rows, and within one target the order is deterministic and reproducible.
func (p multiDeletePlan) stage(ctx context.Context, emit func(orderedStage) error) (err error) {
	rank, err := p.dependencyRanks()
	if err != nil {
		return err
	}
	codec := multiDeleteCodec{}
	sorter, err := newExternalRowSorter(p.session.query, p.budget, func(a, b []any) int {
		if c := strings.Compare(a[0].(string), b[0].(string)); c != 0 {
			return c
		}
		return strings.Compare(a[1].(string), b[1].(string))
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sorter.Close()) }()

	if err := p.deduped().Run(ctx, func(candidate multiDeleteCandidate) error {
		payload, err := codec.encode(candidate)
		if err != nil {
			return err
		}
		return sorter.Add([]any{rank[candidate.target], string(candidate.Identity.Key), payload})
	}); err != nil {
		return err
	}
	// Merge the staged rows into one run. The write phase re-reads that run once per target, so it
	// has to be a single ordered file rather than a batch plus a pile of runs.
	if err := sorter.mergeToSingleRun(); err != nil {
		return err
	}
	return emit(orderedStage{values: sorter, codec: codec})
}

// orderedStage presents a materialised staging run as the sequence of candidates the write phase
// reads. Reading it is repeatable: the run is already sorted, and each read is a fresh sequential
// scan of the same file, so a later pass re-reads it rather than consuming it.
type orderedStage struct {
	values *externalRowSorter
	codec  multiDeleteCodec
}

// forTarget streams the staged candidates belonging to one target, in storage-key order.
//
// The staged run is ordered by (dependency rank, storage key), so this is a filtered sequential scan
// and it holds one row at a time.
func (s orderedStage) forTarget(target int, yield func(multiDeleteCandidate) error) error {
	return s.values.readSingleRun(func(values []any) error {
		if len(values) != 3 {
			return errMultiDeleteStagingPayload
		}
		payload, ok := values[2].([]byte)
		if !ok {
			return errMultiDeleteStagingPayload
		}
		candidate, err := s.codec.decode(payload)
		if err != nil {
			return err
		}
		// The staged run is ordered by (dependency rank, storage key), so a single sequential scan
		// in target order yields exactly this target's rows, in storage-key order.
		if candidate.target != target {
			return nil
		}
		return yield(candidate)
	})
}

func (s orderedStage) Close() error { return s.values.Close() }

// delete applies the staged winners through the shared DeleteOperator, one operator per target in
// dependency order, so a child table is always deleted before the parent that references it.
//
// The operators are built up front — there are only as many as the statement names targets, which
// the join's own input count bounds — but the rows are not: each operator reads its own target's
// rows from the staged run in dependency order, one row at a time, so nothing accumulates and no
// in-memory winner slice exists at any point.
//
// Every operator writes through the statement child transaction, while the join above read the
// parent statement snapshot.
func (p multiDeletePlan) delete(ctx context.Context, write storageengine.Txn, session *Session) (uint64, error) {
	order, err := multiDeleteOrder(p.targets)
	if err != nil {
		return 0, err
	}
	var affected uint64
	err = p.stage(ctx, func(staged orderedStage) error {
		for _, index := range order {
			target := index
			operator := &DeleteOperator{
				Target:  p.targets[target].definition,
				Write:   write,
				Session: session,
			}
			operator.Input = physical.Source[DeleteCandidate](func(_ context.Context, yield physical.Yield[DeleteCandidate]) error {
				return staged.forTarget(target, func(candidate multiDeleteCandidate) error {
					return yield(DeleteCandidate{Identity: candidate.Identity, OldRow: candidate.OldRow})
				})
			})
			if err := operator.Run(ctx); err != nil {
				return err
			}
			affected += operator.Result.AffectedRows
		}
		return nil
	})
	if err != nil {
		return affected, err
	}
	return affected, nil
}
