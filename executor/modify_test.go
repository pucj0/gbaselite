package executor

import (
	"bytes"
	"encoding/binary"
	"gbaselite/storage"
	"testing"
)

// The A05 base model tests are pure model tests: they pin the identity,
// ownership and result semantics later phases build on, without touching SQL.

func TestRowIdentityEqualityUsesTableIDAndPhysicalKey(t *testing.T) {
	key := []byte{0x01, 0x02}
	same := NewRowIdentity("db/t", []byte{0x01, 0x02})
	other := NewRowIdentity("db/t", []byte{0x01, 0x03})

	if same.TableID != "db/t" {
		t.Fatalf("table id = %q", same.TableID)
	}
	if !bytes.Equal(same.Key, key) {
		t.Fatalf("key = %v, want %v", same.Key, key)
	}
	if !bytes.Equal(same.Key, NewRowIdentity("db/t", key).Key) {
		t.Fatal("same table id plus same key must describe the same target")
	}
	if bytes.Equal(same.Key, other.Key) {
		t.Fatal("different physical keys must describe different targets")
	}
}

func TestRowIdentityDedupKeySeparatesTablesWithEqualKeyBytes(t *testing.T) {
	// Two tables can observe the same storage key bytes; they are still two
	// targets, so the dedup ledger key must not be the storage key alone.
	first := NewRowIdentity("db/a", []byte{0x01})
	second := NewRowIdentity("db/b", []byte{0x01})

	firstKey, ok := first.StorageKey()
	if !ok {
		t.Fatal("valid identity has no dedup key")
	}
	secondKey, ok := second.StorageKey()
	if !ok {
		t.Fatal("valid identity has no dedup key")
	}
	if bytes.Equal(firstKey, secondKey) {
		t.Fatalf("equal storage keys from different tables collapsed: %v", firstKey)
	}

	// Encoding is [uint32 len(TableID)][TableID][Key], so a table identifier can
	// never absorb bytes of the storage key.
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(first.TableID)))
	if want := append(length[:], append([]byte(first.TableID), first.Key...)...); !bytes.Equal(firstKey, want) {
		t.Fatalf("dedup key = %v, want %v", firstKey, want)
	}
	if got := len(firstKey); got != 4+len(first.TableID)+len(first.Key) {
		t.Fatalf("dedup key length = %d", got)
	}

	// The name boundary is what keeps prefixed table names apart.
	if long, _ := NewRowIdentity("db/aa", []byte{0x01, 0x02}).StorageKey(); bytes.Equal(long, firstKey) {
		t.Fatalf("table name and key bytes were not separated: %v", long)
	}
}

func TestRowIdentityInvalidDoesNotProduceMutationTarget(t *testing.T) {
	cases := []struct {
		name     string
		identity RowIdentity
	}{
		// Outer join NULL extension and derived/CTE/view rows have no physical
		// provenance at all.
		{"zero value", RowIdentity{}},
		{"absent outer join target", RowIdentity{TableID: "db/t", Valid: false}},
		// A physically valid row always has a storage key, so a keyless identity is
		// also not a target.
		{"valid without key", RowIdentity{TableID: "db/t", Valid: true}},
		{"empty key", RowIdentity{TableID: "db/t", Key: []byte{}, Valid: true}},
	}
	for _, c := range cases {
		if c.identity.IsTarget() {
			t.Errorf("%s: reported a mutation target", c.name)
		}
		if key, ok := c.identity.StorageKey(); ok || key != nil {
			t.Errorf("%s: produced dedup key %v ok=%v", c.name, key, ok)
		}
	}

	// A real row whose columns are all NULL is still a target: absence is decided
	// by physical provenance, never by column values.
	real := NewRowIdentity("db/t", []byte{0x00})
	if !real.IsTarget() {
		t.Fatal("physical row without values is not a target")
	}
	if _, ok := real.StorageKey(); !ok {
		t.Fatal("physical row without values has no dedup key")
	}
}

