package executor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"gbaselite/storage"
)

// This file owns the temporary spill codec for modify-pipeline candidates.
//
// The unified Modify Pipeline deduplicates candidates whose winner payload must survive a
// spill, so a candidate has to be encodable as something the external sorter can persist. The
// codec is deliberately separate from every durable format in the engine: it is a query
// temporary, written to a private run file that is deleted when the sorter closes, and no
// snapshot, journal or table file ever reads it. Changing it therefore cannot change what is
// stored on disk.
//
// The encoding is hand-rolled rather than gob for two reasons. Its size is computable, and the
// sorter has to reject a candidate that is too wide for its budget *before* allocating for it;
// decoding also then owns every byte it returns, which is what lets a decoded candidate
// outlive the sorter's read buffer. Nothing here derives a value from Value.String(): a row is
// stored field by field so the value's type, nullness and payload all survive exactly.
//
// Format, little-endian throughout:
//
//	row   := uint32 count, count * cell
//	cell  := uint8 tag, payload
//	tag 0 := null, no payload
//	tag 1 := text   : uint32 length, bytes
//	tag 2 := int    : int64
//	tag 3 := float  : float64 bits
//	tag 4 := bool   : one byte (1 or 0)
//	tag 5 := date   : int64 seconds, uint32 nanoseconds, int32 zone offset in seconds
//	tag 6 := bigint : int64
//	tag 7 := double : float64 bits
//	tag 8 := decimal: float64 bits, uint32 length, bytes
//	tag 9 := varchar: uint32 length, bytes
//	tag 10:= date   : int64 seconds, uint32 nanoseconds, int32 zone offset in seconds
//
// A NULL cell is written as its own tag plus the declared type name, because Null values retain the
// column's declared type and the write path validates against it.
//
// Every field storage.Value carries is written, and the tag keeps the declared DataType rather than
// a widened stand-in. That matters because the write path validates a column's declared type: a
// materialised VARCHAR retrieved as TEXT, or a 32-bit INT as BIGINT, or a DATE as DATETIME, is a
// type mismatch rather than the same value. TestTemporaryPayloadCodecRoundTripsEveryValueShape pins
// the round trip against the struct itself, so a new storage type fails that test instead of losing
// its identity silently.

const (
	temporaryTagNull    byte = 0
	temporaryTagText    byte = 1
	temporaryTagInt     byte = 2
	temporaryTagFloat   byte = 3
	temporaryTagBool    byte = 4
	temporaryTagDate    byte = 5
	temporaryTagBigInt  byte = 6
	temporaryTagDouble  byte = 7
	temporaryTagDecimal byte = 8
	temporaryTagVarchar byte = 9
	temporaryTagDateDay byte = 10
)

func appendTemporaryUint32(dst []byte, value uint32) []byte {
	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[:], value)
	return append(dst, scratch[:]...)
}

func appendTemporaryUint64(dst []byte, value uint64) []byte {
	var scratch [8]byte
	binary.LittleEndian.PutUint64(scratch[:], value)
	return append(dst, scratch[:]...)
}

func appendTemporaryText(dst []byte, text string) []byte {
	dst = appendTemporaryUint32(dst, uint32(len(text)))
	return append(dst, text...)
}

