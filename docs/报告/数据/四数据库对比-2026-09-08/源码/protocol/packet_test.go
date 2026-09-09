package protocol

import (
	"net"
	"testing"
)

func TestPacketReadBudgetRejectsBeforePayloadAllocation(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	done := make(chan error, 1)
	go func() { _, err := right.Write([]byte{0, 0, 2, 0}); done <- err }()
	packet := &PacketConn{Conn: left, MaxReadBytes: 1 << 16}
	if _, err := packet.ReadPacket(); err == nil {
		t.Fatal("oversized packet accepted")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(packet.readBuffer) != 0 {
		t.Fatal("allocated payload above budget")
	}
}
