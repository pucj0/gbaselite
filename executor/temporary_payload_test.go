package executor

import (
	"reflect"
	"testing"
	"time"

	"gbaselite/storage"
)

// This file pins the temporary spill codec: the query-temporary format the modify pipeline uses to
// carry a candidate through a spillable sorter.
//
// Two properties matter and both are asserted here:
//
//   - the round trip is lossless for every shape storage.Value can take, including the declared
//     DataType, because the write path validates a column against its declared type;
//   - a decoded candidate owns its bytes, so nothing it returns aliases the sorter's read buffer.

func timeAt(t *testing.T, layout, text string) time.Time {
	t.Helper()
	value, err := time.Parse(layout, text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// TestTemporaryPayloadCodecRoundTripsEveryValueShape walks one row holding every value shape the
// codec has to survive, and compares field for field after a round trip.
func TestTemporaryPayloadCodecRoundTripsEveryValueShape(t *testing.T) {
	zone := time.FixedZone("offset", 2*60*60)
	row := storage.Row{
		{Type: storage.TypeInt, Int64: -7},
		{Type: storage.TypeBigInt, Int64: 9007199254740993},
		{Type: storage.TypeFloat, Float: 1.5},
		{Type: storage.TypeDouble, Float: -2.25},
		{Type: storage.TypeDecimal, Float: 12.5, Text: "12.50"},
		{Type: storage.TypeVarchar, Text: "varchar value"},
		{Type: storage.TypeText, Text: "text value"},
		{Type: storage.TypeBoolean, Bool: true},
		{Type: storage.TypeBoolean, Bool: false},
		{Type: storage.TypeDateTime, Date: timeAt(t, "2006-01-02 15:04:05", "2024-03-04 05:06:07").In(zone)},
		{Type: storage.TypeDate, Date: timeAt(t, "2006-01-02", "2020-12-31").UTC()},
		{Type: storage.TypeInt, Null: true},
		{Type: storage.TypeVarchar, Null: true},
	}
	encoded := encodeTemporaryRow(nil, row)
	if got := temporaryRowBytes(row); got != len(encoded) {
		t.Fatalf("temporaryRowBytes = %d, encoded = %d: the width estimate must match the encoder", got, len(encoded))
	}
	decoded, err := decodeTemporaryRow(&temporaryRowReader{data: encoded})
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(row) {
		t.Fatalf("decoded %d values, want %d", len(decoded), len(row))
	}
	for i := range row {
		want, got := row[i], decoded[i]
		if want.Type != got.Type || want.Null != got.Null || want.Text != got.Text ||
			want.Int64 != got.Int64 || want.Float != got.Float || want.Bool != got.Bool {
			t.Fatalf("value %d = %#v, want %#v", i, got, want)
		}
		if !want.Date.Equal(got.Date) {
			t.Fatalf("value %d date = %s, want %s", i, got.Date, want.Date)
		}
		// The instant and its rendered form both survive; the zone is reconstructed from the offset.
		if _, wantOffset := want.Date.Zone(); true {
			_, gotOffset := got.Date.Zone()
			if wantOffset != gotOffset {
				t.Fatalf("value %d zone offset = %d, want %d", i, gotOffset, wantOffset)
			}
		}
	}
}

// TestTemporaryPayloadCodecDecodedRowIsOwned verifies a decoded candidate shares no backing array
// with the buffer it was read from, which is what lets it outlive the sorter's read buffer.
func TestTemporaryPayloadCodecDecodedRowIsOwned(t *testing.T) {
	row := storage.Row{{Type: storage.TypeVarchar, Text: "shared-buffer"}}
	encoded := encodeTemporaryRow(nil, row)
	decoded, err := decodeTemporaryRow(&temporaryRowReader{data: encoded})
	if err != nil {
		t.Fatal(err)
	}
	// Clobber the source buffer the way a reused read buffer would.
	for i := range encoded {
		encoded[i] = 0xFF
	}
	if decoded[0].Text != "shared-buffer" {
		t.Fatalf("decoded text aliases the read buffer: %q", decoded[0].Text)
	}
}

func TestTemporaryPayloadCodecRejectsCorruptRow(t *testing.T) {
	if _, err := decodeTemporaryRow(&temporaryRowReader{data: []byte{0xFF, 0xFF, 0xFF, 0xFF}}); err == nil {
		t.Fatal("a row count beyond the buffer was accepted")
	}
	if _, err := decodeTemporaryRow(&temporaryRowReader{data: []byte{1, 0, 0, 0, 99}}); err == nil {
		t.Fatal("an unknown value tag was accepted")
	}
}

// TestUpdateJoinCodecRoundTripsCandidate verifies the UPDATE JOIN payload survives a round trip: the
// old physical identity, the ordinal that decided first occurrence, and the whole joined row.
func TestUpdateJoinCodecRoundTripsCandidate(t *testing.T) {
	codec := updateJoinCodec{}
	candidate := updateJoinCandidate{
		Ordinal:  42,
		Identity: NewRowIdentity("test.t", []byte{0x80, 0x01}),
		OldRow:   storage.Row{{Type: storage.TypeInt, Int64: 1}},
		EvalRow: storage.Row{
			{Type: storage.TypeInt, Int64: 1},
			{Type: storage.TypeVarchar, Text: "joined"},
			{Type: storage.TypeBigInt, Int64: 7},
		},
	}
	encoded, err := codec.encode(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := codec.bytes(candidate); got != len(encoded) {
		t.Fatalf("codec.bytes = %d, encoded = %d", got, len(encoded))
	}
	decoded, err := codec.decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Ordinal != candidate.Ordinal {
		t.Fatalf("ordinal = %d, want %d", decoded.Ordinal, candidate.Ordinal)
	}
	if decoded.Identity.TableID != candidate.Identity.TableID || string(decoded.Identity.Key) != string(candidate.Identity.Key) || !decoded.Identity.Valid {
		t.Fatalf("identity = %#v, want %#v", decoded.Identity, candidate.Identity)
	}
	if !reflect.DeepEqual(decoded.EvalRow, candidate.EvalRow) {
		t.Fatalf("eval row = %#v, want %#v", decoded.EvalRow, candidate.EvalRow)
	}
	// The write path reads OldRow restricted to the target's own columns.
	if !reflect.DeepEqual(decoded.rebuild(1).OldRow, candidate.OldRow) {
		t.Fatalf("rebuilt old row = %#v, want %#v", decoded.rebuild(1).OldRow, candidate.OldRow)
	}
}

// TestMultiDeleteCodecRoundTripsCandidate verifies the multi-table DELETE payload survives a round
// trip: which target it addresses, that target's physical identity, and the target's own old row.
func TestMultiDeleteCodecRoundTripsCandidate(t *testing.T) {
	codec := multiDeleteCodec{}
	candidate := multiDeleteCandidate{
		target:   3,
		Identity: NewRowIdentity("test.heap", []byte{0x01, 0x02, 0x03}),
		OldRow: storage.Row{
			{Type: storage.TypeVarchar, Text: "no primary key"},
			{Type: storage.TypeDouble, Float: 2.5},
		},
	}
	encoded, err := codec.encode(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := codec.bytes(candidate); got != len(encoded) {
		t.Fatalf("codec.bytes = %d, encoded = %d", got, len(encoded))
	}
	decoded, err := codec.decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.target != candidate.target {
		t.Fatalf("target = %d, want %d", decoded.target, candidate.target)
	}
	if decoded.Identity.TableID != candidate.Identity.TableID || string(decoded.Identity.Key) != string(candidate.Identity.Key) {
		t.Fatalf("identity = %#v, want %#v", decoded.Identity, candidate.Identity)
	}
	if !reflect.DeepEqual(decoded.OldRow, candidate.OldRow) {
		t.Fatalf("old row = %#v, want %#v", decoded.OldRow, candidate.OldRow)
	}
}
