package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"
)

const (
	MySQLTypeDecimal    byte = 0
	MySQLTypeTiny       byte = 1
	MySQLTypeShort      byte = 2
	MySQLTypeLong       byte = 3
	MySQLTypeFloat      byte = 4
	MySQLTypeDouble     byte = 5
	MySQLTypeNull       byte = 6
	MySQLTypeTimestamp  byte = 7
	MySQLTypeLongLong   byte = 8
	MySQLTypeInt24      byte = 9
	MySQLTypeDate       byte = 10
	MySQLTypeTime       byte = 11
	MySQLTypeDateTime   byte = 12
	MySQLTypeYear       byte = 13
	MySQLTypeVarchar    byte = 15
	MySQLTypeNewDecimal byte = 246
	MySQLTypeTinyBlob   byte = 249
	MySQLTypeMediumBlob byte = 250
	MySQLTypeLongBlob   byte = 251
	MySQLTypeBlob       byte = 252
	MySQLTypeVarString  byte = 253
	MySQLTypeString     byte = 254
)

func PrepareOKPacket(statementID uint32, parameterCount uint16) []byte {
	data := make([]byte, 12)
	data[0] = 0
	binary.LittleEndian.PutUint32(data[1:5], statementID)
	binary.LittleEndian.PutUint16(data[5:7], 0)
	binary.LittleEndian.PutUint16(data[7:9], parameterCount)
	data[9] = 0
	binary.LittleEndian.PutUint16(data[10:12], 0)
	return data
}

func DecodeStmtExecute(data []byte, parameterCount int, previousTypes []uint16) (uint32, []any, []uint16, error) {
	if len(data) < 9 {
		return 0, nil, previousTypes, io.ErrUnexpectedEOF
	}
	statementID := binary.LittleEndian.Uint32(data[:4])
	position := 9
	if parameterCount == 0 {
		return statementID, nil, previousTypes, nil
	}
	nullBitmapLength := (parameterCount + 7) / 8
	if position+nullBitmapLength+1 > len(data) {
		return 0, nil, previousTypes, io.ErrUnexpectedEOF
	}
	nullBitmap := data[position : position+nullBitmapLength]
	position += nullBitmapLength
	newTypes := data[position] != 0
	position++
	types := append([]uint16(nil), previousTypes...)
	if newTypes {
		if position+parameterCount*2 > len(data) {
			return 0, nil, previousTypes, io.ErrUnexpectedEOF
		}
		types = make([]uint16, parameterCount)
		for index := range types {
			types[index] = binary.LittleEndian.Uint16(data[position : position+2])
			position += 2
		}
	}
	if len(types) != parameterCount {
		return 0, nil, previousTypes, fmt.Errorf("missing prepared parameter types")
	}
	values := make([]any, parameterCount)
	for index, fieldType := range types {
		if nullBitmap[index/8]&(1<<uint(index%8)) != 0 || byte(fieldType) == MySQLTypeNull {
			values[index] = nil
			continue
		}
		value, next, err := decodeBinaryParameter(data, position, byte(fieldType), fieldType&0x8000 != 0)
		if err != nil {
			return 0, nil, previousTypes, fmt.Errorf("parameter %d: %w", index+1, err)
		}
		position = next
		values[index] = value
	}
	return statementID, values, types, nil
}

func decodeBinaryParameter(data []byte, position int, fieldType byte, unsigned bool) (any, int, error) {
	require := func(length int) error {
		if position+length > len(data) {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	switch fieldType {
	case MySQLTypeTiny:
		if err := require(1); err != nil {
			return nil, position, err
		}
		if unsigned {
			return uint64(data[position]), position + 1, nil
		}
		return int64(int8(data[position])), position + 1, nil
	case MySQLTypeShort, MySQLTypeYear:
		if err := require(2); err != nil {
			return nil, position, err
		}
		value := binary.LittleEndian.Uint16(data[position:])
		if unsigned {
			return uint64(value), position + 2, nil
		}
		return int64(int16(value)), position + 2, nil
	case MySQLTypeLong, MySQLTypeInt24:
		if err := require(4); err != nil {
			return nil, position, err
		}
		value := binary.LittleEndian.Uint32(data[position:])
		if unsigned {
			return uint64(value), position + 4, nil
		}
		return int64(int32(value)), position + 4, nil
	case MySQLTypeLongLong:
		if err := require(8); err != nil {
			return nil, position, err
		}
		value := binary.LittleEndian.Uint64(data[position:])
		if unsigned {
			return value, position + 8, nil
		}
		return int64(value), position + 8, nil
	case MySQLTypeFloat:
		if err := require(4); err != nil {
			return nil, position, err
		}
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(data[position:]))), position + 4, nil
	case MySQLTypeDouble:
		if err := require(8); err != nil {
			return nil, position, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(data[position:])), position + 8, nil
	case MySQLTypeDate, MySQLTypeDateTime, MySQLTypeTimestamp:
		return decodeBinaryDate(data, position)
	case MySQLTypeDecimal, MySQLTypeNewDecimal, MySQLTypeVarchar, MySQLTypeTinyBlob,
		MySQLTypeMediumBlob, MySQLTypeLongBlob, MySQLTypeBlob, MySQLTypeVarString, MySQLTypeString:
		lengthPosition := position
		length, err := ReadLenEncInteger(data, &lengthPosition)
		if err != nil || length > uint64(len(data)-lengthPosition) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return nil, position, err
		}
		end := lengthPosition + int(length)
		return string(data[lengthPosition:end]), end, nil
	default:
		return nil, position, fmt.Errorf("unsupported MySQL parameter type %d", fieldType)
	}
}

func decodeBinaryDate(data []byte, position int) (any, int, error) {
	if position >= len(data) {
		return nil, position, io.ErrUnexpectedEOF
	}
	length := int(data[position])
	position++
	if length == 0 {
		return nil, position, nil
	}
	if length != 4 && length != 7 && length != 11 || position+length > len(data) {
		return nil, position, fmt.Errorf("invalid binary date length %d", length)
	}
	year := int(binary.LittleEndian.Uint16(data[position:]))
	month, day := time.Month(data[position+2]), int(data[position+3])
	hour, minute, second, nanosecond := 0, 0, 0, 0
	if length >= 7 {
		hour, minute, second = int(data[position+4]), int(data[position+5]), int(data[position+6])
	}
	if length == 11 {
		nanosecond = int(binary.LittleEndian.Uint32(data[position+7:])) * 1000
	}
	return time.Date(year, month, day, hour, minute, second, nanosecond, time.Local), position + length, nil
}
