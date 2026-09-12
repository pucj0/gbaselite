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
}

type multiDeleteRow struct {
	target multiDeleteTarget
	key    []byte
	row    storage.Row
}

// multiTableDeleteSQL implements multi-table DELETE on the MVCC runtime
// (`DELETE t1,t2 FROM ... JOIN ...` and `DELETE FROM t1,t2 USING ...`). The
// joined relation is scanned once from the statement read snapshot; every row
// that satisfies WHERE contributes each requested target's row identity, deduped
// by primary key, and the collected rows are deleted through the statement child
// transaction. Target tables are deleted children-first so RESTRICT/NO ACTION
// foreign keys see the complete statement mutation set, matching the legacy
// executor, and the child transaction rollback keeps the statement atomic.
func (e *Engine) multiTableDeleteSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Delete) (*Result, error) {
	if statement.HasLimit {
		return nil, errors.New("LIMIT is not supported for multi-table DELETE")
	}
	shape := parser.Select{Table: statement.Table, TableAlias: statement.TableAlias, Joins: statement.Joins}
	inputs, err := bindJoins(read, session, shape)
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
	driving := bindScan(read, inputs[0].definition, sqlAccessPlan{kind: sqlAccessAll}, func(v []byte) (storage.Row, error) {
		return decodeSQLRow(inputs[0].definition, v)
	})
	op := chainJoinInputs(read, session, inputs, driving)
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
			key, ok := sqlPrimaryKey(target.definition, target.primary, values)
			if !ok {
				// LEFT JOIN null-extends a target's columns when the right side has no
				// match; those rows are not deletions for that target.
				continue
			}
			identity := target.definition.ID + "\x00" + string(key)
			if seen[identity] {
				continue
			}
			seen[identity] = true
			rows = append(rows, multiDeleteRow{target: target, key: append([]byte(nil), key...), row: append(storage.Row(nil), values...)})
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
			if err := writeVersionedRow(ctx, write, row.target.definition, row.key, row.row, nil, ""); err != nil {
				return nil, err
			}
			affected++
		}
	}
	return &Result{AffectedRows: uint64(affected)}, nil
}

// resolveMultiDeleteTargets maps the requested target names onto join inputs and
// validates that each distinct destination is a base table with a primary key.
func resolveMultiDeleteTargets(read, write storageengine.Txn, session *Session, inputs []joinInput, statement parser.Delete) ([]multiDeleteTarget, error) {
	available := make(map[string]int, len(inputs)*3)
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
	requested := statement.Targets
	if len(requested) == 0 {
		requested = []string{mutationQualifier(statement.Table, statement.TableAlias)}
	}
	targets := make([]multiDeleteTarget, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, name := range requested {
		index, ok := available[strings.ToLower(name)]
		if !ok {
			if _, base := splitTableName(name); base != name {
				index, ok = available[strings.ToLower(base)]
			}
		}
		if !ok {
			return nil, fmt.Errorf("unknown DELETE target %s", name)
		}
		input := inputs[index]
		if seen[input.definition.ID] {
			continue
		}
		primary, ok := sqlPrimaryIndex(input.definition)
		if !ok {
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
		targets = append(targets, multiDeleteTarget{definition: input.definition, primary: primary, start: start, count: len(input.schema.ColumnsView())})
	}
	return targets, nil
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
