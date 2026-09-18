package physical

import "encoding/binary"

// RowIdentity is the stable identity of one physical storage row inside one target
// table. It is the dedup key vocabulary of the unified Modify Pipeline: an operator
// only ever needs "which physical row", never the statement family that produced it,
// so the identity model lives with the operator infrastructure and the executor
// binds statement-specific candidates onto it.
//
// Identity rules:
//
//	TableID equal AND Key equal  =>  the same mutation target
//	TableID differs              =>  different targets, even when Key bytes match
//
// Key is the *physical* storage key, never a logical value:
//
//   - a table with a primary key uses the encoded primary key the write path
//     already computes, so a primary-key UPDATE keeps addressing the old physical
//     row;
//   - a table without a primary key uses the storage row key its scan observed, so
//     two heap rows that hold identical values remain two distinct targets;
//   - outer-join NULL extension has no physical provenance, so it has no identity
//     (see Valid) instead of an identity inferred from NULL column values.
//
// Ownership: Key is owned by the RowIdentity and must never alias a borrowed
// scanner/decoder buffer that dies with the Yield callback. Build identities from a
// borrowed key with NewRowIdentity or Clone, both of which copy. Tables that already
// hold a scanner-owned key (for example a hidden join identity column) must copy
// explicitly as well; see cloneKey.
type RowIdentity struct {
	// TableID identifies the target table. Runtime uses the versioned table
	// identifier, the same value the write path keys tables by, so identities from
	// different tables can never collapse into one another.
	TableID string
	// Key is the owned physical storage key of the target row. It is nil when the
	// identity carries no provenance, which IsTarget rejects.
	Key []byte
	// Valid reports whether this identity denotes a real physical row. It is false
	// for outer-join NULL-extension rows, derived/CTE/view rows, and rows whose
	// physical provenance was lost. Invalid identities must never produce a
	// mutation target, so callers check Valid before writing or deleting.
	Valid bool
}

// NewRowIdentity builds an identity that owns a private copy of key.
//
// The copy is what makes an identity safe to retain past the Yield callback that
// produced the borrowed key. An empty key denotes lost physical provenance, so an
// empty or nil key normalises to a nil key, which IsTarget then rejects.
func NewRowIdentity(tableID string, key []byte) RowIdentity {
	if len(key) == 0 {
		return RowIdentity{TableID: tableID, Valid: true}
	}
	return RowIdentity{TableID: tableID, Key: cloneKey(key), Valid: true}
}

// Clone returns an identity that shares nothing mutable with r.
//
// Retaining operators (staging, target dedup, multi-target ordering) must clone an
// identity they keep, exactly as they clone a retained row.
func (r RowIdentity) Clone() RowIdentity {
	return RowIdentity{TableID: r.TableID, Key: cloneKey(r.Key), Valid: r.Valid}
}

// IsTarget reports whether the identity can drive a mutation. An invalid or
// keyless identity denotes "no physical row here" rather than a target row whose
// columns happen to be NULL.
//
// A zero-length key is not a target either: every storage key this runtime produces
// is non-empty, so an empty key can only mean lost provenance, and treating it as a
// target would let every such row collapse into one mutation.
func (r RowIdentity) IsTarget() bool {
	return r.Valid && len(r.Key) > 0
}

// StorageKey returns the dedup ledger key for this identity, or ok=false when the
// identity is not a usable target.
//
// The encoding is length-prefixed rather than a plain TableID + separator + Key
// concatenation, so a table identifier can never absorb bytes of the storage key
// and make two different targets compare equal:
//
//	[uint32 len(TableID)][TableID][Key]
//
// The result is a fresh slice owned by the caller.
func (r RowIdentity) StorageKey() ([]byte, bool) {
	if !r.IsTarget() {
		return nil, false
	}
	key := make([]byte, 4+len(r.TableID)+len(r.Key))
	binary.BigEndian.PutUint32(key, uint32(len(r.TableID)))
	offset := copy(key[4:], r.TableID)
	copy(key[4+offset:], r.Key)
	return key, true
}

// cloneKey copies a borrowed storage key into a slice the caller owns. It is the
// single copy boundary for retained identities, so a later scan reusing its
// iterator buffer cannot mutate a stored identity underneath its owner. The copy
// always allocates, so an empty copy shares no array with the borrowed key.
func cloneKey(key []byte) []byte {
	if key == nil {
		return nil
	}
	return append(make([]byte, 0, len(key)), key...)
}
