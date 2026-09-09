package mvcc

import (
	"bytes"
	"fmt"
)

const rangeGuardSpace = "_mvcc_range_guard"

var rangeGuardPrefix = []byte(rangeGuardSpace + "\x00")

// GuardRange validates that no committed row in a namespace changed after this
// transaction's snapshot, including inserts and deletes. Intended for DDL/FK
// validation; ordinary row writes do not acquire a table-wide conflict key.
func (t *Tx) GuardRange(space string) error {
	if space == rangeGuardSpace {
		return fmt.Errorf("reserved guard namespace")
	}
	if err := validateKey(space, nil); err != nil {
		return err
	}
	return t.Guard(rangeGuardSpace, []byte(space))
}
func (r *visibilityReader) rangeVersion(k []byte) uint64 {
	prefix := append(bytes.Clone(k[len(rangeGuardPrefix):]), 0)
	c := r.data.Cursor()
	var latest uint64
	for key, _ := c.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = c.Next() {
		_, version, _ := r.visible(key)
		latest = max(latest, version)
	}
	return latest
}