// appendTemporaryValue writes one storage.Value field by field.
//
// The ordering of the cases is the ordering of Value's own precedence: a null cell carries no type
// at all, a date-valued cell keeps its instant and zone, and every other cell is tagged with the
// declared DataType so the value comes back as the column the write path will validate it against.
func appendTemporaryValue(dst []byte, value storage.Value) []byte {
	if value.Null {
		dst = append(dst, temporaryTagNull)
		return appendTemporaryText(dst, string(value.Type))
	}
	switch value.Type {
	case storage.TypeDate:
		dst = append(dst, temporaryTagDateDay)
		return appendTemporaryDate(dst, value.Date)
	case storage.TypeDateTime:
		dst = append(dst, temporaryTagDate)
		return appendTemporaryDate(dst, value.Date)
	case storage.TypeInt:
		dst = append(dst, temporaryTagInt)
		return appendTemporaryUint64(dst, uint64(value.Int64))
	case storage.TypeBigInt:
		dst = append(dst, temporaryTagBigInt)
		return appendTemporaryUint64(dst, uint64(value.Int64))
	case storage.TypeFloat:
		dst = append(dst, temporaryTagFloat)
		return appendTemporaryUint64(dst, math.Float64bits(value.Float))
	case storage.TypeDouble:
		dst = append(dst, temporaryTagDouble)
		return appendTemporaryUint64(dst, math.Float64bits(value.Float))
	case storage.TypeDecimal:
		// A DECIMAL keeps both its numeric value and its canonical digits: the numeric field is what
		// arithmetic reads and the text is what the value renders as, so storing only one would lose
		// the other.
		dst = append(dst, temporaryTagDecimal)
		dst = appendTemporaryUint64(dst, math.Float64bits(value.Float))
		return appendTemporaryText(dst, value.Text)
	case storage.TypeBoolean:
		dst = append(dst, temporaryTagBool)
		if value.Bool {
			return append(dst, 1)
		}
		return append(dst, 0)
	case storage.TypeVarchar:
		dst = append(dst, temporaryTagVarchar)
		return appendTemporaryText(dst, value.Text)
	default:
		// TEXT and every other string-shaped type.
		dst = append(dst, temporaryTagText)
		return appendTemporaryText(dst, value.Text)
	}
}

// appendTemporaryDate writes an instant with the zone offset it was observed in, so a decoded value
// keeps both its instant and its rendering.
func appendTemporaryDate(dst []byte, value time.Time) []byte {
	dst = appendTemporaryUint64(dst, uint64(value.Unix()))
	dst = appendTemporaryUint32(dst, uint32(value.Nanosecond()))
	_, offset := value.Zone()
	return appendTemporaryUint32(dst, uint32(int32(offset)))
}

// encodeTemporaryRow writes a whole row.
func encodeTemporaryRow(dst []byte, row storage.Row) []byte {
	dst = appendTemporaryUint32(dst, uint32(len(row)))
	for _, value := range row {
		dst = appendTemporaryValue(dst, value)
	}
	return dst
}

// temporaryRowBytes is the exact encoded size of a row, so the sorter can apply its width limit
// before anything is allocated. It mirrors encodeTemporaryRow cell for cell.
func temporaryRowBytes(row storage.Row) int {
	size := 4
	for _, value := range row {
		size++
		if value.Null {
			size += 4 + len(value.Type)
			continue
		}
		switch value.Type {
		case storage.TypeDate, storage.TypeDateTime:
			size += 16
		case storage.TypeInt, storage.TypeBigInt, storage.TypeFloat, storage.TypeDouble:
			size += 8
		case storage.TypeDecimal:
			size += 8 + 4 + len(value.Text)
		case storage.TypeBoolean:
			size++
		default:
			size += 4 + len(value.Text)
		}
	}
	return size
}

type temporaryRowReader struct {
	data   []byte
	offset int
}

var errTemporaryPayloadCorrupt = errors.New("temporary spill payload is corrupt")

// date reads an instant together with the zone offset it was written with.
func (r *temporaryRowReader) date() (time.Time, error) {
	seconds, err := r.uint64()
	if err != nil {
		return time.Time{}, err
	}
	nanos, err := r.uint32()
	if err != nil {
		return time.Time{}, err
	}
	offset, err := r.uint32()
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(seconds), int64(nanos)).In(time.FixedZone("", int(int32(offset)))), nil
}

func (r *temporaryRowReader) byte() (byte, error) {
	if r.offset >= len(r.data) {
		return 0, errTemporaryPayloadCorrupt
	}
	value := r.data[r.offset]
	r.offset++
	return value, nil
}

func (r *temporaryRowReader) uint32() (uint32, error) {
	if r.offset+4 > len(r.data) {
		return 0, errTemporaryPayloadCorrupt
	}
	value := binary.LittleEndian.Uint32(r.data[r.offset:])
	r.offset += 4
	return value, nil
}

