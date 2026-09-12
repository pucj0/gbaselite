package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

// insertMode is the duplicate-key policy of one INSERT/REPLACE statement.
type insertMode struct {
	ignore      bool
	replace     bool
	onDuplicate []parser.InsertAssignment
}

func insertStatementMode(statement parser.Insert) insertMode {
	return insertMode{ignore: statement.Ignore, replace: statement.Replace, onDuplicate: statement.OnDuplicate}
}

type insertOutcome struct {
	affected int
	inserted bool
}

type insertConflict struct {
	key []byte
	row storage.Row
}

// insertSetStatement rewrites INSERT ... SET into the single-row VALUES form the
// shared writer understands. Every SET expression becomes a value expression
// evaluated against the row built so far, so defaults, NULLs, auto-increment and
// conflict handling behave exactly like INSERT ... VALUES.
func insertSetStatement(statement parser.Insert) parser.Insert {
	row := make([]parser.Literal, len(statement.SetValues))
	expressions := make(map[[2]int]parser.Expr, len(statement.SetValues))
	for i, expression := range statement.SetValues {
		row[i] = parser.Literal{Kind: parser.LiteralNull}
		expressions[[2]int{0, i}] = expression
	}
	statement.Values = [][]parser.Literal{row}
	statement.ValueExpressions = expressions
	statement.SetValues = nil
	return statement
}

// writeInsertedRow stores one assembled destination row and applies the
// statement's duplicate-key policy. Conflicts are resolved against the statement
// child transaction, so rows written earlier in the same statement are visible:
// multi-row IGNORE/REPLACE and ON DUPLICATE KEY UPDATE behave like single-row
// statements repeated in order, as the legacy executor does.
func writeInsertedRow(ctx context.Context, write storageengine.Txn, session *Session, target *insertTarget, mode insertMode, row storage.Row, ordinal uint64) (insertOutcome, error) {
	fallback := fmt.Sprintf("%s/%020d", write.ID(), ordinal)
	if !mode.ignore && !mode.replace && len(mode.onDuplicate) == 0 {
		// Plain inserts keep the original path: a duplicate error aborts the
		// statement, and the child transaction rollback discards any partial state.
		if err := writeVersionedRow(ctx, write, target.definition, nil, nil, row, fallback); err != nil {
			return insertOutcome{}, err
		}
		return insertOutcome{affected: 1, inserted: true}, nil
	}
	conflicts, err := insertConflicts(write, target, row)
	if err != nil {
		return insertOutcome{}, err
	}
	if len(conflicts) == 0 {
		if err := writeVersionedRow(ctx, write, target.definition, nil, nil, row, fallback); err != nil {
			return insertOutcome{}, err
		}
		return insertOutcome{affected: 1, inserted: true}, nil
	}
	if len(mode.onDuplicate) > 0 {
		affected, err := updateDuplicateInsertRow(ctx, write, session, target, row, conflicts, mode.onDuplicate)
		return insertOutcome{affected: affected}, err
	}
	if mode.replace {
		return replaceDuplicateInsertRow(ctx, write, target, row, conflicts, fallback)
	}
	return insertOutcome{}, nil // IGNORE skips the conflicting row.
}

// insertConflicts lists rows that already occupy the candidate's primary key or
// any of its unique index values.
func insertConflicts(tx storageengine.Txn, target *insertTarget, candidate storage.Row) ([]insertConflict, error) {
	rows := tx.Table(target.definition.ID)
	seen := make(map[string]bool, 1)
	conflicts := make([]insertConflict, 0, 1)
	collect := func(owner []byte, exists bool) error {
		if !exists {
			return nil
		}
		identity := string(owner)
		if seen[identity] {
			return nil
		}
		encoded, exists, err := rows.Get(owner)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		decoded, err := decodeSQLRow(target.definition, encoded)
		if err != nil {
			return err
		}
		seen[identity] = true
		conflicts = append(conflicts, insertConflict{key: append([]byte(nil), owner...), row: decoded})
		return nil
	}
	if primary, ok := sqlPrimaryIndex(target.definition); ok {
		if key, keyOK := sqlPrimaryKey(target.definition, primary, candidate); keyOK {
			_, exists, err := rows.Get(key)
			if err != nil {
				return nil, err
			}
			if err := collect(key, exists); err != nil {
				return nil, err
			}
		}
	}
	for _, index := range target.definition.Definition.Indexes {
		if index.Primary || !index.Unique {
			continue
		}
		key, ok := storage.IndexValueKey(index, target.columns, candidate)
		if !ok {
			continue
		}
		owner, exists, err := rows.Index(index.Name, storageengine.UniqueIndex).Get([]byte(key))
		if err != nil {
			return nil, err
		}
		if err := collect(owner, exists); err != nil {
			return nil, err
		}
	}
	return conflicts, nil
}

