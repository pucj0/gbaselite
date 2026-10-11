package mvcc

import (
	"context"
	"errors"

	"gbaselite/lockmanager"
	bolt "go.etcd.io/bbolt"
)

// errBaselineNeedsLocalValidation reports that a transaction carrying a locked-read observation
// baseline cannot be published through the replicated apply path.
//
// It is a fail-closed answer, not a limitation the caller may ignore: the durable validator on
// that path sees only the replicated command, so it has no way to check the version the reader
// observed under lock. Accepting the write anyway would be exactly the "durable layer exemption"
// the contract forbids.
var errBaselineNeedsLocalValidation = errors.New("a locked current read with a recorded observation must be validated locally")

// current_read.go implements the locked current read: the read mode a SELECT ... FOR UPDATE /
// FOR SHARE uses, as distinct from the snapshot read a plain SELECT uses.
//
// Two rules define it, and both come from contracts/current-read.md:
//
//  1. A current read observes the *latest published* version (plus this transaction's own
//     unmerged writes), not the version the transaction's readTS pins. The transaction's
//     readTS, Snapshot, GC horizon and Command.Snapshot are never modified, so every other
//     read in the same transaction keeps exact snapshot semantics.
//
//  2. An observation taken while holding the covering lock records a *per-key write baseline*.
//     That baseline is what reconciles the current read with the B01 commit rule: a later write
//     to the same key is validated against the version this transaction actually observed under
//     lock, instead of being rejected because the row changed after readTS.
//
// Rule 2 is a refinement of INV-005 rather than a weakening of it. The recorded version is by
// construction the latest published version at observation time, and the lock the observation was
// taken under prevents any other transaction from changing that key until this root's terminal
// state. So "latest committed change > baseline" remains exactly the lost-update signal it always
// was; what changed is that for a locked key the baseline is the observed version rather than a
// timestamp that may predate the observation. A key with no recorded baseline keeps the original
// readTS comparison, so plain UPDATE/INSERT conflict detection is untouched.

// CurrentObservationSource says where one current observation's value came from.
//
// The three sources must stay distinguishable: "this transaction wrote it and has not merged"
// is not the same as "the database published it", and neither is the same as "the row does not
// exist". A caller that collapsed them would be unable to tell an uncommitted own write from a
// committed one, or a tombstone from an absent row.
type CurrentObservationSource uint8

const (
	// CurrentAbsent means the key has no version at all: it was never published and this
	// transaction has not written it.
	CurrentAbsent CurrentObservationSource = iota
	// CurrentCommitted means the value is the latest published version.
	CurrentCommitted
	// CurrentOwnOverlay means the value comes from this transaction's own write set or one of its
	// ancestors' write sets. It has no commit timestamp of its own.
	CurrentOwnOverlay
)

// CurrentObservation is one locked current read's result.
//
// It carries the change metadata a revalidation needs, not just the bytes: the caller has to be
// able to tell "deleted" from "never existed" and to compare the observed version with what it
// later validates.
type CurrentObservation struct {
	// Value is the observed value. It is a copy; the caller owns it.
	Value []byte
	// Exists reports whether the key exists after the observation.
	Exists bool
	// Tombstone reports that the key exists as a deletion marker: Exists is false but the key
	// has a change, which is how a delete is distinguished from a key that was never written.
	Tombstone bool
	// ChangeTS is the commit sequence that produced the observed published version, or zero when
	// the value came from this transaction's own overlay.
	ChangeTS uint64
	// HasChange reports whether ChangeTS/overlay information is meaningful. A key that never
	// existed has no change to report.
	HasChange bool
	// Source says which of the three cases produced Value.
	Source CurrentObservationSource
}

// ReadCurrentLocked reads the latest published state of one key while the caller holds the
// covering lock, and records the observed version as this key's write baseline.
//
// It must only be called after a successful Acquire for this key. The lock is what makes the
// recorded baseline sound, so a caller that observed without a lock would be recording a race.
//
// The read happens entirely through the store's snapshot API with an "everything published"
// snapshot, so the locking read never needs its own visibility code path and cannot drift from
// the ordinary read's rules about publication markers and tombstones.
func (t *Tx) ReadCurrentLocked(ctx context.Context, space string, rowKey []byte, grant lockmanager.LockGrant) (CurrentObservation, error) {
	return t.ReadCurrentLockedForOwner(ctx, space, rowKey, grant.OwnerID)
}

