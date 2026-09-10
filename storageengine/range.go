package storageengine

import "bytes"

// Contains applies bytewise inclusive/exclusive bounds. Empty non-nil differs
// from nil. Reversed/inverted intervals naturally contain no keys.
func (r KeyRange) Contains(key []byte) bool {
	if r.Lower != nil {
		c := bytes.Compare(key, r.Lower)
		if c < 0 || c == 0 && !r.LowerInclusive {
			return false
		}
	}
	if r.Upper != nil {
		c := bytes.Compare(key, r.Upper)
		if c > 0 || c == 0 && !r.UpperInclusive {
			return false
		}
	}
	return true
}

// CloneBounds owns the endpoints and discards scan-only fields.
func (r KeyRange) CloneBounds() KeyRange {
	return KeyRange{Lower: bytes.Clone(r.Lower), Upper: bytes.Clone(r.Upper), LowerInclusive: r.LowerInclusive, UpperInclusive: r.UpperInclusive}
}
