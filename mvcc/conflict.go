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
}

func newConflictValidator(readTS uint64, latest committedChangeLookup) conflictValidator {
	return conflictValidator{readTS: readTS, latest: latest}
}

// validateKey reports ErrConflict when the newest committed change of one
// dependency is newer than the transaction's read timestamp.
func (v conflictValidator) validateKey(logicalKey []byte) error {
	if v.latest(logicalKey) > v.readTS {
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
