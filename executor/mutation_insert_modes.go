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
	// uniqueIndex is the position of the unique index whose value conflict this is, or -1 when the
	// conflict is the candidate's primary key. It is what lets the locked re-check ask the *reservation*
	// of that value rather than re-reading the row the snapshot named.
	uniqueIndex int
	// valueKey is the encoded unique value the reservation is stored under, set for a unique conflict.
	valueKey []byte
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
//
// locks is the statement's lock scope. The duplicate variants rewrite or remove rows a plain INSERT
// never touches, so they claim the rows they change through the same helpers instead of writing around
// the lock manager; IGNORE writes nothing and therefore claims nothing beyond the insertion claim the
// caller already took.
func writeInsertedRow(ctx context.Context, write storageengine.Txn, session *Session, locks *dmlLockScope, target *insertTarget, mode insertMode, row storage.Row, ordinal uint64, conflicts []insertConflict) (insertOutcome, error) {
	// The fallback heap identity comes from the one canonical encoder, so the key this row is
	// stored under and the key the lock path protects cannot drift apart.
	fallback := heapKeyString(write.ID(), ordinal)
	if !mode.ignore && !mode.replace && len(mode.onDuplicate) == 0 {
		// Plain inserts keep the original path: a duplicate error aborts the
		// statement, and the child transaction rollback discards any partial state.
		if err := writeVersionedRow(ctx, write, target.definition, nil, nil, row, fallback); err != nil {
			return insertOutcome{}, err
		}
		return insertOutcome{affected: 1, inserted: true}, nil
	}
	// The conflicts are the ones the caller re-derived from the current state under the statement's
	// claims, so a value another transaction released while this statement waited is not among them.
	if len(conflicts) == 0 {
		if err := writeVersionedRow(ctx, write, target.definition, nil, nil, row, fallback); err != nil {
			return insertOutcome{}, err
		}
		return insertOutcome{affected: 1, inserted: true}, nil
	}
	if len(mode.onDuplicate) > 0 {
		affected, err := updateDuplicateInsertRow(ctx, write, session, locks, target, row, conflicts, mode.onDuplicate)
		return insertOutcome{affected: affected}, err
	}
	if mode.replace {
		return replaceDuplicateInsertRow(ctx, write, session, locks, target, row, conflicts, fallback)
	}
	return insertOutcome{}, nil // IGNORE skips the conflicting row.
}

// insertConflicts lists rows that already occupy the candidate's primary key or
// any of its unique index values.
func insertConflicts(tx storageengine.Txn, target *insertTarget, candidate storage.Row) ([]insertConflict, error) {
	rows := tx.Table(target.definition.ID)
	seen := make(map[string]bool, 1)
	conflicts := make([]insertConflict, 0, 1)
	collect := func(owner []byte, exists bool, uniqueIndex int, valueKey []byte) error {
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
		conflicts = append(conflicts, insertConflict{
			key:         append([]byte(nil), owner...),
			row:         decoded,
			uniqueIndex: uniqueIndex,
			valueKey:    append([]byte(nil), valueKey...),
		})
		return nil
	}
	if primary, ok := sqlPrimaryIndex(target.definition); ok {
		if key, keyOK := sqlPrimaryKey(target.definition, primary, candidate); keyOK {
			_, exists, err := rows.Get(key)
			if err != nil {
				return nil, err
			}
			if err := collect(key, exists, -1, nil); err != nil {
				return nil, err
			}
		}
	}
	for position, index := range target.definition.Definition.Indexes {
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
		if err := collect(owner, exists, position, []byte(key)); err != nil {
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
func replaceDuplicateInsertRow(ctx context.Context, write storageengine.Txn, session *Session, locks *dmlLockScope, target *insertTarget, candidate storage.Row, conflicts []insertConflict, fallback string) (insertOutcome, error) {
	outcome := insertOutcome{affected: len(conflicts) + 1}
	for _, conflict := range conflicts {
		// REPLACE removes the conflicting row: the removal and the FK actions it triggers are writes
		// like any other and claim their resources through the statement's scope.
		if err := lockCascadeTarget(ctx, write, locks, target.definition, conflict.key, conflict.row, nil); err != nil {
			return insertOutcome{}, err
		}
		if err := applyForeignKeyActions(ctx, write, session, locks, target.definition, conflict.row, nil, nil, 0); err != nil {
			return insertOutcome{}, err
		}
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
//
// The conflict that brought the statement here was found *before* anything was locked, so it describes
// a version of the row rather than the row itself. The order below is therefore: claim the row, re-read
// it under the claim, evaluate the assignments against what is actually there, claim the resources the
// new values install, run the referential actions of those values, and only then write. Evaluating
// against the pre-lock row would write over a change another session committed while this statement
// waited, and skipping the referential actions is what left a cascading child behind the old key.
func updateDuplicateInsertRow(ctx context.Context, write storageengine.Txn, session *Session, locks *dmlLockScope, target *insertTarget, candidate storage.Row, conflicts []insertConflict, assignments []parser.InsertAssignment) (int, error) {
	conflict := conflicts[0]
	currentRow := conflict.row
	if locks != nil {
		grant, err := locks.acquireRowLocksForKey(target.definition, conflict.key, nil)
		if err != nil {
			return 0, err
		}
		observation, err := locks.readLockedValue(ctx, grant, sqllayout.Rows(target.definition.ID), conflict.key)
		if err != nil {
			return 0, err
		}
		// The row is read under the claim to answer one question: is it still the *version* this
		// statement's snapshot read? Byte equality is not that question — a row changed and changed back
		// is byte-identical — so the answer comes from the observation's own version metadata
		// (staleTargetObservation). A stale target is the retryable conflict FR-017/FR-018 prescribe, and
		// re-evaluating on the new version would silently rebase the statement onto data it never read.
		if staleTargetObservation(write, observation) {
			return 0, storageengine.ErrConflict
		}
	}
	columns := target.columns
	updated := append(storage.Row(nil), currentRow...)
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
	// The values the rewrite installs are known now, so the resources *they* need are claimed too: the
	// new primary key and every index entry of the new row, each insertion claim paired with the record
	// claim it belongs to (indexEntryLocks builds that pairing) so a concurrent range cannot slip past.
	if err := lockCascadeTarget(ctx, write, locks, target.definition, conflict.key, currentRow, updated); err != nil {
		return 0, err
	}
	// The referential actions of the values this rewrite installs: an upsert that moves a referenced key
	// must cascade exactly like the equivalent UPDATE, or child rows keep pointing at a key that no
	// longer exists.
	if err := applyForeignKeyActions(ctx, write, session, locks, target.definition, currentRow, updated, nil, 0); err != nil {
		return 0, err
	}
	if err := writeVersionedRow(ctx, write, target.definition, conflict.key, currentRow, updated, ""); err != nil {
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
