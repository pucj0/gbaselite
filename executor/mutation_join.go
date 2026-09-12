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
	"strings"
)

// errJoinUpdateLimit stops the UPDATE JOIN target scan once the statement LIMIT
// is satisfied. Streaming operators pass the sentinel back untouched, so the
// caller maps it to a successful statement instead of an error.
var errJoinUpdateLimit = errors.New("UPDATE JOIN limit reached")

type joinUpdateAssignment struct {
	position int
	joined   int
	value    parser.Expr
}

// joinUpdateSQL implements the legacy UPDATE ... JOIN semantics on MVCC storage.
// The target table is scanned once from the statement snapshot; every target row
// is joined against the remaining inputs, and the first joined row that satisfies
// WHERE supplies the assignment values (legacy first-match dedup). Writes only go
// through the statement child transaction, so a late error rolls the whole
// statement back.
func (e *Engine) joinUpdateSQL(ctx context.Context, read, write storageengine.Txn, session *Session, statement parser.Update) (*Result, error) {
	definition, schema, catalogKey, err := loadVersionedTable(read, session, statement.Table)
	if err != nil {
		return nil, err
	}
	if err = write.Guard(sqllayout.Catalog, catalogKey); err != nil {
		return nil, err
	}
	targetQualifier := mutationQualifier(statement.Table, statement.TableAlias)
	_, targetTable := splitTableName(statement.Table)
	targetColumns := definition.Definition.Columns

	inputs, err := bindJoins(read, session, parser.Select{Table: statement.Table, TableAlias: statement.TableAlias, Joins: statement.Joins})
	if err != nil {
		return nil, err
	}
	if len(inputs[0].schema.ColumnsView()) != len(targetColumns) {
		return nil, errors.New("UPDATE JOIN target columns changed while binding")
	}
	combined := inputs[len(inputs)-1].combined
	if err = bindSQLExplainExprSession(statement.Where, combined, session); err != nil {
		return nil, err
	}

	assigned := make(map[int]string, len(statement.Assignments))
	assignments := make([]joinUpdateAssignment, 0, len(statement.Assignments))
	for _, assignment := range statement.Assignments {
		qualifier, columnName := splitMutationColumn(assignment.Column)
		if qualifier != "" && !strings.EqualFold(qualifier, targetQualifier) && !strings.EqualFold(qualifier, targetTable) {
			return nil, fmt.Errorf("UPDATE JOIN can only assign columns of target table %s", targetQualifier)
		}
		position, ok := queryColumnIndex(schema, columnName)
		if !ok {
			return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, assignment.Column)
		}
		if previous, duplicate := assigned[position]; duplicate {
			return nil, fmt.Errorf("duplicate update column %q (also specified as %q)", assignment.Column, previous)
		}
		joined, ok := queryColumnIndex(combined, targetQualifier+"."+stripQualifier(schema.ColumnsView()[position].Name))
		if !ok || joined != position {
			return nil, fmt.Errorf("internal join column for %s is unavailable", assignment.Column)
		}
		assigned[position] = assignment.Column
		assignments = append(assignments, joinUpdateAssignment{position: position, joined: joined, value: assignment.Value})
	}

	limit := -1
	if statement.HasLimit {
		limit = statement.Limit
	}
	count := 0
	err = runRowModification(ctx, read, definition, schema, session, nil, -1, func(key []byte, row storage.Row) error {
		if limit >= 0 && count >= limit {
			return errJoinUpdateLimit
		}
		left := append(storage.Row(nil), row...)
		op := chainJoinInputs(read, session, inputs, physical.Source[storage.Row](func(_ context.Context, yield physical.Yield[storage.Row]) error {
			return yield(left)
		}), true)
		if statement.Where != nil {
			op = physical.Filter[storage.Row]{Input: op, Predicate: func(joined storage.Row) (bool, error) {
				value, evaluationErr := evaluateExprWithContext(statement.Where, combined, joined, session, nil)
				return truthy(value), evaluationErr
			}}
		}
		var joined storage.Row
		found := false
		if runErr := (physical.Limit[storage.Row]{Input: op, Count: 1}).Run(ctx, func(candidate storage.Row) error {
			joined = append(storage.Row(nil), candidate...)
			found = true
			return nil
		}); runErr != nil {
			return runErr
		}
		if !found {
			return nil
		}
		old := joined[:len(targetColumns)]
		updated := append(storage.Row(nil), old...)
		evaluation := append(storage.Row(nil), joined...)
		for _, assignment := range assignments {
			raw, evaluationErr := evaluateExprWithContext(assignment.value, combined, evaluation, session, nil)
			if evaluationErr != nil {
				return evaluationErr
			}
			converted, conversionErr := interfaceToColumnValue(raw, targetColumns[assignment.position])
			if conversionErr != nil {
				return conversionErr
			}
			updated[assignment.position] = converted
			evaluation[assignment.joined] = converted
		}
		if writeErr := writeVersionedRow(ctx, write, definition, key, old, updated, ""); writeErr != nil {
			return writeErr
		}
		count++
		return nil
	})
	if err != nil && !errors.Is(err, errJoinUpdateLimit) {
		return nil, err
	}
	return &Result{AffectedRows: uint64(count)}, nil
}