// ReadCurrentLockedForOwner is ReadCurrentLocked for a caller that holds the neutral capability
// rather than a lock-manager grant.
//
// It exists so the adapter can validate a grant without fabricating a lock-manager value: the
// only part of a grant this read needs is which root owns it, and that root is then checked
// against the live registry.
func (t *Tx) ReadCurrentLockedForOwner(ctx context.Context, space string, rowKey []byte, ownerID string) (CurrentObservation, error) {
	if err := t.checkOpen(); err != nil {
		return CurrentObservation{}, err
	}
	if err := ctx.Err(); err != nil {
		return CurrentObservation{}, err
	}
	grant := lockmanager.LockGrant{OwnerID: ownerID}
	if err := t.verifyGrantForRead(grant); err != nil {
		return CurrentObservation{}, err
	}
	// A read that waits must re-check the world after the wait, not before it. The context check
	// above and the grant check below are the two ends of that rule: nothing observed here can
	// survive a cancelled context or an invalidated grant.
	observation, err := t.readCurrent(space, rowKey)
	if err != nil {
		return CurrentObservation{}, err
	}
	// The grant is verified again *after* the read, so a value read from a generation that ended
	// mid-read is discarded rather than returned as current.
	if err := t.verifyGrantForRead(grant); err != nil {
		return CurrentObservation{}, err
	}
	t.recordWriteBaseline(space, rowKey, observation)
	return observation, nil
}

// VerifyCurrentRow re-reads one key under its lock and reports the observation.
//
// It is the revalidation step a caller uses after a wait: the identity, the WHERE condition and
// the visibility are all re-derived from this fresh observation rather than from whatever the
// caller had before it waited.
func (t *Tx) VerifyCurrentRow(ctx context.Context, space string, rowKey []byte, grant lockmanager.LockGrant) (CurrentObservation, error) {
	return t.ReadCurrentLocked(ctx, space, rowKey, grant)
}

// readCurrent resolves one key without recording a baseline, so a re-read does not record twice.
func (t *Tx) readCurrent(space string, rowKey []byte) (CurrentObservation, error) {
	// The transaction's own unmerged writes win over the published version, and a child sees its
	// ancestors' writes. A parent must never see an unmerged child, which falls out of walking
	// upward from this transaction only.
	if t.parent != nil {
		op, found, err := t.readOp(space, rowKey)
		if err != nil {
			return CurrentObservation{}, err
		}
		if found {
			return overlayObservation(op), nil
		}
		return t.parent.readCurrent(space, rowKey)
	}
	op, found, err := t.readOp(space, rowKey)
	if err != nil {
		return CurrentObservation{}, err
	}
	if found {
		return overlayObservation(op), nil
	}
	value, version, exists, err := t.store.latestPublished(space, rowKey)
	if err != nil {
		return CurrentObservation{}, err
	}
	return CurrentObservation{
		Value:     value,
		Exists:    exists,
		Tombstone: !exists && version != 0,
		ChangeTS:  version,
		HasChange: version != 0,
		Source:    CurrentCommitted,
	}, nil
}

// overlayObservation converts one of this transaction's own staged ops into an observation.
//
// An overlay value has no commit timestamp: it is unpublished by definition. ChangeTS stays zero
// and Source says where it came from, so the caller cannot mistake it for a published version.
func overlayObservation(op Op) CurrentObservation {
	if op.Check {
		// A guard is a declared dependency, not a value. It carries no observation.
		return CurrentObservation{Source: CurrentAbsent}
	}
	return CurrentObservation{
		Value:     append([]byte(nil), op.Value...),
		Exists:    !op.Delete,
		Tombstone: op.Delete,
		Source:    CurrentOwnOverlay,
	}
}

// latestPublished reads the newest published version of one key.
//
// "Published" means the version has a commit marker. An uncommitted version another transaction
// staged is invisible here, which is correct: this transaction holds the covering lock, so that
// other transaction cannot publish while the lock is held, and the observation is therefore not
// a dirty read.
func (s *Store) latestPublished(space string, rowKey []byte) ([]byte, uint64, bool, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	k, err := key(space, rowKey)
	if err != nil {
		return nil, 0, false, err
	}
	var value []byte
	var version uint64
	var exists bool
	err = s.db.View(func(tx *bolt.Tx) error {
		v, n, ok := visible(tx, k, ^uint64(0))
		value = append([]byte(nil), v...)
		version, exists = n, ok
		return nil
	})
	return value, version, exists, err
}

