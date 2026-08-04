package protocol

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"gbaselite/executor"
	"gbaselite/storage"
)

const (
	TypeLong      byte = 3
	TypeFloat     byte = 4
	TypeDouble    byte = 5
	TypeLongLong  byte = 8
	TypeDate      byte = 10
	TypeDateTime  byte = 12
	TypeBlob      byte = 252
	TypeVarString byte = 253
)

const (
	defaultResultBufferSize     = 256
	maxPooledResultBuffer       = 64 << 10
	serverStatusAutocommit      = uint16(0x0002)
	serverStatusMetadataChanged = uint16(0x0400)
)

var resultBufferPool = sync.Pool{New: func() any {
	buffer := make([]byte, 0, defaultResultBufferSize)
	return &buffer
}}

func acquireResultBuffer() (*[]byte, []byte) {
	pooled := resultBufferPool.Get().(*[]byte)
	return pooled, (*pooled)[:0]
}

func releaseResultBuffer(pooled *[]byte, buffer []byte) {
	if cap(buffer) > maxPooledResultBuffer {
		buffer = make([]byte, 0, defaultResultBufferSize)
	}
	*pooled = buffer[:0]
	resultBufferPool.Put(pooled)
}

func OKPacket(affected uint64, message string) []byte {
	return OKPacketWithCapabilities(affected, message, 0)
}

func OKPacketWithCapabilities(affected uint64, message string, capabilities uint32) []byte {
	return okPacketWithStatus(affected, 0, message, capabilities, serverStatusAutocommit)
}

func okPacketWithStatus(affected, lastInsertID uint64, message string, capabilities uint32, status uint16) []byte {
	data := []byte{0}
	data = AppendLenEncInteger(data, affected)
	data = AppendLenEncInteger(data, lastInsertID)
	data = append(data, byte(status), byte(status>>8), 0, 0)
	if capabilities&ClientSessionTrack != 0 {
		return AppendLenEncString(data, []byte(message))
	}
	return append(data, []byte(message)...)
}
func ErrorPacket(code uint16, message string) []byte {
	data := []byte{0xff, byte(code), byte(code >> 8), '#'}
	data = append(data, []byte("HY000")...)
	return append(data, []byte(message)...)
}
func EOFPacket() []byte { return []byte{0xfe, 0, 0, 2, 0} }

func WriteResult(connection *PacketConn, result *executor.Result, schema, table string) error {
	if len(result.Columns) == 0 {
		return writeOKResult(connection, result)
	}
	if err := connection.WritePacket(AppendLenEncInteger(nil, uint64(len(result.Columns)))); err != nil {
		return err
	}
	columnBuffer := make([]byte, 0, 128)
	for _, column := range result.Columns {
		columnBuffer = appendColumnDefinition(columnBuffer[:0], column, schema, table)
		if err := connection.WritePacket(columnBuffer); err != nil {
			return err
		}
	}
	if err := connection.WritePacket(EOFPacket()); err != nil {
		return err
	}
	pooled, rowBuffer := acquireResultBuffer()
	defer func() { releaseResultBuffer(pooled, rowBuffer) }()
	writeRow := func(row []any) error {
		data := rowBuffer[:0]
		for index, value := range row {
			if value == nil {
				data = append(data, 0xfb)
			} else {
				dataType := storage.TypeVarchar
				if index < len(result.Columns) {
					dataType = result.Columns[index].Type
				}
				data = appendLenEncTypedValue(data, value, dataType)
			}
		}
		rowBuffer = data
		return connection.WritePacket(data)
	}
	writeValueRow := func(row storage.Row) error {
		data := rowBuffer[:0]
		for _, value := range row {
			if value.Null {
				data = append(data, 0xfb)
			} else {
				data = appendLenEncStorageValue(data, value)
			}
		}
		rowBuffer = data
		return connection.WritePacket(data)
	}
	if result.StreamValues != nil {
		if err := result.StreamValues(writeValueRow); err != nil {
			return err
		}
	} else if result.StreamRows != nil {
		if err := result.StreamRows(writeRow); err != nil {
			return err
		}
	} else {
		for _, row := range result.Rows {
			if err := writeRow(row); err != nil {
				return err
			}
		}
	}
	return connection.WritePacket(EOFPacket())
}

