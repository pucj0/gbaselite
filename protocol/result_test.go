package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"gbaselite/executor"
	"gbaselite/storage"
)

func TestOKPacketLegacyInfo(t *testing.T) {
	got := OKPacketWithCapabilities(2, "table created", ClientProtocol41)
	want := append([]byte{0, 2, 0, 2, 0, 0, 0}, []byte("table created")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("OK packet = %v, want %v", got, want)
	}
}

func TestColumnDefinitionIncludesEditableFieldMetadata(t *testing.T) {
	packet := ColumnDefinition(executor.Column{Name: "identifier", OriginalName: "id", Type: storage.TypeInt, Schema: "catalog", Table: "items", Nullable: false, PrimaryKey: true, AutoIncrement: true}, "", "")
	position := 0
	values := make([]string, 6)
	for index := range values {
		length := int(packet[position])
		position++
		values[index] = string(packet[position : position+length])
		position += length
	}
	if values[1] != "catalog" || values[2] != "items" || values[4] != "identifier" || values[5] != "id" {
		t.Fatalf("unexpected field origin: %#v", values)
	}
	flags := binary.LittleEndian.Uint16(packet[position+8 : position+10])
	if flags&0x0001 == 0 || flags&0x0002 == 0 || flags&0x0200 == 0 {
		t.Fatalf("field flags = %#x", flags)
	}
}

func TestColumnDefinitionUsesDeclaredVarcharLength(t *testing.T) {
	packet := ColumnDefinition(executor.Column{Name: "code", Type: storage.TypeVarchar, Length: 64}, "catalog", "items")
	position := 0
	for range 6 {
		length := int(packet[position])
		position += 1 + length
	}
	columnLength := binary.LittleEndian.Uint32(packet[position+3 : position+7])
	if columnLength != 256 {
		t.Fatalf("column length = %d, want 256 utf8mb4 bytes", columnLength)
	}
}

func TestAppendBinaryDateTime(t *testing.T) {
	value := time.Date(2026, 7, 28, 15, 16, 17, 123456000, time.UTC)
	encoded := appendBinaryValue(nil, value, storage.TypeDateTime)
	if len(encoded) != 12 || encoded[0] != 11 || binary.LittleEndian.Uint32(encoded[8:]) != 123456 {
		t.Fatalf("unexpected binary DATETIME: %v", encoded)
	}
}

func TestAppendBinaryStorageValueMatchesInterfaceEncoding(t *testing.T) {
	date := time.Date(2026, 7, 28, 15, 16, 17, 123456000, time.UTC)
	values := []storage.Value{
		storage.MustValue(storage.TypeInt, 42),
		storage.MustValue(storage.TypeBigInt, int64(1)<<40),
		storage.MustValue(storage.TypeFloat, 1.25),
		storage.MustValue(storage.TypeDouble, 9.5),
		storage.MustValue(storage.TypeBoolean, true),
		storage.MustValue(storage.TypeVarchar, "value"),
		storage.MustValue(storage.TypeText, "text"),
		storage.MustValue(storage.TypeDate, date),
		storage.MustValue(storage.TypeDateTime, date),
	}
	for _, value := range values {
		got := appendBinaryStorageValue(nil, value)
		want := appendBinaryValue(nil, value.Interface(), value.Type)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s binary encoding = %v, want %v", value.Type, got, want)
		}
	}
}

func TestAppendTextDateTimeUsesColumnType(t *testing.T) {
	value := time.Date(2026, 7, 28, 15, 16, 17, 123456000, time.UTC)
	date := appendLenEncTypedValue(nil, value, storage.TypeDate)
	dateTime := appendLenEncTypedValue(nil, value, storage.TypeDateTime)
	if string(date[1:]) != "2026-07-28" {
		t.Fatalf("text DATE = %q", date[1:])
	}
	if string(dateTime[1:]) != "2026-07-28 15:16:17.123456" {
		t.Fatalf("text DATETIME = %q", dateTime[1:])
	}
}

func TestOKPacketSessionTrackInfoIsLengthEncoded(t *testing.T) {
	got := OKPacketWithCapabilities(0, "table created", ClientProtocol41|ClientSessionTrack)
	want := append([]byte{0, 0, 0, 2, 0, 0, 0, 13}, []byte("table created")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("session-track OK packet = %v, want %v", got, want)
	}
}

func TestOKPacketMarksRenamedMetadata(t *testing.T) {
	result := &executor.Result{Message: "view created", MetadataChanged: true}
	packet := okPacketWithStatus(0, 0, result.Message, ClientProtocol41, resultStatus(result))
	status := binary.LittleEndian.Uint16(packet[3:5])
	if status&serverStatusMetadataChanged == 0 {
		t.Fatalf("OK status = %#x, metadata-changed flag missing", status)
	}
}