func TestRowIdentityOwnsRetainedKey(t *testing.T) {
	borrowed := []byte{0x01, 0x02, 0x03}
	identity := NewRowIdentity("db/t", borrowed)

	// A later scan reusing its iterator buffer must not mutate the stored identity.
	for i := range borrowed {
		borrowed[i] = 0xFF
	}
	if want := []byte{0x01, 0x02, 0x03}; !bytes.Equal(identity.Key, want) {
		t.Fatalf("identity key = %v, want an independent copy %v", identity.Key, want)
	}
	if &identity.Key[0] == &borrowed[0] {
		t.Fatal("identity key aliases the borrowed buffer")
	}

	original := NewRowIdentity("db/t", []byte{0x04})
	cloned := original.Clone()
	for i := range original.Key {
		original.Key[i] = 0xFE
	}
	if want := []byte{0x04}; !bytes.Equal(cloned.Key, want) {
		t.Fatalf("cloned key = %v, want an independent copy %v", cloned.Key, want)
	}
	if cloned.TableID != original.TableID || cloned.Valid != original.Valid {
		t.Fatalf("clone lost identity fields: %+v", cloned)
	}

	// An empty key means lost physical provenance, so the constructor normalises
	// both nil and empty input to a nil key, which IsTarget rejects.
	if got := NewRowIdentity("db/t", nil); got.Key != nil {
		t.Fatalf("nil key became %v", got.Key)
	}
	if got := NewRowIdentity("db/t", []byte{}); got.Key != nil {
		t.Fatalf("empty key became %v", got.Key)
	}
	if got := NewRowIdentity("db/t", nil); got.IsTarget() {
		t.Fatal("nil key reported a mutation target")
	}
	// Clone still allocates for a non-nil key, including a zero-length one, so a
	// clone never aliases the borrowed array it was taken from.
	if got := (RowIdentity{TableID: "db/t", Key: []byte{}, Valid: true}).Clone(); got.Key == nil {
		t.Fatal("cloning an empty key dropped the allocation")
	}
	invalid := RowIdentity{TableID: "db/t", Valid: false}
	if clonedInvalid := invalid.Clone(); clonedInvalid.Key != nil || clonedInvalid.Valid {
		t.Fatalf("invalid clone = %+v", clonedInvalid)
	}
}

func TestModifyCandidatesExposeTargetIdentity(t *testing.T) {
	row := storage.Row{storage.NullValue(storage.TypeInt)}
	valid := NewRowIdentity("db/t", []byte{0x07})

	update := UpdateCandidate{Identity: valid, OldRow: row, EvalRow: row}
	if _, ok := update.Target(); !ok {
		t.Fatal("update candidate has no target")
	}
	deleteCandidate := DeleteCandidate{Identity: valid, OldRow: row}
	if _, ok := deleteCandidate.Target(); !ok {
		t.Fatal("delete candidate has no target")
	}
	for _, identity := range []RowIdentity{{}, {TableID: "db/t"}, {TableID: "db/t", Valid: false}} {
		if _, ok := (UpdateCandidate{Identity: identity}).Target(); ok {
			t.Errorf("update candidate with %+v reported a target", identity)
		}
		if _, ok := (DeleteCandidate{Identity: identity}).Target(); ok {
			t.Errorf("delete candidate with %+v reported a target", identity)
		}
	}

	// INSERT creates rows instead of addressing them, so it never carries a target:
	// two identical VALUES rows must not be deduplicated into one.
	insert := InsertCandidate{Values: row, Ordinal: 0}
	if _, ok := insert.Target(); ok {
		t.Fatal("insert candidate reported a pre-existing target")
	}
	if _, ok := (InsertCandidate{Values: row, Ordinal: 1}).Target(); ok {
		t.Fatal("insert candidate reported a pre-existing target")
	}
}

func TestModifyResultKeepsFirstGeneratedID(t *testing.T) {
	result := &ModifyResult{}
	if result.HasGeneratedID || result.FirstGeneratedID != 0 || result.AffectedRows != 0 {
		t.Fatalf("zero value = %+v", result)
	}
	result.recordGenerated(11, true)
	result.recordGenerated(12, true)
	result.recordGenerated(0, false)
	result.AffectedRows++
	if !result.HasGeneratedID || result.FirstGeneratedID != 11 {
		t.Fatalf("first generated id = %d has=%v", result.FirstGeneratedID, result.HasGeneratedID)
	}
	if result.AffectedRows != 1 {
		t.Fatalf("affected rows = %d", result.AffectedRows)
	}
	// A generated id of zero stays representable: the explicit flag, not a zero
	// sentinel, decides whether an id was produced.
	zero := &ModifyResult{}
	zero.recordGenerated(0, true)
	if !zero.HasGeneratedID || zero.FirstGeneratedID != 0 {
		t.Fatalf("zero generated id = %+v", zero)
	}
}
