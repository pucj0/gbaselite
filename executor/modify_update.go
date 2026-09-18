package executor

import (
	"context"
	"fmt"
	"gbaselite/parser"
	"gbaselite/physical"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// This file owns the unified UPDATE row mutation. A plain UPDATE (and, in the next
// phase, UPDATE JOIN) lowers its query pipeline into
// physical.Operator[UpdateCandidate] and hands it to UpdateOperator, so the SET
// assignment semantics, ON UPDATE timestamps, FK actions and the versioned write all
// have exactly one implementation.
//
// What stays outside this operator, deliberately:
//
//   - the access plan and scan (the binding layer keeps planSQLAccess, so an indexed
//     UPDATE does not degrade to a full scan);
//   - CHECK / UNIQUE / PRIMARY KEY / secondary index / FK write logic, reached through
//     the existing writeVersionedRow and applyForeignKeyActions;
//   - statement transaction commit and session publishing.
//
// The operator writes only through the statement child transaction it is given, while
// the scan that feeds it reads the parent statement snapshot. That split is what keeps
// Halloween protection: a row updated during the scan cannot be re-read by the same
// statement.

// UpdateCandidate is one target row waiting to be updated.
//
// Identity always describes the *old* physical row, even when the assignment list
// changes the primary key or an indexed column: dedup, Halloween protection and the
// write path all address the row by where it was read, and the operator computes the
// new physical key itself.
type UpdateCandidate struct {
	// Identity is the old physical row identity of the target.
	Identity RowIdentity
	// OldRow holds the target table values from the statement snapshot. The operator
	// never mutates it in place: sequential SET evaluation needs the original row while
	// producing the new one, and the write path re-reads it to remove the old secondary
	// and unique index entries.
	OldRow storage.Row
	// EvalRow is the expression evaluation context. For a plain UPDATE it mirrors
	// OldRow; for UPDATE JOIN it also carries the joined source columns, so an
	// assignment may read both the target and the source relation.
	EvalRow storage.Row
}

// Target exposes the mutation target identity for dedup and operator input.
func (c UpdateCandidate) Target() (RowIdentity, bool) {
	return c.Identity, c.Identity.IsTarget()
}

// updateAssignment is one resolved SET assignment: the target column position inside
// the target table, the position the expression reads inside the evaluation row, and
// the expression itself.
//
// The two positions differ for UPDATE JOIN, where the evaluation row is the joined row
// and a target column therefore sits at a different offset.
type updateAssignment struct {
	position  int
	evaluated int
	column    string
	value     parser.Expr
}

// UpdateOperator writes every UpdateCandidate produced by its source.
type UpdateOperator struct {
	// Input yields candidates for the rows the statement must modify, in access-plan
	// order.
	Input physical.Operator[UpdateCandidate]

	// Target is the mutated table.
	Target versionedTable
	// Schema resolves assignment column names, including qualified ones.
	Schema *storage.Table
	// EvalSchema resolves column names inside the assignment expressions. It is the
	// joined schema for UPDATE JOIN, where an expression may read the source relation,
	// and defaults to Schema for a plain UPDATE.
	EvalSchema *storage.Table
	// Assignments are the statement's resolved SET clauses, applied in statement order.
	Assignments []updateAssignment
	// Write is the statement child transaction. The operator never commits it.
	Write storageengine.Txn
	// Session supplies CURRENT_TIMESTAMP for ON UPDATE columns.
	Session *Session

	// Result accumulates the statement's affected rows.
	Result ModifyResult
}

// Run writes every candidate and discards the operator's own output, which is what a
// terminal mutation operator does.
func (o *UpdateOperator) Run(ctx context.Context) error {
	modify := physical.Modify[UpdateCandidate, struct{}]{Input: o.Input, Apply: func(ctx context.Context, candidate UpdateCandidate) (struct{}, error) {
		return struct{}{}, o.Apply(ctx, candidate)
	}}
	return modify.Run(ctx, func(struct{}) error { return nil })
}

// Apply evaluates the SET assignments for one candidate and writes the new row.
//
// It is exported so the UPDATE JOIN pipeline, which selects its candidates before it
// materialises them, can hand each one to the same implementation a plain UPDATE uses.
func (o *UpdateOperator) Apply(ctx context.Context, candidate UpdateCandidate) error {
	updated, err := o.assign(candidate)
	if err != nil {
		return err
	}
	if err = applyForeignKeyActions(ctx, o.Write, o.Session, o.Target, candidate.OldRow, updated, nil, 0); err != nil {
		return err
	}
	// The old physical key travels unchanged when the candidate carries one: a
	// primary-key UPDATE deletes the old key and stores the new one inside the write path.
	// An UPDATE JOIN candidate leaves the key empty and relies on the target's primary key,
	// which is what the joined-row evaluation context can address.
	if err = writeVersionedRow(ctx, o.Write, o.Target, candidate.Identity.Key, candidate.OldRow, updated, ""); err != nil {
		return err
	}
	o.Result.AffectedRows++
	return nil
}

// assign computes the new row for one candidate.
//
// Assignments are evaluated in statement order against a row that already carries the
// earlier assignments of the same statement, so:
//
//	UPDATE t SET a = a + 1, b = a;
//
// stores the *new* a in b.
//
// The produced row is always a separate allocation from OldRow. That is not just
// hygiene: the write path derives the old unique and secondary index entries from
// OldRow, so aliasing it would make the statement rewrite the new values over the old
// ones and silently skip the conflict checks that keep a UNIQUE index consistent.
func (o *UpdateOperator) assign(candidate UpdateCandidate) (storage.Row, error) {
	evaluation := candidateEvalRow(candidate)
	evalSchema := o.EvalSchema
	if evalSchema == nil {
		evalSchema = o.Schema
	}
	return assignUpdateRow(candidate, o.Target, o.Assignments, o.Schema, evalSchema, o.Session, evaluation)
}

// assignUpdateRow produces the new target row for one candidate.
//
// Assignments are evaluated in statement order against evaluation, which already carries
// the earlier assignments of the same statement, so:
//
//	UPDATE t SET a = a + 1, b = a;
//
// stores the *new* a in b.
//
// OldRow is never written: the write path derives the old unique and secondary index
// entries from it, so overwriting it with the new values would silently skip the conflict
// checks that keep a UNIQUE index consistent. A joined evaluation row is wider than the
// target row, which is why assignment writes refresh the target window inside it as well.
func assignUpdateRow(candidate UpdateCandidate, target versionedTable, assignments []updateAssignment, schema, evalSchema *storage.Table, session *Session, evaluation storage.Row) (storage.Row, error) {
	updated := append(storage.Row(nil), candidate.OldRow...)
	inPlace := sameRowBacking(evaluation, updated)
	for _, assignment := range assignments {
		raw, err := evaluateExprWithContext(assignment.value, evalSchema, evaluation, session, nil)
		if err != nil {
			return nil, err
		}
		value, err := interfaceToColumnValue(raw, target.Definition.Columns[assignment.position])
		if err != nil {
			return nil, err
		}
		updated[assignment.position] = value
		if !inPlace {
			// A separate evaluation buffer keeps the joined source columns readable while
			// the target window tracks the produced values.
			evaluation[assignment.evaluated] = value
		}
	}
	assigned := assignedUpdateColumns(assignments)
	for position, column := range target.Definition.Columns {
		if assigned[position] || column.OnUpdate == "" {
			continue
		}
		if strings.EqualFold(column.OnUpdate, "CURRENT_TIMESTAMP") || strings.EqualFold(column.OnUpdate, "CURRENT_TIMESTAMP()") {
			value, err := storage.NewValue(column.Type, session.Now())
			if err != nil {
				return nil, err
			}
			updated[position] = value
		}
	}
	return updated, nil
}

// assignedUpdateColumns reports which target columns the statement's SET list writes, so an
// ON UPDATE timestamp column the statement assigned explicitly is not overwritten.
func assignedUpdateColumns(assignments []updateAssignment) map[int]bool {
	assigned := make(map[int]bool, len(assignments))
	for _, assignment := range assignments {
		assigned[assignment.position] = true
	}
	return assigned
}

// candidateEvalRow returns the row the assignments evaluate against and mutate.
//
// OldRow must stay a read-only snapshot of the target as it was read: the write path
// derives the old unique and secondary index entries from it, so overwriting it with
// the new values would silently skip the conflict checks that keep a UNIQUE index
// consistent. When the candidate's evaluation row *is* the old row - the plain UPDATE
// shape - the evaluation row therefore needs a private copy. A joined evaluation row is
// already a different allocation that carries the source columns, so it can be used
// directly, which also keeps the joined columns visible while producing the new row.
func candidateEvalRow(candidate UpdateCandidate) storage.Row {
	if sameRowBacking(candidate.EvalRow, candidate.OldRow) {
		return append(storage.Row(nil), candidate.EvalRow...)
	}
	return candidate.EvalRow
}

// sameRowBacking reports whether two rows are the same allocation, which is how the
// plain UPDATE candidate and the joined UPDATE JOIN candidate are told apart.
func sameRowBacking(a, b storage.Row) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return &a[0] == &b[0]
}

