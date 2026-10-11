package executor

import (
	"fmt"

	"gbaselite/storageengine"
)

// heap_key.go holds the single canonical heap-key encoding of a row that has no primary key.
//
// A heap table (one defined without a PRIMARY KEY) has no encoded primary-key identity, so the
// write path synthesises one from the write transaction's id and the ordinal of the row inside its
// statement. The lock path must synthesise exactly the same bytes: the lock identity and the
// written key have to name one resource, otherwise a record lock taken by one transaction does not
// conflict with the row another transaction is about to write, and the two writers never meet.
//
// Because the writer's rendering is the authoritative one, this file produces it with the same
// fmt verb the writer uses ("%020d") rather than with a hand-rolled digit loop. The verb zero-pads
// to at least 20 digits and never truncates: an ordinal that needs more than 20 digits keeps all
// of them, and a re-implementation that computed a fixed 20th digit from an integer power of ten
// would overflow long before that (pow10(19) does not fit in an int64 and mis-rendered every
// ordinal from 10^19 upward). Keeping one implementation removes the possibility of the two sides
// drifting apart again.

// heapKeyString builds the heap identity exactly as the insert path does.
//
// This is the canonical encoding: the write path (writeInsertedRow) and the lock path (heapKey)
// both go through it, so the bytes a lock is taken on are the bytes the row is stored under. The
// transaction id plus a "/" plus the 20-digit, zero-padded ordinal is the whole identity; no other
// form is valid for a heap row.
func heapKeyString(txID string, ordinal uint64) string {
	return fmt.Sprintf("%s/%020d", txID, ordinal)
}

// heapKey is the identity the write path synthesises for a table with no primary key.
//
// It returns the canonical heap-key bytes of heapKeyString, so a lock taken on it and the row the
// write installs address the same resource. Using anything else would protect a key the write
// never touches, which is precisely the inconsistency this encoding exists to prevent. A nil write
// has no transaction id to build an identity from, so the result is nil and the caller must refuse
// the mutation rather than guess one.
func heapKey(write storageengine.Txn, ordinal uint64) []byte {
	if write == nil {
		return nil
	}
	return []byte(heapKeyString(write.ID(), ordinal))
}
