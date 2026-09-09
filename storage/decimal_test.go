package storage

import (
	"bytes"
	"encoding/gob"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

func TestDecimalParsingQuantizationAndBounds(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"+000123.4500", "123.4500"}, {"-.0100", "-0.0100"}, {"-0.000", "0.000"}, {"1.25e2", "125"}, {".01e-2", "0.0001"}, {"9007199254740993.01", "9007199254740993.01"},
	} {
		got, err := ParseDecimal(tc.input)
		if err != nil || string(got) != tc.want {
			t.Fatalf("parse %q = %q, %v", tc.input, got, err)
		}
	}
	for _, input := range []string{"", ".", "--1", "1e999999", "1.2.3", "NaN", "Inf", strings.Repeat("9", 66), "0." + strings.Repeat("0", 30) + "1"} {
		if _, err := ParseDecimal(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	for _, tc := range []struct{ input, want string }{{"1.235", "1.24"}, {"-1.235", "-1.24"}, {"-0.004", "0.00"}, {"99", "99.00"}} {
		d, _ := ParseDecimal(tc.input)
		got, err := d.Quantize(5, 2)
		if err != nil || string(got) != tc.want {
			t.Fatalf("quantize %s = %s,%v", tc.input, got, err)
		}
	}
	d, _ := ParseDecimal("9.995")
	if _, err := d.Quantize(3, 2); !errors.Is(err, ErrDecimalRange) {
		t.Fatalf("rounding overflow = %v", err)
	}
	d, _ = ParseDecimal("0.999")
	if _, err := d.Quantize(3, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := DecimalColumnValue(Column{Type: TypeDecimal, SQLType: "decimal(5,2) unsigned"}, "-0.01"); err == nil {
		t.Fatal("unsigned accepted negative")
	}
	if size := unsafe.Sizeof(Value{}); size != 80 {
		t.Fatalf("Value grew to %d bytes", size)
	}
}
func TestDecimalExactArithmeticAndComparison(t *testing.T) {
	for _, tc := range []struct{ left, op, right, want string }{
		{"0.1", "+", "0.2", "0.3"}, {"9007199254740993.01", "+", "0.01", "9007199254740993.02"}, {"1.00", "-", "0.01", "0.99"}, {"123.45", "*", "2.0", "246.900"}, {"1.00", "/", "8", "0.125000"}, {"5.05", "/", "0.014", "360.714286"}, {"-2", "/", "3", "-0.6667"}, {"-12.34", "%", "2.0", "-0.34"},
	} {
		a, _ := ParseDecimal(tc.left)
		b, _ := ParseDecimal(tc.right)
		got, err := DecimalBinary(tc.op, a, b)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s %s %s = %s,%v", a, tc.op, b, got, err)
		}
	}
	pairs := [][3]string{{"2.0", "10", "-1"}, {"-10", "-2", "-1"}, {"1.000", "1", "0"}, {"9007199254740993", "9007199254740992", "1"}, {"0.001", "0.01", "-1"}}
	for _, tc := range pairs {
		a, _ := ParseDecimal(tc[0])
		b, _ := ParseDecimal(tc[1])
		cmp := CompareDecimal(a, b)
		if (cmp < 0 && tc[2] != "-1") || (cmp == 0 && tc[2] != "0") || (cmp > 0 && tc[2] != "1") {
			t.Fatalf("compare %s %s = %d", a, b, cmp)
		}
	}
	if DecimalKey("1.000") != DecimalKey("1") {
		t.Fatal("noncanonical unique keys")
	}
	if _, err := DecimalBinary("+", Decimal(strings.Repeat("9", 65)), "1"); !errors.Is(err, ErrDecimalRange) {
		t.Fatalf("arithmetic overflow = %v", err)
	}
}
func TestDecimalValueGobAndLegacyConversion(t *testing.T) {
	value, err := NewValue(TypeDecimal, "9007199254740993.01")
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(value); err != nil {
		t.Fatal(err)
	}
	var restored Value
	if err := gob.NewDecoder(&buffer).Decode(&restored); err != nil {
		t.Fatal(err)
	}
	if restored != value || restored.Interface() != Decimal("9007199254740993.01") {
		t.Fatalf("roundtrip %#v", restored)
	}
	old := TableSnapshot{Name: "money", Columns: []Column{{Name: "amount", Type: TypeDouble, SQLType: "decimal(8,2)", HasDefault: true, Default: MustValue(TypeDouble, 1.25)}}, Rows: []Row{{MustValue(TypeDouble, 12.345)}}}
	converted, err := NormalizeDecimalSnapshot(old)
	if err != nil {
		t.Fatal(err)
	}
	if converted.Columns[0].Type != TypeDecimal || converted.Rows[0][0].Text != "12.35" || converted.Columns[0].Default.Text != "1.25" {
		t.Fatalf("upgrade %#v", converted)
	}
	if old.Columns[0].Type != TypeDouble || old.Rows[0][0].Type != TypeDouble {
		t.Fatal("upgrade mutated shared input")
	}
}
func BenchmarkCompareDecimal(b *testing.B) {
	left := Decimal("123456789012345678901234567890.1234567890")
	right := Decimal("123456789012345678901234567890.1234567891")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if CompareDecimal(left, right) >= 0 {
			b.Fatal("wrong comparison")
		}
	}
}
func BenchmarkDecimalAdd(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := DecimalBinary("+", "123456789.12", "0.01"); err != nil {
			b.Fatal(err)
		}
	}
}