// recordWriteBaseline records the version this transaction observed under lock, so a later write
// to the same key is validated against what it actually saw.
//
// A key observed as absent or as a tombstone records zero, which makes the baseline "no version",
// and any published change after that is then a genuine conflict: the row appeared underneath a
// caller that had locked it as missing.
func (t *Tx) recordWriteBaseline(space string, rowKey []byte, observation CurrentObservation) {
	k, err := key(space, rowKey)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	version := observation.ChangeTS
	if t.writeBaseline == nil {
		t.writeBaseline = make(map[string]uint64, 8)
	}
	if current, ok := t.writeBaseline[string(k)]; ok {
		if current > version {
			// A baseline never moves backwards: an older observation must not undo a newer one.
			return
		}
		t.writeBaseline[string(k)] = version
		return
	}
	if len(t.writeBaseline) >= maxObservationBaselines {
		// The bound is reached, so this key keeps no baseline and a later write to it is validated
		// against readTS exactly as it was before B02. That is deliberately fail-closed: an
		// unrecorded observation can only make a write *more* likely to conflict, never less, so
		// hitting the bound cannot silently permit a stale write.
		t.baselineRejections++
		return
	}
	t.writeBaseline[string(k)] = version
}

// maxObservationBaselines bounds the per-key observation baselines one transaction may hold.
//
// A baseline is only recorded for a key the transaction successfully locked, so the natural
// bound is the lock budget; this cap is the smaller of the two and exists so the transaction's
// own state is bounded even when the lock budget is configured very large. Exceeding it is
// fail-closed, never a permit.
const maxObservationBaselines = 16384

// BaselineStats reports the bounded observation state: how many baselines this transaction holds
// and how many observations were dropped because the bound was reached.
func (t *Tx) BaselineStats() (held int, rejected uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.writeBaseline), t.baselineRejections
}

// writeBaselineFor reports the recorded baseline of one logical key.
func (t *Tx) writeBaselineFor(logicalKey []byte) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.writeBaseline == nil {
		return 0, false
	}
	version, ok := t.writeBaseline[string(logicalKey)]
	return version, ok
}

// baselineLookup returns the validator's per-key observation lookup, or nil when this
// transaction never took a locked current read.
//
// Returning nil rather than an empty lookup keeps the common case allocation-free and, more
// importantly, keeps the validator on its original code path so a transaction without locking
// reads is validated exactly as it always was.
func (t *Tx) baselineLookup() func(logicalKey []byte) (uint64, bool) {
	t.mu.Lock()
	empty := len(t.writeBaseline) == 0
	t.mu.Unlock()
	if empty {
		return nil
	}
	return t.writeBaselineFor
}

// HasWriteBaseline reports whether this transaction recorded at least one observation baseline.
func (t *Tx) HasWriteBaseline() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.writeBaseline) != 0
}

// WriteBaselineSize reports how many keys carry a recorded baseline.
//
// It is a diagnostics and test accessor: the map is bounded by the lock budget, because a
// baseline is only ever recorded for a key this transaction successfully locked.
func (t *Tx) WriteBaselineSize() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.writeBaseline)
}

// verifyGrantForRead re-validates one grant against this transaction and its owner.
//
// The check is deliberately about *this* transaction: a grant recorded for another owner must
// never authorise a read on this one, because two roots share no write set. The lock manager
// reports an unknown owner once the root has been released or its generation replaced, which is
// what makes a stale grant fail closed instead of returning a value that is no longer current.
func (t *Tx) verifyGrantForRead(grant lockmanager.LockGrant) error {
	manager, ok := t.LockManager()
	if !ok {
		return ErrClosed
	}
	ownerID := t.LockOwnerID()
	if grant.OwnerID != ownerID {
		return ErrConflict
	}
	if _, stillHeld := manager.Owner(ownerID); !stillHeld {
		return ErrConflict
	}
	return nil
}

// ReadCurrentUnlocked reads the latest published state without recording a baseline.
//
// It exists for a caller that needs to observe the current row for a decision that is not a
// write authorisation — deciding whether a row still matches a WHERE clause, for example. Without
// a lock there is nothing to make a baseline sound, so none is recorded.
func (t *Tx) ReadCurrentUnlocked(ctx context.Context, space string, rowKey []byte) (CurrentObservation, error) {
	if err := t.checkOpen(); err != nil {
		return CurrentObservation{}, err
	}
	if err := ctx.Err(); err != nil {
		return CurrentObservation{}, err
	}
	return t.readCurrent(space, rowKey)
}
