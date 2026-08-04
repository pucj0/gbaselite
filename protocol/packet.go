package protocol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
)

const MaxPacketSize = 1<<24 - 1

type PacketConn struct {
	Conn         net.Conn
	Sequence     uint8
	Capabilities uint32
	writer       *bufio.Writer
	readHeader   [4]byte
	readBuffer   []byte
}

func (p *PacketConn) EnableWriteBuffer(size int) {
	if size > 0 && p.writer == nil {
		p.writer = bufio.NewWriterSize(p.Conn, size)
	}
}

func (p *PacketConn) Flush() error {
	if p.writer == nil {
		return nil
	}
	return p.writer.Flush()
}

func (p *PacketConn) ResetSequence() { p.Sequence = 0 }
func (p *PacketConn) ReadPacket() ([]byte, error) {
	if _, err := io.ReadFull(p.Conn, p.readHeader[:]); err != nil {
		return nil, err
	}
	length := int(p.readHeader[0]) | int(p.readHeader[1])<<8 | int(p.readHeader[2])<<16
	p.Sequence = p.readHeader[3] + 1
	var data []byte
	if length <= 64<<10 {
		if cap(p.readBuffer) < length {
			p.readBuffer = make([]byte, length)
		}
		data = p.readBuffer[:length]
	} else {
		data = make([]byte, length)
	}
	_, err := io.ReadFull(p.Conn, data)
	return data, err
}
func (p *PacketConn) WritePacket(data []byte) error {
	for {
		size := len(data)
		if size > MaxPacketSize {
			size = MaxPacketSize
		}
		header := [4]byte{byte(size), byte(size >> 8), byte(size >> 16), p.Sequence}
		p.Sequence++
		writer := io.Writer(p.Conn)
		if p.writer != nil {
			writer = p.writer
		}
		if _, err := writer.Write(header[:]); err != nil {
			return err
		}
		if size > 0 {
			if _, err := writer.Write(data[:size]); err != nil {
				return err
			}
		}
		data = data[size:]
		if size < MaxPacketSize {
			return nil
		}
	}
}

func AppendLenEncInteger(dst []byte, value uint64) []byte {
	switch {
	case value < 251:
		return append(dst, byte(value))
	case value < 1<<16:
		return append(append(dst, 0xfc), byte(value), byte(value>>8))
	case value < 1<<24:
		return append(append(dst, 0xfd), byte(value), byte(value>>8), byte(value>>16))
	default:
		dst = append(dst, 0xfe)
		buffer := make([]byte, 8)
		binary.LittleEndian.PutUint64(buffer, value)
		return append(dst, buffer...)
	}
}
func AppendLenEncString(dst []byte, value []byte) []byte {
	dst = AppendLenEncInteger(dst, uint64(len(value)))
	return append(dst, value...)
}
func ReadLenEncInteger(data []byte, position *int) (uint64, error) {
	if *position >= len(data) {
		return 0, io.ErrUnexpectedEOF
	}
	first := data[*position]
	*position++
	switch first {
	case 0xfc:
		if *position+2 > len(data) {
			return 0, io.ErrUnexpectedEOF
		}
		value := uint64(binary.LittleEndian.Uint16(data[*position:]))
		*position += 2
		return value, nil
	case 0xfd:
		if *position+3 > len(data) {
			return 0, io.ErrUnexpectedEOF
		}
		value := uint64(data[*position]) | uint64(data[*position+1])<<8 | uint64(data[*position+2])<<16
		*position += 3
		return value, nil
	case 0xfe:
		if *position+8 > len(data) {
			return 0, io.ErrUnexpectedEOF
		}
		value := binary.LittleEndian.Uint64(data[*position:])
		*position += 8
		return value, nil
	case 0xfb:
		return 0, errors.New("NULL length-encoded integer")
	default:
		return uint64(first), nil
	}
}
func ReadNullTerminated(data []byte, position *int) (string, error) {
	start := *position
	for *position < len(data) && data[*position] != 0 {
		*position++
	}
	if *position >= len(data) {
		return "", io.ErrUnexpectedEOF
	}
	value := string(data[start:*position])
	*position++
	return value, nil
}
