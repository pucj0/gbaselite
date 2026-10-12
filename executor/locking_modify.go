package executor

import (
	"bytes"
	"context"

	"gbaselite/sqllayout"
	"gbaselite/storage"
	"gbaselite/storageengine"
)

// locking_modify.go owns the "claim the target, then verify under the claim" rule for the DML
// statements that rewrite or remove a row an earlier read selected.
//
// The rule exists because selecting a candidate and writing it are two different moments. The
// candidate comes from the statement's fixed parent snapshot (FR-017), so it describes the version
// the statement read. Between that read and the write another root may have committed a new version
// of the same row — and the statement, holding a snapshot older than that commit, cannot see it. A
// byte comparison against the freshly observed row answers the wrong question, because a row
// rewritten to another value and back is byte-identical while its version, and every index entry
// that named it, were replaced. The question is therefore asked of the version the storage layer
// recorded, which is what staleTargetObservation compares.
//
// Two halves, in a fixed order:
//
//   - claimModifyTarget claims what the statement is about to *remove* — the old row identity and
//     the index entries that named the old values — and verifies the row is still the version the
//     snapshot read. A target whose newest committed change is newer than the snapshot (or whose row
//     is gone, which a DELETE produces) is the retryable conflict FR-017/FR-018 prescribe.
//   - claimModifyReplacement claims what the statement is about to *install* — the new row identity
//     and the new index entries — and verifies that neither the new primary key nor any unique value
//     it installs is already owned by a different row.
//
// Neither half recomputes a SET expression or re-reads the joined source. A stale target ends the
// statement; it never rebases it onto a row the statement did not read (FR-017, current-read.md §A05).

// modifyClaim is what one successful target claim proves: the row identity the statement may act on.
//
// It carries the resolved key rather than the candidate's, because a candidate that left its
// identity empty (an UPDATE JOIN candidate, which is selected from the joined row and not from the
// target's own physical identity) is resolved here, and the write path must use the same key.
type modifyClaim struct {
	key []byte
}

// claimModifyTarget claims one modification target and verifies it against the statement snapshot.
//
// The row is read *under* the claim, so the verification cannot race the writer it is checking for:
// a root that is about to commit the newer version either has already released the row — in which
// case this read sees it — or is still holding it, in which case this call waited for that root and
// the read happens after its commit. That is the whole reason the verification follows the lock
// instead of preceding it.
func claimModifyTarget(ctx context.Context, locks *dmlLockScope, write storageengine.Txn, table versionedTable, key []byte, oldRow storage.Row) (modifyClaim, error) {
	if locks == nil {
		// The backend has no lock manager, so there is no claim to verify under and no current read
		// to take. The statement keeps the concurrency control its backend provides, exactly as it
		// did before row locking existed.
		return modifyClaim{key: key}, nil
	}
	if len(key) == 0 {
		// The candidate's encoded key is the identity the write path removes, so it is preferred over
		// re-deriving one from the row: a table with no primary key has no derivable key at all.
		derived, ok := primaryKeyBytes(table, oldRow)
		if !ok {
			return modifyClaim{}, errLockIdentityUnavailable
		}
		key = derived
	}
	// The old values are being removed, not created, so this half carries no insertion claim. An
	// interval that only blocks new keys must not hold up a removal.
	extra, err := indexEntryLocks(table, key, oldRow, false)
	if err != nil {
		return modifyClaim{}, err
	}
	grant, err := locks.acquireRowLocksForKey(table, key, extra)
	if err != nil {
		return modifyClaim{}, err
	}
	observation, err := locks.readLockedValue(ctx, grant, sqllayout.Rows(table.ID), key)
	if err != nil {
		return modifyClaim{}, err
	}
	if staleTargetObservation(write, observation) {
		return modifyClaim{}, staleTargetConflict()
	}
	return modifyClaim{key: key}, nil
}

