package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"sort"
	"strings"
)

// multiDeleteTarget is one requested DELETE destination: the bound join input
// plus the column window that reads its row back out of a combined join row.
type multiDeleteTarget struct {
	definition versionedTable
	primary    storage.Index
	start      int
	count      int
	// identityAt is the position of the hidden storage-key column inside the
	// target window, or -1 when the target is identified by its primary key.
	identityAt   int
	identityKeys map[int64][]byte
}

// key resolves the physical row identity of a target window taken from a
// combined join row. Base-table targets carry the storage row key the join scan
// observed on the statement snapshot, so tables without a primary key are still
// addressable, while primary-key targets fall back to the encoded key used by
// every other mutation.
func (t multiDeleteTarget) key(values storage.Row) ([]byte, bool) {
	if t.identityAt >= 0 {
		identity := values[t.identityAt]
		if identity.Null {
			return nil, false
		}
		stored, ok := t.identityKeys[identity.Int64]
		return stored, ok
	}
	return sqlPrimaryKey(t.definition, t.primary, values)
}

// columns strips the hidden row-identity column from a target window so the row
// handed to foreign-key actions and the write path is the table row itself.
func (t multiDeleteTarget) columns(values storage.Row) storage.Row {
	if t.identityAt >= 0 {
		return values[:t.identityAt]
	}
	return values
}

type multiDeleteRow struct {
	target multiDeleteTarget
	key    []byte
	row    storage.Row
}