func (r *temporaryRowReader) uint64() (uint64, error) {
	if r.offset+8 > len(r.data) {
		return 0, errTemporaryPayloadCorrupt
	}
	value := binary.LittleEndian.Uint64(r.data[r.offset:])
	r.offset += 8
	return value, nil
}

// text copies its payload out of the buffer, so a decoded value never aliases it.
func (r *temporaryRowReader) text() (string, error) {
	size, err := r.uint32()
	if err != nil {
		return "", err
	}
	if uint64(size) > uint64(len(r.data)-r.offset) {
		return "", errTemporaryPayloadCorrupt
	}
	text := string(r.data[r.offset : r.offset+int(size)])
	r.offset += int(size)
	return text, nil
}

// decodeTemporaryRow rebuilds a row. The result and every string in it are fresh allocations,
// so a decoded candidate owns its bytes and does not retain the sorter's read buffer.
func decodeTemporaryRow(r *temporaryRowReader) (storage.Row, error) {
	count, err := r.uint32()
	if err != nil {
		return nil, err
	}
	// A corrupt length must not become an unbounded allocation: every cell costs at least one
	// tag byte, so the remaining buffer bounds how many cells can legitimately follow.
	if uint64(count) > uint64(len(r.data)-r.offset) {
		return nil, errTemporaryPayloadCorrupt
	}
	row := make(storage.Row, 0, count)
	for index := uint32(0); index < count; index++ {
		tag, err := r.byte()
		if err != nil {
			return nil, err
		}
		switch tag {
		case temporaryTagNull:
			declared, err := r.text()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.DataType(declared), Null: true})
		case temporaryTagText:
			text, err := r.text()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeText, Text: text})
		case temporaryTagVarchar:
			text, err := r.text()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeVarchar, Text: text})
		case temporaryTagInt:
			number, err := r.uint64()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeInt, Int64: int64(number)})
		case temporaryTagBigInt:
			number, err := r.uint64()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeBigInt, Int64: int64(number)})
		case temporaryTagFloat:
			bits, err := r.uint64()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeFloat, Float: math.Float64frombits(bits)})
		case temporaryTagDouble:
			bits, err := r.uint64()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeDouble, Float: math.Float64frombits(bits)})
		case temporaryTagDecimal:
			bits, err := r.uint64()
			if err != nil {
				return nil, err
			}
			text, err := r.text()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeDecimal, Float: math.Float64frombits(bits), Text: text})
		case temporaryTagBool:
			value, err := r.byte()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeBoolean, Bool: value != 0})
		case temporaryTagDate:
			instant, err := r.date()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeDateTime, Date: instant})
		case temporaryTagDateDay:
			instant, err := r.date()
			if err != nil {
				return nil, err
			}
			row = append(row, storage.Value{Type: storage.TypeDate, Date: instant})
		default:
			return nil, fmt.Errorf("%w: unknown storage value tag %d", errTemporaryPayloadCorrupt, tag)
		}
	}
	return row, nil
}

// encodeIdentityKey writes a RowIdentity's table identifier and physical key. A target table
// identifier is part of the identity, so it travels with the candidate.
func encodeIdentityKey(dst []byte, identity RowIdentity) []byte {
	dst = appendTemporaryText(dst, identity.TableID)
	dst = appendTemporaryUint32(dst, uint32(len(identity.Key)))
	return append(dst, identity.Key...)
}

func decodeIdentityKey(r *temporaryRowReader) (RowIdentity, error) {
	tableID, err := r.text()
	if err != nil {
		return RowIdentity{}, err
	}
	size, err := r.uint32()
	if err != nil {
		return RowIdentity{}, err
	}
	if uint64(size) > uint64(len(r.data)-r.offset) {
		return RowIdentity{}, errTemporaryPayloadCorrupt
	}
	key := make([]byte, size)
	copy(key, r.data[r.offset:r.offset+int(size)])
	r.offset += int(size)
	return RowIdentity{TableID: tableID, Key: key, Valid: true}, nil
}