func appendLenEncTypedValue(dst []byte, value any, dataType storage.DataType) []byte {
	if date, ok := value.(time.Time); ok {
		var scratch [64]byte
		layout := "2006-01-02"
		if dataType == storage.TypeDateTime {
			layout = "2006-01-02 15:04:05.999999"
		}
		return appendLenEncBytes(dst, date.AppendFormat(scratch[:0], layout))
	}
	return appendLenEncValue(dst, value)
}

func WriteBinaryResult(connection *PacketConn, result *executor.Result, schema, table string) error {
	if len(result.Columns) == 0 {
		return writeOKResult(connection, result)
	}
	if err := connection.WritePacket(AppendLenEncInteger(nil, uint64(len(result.Columns)))); err != nil {
		return err
	}
	columnBuffer := make([]byte, 0, 128)
	for _, column := range result.Columns {
		columnBuffer = appendColumnDefinition(columnBuffer[:0], column, schema, table)
		if err := connection.WritePacket(columnBuffer); err != nil {
			return err
		}
	}
	if err := connection.WritePacket(EOFPacket()); err != nil {
		return err
	}
	pooled, rowBuffer := acquireResultBuffer()
	defer func() { releaseResultBuffer(pooled, rowBuffer) }()
	startRow := func(columnCount int) []byte {
		length := 1 + (columnCount+7+2)/8
		if cap(rowBuffer) < length {
			rowBuffer = make([]byte, length)
		} else {
			rowBuffer = rowBuffer[:length]
			clear(rowBuffer)
		}
		return rowBuffer
	}
	writeRow := func(row []any) error {
		if len(row) != len(result.Columns) {
			return fmt.Errorf("binary result row has %d values, expected %d", len(row), len(result.Columns))
		}
		data := startRow(len(row))
		for index, value := range row {
			if value == nil {
				data[1+(index+2)/8] |= 1 << uint((index+2)%8)
				continue
			}
			data = appendBinaryValue(data, value, result.Columns[index].Type)
		}
		rowBuffer = data
		return connection.WritePacket(data)
	}
	if result.StreamValues != nil {
		if err := result.StreamValues(func(row storage.Row) error {
			if len(row) != len(result.Columns) {
				return fmt.Errorf("binary result row has %d values, expected %d", len(row), len(result.Columns))
			}
			data := startRow(len(row))
			for index, value := range row {
				if value.Null {
					data[1+(index+2)/8] |= 1 << uint((index+2)%8)
					continue
				}
				data = appendBinaryStorageValue(data, value)
			}
			rowBuffer = data
			return connection.WritePacket(data)
		}); err != nil {
			return err
		}
	} else if result.StreamRows != nil {
		if err := result.StreamRows(writeRow); err != nil {
			return err
		}
	} else {
		for _, row := range result.Rows {
			if err := writeRow(row); err != nil {
				return err
			}
		}
	}
	return connection.WritePacket(EOFPacket())
}

func writeOKResult(connection *PacketConn, result *executor.Result) error {
	return connection.WritePacket(okPacketWithStatus(result.AffectedRows, result.LastInsertID, result.Message, connection.Capabilities, resultStatus(result)))
}

func resultStatus(result *executor.Result) uint16 {
	status := serverStatusAutocommit
	if result.MetadataChanged {
		status |= serverStatusMetadataChanged
	}
	return status
}

func appendBinaryStorageValue(dst []byte, value storage.Value) []byte {
	switch value.Type {
	case storage.TypeInt:
		var buffer [4]byte
		binary.LittleEndian.PutUint32(buffer[:], uint32(value.Int64))
		return append(dst, buffer[:]...)
	case storage.TypeBigInt:
		var buffer [8]byte
		binary.LittleEndian.PutUint64(buffer[:], uint64(value.Int64))
		return append(dst, buffer[:]...)
	case storage.TypeFloat:
		var buffer [4]byte
		binary.LittleEndian.PutUint32(buffer[:], math.Float32bits(float32(value.Float)))
		return append(dst, buffer[:]...)
	case storage.TypeDouble:
		var buffer [8]byte
		binary.LittleEndian.PutUint64(buffer[:], math.Float64bits(value.Float))
		return append(dst, buffer[:]...)
	case storage.TypeBoolean:
		var buffer [4]byte
		if value.Bool {
			binary.LittleEndian.PutUint32(buffer[:], 1)
		}
		return append(dst, buffer[:]...)
	case storage.TypeDate:
		if value.Date.IsZero() {
			return append(dst, 0)
		}
		return append(dst, 4, byte(value.Date.Year()), byte(value.Date.Year()>>8), byte(value.Date.Month()), byte(value.Date.Day()))
	case storage.TypeDateTime:
		if value.Date.IsZero() {
			return append(dst, 0)
		}
		result := append(dst, 7, byte(value.Date.Year()), byte(value.Date.Year()>>8), byte(value.Date.Month()), byte(value.Date.Day()), byte(value.Date.Hour()), byte(value.Date.Minute()), byte(value.Date.Second()))
		if microseconds := value.Date.Nanosecond() / 1000; microseconds != 0 {
			result[len(dst)] = 11
			var buffer [4]byte
			binary.LittleEndian.PutUint32(buffer[:], uint32(microseconds))
			result = append(result, buffer[:]...)
		}
		return result
	default:
		return appendLenEncStorageValue(dst, value)
	}
}

