package protocol

import (
	"encoding/binary"
	"testing"
)

func TestParseHandshakeResponseCharacterSet(t *testing.T) {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[:4], ClientProtocol41)
	data[8] = 47
	data = append(data, []byte("root\x00\x00")...)
	response, err := ParseHandshakeResponse(data)
	if err != nil {
		t.Fatal(err)
	}
	if response.CharacterSet != 47 {
		t.Fatalf("character set = %d, want 47", response.CharacterSet)
	}
}
