package mvcc

import (
	"bytes"

	bolt "go.etcd.io/bbolt"
)

// conflict.go holds the single commit-time conflict rule shared by every MVCC
// commit path:
//
//	latestCommittedChange(dependency) > readTS => ErrConflict
//
// A dependency is one Put or Delete (the write set) or one Guard/GuardRange (the
// dependency set). Ordinary reads -- Get, Scan, ScanRange, SELECT -- are never
// dependencies, so Snapshot Isolation is not upgraded to Serializable: two
// transactions that read overlapping data and write disjoint keys both commit.
//
// Only the *rule* is shared. Each commit path keeps its own physical scan,
// batching and durability: a bbolt read view, a bbolt update transaction, a
// private staging database, or a view layered with the changes accepted earlier in
// the same commit group. That is deliberate: unifying the rule must not force the
// optimized paths through one slow implementation.

// committedChangeLookup reports the newest committed change for one logical key.
//
// A lookup is scoped to the view or group that produced it and must not be used
// after that view closes.
type committedChangeLookup func(logicalKey []byte) uint64

// newViewChangeLookup returns a lookup backed by one bbolt view.
//
// Range-guard keys resolve to the newest committed change inside the guarded
// range, because visibilityReader.visible dispatches range-guard keys to
// rangeVersion. Point keys, including Guard(point), resolve to the newest
// committed version of that key.
func newViewChangeLookup(tx *bolt.Tx) committedChangeLookup {
	reader := newVisibilityReader(tx, ^uint64(0))
	return func(logicalKey []byte) uint64 {
		_, version, _ := reader.visible(logicalKey)
		return version
	}
}

// newGroupChangeLookup layers the changes accepted earlier in one commit group
// over a base lookup.
//
// Those changes are already committed decisions for the transactions that follow
// them in the same group, but on a path that validates before it installs (local
// WAL) they are not in the database yet. It also preserves the order a group
// already has: a guard validated before a later write in the same group does not
// see that write, exactly as the same-transaction install path behaves.
func newGroupChangeLookup(base committedChangeLookup, accepted map[string]uint64) committedChangeLookup {
	return func(logicalKey []byte) uint64 {
		latest := base(logicalKey)
		if index, ok := accepted[string(logicalKey)]; ok && index > latest {
			latest = index
		}
		if !bytes.HasPrefix(logicalKey, rangeGuardPrefix) {
			return latest
		}
		// A range guard must also see accepted changes inside its bounds. The guard
		// row key is everything after the reserved namespace prefix, which is what
		// decodeRangeDependency expects.
		guard := logicalKey[len(rangeGuardPrefix):]
		for changed, index := range accepted {
			if index > latest && rangeDependencyContains(guard, []byte(changed)) {
				latest = index
			}
		}
		return latest
	}
}

// conflictValidator applies the commit rule to one transaction's dependency set.
type conflictValidator struct {
	readTS uint64
	latest committedChangeLookup
	// baseline reports the version one key was observed at under its covering lock. It is nil for
	// every transaction that never took a locked current read, in which case the validator behaves
	// exactly as it always has.
	//
	// This is the B02 reconciliation of a locked current read with INV-005: the rule stays
	// "latest committed change must not be newer than what this transaction observed", but for a
	// key read under lock the observation is the version the reader actually saw rather than a
	// timestamp that may predate it. The lock guarantees nothing changed between the observation
	// and the commit, so the check remains a true lost-update signal.
	baseline func(logicalKey []byte) (uint64, bool)
}

func newConflictValidator(readTS uint64, latest committedChangeLookup) conflictValidator {
	return conflictValidator{readTS: readTS, latest: latest}
}

// withBaseline returns a validator that consults a per-key observation baseline.
//
// A key with no recorded baseline is validated against readTS exactly as before, so attaching a
// baseline can only ever make a locked, observed write succeed; it never changes the verdict for a
// key the transaction did not read under lock.
func (v conflictValidator) withBaseline(baseline func(logicalKey []byte) (uint64, bool)) conflictValidator {
	v.baseline = baseline
	return v
}

// validateKey reports ErrConflict when the newest committed change of one dependency is newer
// than what this transaction is entitled to build on: the version it observed under lock when it
// has one, and its read timestamp otherwise.
func (v conflictValidator) validateKey(logicalKey []byte) error {
	latest := v.latest(logicalKey)
	if v.baseline != nil {
		if observed, ok := v.baseline(logicalKey); ok {
			// The key was read under its covering lock, so this transaction may write the version
			// it observed. Anything newer means the lock was lost or the row changed underneath it,
			// which is still a conflict.
			if latest > observed {
				return ErrConflict
			}
			return nil
		}
	}
	if latest > v.readTS {
		return ErrConflict
	}
	return nil
}

// validateOps applies the rule to every dependency of one write set, in order,
// stopping at the first conflict.
func (v conflictValidator) validateOps(ops []Op) error {
	for _, op := range ops {
		logicalKey, err := key(op.Space, op.Key)
		if err != nil {
			return err
		}
		if err := v.validateKey(logicalKey); err != nil {
			return err
		}
	}
	return nil
}
