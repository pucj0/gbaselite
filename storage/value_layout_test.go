package storage

import (
	"bytes"
	"encoding/gob"
	"reflect"
	"testing"
	"time"
	"unsafe"
)

// Retain the original exported field order as a compatibility fixture.
type legacyValueLayout struct {
	Type  DataType
	Null  bool
	Int64 int64
	Float float64
	Text  string
	Bool  bool
	Date  time.Time
}

func TestCompactValueReadsLegacyGob(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(Value{}) > 80 {
		t.Fatalf("Value uses %d bytes", unsafe.Sizeof(Value{}))
	}
	old := []legacyValueLayout{
		{Type: TypeInt, Int64: -123}, {Type: TypeText, Text: "中文"}, {Type: TypeDateTime, Date: time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)},
		{Type: TypeBoolean, Bool: true}, {Type: TypeDouble, Float: 1.25}, {Type: TypeInt, Null: true},
	}
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(old); err != nil {
		t.Fatal(err)
	}
	var decoded []Value
	if err := gob.NewDecoder(&encoded).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	for i, value := range old {
		expected := Value{Type: value.Type, Null: value.Null, Int64: value.Int64, Float: value.Float, Text: value.Text, Bool: value.Bool, Date: value.Date}
		if !reflect.DeepEqual(decoded[i], expected) {
			t.Fatalf("value %d changed: %+v", i, decoded[i])
		}
	}
	encoded.Reset()
	if err := gob.NewEncoder(&encoded).Encode(decoded); err != nil {
		t.Fatal(err)
	}
	var roundtrip []legacyValueLayout
	if err := gob.NewDecoder(&encoded).Decode(&roundtrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundtrip, old) {
		t.Fatal("new layout cannot be read by legacy layout")
	}
}
