package executor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"gbaselite/storage"
	"gbaselite/storageengine"
	"math"
)

const sqlCompactRowEncoding = 1

var sqlRowMagic = []byte{'G', 'B', 'R', 1}
var errSQLRowEncoding = errors.New("invalid compact MVCC row encoding")

// Column types live in the transaction's versioned schema, not in every row.
// Existing tables retain the gob codec until explicitly migrated to a new table.
func encodeSQLRow(table versionedTable, row storage.Row) ([]byte, error) {
	if table.RowEncoding == 0 {
		return encodeVersioned(row)
	}
	if table.RowEncoding != sqlCompactRowEncoding || len(row) != len(table.Definition.Columns) {
		return nil, errSQLRowEncoding
	}
	out := append([]byte(nil), sqlRowMagic...)
	out = binary.AppendUvarint(out, uint64(len(row)))
	nullStart := len(out)
	out = append(out, make([]byte, (len(row)+7)/8)...)
	for i, v := range row {
		if v.Type != table.Definition.Columns[i].Type {
			return nil, errSQLRowEncoding
		}
		if v.Null {
			out[nullStart+i/8] |= 1 << uint(i%8)
			continue
		}
		switch v.Type {
		case storage.TypeInt, storage.TypeBigInt:
			out = binary.AppendVarint(out, v.Int64)
		case storage.TypeFloat, storage.TypeDouble:
			out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v.Float))
		case storage.TypeBoolean:
			if v.Bool {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		case storage.TypeDecimal, storage.TypeText, storage.TypeVarchar:
			if len(v.Text) > storageengine.MaxValueBytes {
				return nil, fmt.Errorf("%w: compact row value exceeds limit", ErrQueryResourceLimit)
			}
			out = binary.AppendUvarint(out, uint64(len(v.Text)))
			out = append(out, v.Text...)
		case storage.TypeDate, storage.TypeDateTime:
			b, err := v.Date.MarshalBinary()
			if err != nil {
				return nil, err
			}
			out = binary.AppendUvarint(out, uint64(len(b)))
			out = append(out, b...)
		default:
			return nil, errSQLRowEncoding
		}
		if len(out) > storageengine.MaxValueBytes {
			return nil, fmt.Errorf("%w: encoded row exceeds %d bytes", ErrQueryResourceLimit, storageengine.MaxValueBytes)
		}
	}
	return out, nil
}

type sqlRowReader struct {
	data   []byte
	offset int
}

func (r *sqlRowReader) blob() ([]byte, error) {
	n, k := binary.Uvarint(r.data[r.offset:])
	if k <= 0 {
		return nil, errSQLRowEncoding
	}
	r.offset += k
	if n > uint64(len(r.data)-r.offset) {
		return nil, errSQLRowEncoding
	}
	b := r.data[r.offset : r.offset+int(n)]
	r.offset += int(n)
	return b, nil
}
func decodeSQLRow(table versionedTable, encoded []byte) (storage.Row, error) {
	return decodeSQLRowProjected(table, encoded, nil)
}

func decodeSQLRowProjected(table versionedTable, encoded []byte, needed []bool) (storage.Row, error) {
	return decodeSQLRowInto(table, encoded, needed, nil)
}

// The caller may reuse dst only when it consumes the row synchronously and
// retains no row references. Values are cleared, including NULL/skipped fields.
func decodeSQLRowInto(table versionedTable, encoded []byte, needed []bool, dst storage.Row) (storage.Row, error) {
	if table.RowEncoding == 0 {
		var row storage.Row
		err := decodeVersioned(encoded, &row)
		return row, err
	}
	if table.RowEncoding != sqlCompactRowEncoding || !bytes.HasPrefix(encoded, sqlRowMagic) || len(encoded) > storageengine.MaxValueBytes {
		return nil, errSQLRowEncoding
	}
	columns := table.Definition.Columns
	if needed != nil && len(needed) != len(columns) {
		return nil, errSQLRowEncoding
	}
	n, k := binary.Uvarint(encoded[len(sqlRowMagic):])
	if k <= 0 || n != uint64(len(columns)) {
		return nil, errSQLRowEncoding
	}
	pos := len(sqlRowMagic) + k
	nullBytes := (len(columns) + 7) / 8
	if len(encoded)-pos < nullBytes {
		return nil, errSQLRowEncoding
	}
	nulls := encoded[pos : pos+nullBytes]
	pos += nullBytes
	row := dst
	if cap(row) < len(columns) {
		row = make(storage.Row, len(columns))
	} else {
		row = row[:len(columns)]
		clear(row)
	}
	r := sqlRowReader{data: encoded, offset: pos}
	for i, c := range columns {
		v := &row[i]
		v.Type = c.Type
		v.Null = nulls[i/8]&(1<<uint(i%8)) != 0
		if v.Null {
			continue
		}
		switch c.Type {
		case storage.TypeInt, storage.TypeBigInt:
			n, k := binary.Varint(encoded[r.offset:])
			if k <= 0 {
				return nil, errSQLRowEncoding
			}
			r.offset += k
			v.Int64 = n
		case storage.TypeFloat, storage.TypeDouble:
			if len(encoded)-r.offset < 8 {
				return nil, errSQLRowEncoding
			}
			v.Float = math.Float64frombits(binary.LittleEndian.Uint64(encoded[r.offset:]))
			r.offset += 8
		case storage.TypeBoolean:
			if r.offset >= len(encoded) || encoded[r.offset] > 1 {
				return nil, errSQLRowEncoding
			}
			v.Bool = encoded[r.offset] == 1
			r.offset++
		case storage.TypeDecimal, storage.TypeText, storage.TypeVarchar:
			b, err := r.blob()
			if err != nil {
				return nil, err
			}
			if needed == nil || needed[i] {
				v.Text = string(b)
			}
		case storage.TypeDate, storage.TypeDateTime:
			b, err := r.blob()
			if err != nil {
				return nil, err
			}
			if err = v.Date.UnmarshalBinary(b); err != nil {
				return nil, errSQLRowEncoding
			}
		default:
			return nil, errSQLRowEncoding
		}
	}
	if r.offset != len(encoded) {
		return nil, errSQLRowEncoding
	}
	return row, nil
}
