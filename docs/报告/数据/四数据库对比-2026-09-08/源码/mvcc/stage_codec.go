package mvcc

import (
	"bytes"
	"errors"
)

// Temporary write sets are discarded on restart. The namespace/key already
// lives in the B+tree key; only a version, flags and raw value are stored here.
// This changes neither persistent pending JSON nor the Raft wire format.
func encodeStagedOp(op Op) []byte {
	flags := byte(0)
	if op.Delete {
		flags |= 1
	}
	if op.Check {
		flags |= 2
	}
	value := make([]byte, 2+len(op.Value))
	value[0], value[1] = 1, flags
	copy(value[2:], op.Value)
	return value
}

// stagedOpHeader validates the existing temporary format without copying payloads.
func stagedOpHeader(k, value []byte) (int, error) {
	split := bytes.IndexByte(k, 0)
	if split < 1 || split > 1024 || len(k)-split-1 > 8192 || len(value) < 2 || value[0] != 1 || value[1] > 3 || len(value)-2 > MaxValueBytes {
		return 0, errors.New("invalid temporary MVCC write encoding")
	}
	return split, nil
}
func decodeStagedOp(k, value []byte) (Op, error) {
	split, err := stagedOpHeader(k, value)
	if err != nil {
		return Op{}, err
	}
	return Op{Space: string(k[:split]), Key: bytes.Clone(k[split+1:]), Value: bytes.Clone(value[2:]), Delete: value[1]&1 != 0, Check: value[1]&2 != 0}, nil
}
