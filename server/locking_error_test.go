package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"gbaselite/protocol"
	"gbaselite/storageengine"
)

// locking_error_test.go is the T057 suite for MySQL client wire error code mapping and ErrorPacket encoding.

func TestT057LockingWireErrorPacketEncoding(t *testing.T) {
	// 1. Verify ErrorPacket binary layout for 1205 (Lock wait timeout exceeded)
	pkt1205 := protocol.ErrorPacket(1205, "Lock wait timeout exceeded; try restarting transaction")
	if len(pkt1205) < 9 || pkt1205[0] != 0xff {
		t.Fatalf("invalid MySQL Error packet payload marker: %x", pkt1205)
	}
	code1205 := binary.LittleEndian.Uint16(pkt1205[1:3])
	if code1205 != 1205 {
		t.Fatalf("expected wire error code 1205, got %d", code1205)
	}

	// 2. Verify ErrorPacket binary layout for 1213 (Deadlock / Conflict)
	pkt1213 := protocol.ErrorPacket(1213, "Deadlock found when trying to get lock; try restarting transaction")
	if len(pkt1213) < 9 || pkt1213[0] != 0xff {
		t.Fatalf("invalid MySQL Error packet payload marker: %x", pkt1213)
	}
	code1213 := binary.LittleEndian.Uint16(pkt1213[1:3])
	if code1213 != 1213 {
		t.Fatalf("expected wire error code 1213, got %d", code1213)
	}
}

func TestT057LockingWireErrorCodeMapping(t *testing.T) {
	cases := []struct {
		err  error
		want uint16
	}{
		{storageengine.ErrLockWaitTimeout, 1205},
		{fmt.Errorf("wrapped lock timeout: %w", storageengine.ErrLockWaitTimeout), 1205},
		{storageengine.ErrDeadlock, 1213},
		{fmt.Errorf("wrapped deadlock: %w", storageengine.ErrDeadlock), 1213},
		{storageengine.ErrConflict, 1213},
		{fmt.Errorf("wrapped conflict: %w", storageengine.ErrConflict), 1213},
		{errors.New("unrelated syntax error"), 1064},
	}

	for i, c := range cases {
		code := mysqlExecutionErrorCode(c.err)
		if code != c.want {
			t.Fatalf("case %d (%v): expected code %d, got %d", i, c.err, c.want, code)
		}
	}
}