// claimModifyReplacement claims the row an UPDATE is about to install and verifies its new identity
// and unique values are free.
//
// It runs for every candidate, not only one that moves its primary key, because the *index values*
// it installs can change while the key stays put: an UPDATE that writes a unique value another row
// already holds must meet that row's reservation, and an UPDATE that frees a value must still hold
// the reservation it took so a concurrent insert cannot slip in. A key-preserving candidate
// re-acquires components it already holds, which the manager answers as a re-entrant no-op, so the
// repeat costs no claim and cannot self-conflict.
//
// oldRow is the row as this statement read it, and it is what decides whether each index entry the
// replacement installs is a *new key* or the entry the row already had. The distinction is not a
// refinement: an entry whose key is unchanged is not an insertion, the write path updates it in place,
// and claiming an insertion intention for it would let an interval that only blocks new keys hold up a
// rewrite of an existing record. Passing the old row is therefore what keeps the interval rule honest
// about what it blocks.
func claimModifyReplacement(ctx context.Context, locks *dmlLockScope, write storageengine.Txn, table versionedTable, oldKey []byte, oldRow, updated storage.Row) error {
	if locks == nil {
		return nil
	}
	newKey, ok := primaryKeyBytes(table, updated)
	if !ok || len(newKey) == 0 {
		// The identity is not derivable from the replacement values, which for an UPDATE means the
		// key did not move: an UPDATE that leaves the primary key alone is already covered by the
		// claim taken for the target row, and its index entries are claimed below with that key.
		newKey = oldKey
	}
	if len(newKey) == 0 {
		return errLockIdentityUnavailable
	}
	moved := !bytes.Equal(newKey, oldKey)
	// Each entry carries the insertion half only when its key is one the old row did not have, so a
	// range another transaction protects over that index blocks a genuine new key and nothing else.
	extra, err := indexReplacementLocks(table, oldKey, newKey, oldRow, updated)
	if err != nil {
		return err
	}
	if moved {
		// The row itself is a new key only when the identity moved. A key-preserving rewrite leaves
		// an existing row where it is, and claiming an insertion there would let an interval that
		// protects the range around an untouched key hold up a rewrite of that key's own row.
		//
		// The intention is paired with the record lock the bundle already carries
		// (acquireRowLocksForKey adds the primary row component); the manager refuses an unpaired
		// intention, so the two halves cannot drift apart.
		extra = append(extra, insertionIntention(sqllayout.Rows(table.ID), table.ID, "", newKey)...)
	}
	grant, err := locks.acquireRowLocksForKey(table, newKey, extra)
	if err != nil {
		return err
	}
	if moved {
		// A moved primary key onto a row that already exists is the permanent duplicate, not a
		// retryable conflict: no amount of retrying frees that key.
		observation, err := locks.readLockedValue(ctx, grant, sqllayout.Rows(table.ID), newKey)
		if err != nil {
			return err
		}
		if observation.Exists {
			return storage.ErrDuplicateKey
		}
	}
	return verifyReplacementUniqueValues(ctx, locks, write, table, oldKey, newKey, updated, grant)
}

// verifyReplacementUniqueValues re-checks every unique value the replacement installs against the
// latest published reservation, under the claims this statement holds.
//
// The check the write path performs is a snapshot read, which is the wrong instrument after a wait:
// a root that committed the same unique value while this statement was blocked is not in this
// statement's snapshot, so the snapshot says the value is free while it is not. Asking the
// reservation itself is the only question that can see that root.
//
// A reservation naming *either* identity of the row being rewritten is that row's own reservation
// and not a conflict. Both identities have to be accepted, because a primary-key UPDATE rewrites the
// row under a new key while every unique value it did not touch keeps a reservation that still names
// the old key: the write path repoints those reservations as part of the same write, so treating the
// old owner as a foreign row would make every key-moving UPDATE of a row with an unchanged unique
// value fail on its own reservation.
//
// The answer for a genuine foreign owner distinguishes two cases, because they are different facts
// about the world:
//
//   - the owning row is observable in this statement's snapshot: the statement already sees the
//     duplicate, so this is the permanent constraint violation the caller has always reported;
//   - it is not observable: the duplicate was published after this statement's readTS. Rewriting on
//     top of it would rebase the statement onto a row it never read, which the B01 invariant
//     forbids, so the answer is the retryable conflict instead.
func verifyReplacementUniqueValues(ctx context.Context, locks *dmlLockScope, write storageengine.Txn, table versionedTable, oldKey, newKey []byte, updated storage.Row, grant storageengine.LockGrant) error {
	for _, index := range table.Definition.Indexes {
		if index.Primary || !index.Unique {
			continue
		}
		valueKey, ok := storage.IndexValueKey(index, table.Definition.Columns, updated)
		if !ok {
			// A value that does not constrain (a NULL part, an unresolvable column) reserves nothing
			// and therefore cannot conflict, exactly as it constrains nothing.
			continue
		}
		observation, err := locks.readLockedValue(ctx, grant, sqllayout.UniqueIndex(table.ID, index.Name), []byte(valueKey))
		if err != nil {
			return err
		}
		if !observation.Exists {
			continue
		}
		if bytes.Equal(observation.Value, newKey) || bytes.Equal(observation.Value, oldKey) {
			// The reservation is held by this very row, under the identity it is being rewritten to
			// or the one it is being rewritten from. Neither is a conflict with another row.
			continue
		}
		_, visible, err := write.Table(table.ID).Get(observation.Value)
		if err != nil {
			return err
		}
		if visible {
			return storage.ErrDuplicateKey
		}
		return staleTargetConflict()
	}
	return nil
}