// updateJoinCodec encodes one UPDATE JOIN candidate: the old physical identity and the full
// joined evaluation row.
//
// OldRow is not written: it is by definition the prefix of EvalRow holding the target's own
// columns, so storing it separately would double the payload for no information. Identity.Key is
// carried even though it is also the ledger key, because the ledger key is a length-prefixed
// encoding of (table, key) while the write path needs the plain storage key back.
type updateJoinCodec struct{}

func (updateJoinCodec) encode(candidate updateJoinCandidate) ([]byte, error) {
	dst := make([]byte, 0, temporaryRowBytes(candidate.EvalRow)+len(candidate.Identity.Key)+32)
	dst = appendTemporaryUint64(dst, candidate.Ordinal)
	dst = encodeIdentityKey(dst, candidate.Identity)
	return encodeTemporaryRow(dst, candidate.EvalRow), nil
}

func (updateJoinCodec) decode(data []byte) (updateJoinCandidate, error) {
	reader := &temporaryRowReader{data: data}
	ordinal, err := reader.uint64()
	if err != nil {
		return updateJoinCandidate{}, err
	}
	identity, err := decodeIdentityKey(reader)
	if err != nil {
		return updateJoinCandidate{}, err
	}
	evalRow, err := decodeTemporaryRow(reader)
	if err != nil {
		return updateJoinCandidate{}, err
	}
	candidate := updateJoinCandidate{Ordinal: ordinal, Identity: identity, EvalRow: evalRow}
	// A decoded candidate must be self-contained. OldRow is rebuilt from the owned evaluation row,
	// so the write path never reads through to the sorter's buffer.
	if len(evalRow) > 0 {
		candidate.OldRow = append(storage.Row(nil), evalRow...)
	}
	return candidate, nil
}

func (updateJoinCodec) bytes(candidate updateJoinCandidate) int {
	return 8 + len(candidate.Identity.TableID) + 4 + len(candidate.Identity.Key) + 4 + temporaryRowBytes(candidate.EvalRow)
}

// rebuild turns a decoded candidate into the write path's candidate shape, restricting OldRow to
// the target's own leading columns. The prefix was captured while the joined row was in hand, so
// targetCount is the same width the join observed.
func (c updateJoinCandidate) rebuild(targetCount int) UpdateCandidate {
	oldRow := c.OldRow
	if len(oldRow) > targetCount {
		oldRow = oldRow[:targetCount]
	}
	return UpdateCandidate{Identity: c.Identity, OldRow: oldRow, EvalRow: c.EvalRow}
}

// multiDeleteCodec encodes one multi-table DELETE candidate: which requested target it addresses,
// that target's physical identity, and the target's own old row.
type multiDeleteCodec struct{}

func (multiDeleteCodec) encode(candidate multiDeleteCandidate) ([]byte, error) {
	dst := make([]byte, 0, temporaryRowBytes(candidate.OldRow)+len(candidate.Identity.Key)+32)
	dst = appendTemporaryUint32(dst, uint32(candidate.target))
	dst = encodeIdentityKey(dst, candidate.Identity)
	return encodeTemporaryRow(dst, candidate.OldRow), nil
}

func (multiDeleteCodec) decode(data []byte) (multiDeleteCandidate, error) {
	reader := &temporaryRowReader{data: data}
	target, err := reader.uint32()
	if err != nil {
		return multiDeleteCandidate{}, err
	}
	identity, err := decodeIdentityKey(reader)
	if err != nil {
		return multiDeleteCandidate{}, err
	}
	oldRow, err := decodeTemporaryRow(reader)
	if err != nil {
		return multiDeleteCandidate{}, err
	}
	return multiDeleteCandidate{target: int(target), Identity: identity, OldRow: oldRow}, nil
}

func (multiDeleteCodec) bytes(candidate multiDeleteCandidate) int {
	return 4 + len(candidate.Identity.TableID) + 4 + len(candidate.Identity.Key) + 4 + temporaryRowBytes(candidate.OldRow)
}
