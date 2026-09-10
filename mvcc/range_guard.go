package mvcc

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const rangeGuardSpace = "_mvcc_range_guard"

var rangeGuardPrefix = []byte(rangeGuardSpace + "\x00")

type rangeDependency struct {
	Space  string
	Bounds KeyRange
}

// GuardRange validates phantom and row changes within precise logical bounds.
// Whole-space guards retain their historical encoding. Bounded guards are
// check-only operations, using a versioned key; row and backup formats are unchanged.
func (t *Tx) GuardRange(space string, bounds KeyRange) error {
	if space == rangeGuardSpace {
		return fmt.Errorf("reserved guard namespace")
	}
	if err := validateKey(space, bounds.Lower); err != nil {
		return err
	}
	if err := validateKey(space, bounds.Upper); err != nil {
		return err
	}
	if bounds.Lower == nil && bounds.Upper == nil {
		return t.Guard(rangeGuardSpace, []byte(space))
	}
	payload, err := json.Marshal(rangeDependency{space, bounds.CloneBounds()})
	if err != nil {
		return err
	}
	return t.Guard(rangeGuardSpace, append([]byte{0, 1}, payload...))
}
func decodeRangeDependency(key []byte) (rangeDependency, bool) {
	if len(key) > 0 && key[0] != 0 {
		return rangeDependency{Space: string(key)}, true
	}
	var d rangeDependency
	if len(key) < 2 || key[1] != 1 || json.Unmarshal(key[2:], &d) != nil {
		return d, false
	}
	return d, d.Space != ""
}
func rangeDependencyContains(guard, logical []byte) bool {
	d, ok := decodeRangeDependency(guard)
	if !ok {
		return true
	} // malformed dependencies fail closed
	prefix := append([]byte(d.Space), 0)
	return bytes.HasPrefix(logical, prefix) && d.Bounds.Contains(logical[len(prefix):])
}
func (r *visibilityReader) rangeVersion(k []byte) uint64 {
	d, ok := decodeRangeDependency(k[len(rangeGuardPrefix):])
	if !ok {
		return ^uint64(0)
	}
	prefix := append([]byte(d.Space), 0)
	start := append(bytes.Clone(prefix), d.Bounds.Lower...)
	c := r.data.Cursor()
	var latest uint64
	for key, _ := c.Seek(start); key != nil && bytes.HasPrefix(key, prefix); key, _ = c.Next() {
		rowKey := key[len(prefix):]
		if d.Bounds.Upper != nil && bytes.Compare(rowKey, d.Bounds.Upper) > 0 {
			break
		}
		if !d.Bounds.Contains(rowKey) {
			continue
		}
		_, version, _ := r.visible(key)
		latest = max(latest, version)
	}
	return latest
}