// multiTableDeleteSQL implements multi-table DELETE on the MVCC runtime
// (`DELETE t1,t2 FROM ... JOIN ...` and `DELETE FROM t1,t2 USING ...`). The
// joined relation is scanned once from the statement read snapshot; every row
// that satisfies WHERE contributes each requested target's physical row identity
// (the storage key its scan observed, which also covers tables without a primary
// key), deduped so one physical row is deleted once, and the collected rows are
// deleted through the statement child
// transaction. Target tables are deleted children-first so RESTRICT/NO ACTION
// foreign keys see the complete statement mutation set, matching the legacy
// executor, and the child transaction rollback keeps the statement atomic.
func (e *Engine) multiTableDeleteSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Delete) (*Result, error) {
	if statement.HasLimit {
		return nil, errors.New("LIMIT is not supported for multi-table DELETE")
	}
	shape := parser.Select{Table: statement.Table, TableAlias: statement.TableAlias, Joins: statement.Joins}
	// Binding with row identity gives every joined base table a hidden storage
	// key column so joined targets without a primary key stay addressable.
	inputs, err := bindJoinsIdentified(read, session, shape)
	if err != nil {
		return nil, err
	}
	combined := inputs[len(inputs)-1].combined
	if err = bindSQLExplainExprSession(statement.Where, combined, session); err != nil {
		return nil, err
	}
	targets, err := resolveMultiDeleteTargets(read, write, session, inputs, statement)
	if err != nil {
		return nil, err
	}
	// A single target that drives the join is deleted through its own row scan, so
	// the storage row key is the row identity and tables without a primary key are
	// supported exactly like the legacy executor.
	if len(targets) == 1 && len(inputs) > 0 && inputs[0].definition.ID == targets[0].definition.ID {
		return e.deleteDrivingTargetSQL(ctx, read, write, session, statement, inputs, combined)
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
	// Targets are collected and deleted after the join scan, so the join keeps its
	// full SQL semantics (a RIGHT JOIN still visits unmatched right rows) instead of
	// being forced to drive from a target.
	op := chainJoinInputs(read, session, inputs, driving, false)
	if statement.Where != nil {
		op = physical.Filter[storage.Row]{Input: op, Predicate: func(row storage.Row) (bool, error) {
			value, evaluationErr := evaluateExprWithContext(statement.Where, combined, row, session, nil)
			return truthy(value), evaluationErr
		}}
	}
	seen := make(map[string]bool)
	rows := make([]multiDeleteRow, 0)
	err = op.Run(ctx, func(row storage.Row) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, target := range targets {
			values := row[target.start : target.start+target.count]
			key, ok := target.key(values)
			if !ok {
				// LEFT/RIGHT JOIN null-extends a target's columns when the other side
				// has no match; those rows are not deletions for that target.
				continue
			}
			identity := target.definition.ID + "\x00" + string(key)
			if seen[identity] {
				continue
			}
			seen[identity] = true
			rows = append(rows, multiDeleteRow{target: target, key: append([]byte(nil), key...), row: append(storage.Row(nil), target.columns(values)...)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	order, err := multiDeleteOrder(targets)
	if err != nil {
		return nil, err
	}
	affected := 0
	for _, index := range order {
		target := targets[index]
		batched := make([]multiDeleteRow, 0, len(rows))
		for _, row := range rows {
			if row.target.definition.ID == target.definition.ID {
				batched = append(batched, row)
			}
		}
		sort.Slice(batched, func(i, j int) bool { return string(batched[i].key) < string(batched[j].key) })
		for _, row := range batched {
			if err := applyForeignKeyActions(ctx, write, session, row.target.definition, row.row, nil, nil, 0); err != nil {
				return nil, err
			}
			if err := writeVersionedRow(ctx, write, row.target.definition, row.key, row.row, nil, ""); err != nil {
				return nil, err
			}
			affected++
		}
	}
	return &Result{AffectedRows: uint64(affected)}, nil
}

// resolveMultiDeleteTargets maps the requested target names onto join inputs and
// validates that each distinct destination is a base table carrying a deletable
// physical row identity: either the hidden storage key the join scan recorded or,
// when identity tracking is unavailable, the encoded primary key.
func resolveMultiDeleteTargets(read, write storageengine.Txn, session *Session, inputs []joinInput, statement parser.Delete) ([]multiDeleteTarget, error) {
	available := multiDeleteTargetIndexes(statement)
	requested := statement.Targets
	if len(requested) == 0 {
		requested = []string{mutationQualifier(statement.Table, statement.TableAlias)}
	}
	targets := make([]multiDeleteTarget, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, name := range requested {
		index, ok := lookupMultiDeleteTarget(available, name)
		if !ok {
			return nil, fmt.Errorf("unknown DELETE target %s", name)
		}
		input := inputs[index]
		if input.definition.ID == "" {
			// Derived tables, CTEs and views expose no deletable physical identity,
			// so they are rejected as targets exactly like the legacy executor.
			return nil, fmt.Errorf("unknown DELETE target %s", name)
		}
		if seen[input.definition.ID] {
			continue
		}
		primary, hasPrimary := sqlPrimaryIndex(input.definition)
		identityAt := -1
		if input.identityColumn != "" && input.identityKeys != nil {
			identityAt = len(input.schema.ColumnsView()) - 1
		} else if !hasPrimary {
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
		targets = append(targets, multiDeleteTarget{
			definition:   input.definition,
			primary:      primary,
			start:        start,
			count:        len(input.schema.ColumnsView()),
			identityAt:   identityAt,
			identityKeys: input.identityKeys,
		})
	}
	return targets, nil
}

// multiDeleteTargetIndexes maps every name a multi-table DELETE may use for a
// relation (qualified name, base name and alias) onto its bound join input.
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

// multiDeleteOrder returns target indices ordered so that a table referenced by
// another target is deleted after the referencing table. Cyclic target foreign
// keys have no valid single-statement order and are rejected.
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

// deleteDrivingTargetSQL deletes the join-driving target row by row. Identity is
// the storage key returned by the scan, so heap tables without a primary key are
// handled with the same stable identity the write path uses.
func (e *Engine) deleteDrivingTargetSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Delete, inputs []joinInput, combined *storage.Table) (*Result, error) {
	definition := inputs[0].definition
	count := 0
	err := runRowModification(ctx, read, definition, combined, session, nil, -1, func(key []byte, row storage.Row) error {
		left := append(storage.Row(nil), row...)
		if inputs[0].identityColumn != "" {
			// The join chain reads the driving row back with its hidden identity
			// column, so the row scan's storage key travels with the row.
			inputs[0].identityNext++
			identity := inputs[0].identityNext
			inputs[0].identityKeys[identity] = append([]byte(nil), key...)
			value, valueErr := storage.NewValue(storage.TypeBigInt, identity)
			if valueErr != nil {
				return valueErr
			}
			left = append(left, value)
		}
		op := chainJoinInputs(read, session, inputs, physical.Source[storage.Row](func(_ context.Context, yield physical.Yield[storage.Row]) error {
			return yield(left)
		}), true)
		if statement.Where != nil {
			op = physical.Filter[storage.Row]{Input: op, Predicate: func(joined storage.Row) (bool, error) {
				value, evaluationErr := evaluateExprWithContext(statement.Where, combined, joined, session, nil)
				return truthy(value), evaluationErr
			}}
		}
		matched := false
		if runErr := (physical.Limit[storage.Row]{Input: op, Count: 1}).Run(ctx, func(storage.Row) error {
			matched = true
			return nil
		}); runErr != nil {
			return runErr
		}
		if !matched {
			return nil
		}
		if err := applyForeignKeyActions(ctx, write, session, definition, row, nil, nil, 0); err != nil {
			return err
		}
		if err := writeVersionedRow(ctx, write, definition, key, row, nil, ""); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{AffectedRows: uint64(count)}, nil
}
