package storage

import (
	"errors"
	"testing"
	"time"
)

func TestNewValue(t *testing.T) {
	tests := []struct {
		name     string
		dataType DataType
		raw      any
		want     any
	}{
		{name: "int", dataType: TypeInt, raw: 42, want: int64(42)},
		{name: "bigint", dataType: TypeBigInt, raw: int64(9_000_000_000), want: int64(9_000_000_000)},
		{name: "float", dataType: TypeFloat, raw: float32(1.5), want: float64(1.5)},
		{name: "double", dataType: TypeDouble, raw: 3.25, want: 3.25},
		{name: "varchar", dataType: TypeVarchar, raw: "alice", want: "alice"},
		{name: "text", dataType: TypeText, raw: "hello", want: "hello"},
		{name: "boolean", dataType: TypeBoolean, raw: true, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewValue(tt.dataType, tt.raw)
			if err != nil {
				t.Fatalf("NewValue() error = %v", err)
			}
			if got.Interface() != tt.want {
				t.Fatalf("Interface() = %#v, want %#v", got.Interface(), tt.want)
			}
		})
	}
}

func TestDateAndNullValues(t *testing.T) {
	value, err := NewValue(TypeDate, "2026-07-27")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	if !value.Date.Equal(want) {
		t.Fatalf("date = %v, want %v", value.Date, want)
	}

	null := NullValue(TypeText)
	if !null.Null || null.Interface() != nil || null.String() != "NULL" {
		t.Fatalf("unexpected null value: %#v", null)
	}
}

func TestNewValueRejectsInvalidValue(t *testing.T) {
	_, err := NewValue(TypeInt, "42")
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("error = %v, want ErrTypeMismatch", err)
	}
}

func TestNewValueNormalizesDataType(t *testing.T) {
	value, err := NewValue(DataType("int"), 42)
	if err != nil {
		t.Fatal(err)
	}
	if value.Type != TypeInt || value.Int64 != 42 {
		t.Fatalf("unexpected normalized value: %#v", value)
	}
}

func TestMySQLDataTypeAliases(t *testing.T) {
	tests := map[string]DataType{
		"tinyint": TypeInt, "decimal": TypeDouble, "char": TypeVarchar,
		"tinytext": TypeText, "longblob": TypeText, "bool": TypeBoolean,
		"datetime": TypeDateTime, "timestamp": TypeDateTime, "time": TypeText,
		"year": TypeBigInt, "bit": TypeBigInt, "json": TypeText, "enum": TypeText,
		"point": TypeText, "geometrycollection": TypeText, "national": TypeVarchar,
	}
	for raw, want := range tests {
		got, err := ParseDataType(raw)
		if err != nil {
			t.Fatalf("ParseDataType(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseDataType(%q) = %s, want %s", raw, got, want)
		}
	}
	value, err := NewValue(DataType("datetime"), "2026-07-27 17:20:43")
	if err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "2026-07-27 17:20:43" {
		t.Fatalf("datetime value = %q", got)
	}
}

func TestColumnSQLTypeMetadata(t *testing.T) {
	tests := []struct {
		column   Column
		sqlType  string
		dataType string
	}{
		{Column{Type: TypeVarchar, Length: 64}, "varchar(64)", "varchar"},
		{Column{Type: TypeDouble, SQLType: "DECIMAL(20,6) UNSIGNED"}, "decimal(20,6) unsigned", "decimal"},
		{Column{Type: TypeText, SQLType: "ENUM('new','done')"}, "enum('new','done')", "enum"},
	}
	for _, test := range tests {
		if got := ColumnSQLType(test.column); got != test.sqlType {
			t.Fatalf("ColumnSQLType(%#v) = %q, want %q", test.column, got, test.sqlType)
		}
		if got := ColumnDataType(test.column); got != test.dataType {
			t.Fatalf("ColumnDataType(%#v) = %q, want %q", test.column, got, test.dataType)
		}
	}
}
