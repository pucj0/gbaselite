package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// multiDeleteTarget is one requested DELETE destination: the bound join input plus the column
// window that reads its row back out of a combined join row.
type multiDeleteTarget struct {
	definition versionedTable
	primary    storage.Index
	start      int
	count      int
	// identityAt is the position of the hidden identity-marker column, which decides whether the
	// outer join null-extended this target; provenanceAt is the position of the hidden column that
	// carries the physical storage key itself. Both index into the *joined* row, not into the
	// target window, because the provenance columns sit immediately after that window. Both are -1
	// when the target carries no provenance columns.
	identityAt   int
	provenanceAt int
	identityKeys map[int64][]byte
}

// key resolves the physical row identity of one target from the *joined* row.
//
// Two sources can carry the identity, consulted in this order:
//
//  1. the hidden provenance column, which carries the storage key inside the row itself. This is
//     the source that stays correct across a nested-loop re-scan, because the key travels with the
//     row instead of through a counter-keyed ledger that a later scan can overwrite.
//  2. the identity-marker column plus the identityKeys ledger, kept as a fallback for inputs that
//     were bound without a provenance column.
//
// The primary-key encoding remains the last resort for inputs that carry no identity at all.
func (t multiDeleteTarget) key(row storage.Row) ([]byte, bool) {
	if key, ok := ProvenanceKey(row, t.provenanceAt); ok {
		return key, true
	}
	if t.identityAt >= 0 && t.identityAt < len(row) {
		identity := row[t.identityAt]
		if identity.Null {
			return nil, false
		}
		if stored, ok := t.identityKeys[identity.Int64]; ok {
			return stored, true
		}
		return nil, false
	}
	return sqlPrimaryKey(t.definition, t.primary, row[t.start:t.start+t.count])
}

// multiTableDeleteSQL implements multi-table DELETE on the MVCC runtime
// (`DELETE t1,t2 FROM ... JOIN ...` and `DELETE FROM t1,t2 USING ...`).
//
// The joined relation is read from the parent statement snapshot and lowered into the unified
// pipeline:
//
//	JOIN -> WHERE -> self-contained DeleteCandidates -> TargetRowDedup
//	     -> bounded/spillable staging -> multiDeleteOrder -> DeleteOperator
//
// Every row that satisfies WHERE contributes each requested target's physical row identity (the
// storage key its scan observed, which also covers tables without a primary key). Candidates are
// deduplicated through TargetRowDedup so one physical row is deleted once, staged so they can be
// ordered children-first for RESTRICT/NO ACTION foreign keys, and finally removed by DeleteOperator
// through the statement child transaction — so a failure anywhere rolls the whole statement back.
func (e *Engine) multiTableDeleteSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Delete) (*Result, error) {
	if statement.HasLimit {
		return nil, errors.New("LIMIT is not supported for multi-table DELETE")
	}
	// Binding happens exactly once: the resolved targets and the pipeline must share one join
	// layout, so target windows and provenance positions are computed from these same inputs.
	// Binding with row identity gives every joined base table hidden provenance columns, which is
	// what keeps joined targets without a primary key addressable.
	shape := parser.Select{Table: statement.Table, TableAlias: statement.TableAlias, Joins: statement.Joins}
	inputs, err := bindJoinsIdentified(read, session, shape)
	if err != nil {
		return nil, err
	}
	targets, err := resolveMultiDeleteTargets(read, write, session, inputs, statement)
	if err != nil {
		return nil, err
	}
	plan, err := bindMultiDeletePlan(read, session, statement, inputs, targets)
	if err != nil {
		return nil, err
	}
	staged, err := plan.selection(ctx)
	if err != nil {
		return nil, err
	}
	affected, err := plan.delete(ctx, staged, write, session)
	if err != nil {
		return nil, err
	}
	return &Result{AffectedRows: affected}, nil
}

