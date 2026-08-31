package protocol

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

const (
	ClientLongPassword         uint32 = 1
	ClientLongFlag             uint32 = 4
	ClientConnectWithDB        uint32 = 8
	ClientSSL                  uint32 = 0x0800
	ClientProtocol41           uint32 = 0x0200
	ClientTransactions         uint32 = 0x2000
	ClientSecureConnection     uint32 = 0x8000
	ClientMultiResults         uint32 = 0x20000
	ClientPluginAuth           uint32 = 0x80000
	ClientConnectAttrs         uint32 = 0x100000
	ClientPluginAuthLenEncData uint32 = 0x200000
	ClientSessionTrack         uint32 = 0x800000
)

const ServerCapabilities = ClientLongPassword | ClientLongFlag | ClientConnectWithDB | ClientProtocol41 | ClientTransactions | ClientSecureConnection | ClientMultiResults | ClientPluginAuth | ClientConnectAttrs | ClientPluginAuthLenEncData | ClientSessionTrack

type HandshakeResponse struct {
	CharacterSet byte
	Capabilities uint32
	Username     string
	AuthResponse []byte
	Database     string
}

func NewSeed() ([]byte, error) {
	seed := make([]byte, 20)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	for i := range seed {
		if seed[i] == 0 {
			seed[i] = 1
		}
	}
	return seed, nil
}
func HandshakePacket(connectionID uint32, seed []byte) []byte {
	return HandshakePacketWithCapabilities(connectionID, seed, ServerCapabilities)
}

func HandshakePacketWithCapabilities(connectionID uint32, seed []byte, capabilities uint32) []byte {
	data := []byte{10}
	data = append(data, []byte("5.7.44-GBaseLite")...)
	data = append(data, 0)
	buffer := make([]byte, 4)
	binary.LittleEndian.PutUint32(buffer, connectionID)
	data = append(data, buffer...)
	data = append(data, seed[:8]...)
	data = append(data, 0)
	data = append(data, byte(capabilities&0xff), byte((capabilities>>8)&0xff))
	data = append(data, 45)
	data = append(data, 2, 0)
	data = append(data, byte((capabilities>>16)&0xff), byte((capabilities>>24)&0xff))
	data = append(data, 21)
	data = append(data, make([]byte, 10)...)
	data = append(data, seed[8:]...)
	data = append(data, 0)
	data = append(data, []byte("mysql_native_password")...)
	data = append(data, 0)
	return data
}
func ParseHandshakeResponse(data []byte) (HandshakeResponse, error) {
	var response HandshakeResponse
	if len(data) < 32 {
		return response, fmt.Errorf("short handshake response")
	}
	response.Capabilities = binary.LittleEndian.Uint32(data[:4])
	response.CharacterSet = data[8]
	position := 32
	username, err := ReadNullTerminated(data, &position)
	if err != nil {
		return response, err
	}
	response.Username = username
	if response.Capabilities&ClientPluginAuthLenEncData != 0 {
		length, err := ReadLenEncInteger(data, &position)
		if err != nil {
			return response, err
		}
		if position+int(length) > len(data) {
			return response, fmt.Errorf("short auth response")
		}
		response.AuthResponse = append([]byte(nil), data[position:position+int(length)]...)
		position += int(length)
	} else if response.Capabilities&ClientSecureConnection != 0 {
		if position >= len(data) {
			return response, fmt.Errorf("missing auth response")
		}
		length := int(data[position])
		position++
		if position+length > len(data) {
			return response, fmt.Errorf("short auth response")
		}
		response.AuthResponse = append([]byte(nil), data[position:position+length]...)
		position += length
	} else {
		auth, err := ReadNullTerminated(data, &position)
		if err != nil {
			return response, err
		}
		response.AuthResponse = []byte(auth)
	}
	if response.Capabilities&ClientConnectWithDB != 0 && position < len(data) {
		response.Database, _ = ReadNullTerminated(data, &position)
	}
	return response, nil
}
