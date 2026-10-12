package executor

import (
	"context"
	"errors"
	"fmt"
	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"strings"
)

const maxForeignKeyCascadeDepth = 32

// normalizeSQLReferentialAction matches the storage layer's normalization.
func normalizeSQLReferentialAction(action string) string {
	return strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(action, "_", " ")))
}

// applyForeignKeyActions performs the ON DELETE / ON UPDATE actions of every
// foreign key that references this row, before the row itself is written. Child
// mutations stay inside the caller's statement child transaction, so a failure
// anywhere in the cascade rolls the whole original statement back.
//
// locks is the *statement's* lock scope, the same one the explicit statement used: the derived writes
// reach child rows through the ordinary index and primary identities, so they claim their resources
// through the same helpers instead of writing around the lock manager. A nil scope means the backend
// has no lock manager, in which case the derived write keeps the concurrency control that backend has.
func applyForeignKeyActions(ctx context.Context, write storageengine.Txn, session *Session, locks *dmlLockScope, table versionedTable, oldRow, newRow storage.Row, visiting map[string]bool, depth int) error {
	if oldRow == nil {
		return nil
	}
	if session != nil && session.ForeignKeyChecksDisabled {
		return nil
	}
	if depth >= maxForeignKeyCascadeDepth {
		return fmt.Errorf("%w: foreign key cascade exceeds %d levels", storage.ErrForeignKey, maxForeignKeyCascadeDepth)
	}
	refs, err := sqlForeignReferrers(write, table)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		child, _, _, loadErr := loadVersionedTable(write, &Session{}, ref)
		if errors.Is(loadErr, storage.ErrTableNotFound) || errors.Is(loadErr, storage.ErrDatabaseNotFound) {
			continue
		}
		if loadErr != nil {
			return loadErr
		}
		for _, foreignKey := range child.Definition.ForeignKeys {
			if !strings.EqualFold(foreignKey.RefTable, table.CatalogName) {
				continue
			}
			action := normalizeSQLReferentialAction(foreignKey.OnUpdate)
			if newRow == nil {
				action = normalizeSQLReferentialAction(foreignKey.OnDelete)
			}
			if action != "CASCADE" && action != "SET NULL" {
				continue
			}
			index, indexErr := sqlFKIndex(table, foreignKey)
			if indexErr != nil {
				return indexErr
			}
			oldKey, ok := storage.IndexValueKey(index, table.Definition.Columns, oldRow)
			if !ok {
				continue
			}
			if newRow != nil {
				nextKey, nextOK := storage.IndexValueKey(index, table.Definition.Columns, newRow)
				if !nextOK || nextKey == oldKey {
					continue
				}
			}
			if err := cascadeForeignKeyAction(ctx, write, session, locks, table, child, foreignKey, oldKey, newRow, action, visiting, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

// cascadeForeignKeyAction applies one action to every child row that references
// the parent's old key. Matching rows are collected first so the child table is
// never mutated while it is being scanned.
func cascadeForeignKeyAction(ctx context.Context, write storageengine.Txn, session *Session, locks *dmlLockScope, parent, child versionedTable, foreignKey storage.ForeignKey, oldKey string, newRow storage.Row, action string, visiting map[string]bool, depth int) error {
	childPositions := make([]int, len(foreignKey.Columns))
	for index, name := range foreignKey.Columns {
		position := sqlColumnPosition(child, name)
		if position < 0 {
			return storage.ErrColumnNotFound
		}
		childPositions[index] = position
	}
	type childTarget struct {
		key []byte
		row storage.Row
	}
	targets := make([]childTarget, 0, 4)
	iterator, err := openAccessIterator(ctx, write, child, sqlAccessPlan{kind: sqlAccessAll})
	if err != nil {
		return err
	}
	for iterator.Next() {
		row, decodeErr := decodeSQLRow(child, iterator.Value())
		if decodeErr != nil {
			iterator.Close()
			return decodeErr
		}
		if !childReferencesKey(child, parent, foreignKey, row, oldKey) {
			continue
		}
		targets = append(targets, childTarget{key: append([]byte(nil), iterator.Key()...), row: row})
	}
	if err = iterator.Close(); err != nil {
		return err
	}
	for _, target := range targets {
		identity := child.ID + "\x00" + string(target.key)
		if visiting != nil && visiting[identity] {
			continue
		}
		if visiting == nil {
			visiting = make(map[string]bool)
		}
		visiting[identity] = true
		updated := append(storage.Row(nil), target.row...)
		switch action {
		case "CASCADE":
			if newRow != nil {
				for index := range foreignKey.Columns {
					parentPosition := sqlColumnPosition(parent, foreignKey.RefColumns[index])
					if parentPosition < 0 || parentPosition >= len(newRow) {
						return storage.ErrColumnNotFound
					}
					updated[childPositions[index]] = newRow[parentPosition]
				}
			}
		case "SET NULL":
			for index := range childPositions {
				updated[childPositions[index]] = storage.NullValue(child.Definition.Columns[childPositions[index]].Type)
			}
		}
		// A derived write is still a write. CASCADE and SET NULL reach child rows through the same
		// index entries and primary identities an explicit statement would, so they claim the same
		// resources through the statement's own lock scope: a derived write that wrote without claiming
		// would change rows an explicit writer is holding a lock on, which is precisely the bypass the
		// lock manager exists to prevent.
		if err := lockCascadeTarget(ctx, write, locks, child, target.key, target.row, updated); err != nil {
			return err
		}
		if action == "CASCADE" && newRow == nil {
			if err := applyForeignKeyActions(ctx, write, session, locks, child, target.row, nil, visiting, depth+1); err != nil {
				return err
			}
			if err := writeVersionedRow(ctx, write, child, target.key, target.row, nil, ""); err != nil {
				return err
			}
			continue
		}
		if err := applyForeignKeyActions(ctx, write, session, locks, child, target.row, updated, visiting, depth+1); err != nil {
			return err
		}
		if err := writeVersionedRowUnchecked(ctx, write, child, target.key, target.row, updated, ""); err != nil {
			return err
		}
	}
	return nil
}

// lockCascadeTarget claims one child row and its index entries before a derived write touches it.
//
// It reuses the lock components of the ordinary write path, so a cascaded row and an explicitly
// written row meet on exactly the same resources. The old identity is locked first (the row the
// derivation observed), then the replacement's unique reservations: a SET NULL or CASCADE update frees
// a value and installs another, and both halves must be claimed exactly as an explicit UPDATE claims
// them.
func lockCascadeTarget(ctx context.Context, write storageengine.Txn, locks *dmlLockScope, child versionedTable, key []byte, oldRow, updated storage.Row) error {
	if locks == nil {
		// A backend without a lock manager keeps the concurrency control it always had.
		return nil
	}
	extra, err := indexEntryLocks(child, key, oldRow, false)
	if err != nil {
		return err
	}
	grant, err := locks.acquireRowLocksForKey(child, key, extra)
	if err != nil {
		return err
	}
	// The derived write was computed from the row this statement's snapshot read. Another transaction may
	// have committed a change to that row while this statement waited for the claim — including deleting
	// it — and writing the derivation over that would discard the commit (or write the row back into
	// existence). The contract's answer for a target whose newest committed change is newer than the
	// statement's readTS is the retryable conflict (FR-017/FR-018), reported here before any derived
	// write. The check is on the row's *version*, not its bytes: a child rewritten and restored while the
	// statement waited is byte-identical and still stale.
	//
	// The statement's own context is used, so a cancelled statement stops waiting for this verification
	// instead of running it to completion on a background context.
	if oldRow != nil {
		observation, err := locks.readLockedValue(ctx, grant, sqllayout.Rows(child.ID), key)
		if err != nil {
			return err
		}
		if staleTargetObservation(write, observation) {
			return staleTargetConflict()
		}
	}
	if updated == nil {
		return nil
	}
	// The derived replacement is an UPDATE onto the child table, so it must claim its new identity
	// and index entries through the same rules an explicit UPDATE uses: moving the child's primary key
	// claims the insertion intention at that new key (so an interval covering the insertion point blocks
	// the cascade), unchanged index entries claim no intention, and unique reservations are verified.
	return claimModifyReplacement(ctx, locks, write, child, key, oldRow, updated)
}

func childReferencesKey(child, parent versionedTable, foreignKey storage.ForeignKey, row storage.Row, oldKey string) bool {
	index, err := sqlFKIndex(parent, foreignKey)
	if err != nil {
		return false
	}
	candidate := make(storage.Row, len(parent.Definition.Columns))
	for position := range candidate {
		candidate[position] = storage.NullValue(parent.Definition.Columns[position].Type)
	}
	for position, name := range foreignKey.Columns {
		childPosition := sqlColumnPosition(child, name)
		parentPosition := sqlColumnPosition(parent, foreignKey.RefColumns[position])
		if childPosition < 0 || parentPosition < 0 || row[childPosition].Null {
			return false
		}
		candidate[parentPosition] = row[childPosition]
	}
	key, ok := storage.IndexValueKey(index, parent.Definition.Columns, candidate)
	return ok && key == oldKey
}