func appendBinaryValue(dst []byte, value any, dataType storage.DataType) []byte {
	switch dataType {
	case storage.TypeInt:
		var buffer [4]byte
		binary.LittleEndian.PutUint32(buffer[:], uint32(binaryInt64(value)))
		return append(dst, buffer[:]...)
	case storage.TypeBigInt:
		var buffer [8]byte
		binary.LittleEndian.PutUint64(buffer[:], uint64(binaryInt64(value)))
		return append(dst, buffer[:]...)
	case storage.TypeFloat:
		var buffer [4]byte
		binary.LittleEndian.PutUint32(buffer[:], math.Float32bits(float32(binaryFloat64(value))))
		return append(dst, buffer[:]...)
	case storage.TypeDouble:
		var buffer [8]byte
		binary.LittleEndian.PutUint64(buffer[:], math.Float64bits(binaryFloat64(value)))
		return append(dst, buffer[:]...)
	case storage.TypeBoolean:
		var buffer [4]byte
		if truth, ok := value.(bool); ok && truth {
			binary.LittleEndian.PutUint32(buffer[:], 1)
		}
		return append(dst, buffer[:]...)
	case storage.TypeDate:
		date, ok := value.(time.Time)
		if !ok || date.IsZero() {
			return append(dst, 0)
		}
		return append(dst, 4, byte(date.Year()), byte(date.Year()>>8), byte(date.Month()), byte(date.Day()))
	case storage.TypeDateTime:
		date, ok := value.(time.Time)
		if !ok || date.IsZero() {
			return append(dst, 0)
		}
		result := append(dst, 7, byte(date.Year()), byte(date.Year()>>8), byte(date.Month()), byte(date.Day()), byte(date.Hour()), byte(date.Minute()), byte(date.Second()))
		if microseconds := date.Nanosecond() / 1000; microseconds != 0 {
			result[len(dst)] = 11
			var buffer [4]byte
			binary.LittleEndian.PutUint32(buffer[:], uint32(microseconds))
			result = append(result, buffer[:]...)
		}
		return result
	default:
		return AppendLenEncString(dst, []byte(formatValue(value)))
	}
}

func binaryInt64(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint64:
		return int64(typed)
	default:
		return 0
	}
}