// replaceDuplicateInsertRow applies REPLACE semantics: every conflicting row is
// removed and the candidate is stored, so the affected count is one per removed
// row plus the inserted row. A conflict on the candidate's own primary key is
// rewritten in place instead of deleted and re-inserted, which keeps rows that
// reference it satisfying RESTRICT/NO ACTION exactly like the legacy executor's
// single staged delete+insert mutation.
// replaceDuplicateInsertRow applies REPLACE semantics: every conflicting row is
// removed and the candidate is stored, so the affected count is one per removed
// row plus the inserted row. Referenced conflict rows are reported exactly like
// the legacy executor, which rejects replacing a row that child rows reference.
func replaceDuplicateInsertRow(ctx context.Context, write storageengine.Txn, target *insertTarget, candidate storage.Row, conflicts []insertConflict, fallback string) (insertOutcome, error) {
	outcome := insertOutcome{affected: len(conflicts) + 1}
	for _, conflict := range conflicts {
		if err := writeVersionedRow(ctx, write, target.definition, conflict.key, conflict.row, nil, ""); err != nil {
			if errors.Is(err, storage.ErrForeignKey) {
				return insertOutcome{}, storage.ErrForeignKeyReferenced
			}
			return insertOutcome{}, err
		}
	}
	if err := writeVersionedRow(ctx, write, target.definition, nil, nil, candidate, fallback); err != nil {
		return insertOutcome{}, err
	}
	outcome.inserted = true
	return outcome, nil
}

// updateDuplicateInsertRow implements ON DUPLICATE KEY UPDATE on the first
// conflicting row. Assignments are evaluated like the legacy executor: plain
// column names read the row being updated (including earlier assignments in the
// same list) and VALUES(column) reads the candidate row.
func updateDuplicateInsertRow(ctx context.Context, write storageengine.Txn, session *Session, target *insertTarget, candidate storage.Row, conflicts []insertConflict, assignments []parser.InsertAssignment) (int, error) {
	conflict := conflicts[0]
	columns := target.columns
	updated := append(storage.Row(nil), conflict.row...)
	for _, assignment := range assignments {
		position, ok := insertColumnPosition(target, assignment.Column)
		if !ok {
			return 0, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, assignment.Column)
		}
		value, err := evaluateExprWithLookup(assignment.Value, func(name string) (any, error) {
			if name == sessionLookupIdentifier {
				return session, nil
			}
			if session != nil && strings.EqualFold(name, "LAST_INSERT_ID()") {
				return int64(session.LastInsertID), nil
			}
			if strings.HasPrefix(strings.ToUpper(name), "VALUES(") {
				columnName := strings.TrimSuffix(strings.TrimPrefix(name, "VALUES("), ")")
				index, ok := insertColumnPosition(target, columnName)
				if !ok {
					return nil, fmt.Errorf("%w: %s", storage.ErrColumnNotFound, columnName)
				}
				return jsonColumnValue(columns[index], candidate[index]), nil
			}
			index, ok := insertColumnPosition(target, name)
			if !ok {
				return nil, fmt.Errorf("unknown column %s", name)
			}
			return jsonColumnValue(columns[index], updated[index]), nil
		})
		if err != nil {
			return 0, err
		}
		converted, err := interfaceToColumnValue(value, columns[position])
		if err != nil {
			return 0, err
		}
		updated[position] = converted
	}
	if err := writeVersionedRow(ctx, write, target.definition, conflict.key, conflict.row, updated, ""); err != nil {
		return 0, err
	}
	return 1, nil
}

func insertColumnPosition(target *insertTarget, name string) (int, bool) {
	unqualified := stripQualifier(name)
	for index, column := range target.columns {
		if strings.EqualFold(stripQualifier(column.Name), unqualified) {
			return index, true
		}
	}
	return 0, false
}

var _ = errors.Is