// resolveMultiDeleteTargets maps the requested target names onto join inputs and validates that
// each distinct destination is a base table carrying a deletable physical row identity: the hidden
// storage key the join scan recorded, or — when identity tracking is unavailable — the encoded
// primary key.
func resolveMultiDeleteTargets(read, write storageengine.Txn, session *Session, inputs []joinInput, statement parser.Delete) ([]multiDeleteTarget, error) {
	available := multiDeleteTargetIndexes(statement)
	requested := statement.Targets
	if len(requested) == 0 {
		requested = []string{mutationQualifier(statement.Table, statement.TableAlias)}
	}
	targets := make([]multiDeleteTarget, 0, len(requested))
	// seen deduplicates the *requested names*, which are statement-level: a query may name the same
	// destination twice. It is bounded by the number of names in the statement, so unlike a
	// row-accumulating set it cannot grow with the result.
	seen := make(map[string]bool, len(requested))
	for _, name := range requested {
		index, ok := lookupMultiDeleteTarget(available, name)
		if !ok {
			return nil, fmt.Errorf("unknown DELETE target %s", name)
		}
		input := inputs[index]
		if input.definition.ID == "" {
			// Derived tables, CTEs and views expose no deletable physical identity, so they are
			// rejected as targets exactly like the legacy executor.
			return nil, fmt.Errorf("unknown DELETE target %s", name)
		}
		if seen[input.definition.ID] {
			continue
		}
		primary, hasPrimary := sqlPrimaryIndex(input.definition)
		// The identified binding appends two hidden provenance columns after the input's own
		// columns: the identity marker and then the physical storage key. Both index into the
		// joined row; the target window itself stays the input's own real columns.
		identityAt := provenanceIdentityAt(&input)
		keyAt := provenanceKeyAt(&input)
		if keyAt < 0 && !hasPrimary {
			return nil, fmt.Errorf("multi-table DELETE requires a primary key on %s", input.definition.CatalogName)
		}
		_, _, catalogKey, err := loadVersionedTable(read, session, input.definition.CatalogName)
		if err != nil {
			return nil, err
		}
		if err = write.Guard(sqllayout.Catalog, catalogKey); err != nil {
			return nil, err
		}
		seen[input.definition.ID] = true
		start := len(input.combined.ColumnsView()) - len(input.schema.ColumnsView())
		count := len(input.schema.ColumnsView())
		if input.identityColumn != "" {
			count -= 2
		}
		targets = append(targets, multiDeleteTarget{
			definition:   input.definition,
			primary:      primary,
			start:        start,
			count:        count,
			identityAt:   identityAt,
			provenanceAt: keyAt,
			identityKeys: input.identityKeys,
		})
	}
	return targets, nil
}

// multiDeleteTargetIndexes maps every name a multi-table DELETE may use for a relation (qualified
// name, base name and alias) onto its bound join input.
func multiDeleteTargetIndexes(statement parser.Delete) map[string]int {
	available := make(map[string]int, len(statement.Joins)*3+3)
	register := func(index int, table, alias string) {
		if table == "" {
			return
		}
		_, base := splitTableName(table)
		available[strings.ToLower(table)] = index
		available[strings.ToLower(base)] = index
		if alias != "" {
			available[strings.ToLower(alias)] = index
		}
	}
	register(0, statement.Table, statement.TableAlias)
	for i, join := range statement.Joins {
		register(i+1, join.Table, join.TableAlias)
	}
	return available
}

func lookupMultiDeleteTarget(available map[string]int, name string) (int, bool) {
	index, ok := available[strings.ToLower(name)]
	if !ok {
		if _, base := splitTableName(name); base != name {
			index, ok = available[strings.ToLower(base)]
		}
	}
	return index, ok
}

func sqlPrimaryIndex(table versionedTable) (storage.Index, bool) {
	for _, index := range table.Definition.Indexes {
		if index.Primary {
			return index, true
		}
	}
	return storage.Index{}, false
}

// multiDeleteOrder returns target indices ordered so that a table referenced by another target is
// deleted after the referencing table. Cyclic target foreign keys have no valid single-statement
// order and are rejected.
func multiDeleteOrder(targets []multiDeleteTarget) ([]int, error) {
	referenced := func(child, parent versionedTable) bool {
		for _, fk := range child.Definition.ForeignKeys {
			if strings.EqualFold(fk.RefTable, parent.CatalogName) {
				return true
			}
		}
		return false
	}
	state := make([]int, len(targets))
	order := make([]int, 0, len(targets))
	var visit func(int) error
	visit = func(index int) error {
		switch state[index] {
		case 1:
			return errors.New("multi-table DELETE does not support cyclic foreign keys between target tables")
		case 2:
			return nil
		}
		state[index] = 1
		for child := range targets {
			if child == index {
				continue
			}
			if referenced(targets[child].definition, targets[index].definition) {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		state[index] = 2
		order = append(order, index)
		return nil
	}
	for index := range targets {
		if err := visit(index); err != nil {
			return nil, err
		}
	}
	return order, nil
}