// resolveUpdateAssignments turns the statement's SET clauses into positions.
//
// Every assignment is resolved before any row is read, so an unknown column, a
// duplicate target column, or an assignment aimed at another relation fails as a
// binding error instead of part-way through the mutation.
//
// When evaluateSchema differs from schema, the evaluation row is a joined row and a
// target column sits at a different offset, which evaluateQualifier identifies.
func resolveUpdateAssignments(statement parser.Update, target versionedTable, schema *storage.Table, targetQualifier, targetTable string, evaluateQualifier string, evaluateSchema *storage.Table) ([]updateAssignment, error) {
	assigned := make(map[int]string, len(statement.Assignments))
	assignments := make([]updateAssignment, 0, len(statement.Assignments))
	for _, assignment := range statement.Assignments {
		qualifier, columnName := splitMutationColumn(assignment.Column)
		if qualifier != "" && !strings.EqualFold(qualifier, targetQualifier) && !strings.EqualFold(qualifier, targetTable) {
			return nil, fmt.Errorf("UPDATE can only assign columns of target table %s", targetQualifier)
		}
		position, ok := queryColumnIndex(schema, columnName)
		if !ok {
			return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, assignment.Column)
		}
		if previous, duplicate := assigned[position]; duplicate {
			return nil, fmt.Errorf("duplicate update column %q (also specified as %q)", assignment.Column, previous)
		}
		assigned[position] = assignment.Column
		evaluated := position
		if evaluateSchema != schema {
			joined, ok := queryColumnIndex(evaluateSchema, evaluateQualifier+"."+stripQualifier(schema.ColumnsView()[position].Name))
			if !ok {
				return nil, fmt.Errorf("internal join column for %s is unavailable", assignment.Column)
			}
			evaluated = joined
		}
		assignments = append(assignments, updateAssignment{position: position, evaluated: evaluated, column: assignment.Column, value: assignment.Value})
	}
	return assignments, nil
}