func binaryFloat64(value any) float64 {
	switch typed := value.(type) {
	case float32:
		return float64(typed)
	case float64:
		return typed
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

func appendLenEncValue(dst []byte, value any) []byte {
	var scratch [64]byte
	var encoded []byte
	switch typed := value.(type) {
	case string:
		return appendLenEncBytes(dst, []byte(typed))
	case []byte:
		return appendLenEncBytes(dst, typed)
	case int:
		encoded = strconv.AppendInt(scratch[:0], int64(typed), 10)
	case int32:
		encoded = strconv.AppendInt(scratch[:0], int64(typed), 10)
	case int64:
		encoded = strconv.AppendInt(scratch[:0], typed, 10)
	case uint:
		encoded = strconv.AppendUint(scratch[:0], uint64(typed), 10)
	case uint64:
		encoded = strconv.AppendUint(scratch[:0], typed, 10)
	case float32:
		encoded = strconv.AppendFloat(scratch[:0], float64(typed), 'g', -1, 32)
	case float64:
		encoded = strconv.AppendFloat(scratch[:0], typed, 'g', -1, 64)
	case bool:
		if typed {
			encoded = append(scratch[:0], '1')
		} else {
			encoded = append(scratch[:0], '0')
		}
	case time.Time:
		encoded = typed.AppendFormat(scratch[:0], "2006-01-02")
	default:
		encoded = []byte(fmt.Sprint(typed))
	}
	return appendLenEncBytes(dst, encoded)
}

func appendLenEncStorageValue(dst []byte, value storage.Value) []byte {
	var scratch [64]byte
	var encoded []byte
	switch value.Type {
	case storage.TypeInt, storage.TypeBigInt:
		encoded = strconv.AppendInt(scratch[:0], value.Int64, 10)
	case storage.TypeFloat:
		encoded = strconv.AppendFloat(scratch[:0], value.Float, 'g', -1, 32)
	case storage.TypeDouble:
		encoded = strconv.AppendFloat(scratch[:0], value.Float, 'g', -1, 64)
	case storage.TypeVarchar, storage.TypeText:
		return appendLenEncBytes(dst, []byte(value.Text))
	case storage.TypeBoolean:
		if value.Bool {
			encoded = append(scratch[:0], '1')
		} else {
			encoded = append(scratch[:0], '0')
		}
	case storage.TypeDate:
		encoded = value.Date.AppendFormat(scratch[:0], "2006-01-02")
	case storage.TypeDateTime:
		encoded = value.Date.AppendFormat(scratch[:0], "2006-01-02 15:04:05.999999")
	default:
		encoded = []byte(value.String())
	}
	return appendLenEncBytes(dst, encoded)
}

func appendLenEncBytes(dst, value []byte) []byte {
	dst = AppendLenEncInteger(dst, uint64(len(value)))
	return append(dst, value...)
}
func ColumnDefinition(column executor.Column, schema, table string) []byte {
	return appendColumnDefinition(nil, column, schema, table)
}

func appendColumnDefinition(data []byte, column executor.Column, schema, table string) []byte {
	if column.Schema != "" {
		schema = column.Schema
	}
	if column.Table != "" {
		table = column.Table
	}
	originalName := column.OriginalName
	if originalName == "" {
		originalName = column.Name
	}
	for _, value := range []string{"def", schema, table, table, column.Name, originalName} {
		data = AppendLenEncString(data, []byte(value))
	}
	data = append(data, 0x0c, 45, 0)
	length := uint32(1024)
	typeCode := TypeVarString
	switch column.Type {
	case storage.TypeInt:
		typeCode = TypeLong
		length = 11
	case storage.TypeBigInt:
		typeCode = TypeLongLong
		length = 20
	case storage.TypeFloat:
		typeCode = TypeFloat
		length = 12
	case storage.TypeDouble:
		typeCode = TypeDouble
		length = 22
	case storage.TypeText:
		typeCode = TypeBlob
		length = 65535
	case storage.TypeDate:
		typeCode = TypeDate
		length = 10
	case storage.TypeDateTime:
		typeCode = TypeDateTime
		length = 26
	case storage.TypeBoolean:
		typeCode = TypeLong
		length = 1
	}
	if column.Type == storage.TypeVarchar && column.Length > 0 {
		// utf8mb4 column_length is the maximum encoded byte length.
		length = uint32(column.Length) * 4
	}
	var buffer [4]byte
	binary.LittleEndian.PutUint32(buffer[:], length)
	data = append(data, buffer[:]...)
	flags := uint16(0)
	if !column.Nullable && column.Table != "" {
		flags |= 0x0001 // NOT_NULL_FLAG
	}
	if column.PrimaryKey {
		flags |= 0x0002 // PRI_KEY_FLAG
	}
	if column.UniqueKey {
		flags |= 0x0004 // UNIQUE_KEY_FLAG
	}
	if column.MultipleKey {
		flags |= 0x0008 // MULTIPLE_KEY_FLAG
	}
	if column.AutoIncrement {
		flags |= 0x0200 // AUTO_INCREMENT_FLAG
	}
	data = append(data, typeCode, byte(flags), byte(flags>>8), 0, 0, 0)
	return data
}
func formatValue(value any) string {
	switch v := value.(type) {
	case time.Time:
		return v.Format("2006-01-02")
	case bool:
		if v {
			return "1"
		}
		return "0"
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}
