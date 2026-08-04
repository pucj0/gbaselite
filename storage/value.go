package storage

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// DataType is the logical type stored by a table column.
type DataType string

const (
	TypeInt      DataType = "INT"
	TypeBigInt   DataType = "BIGINT"
	TypeFloat    DataType = "FLOAT"
	TypeDouble   DataType = "DOUBLE"
	TypeVarchar  DataType = "VARCHAR"
	TypeText     DataType = "TEXT"
	TypeBoolean  DataType = "BOOLEAN"
	TypeDate     DataType = "DATE"
	TypeDateTime DataType = "DATETIME"
)

const (
	dateLayout     = "2006-01-02"
	dateTimeLayout = "2006-01-02 15:04:05.999999"
)

// Value is a serializable tagged value. Only the field associated with Type is
// meaningful. Null values retain their declared type.
type Value struct {
	Type  DataType
	Null  bool
	Int64 int64
	Float float64
	Text  string
	Bool  bool
	Date  time.Time
}

func ParseDataType(raw string) (DataType, error) {
	t := DataType(strings.ToUpper(strings.TrimSpace(raw)))
	switch t {
	case TypeInt, TypeBigInt, TypeFloat, TypeDouble, TypeVarchar, TypeText, TypeBoolean, TypeDate, TypeDateTime:
		return t, nil
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INTEGER", "INT1", "INT2", "INT3", "INT4":
		return TypeInt, nil
	case "INT8", "SERIAL":
		return TypeBigInt, nil
	case "DECIMAL", "NUMERIC", "DEC", "FIXED", "REAL", "DOUBLE PRECISION":
		return TypeDouble, nil
	case "CHAR", "CHARACTER", "NCHAR", "NATIONAL", "NATIONAL CHAR", "NATIONAL VARCHAR",
		"BINARY", "VARBINARY":
		return TypeVarchar, nil
	case "TINYTEXT", "MEDIUMTEXT", "LONGTEXT", "JSON", "ENUM", "SET",
		"TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB", "TIME",
		"GEOMETRY", "POINT", "LINESTRING", "POLYGON", "MULTIPOINT",
		"MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION":
		return TypeText, nil
	case "BOOL":
		return TypeBoolean, nil
	case "BIT", "YEAR":
		return TypeBigInt, nil
	case "TIMESTAMP":
		return TypeDateTime, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidDataType, raw)
	}
}

func NewValue(dataType DataType, raw any) (Value, error) {
	normalizedType, err := ParseDataType(string(dataType))
	if err != nil {
		return Value{}, err
	}
	dataType = normalizedType
	if raw == nil {
		return NullValue(dataType), nil
	}

	switch dataType {
	case TypeInt:
		n, ok := toInt64(raw)
		if !ok || n < math.MinInt32 || n > math.MaxInt32 {
			return Value{}, typeError(dataType, raw)
		}
		return Value{Type: dataType, Int64: n}, nil
	case TypeBigInt:
		n, ok := toInt64(raw)
		if !ok {
			return Value{}, typeError(dataType, raw)
		}
		return Value{Type: dataType, Int64: n}, nil
	case TypeFloat, TypeDouble:
		n, ok := toFloat64(raw)
		if !ok || math.IsInf(n, 0) || math.IsNaN(n) {
			return Value{}, typeError(dataType, raw)
		}
		return Value{Type: dataType, Float: n}, nil
	case TypeVarchar, TypeText:
		s, ok := raw.(string)
		if !ok {
			return Value{}, typeError(dataType, raw)
		}
		return Value{Type: dataType, Text: s}, nil
	case TypeBoolean:
		b, ok := raw.(bool)
		if !ok {
			if number, numeric := toInt64(raw); numeric && (number == 0 || number == 1) {
				b, ok = number == 1, true
			}
		}
		if !ok {
			return Value{}, typeError(dataType, raw)
		}
		return Value{Type: dataType, Bool: b}, nil
	case TypeDate:
		date, err := normalizeDate(raw)
		if err != nil {
			return Value{}, typeError(dataType, raw)
		}
		return Value{Type: dataType, Date: date}, nil
	case TypeDateTime:
		date, err := normalizeDateTime(raw)
		if err != nil {
			return Value{}, typeError(dataType, raw)
		}
		return Value{Type: dataType, Date: date}, nil
	default:
		return Value{}, fmt.Errorf("%w: %q", ErrInvalidDataType, dataType)
	}
}

