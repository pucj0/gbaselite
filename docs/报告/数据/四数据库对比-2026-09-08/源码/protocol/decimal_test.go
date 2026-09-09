package protocol

import (
	"bytes"
	"gbaselite/executor"
	"gbaselite/storage"
	"testing"
)

func TestDecimalProtocolExactTextAndMetadata(t *testing.T) {
	d := storage.Decimal("9007199254740993.01")
	expected := append([]byte{byte(len(d))}, []byte(d)...)
	if got := appendLenEncValue(nil, d); !bytes.Equal(got, expected) {
		t.Fatalf("text = %q", got)
	}
	if got := appendBinaryValue(nil, d, storage.TypeDecimal); !bytes.Equal(got, expected) {
		t.Fatalf("binary = %q", got)
	}
	value := storage.Value{Type: storage.TypeDecimal, Text: string(d)}
	if got := appendBinaryStorageValue(nil, value); !bytes.Equal(got, expected) {
		t.Fatalf("storage binary = %q", got)
	}
	definition := ColumnDefinition(executor.Column{Name: "amount", Type: storage.TypeDecimal}, "db", "money")
	if definition[len(definition)-6] != TypeNewDecimal {
		t.Fatalf("not NEWDECIMAL metadata: %x", definition)
	}
}

func TestDecimalPreparedParameterDecode(t *testing.T) {
	text := []byte("9007199254740993.01")
	for _, code := range []byte{MySQLTypeDecimal, MySQLTypeNewDecimal} {
		packet := make([]byte, 9)
		packet = append(packet, 0, 1, code, 0, byte(len(text)))
		packet = append(packet, text...)
		_, values, _, err := DecodeStmtExecute(packet, 1, nil)
		if err != nil || len(values) != 1 || values[0] != storage.Decimal(string(text)) {
			t.Fatalf("decode type %d: %#v, %v", code, values, err)
		}
	}
	packet := make([]byte, 9)
	packet = append(packet, 0, 1, MySQLTypeNewDecimal, 0, 3, 'x', 'y', 'z')
	if _, _, _, err := DecodeStmtExecute(packet, 1, nil); err == nil {
		t.Fatal("invalid decimal accepted")
	}
}

func TestDecimalProtocolDeclaredScale(t *testing.T) {
	definition := ColumnDefinition(executor.Column{Name: "amount", Type: storage.TypeDecimal, SQLType: "decimal(25,2)"}, "db", "money")
	if definition[len(definition)-3] != 2 {
		t.Fatalf("decimal scale metadata %x", definition)
	}
}