// ColumnSQLType returns the persisted MySQL declaration used by metadata and
// SQL exports. Old snapshots without SQLType receive a canonical fallback.
func ColumnSQLType(column Column) string {
	return normalizeColumnSQLType(column)
}

// ColumnDataType returns the MySQL information_schema.DATA_TYPE name without
// length, precision, or modifiers.
func ColumnDataType(column Column) string {
	declared := ColumnSQLType(column)
	if open := strings.IndexByte(declared, '('); open >= 0 {
		declared = declared[:open]
	}
	base := strings.ToLower(strings.Fields(declared)[0])
	if base == "national" {
		return "varchar"
	}
	if base == "double" {
		return "double"
	}
	return base
}

func normalizeColumnSQLType(column Column) string {
	if declared := strings.TrimSpace(column.SQLType); declared != "" {
		return strings.ToLower(declared)
	}
	if column.Type == TypeVarchar {
		length := column.Length
		if length <= 0 {
			length = 255
		}
		return fmt.Sprintf("varchar(%d)", length)
	}
	return strings.ToLower(string(column.Type))
}

func MustValue(dataType DataType, raw any) Value {
	v, err := NewValue(dataType, raw)
	if err != nil {
		panic(err)
	}
	return v
}

func NullValue(dataType DataType) Value {
	return Value{Type: dataType, Null: true}
}

func (v Value) Interface() any {
	if v.Null {
		return nil
	}
	switch v.Type {
	case TypeInt, TypeBigInt:
		return v.Int64
	case TypeFloat, TypeDouble:
		return v.Float
	case TypeVarchar, TypeText:
		return v.Text
	case TypeBoolean:
		return v.Bool
	case TypeDate, TypeDateTime:
		return v.Date
	default:
		return nil
	}
}

func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	switch v.Type {
	case TypeInt, TypeBigInt:
		return fmt.Sprintf("%d", v.Int64)
	case TypeFloat, TypeDouble:
		return fmt.Sprintf("%g", v.Float)
	case TypeVarchar, TypeText:
		return v.Text
	case TypeBoolean:
		return fmt.Sprintf("%t", v.Bool)
	case TypeDate:
		return v.Date.Format(dateLayout)
	case TypeDateTime:
		return v.Date.Format(dateTimeLayout)
	default:
		return ""
	}
}

func typeError(dataType DataType, raw any) error {
	return fmt.Errorf("%w: cannot store %T as %s", ErrTypeMismatch, raw, dataType)
}

func toInt64(raw any) (int64, bool) {
	switch n := raw.(type) {
	case int:
		return int64(n), int64(int(n)) == int64(n)
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		if uint64(n) > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

func toFloat64(raw any) (float64, bool) {
	switch n := raw.(type) {
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		integer, ok := toInt64(raw)
		return float64(integer), ok
	}
}

func normalizeDate(raw any) (time.Time, error) {
	var date time.Time
	switch value := raw.(type) {
	case time.Time:
		date = value
	case string:
		if len(value) >= len(dateLayout) {
			value = value[:len(dateLayout)]
		}
		parsed, err := time.Parse(dateLayout, value)
		if err != nil {
			return time.Time{}, err
		}
		date = parsed
	default:
		return time.Time{}, fmt.Errorf("unsupported date value")
	}
	return time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC), nil
}

func normalizeDateTime(raw any) (time.Time, error) {
	var date time.Time
	switch value := raw.(type) {
	case time.Time:
		date = value
	case string:
		var err error
		for _, layout := range []string{dateTimeLayout, "2006-01-02 15:04:05", dateLayout} {
			date, err = time.Parse(layout, value)
			if err == nil {
				break
			}
		}
		if err != nil {
			return time.Time{}, err
		}
	default:
		return time.Time{}, fmt.Errorf("unsupported datetime value")
	}
	// MySQL DATETIME stores a wall-clock value without timezone conversion.
	return time.Date(
		date.Year(), date.Month(), date.Day(),
		date.Hour(), date.Minute(), date.Second(), date.Nanosecond(),
		time.UTC,
	).Truncate(time.Microsecond), nil
}
