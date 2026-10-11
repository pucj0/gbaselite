package lockmanager

import (
	"testing"
)

// recordRef builds one Record component with a mode, for conflict assertions that do not depend
// on interval bounds.
func recordRef(mode Mode) ResourceRef {
	return ResourceRef{Identity: Identity{Kind: KindRecord, Mode: mode, Space: "rows", TableID: "t1", Key: []byte("k")}}
}

// compatibility_test.go exhaustively pins the published compatibility matrix.
//
// The matrix is the one thing every later lock decision depends on, so it is asserted
// as a table rather than by spot checks: every ordered pair of the six B02 modes is
// compared, and the three contract tables (record, table intention, and the intention
// prerequisite) are reproduced literally.
//
// The "held" side of every pair is what an *other* owner holds; same-owner re-entrancy
// is deliberately outside the matrix and is asserted separately.

// TestB02RecordMatrixExhaustive reproduces the contract's record table for every ordered
// pair, and pins the boundary that S/S is the only compatible record pair.
func TestB02RecordMatrixExhaustive(t *testing.T) {
	// request \ held, exactly as contracts/lock-manager.md states it.
	table := map[Mode]map[Mode]bool{
		ModeS: {ModeS: true, ModeX: false},
		ModeX: {ModeS: false, ModeX: false},
	}
	for _, request := range []Mode{ModeS, ModeX} {
		for _, held := range []Mode{ModeS, ModeX} {
			want := table[request][held]
			if got := modesCompatible(request, held); got != want {
				t.Fatalf("record matrix[request=%s][held=%s] = %v, want %v", request, held, got, want)
			}
			if got := componentsConflict(recordRef(request), recordRef(held)); got == want {
				t.Fatalf("componentsConflict[request=%s][held=%s] = %v is not the negation of the matrix", request, held, got)
			}
		}
	}
}

// TestB02TableIntentionMatrixExhaustive reproduces the contract's table-intention table:
// all four combinations of IS and IX are compatible with each other.
func TestB02TableIntentionMatrixExhaustive(t *testing.T) {
	for _, request := range []Mode{ModeIS, ModeIX} {
		for _, held := range []Mode{ModeIS, ModeIX} {
			if !modesCompatible(request, held) {
				t.Fatalf("table intention matrix[request=%s][held=%s] = false, want true", request, held)
			}
		}
	}
	// A table intention is also compatible with a record lock in both directions: the
	// intention describes the table, the record lock describes one key.
	for _, intention := range []Mode{ModeIS, ModeIX} {
		for _, record := range []Mode{ModeS, ModeX} {
			if !modesCompatible(intention, record) {
				t.Fatalf("intention %s against held record %s must be compatible", intention, record)
			}
			if !modesCompatible(record, intention) {
				t.Fatalf("record %s against held intention %s must be compatible", record, intention)
			}
		}
	}
}

// TestB02MatrixIsSymmetric proves the matrix has no direction-dependent answer for the
// mode pairs that are meant to be mutual, which is what keeps a grant decision
// independent of which request arrived first.
func TestB02MatrixIsSymmetric(t *testing.T) {
	modes := []Mode{ModeIS, ModeIX, ModeS, ModeX, ModeGap, ModeInsertIntention}
	for _, a := range modes {
		for _, b := range modes {
			if modesCompatible(a, b) != modesCompatible(b, a) {
				t.Fatalf("matrix is asymmetric for %s/%s: %v vs %v", a, b, modesCompatible(a, b), modesCompatible(b, a))
			}
		}
	}
}

// TestB02UnsupportedModesFailClosed pins the explicit boundary of B02's mode set.
//
// B02 has no table S/X and no SIX, and the contract forbids automatic table escalation.
// A table component carrying a record mode is therefore refused with a distinct error
// rather than being approximated by an intention lock, which is what "the spec needs a
// table-level S/X judgement" means here: the judgement is a refusal, and it is tested.
func TestB02TableSXAndSIXAreRejected(t *testing.T) {
	table := func(mode Mode) ResourceRef {
		return ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: mode, Space: "table", TableID: "t1"}}
	}
	for _, unsupported := range []Mode{ModeS, ModeX, "SIX", "TABLE_S", "TABLE_X"} {
		if unsupported.Valid() && !unsupported.Intention() {
			// A mode this package knows but that is not an intention: the kind/mode pair
			// must be refused.
			if err := ValidateComponent(table(unsupported)); err != ErrUnsupportedMode {
				t.Fatalf("table %s = %v, want ErrUnsupportedMode", unsupported, err)
			}
		} else if err := ValidateComponent(table(unsupported)); err != ErrInvalidResource && err != ErrUnsupportedMode {
			t.Fatalf("table %q = %v, want a fail-closed error", unsupported, err)
		}
	}
	// A record component carrying an intention mode is refused for the same reason.
	recordWithIntention := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeIS, Space: "rows", Key: []byte("k")}}
	if err := ValidateComponent(recordWithIntention); err != ErrUnsupportedMode && err != ErrInvalidResource {
		t.Fatalf("record with an intention mode = %v, want a fail-closed error", err)
	}
	// The legal table components still pass.
	for _, mode := range []Mode{ModeIS, ModeIX} {
		if err := ValidateComponent(table(mode)); err != nil {
			t.Fatalf("table %s must be legal: %v", mode, err)
		}
	}
	// The package's own mode set confirms there is no table S/X member at all.
	if ModeS.Intention() || ModeX.Intention() {
		t.Fatal("record modes must not be classified as table intentions")
	}
	if ModeIS.Record() || ModeIX.Record() {
		t.Fatal("intention modes must not be classified as record modes")
	}
}

// TestB02IntentionPrerequisite covers the bundle-level rule: record S needs IS or IX, and
// record X or an insert intention needs IX, in the *same* bundle.
func TestB02IntentionPrerequisite(t *testing.T) {
	rows := func(mode Mode, tableID string) ResourceRef {
		return ResourceRef{Identity: Identity{Kind: KindRecord, Mode: mode, Space: "rows", TableID: tableID, Key: []byte("k")}}
	}
	intent := func(mode Mode, tableID string) ResourceRef {
		return ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: mode, Space: "table", TableID: tableID}}
	}
	cases := []struct {
		name       string
		components []ResourceRef
		wantErr    error
	}{
		{"S with IS", []ResourceRef{rows(ModeS, "t1"), intent(ModeIS, "t1")}, nil},
		{"S with IX", []ResourceRef{rows(ModeS, "t1"), intent(ModeIX, "t1")}, nil},
		{"X with IX", []ResourceRef{rows(ModeX, "t1"), intent(ModeIX, "t1")}, nil},
		{"X with only IS", []ResourceRef{rows(ModeX, "t1"), intent(ModeIS, "t1")}, ErrTableIntentRequired},
		{"S with no intention", []ResourceRef{rows(ModeS, "t1")}, ErrTableIntentRequired},
		{"X with no intention", []ResourceRef{rows(ModeX, "t1")}, ErrTableIntentRequired},
		{"S intending another table", []ResourceRef{rows(ModeS, "t1"), intent(ModeIS, "t2")}, ErrTableIntentRequired},
		{"record with no table at all", []ResourceRef{rows(ModeX, "")}, nil},
		{"empty bundle", nil, ErrInvalidResource},
	}
	for _, tc := range cases {
		got := ValidateBundle(tc.components, nil)
		if tc.wantErr == nil && got != nil {
			t.Fatalf("%s: ValidateBundle = %v, want nil", tc.name, got)
		}
		if tc.wantErr != nil && got != tc.wantErr {
			t.Fatalf("%s: ValidateBundle = %v, want %v", tc.name, got, tc.wantErr)
		}
	}

	// Both the shared and the exclusive requirement are reported once per (table, need),
	// and the missing list never repeats a table.
	missing := MissingTableIntents([]ResourceRef{rows(ModeS, "t1"), rows(ModeS, "t1")}, nil)
	if len(missing) != 1 || missing[0] != "t1" {
		t.Fatalf("missing intents = %v, want exactly [t1]", missing)
	}
	missing = MissingTableIntents([]ResourceRef{rows(ModeX, "t1"), rows(ModeS, "t2")}, nil)
	if len(missing) != 2 {
		t.Fatalf("missing intents = %v, want two distinct tables", missing)
	}
}

// TestB02IntentionPrerequisiteUsesNamespaceBinding proves the prerequisite can be
// satisfied from the adapter-published binding when the record component carries no
// table label, so a SQL write that only knows the namespace still meets the same table.
func TestB02IntentionPrerequisiteUsesNamespaceBinding(t *testing.T) {
	bindings := NewBindings(1)
	if err := bindings.Bind(NamespaceBinding{Namespace: "index/u1", TableID: "t1", IndexID: "u1", Node: "t1"}); err != nil {
		t.Fatal(err)
	}
	record := ResourceRef{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "index/u1", Key: []byte("k")}}
	if err := ValidateBundle([]ResourceRef{record}, bindings); err != ErrTableIntentRequired {
		t.Fatalf("bound namespace without a table intent = %v, want ErrTableIntentRequired", err)
	}
	intent := ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: "t1"}}
	if err := ValidateBundle([]ResourceRef{record, intent}, bindings); err != nil {
		t.Fatalf("bound namespace with the matching table intent = %v, want nil", err)
	}
	// An intention for a different table does not satisfy it.
	other := ResourceRef{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: "t9"}}
	if err := ValidateBundle([]ResourceRef{record, other}, bindings); err != ErrTableIntentRequired {
		t.Fatalf("bound namespace with a foreign table intent = %v, want ErrTableIntentRequired", err)
	}
}

// TestB02ReentrancyAndUpgradeDetection proves that holding a mode and re-requesting it
// needs no conversion, while requesting something stronger does.
func TestB02ReentrancyAndUpgradeDetection(t *testing.T) {
	cases := []struct {
		request, held Mode
		wantUpgrade   bool
	}{
		{ModeS, ModeS, false},
		{ModeIS, ModeIS, false},
		{ModeIX, ModeIX, false},
		{ModeX, ModeX, false},
		{ModeS, ModeX, false},
		{ModeIS, ModeIX, false},
		{ModeX, ModeS, true},
		{ModeIX, ModeIS, true},
		{ModeS, "", false},
		{ModeX, "", false},
	}
	for _, tc := range cases {
		if got := upgradeNeeded(tc.request, tc.held); got != tc.wantUpgrade {
			t.Fatalf("upgradeNeeded(request=%s, held=%q) = %v, want %v", tc.request, tc.held, got, tc.wantUpgrade)
		}
	}
	// A non-empty held mode with an unknown value must never be treated as an upgrade:
	// it is refused before this point, so reporting "no upgrade" is the conservative half.
	if upgradeNeeded(ModeS, Mode("BOGUS")) {
		t.Fatal("an unknown held mode must not report an upgrade")
	}
}

// TestB02EffectiveModeIsAProjectionNotAMatrix proves the strength projection used for a
// resource's reported state, and that it is never the conflict decision: IS and IX have
// different strength yet are compatible.
func TestB02EffectiveModeIsAProjectionNotAMatrix(t *testing.T) {
	if got := effectiveMode(ModeIS, ModeIX); got != ModeIX {
		t.Fatalf("effectiveMode(IS, IX) = %s, want IX", got)
	}
	if got := effectiveMode(ModeS, ModeX); got != ModeX {
		t.Fatalf("effectiveMode(S, X) = %s, want X", got)
	}
	if got := effectiveMode(ModeS, ModeS); got != ModeS {
		t.Fatalf("effectiveMode(S, S) = %s, want S", got)
	}
	if got := effectiveMode(); got != "" {
		t.Fatalf("effectiveMode() = %q, want empty", got)
	}
	// IS and IX differ in strength but are compatible: strength must not decide.
	if modeStrength(ModeIS) == modeStrength(ModeIX) {
		t.Fatal("test premise: IS and IX must differ in strength")
	}
	if !modesCompatible(ModeIS, ModeIX) {
		t.Fatal("IS and IX must be compatible despite differing strength")
	}
}

// TestB02DescribeProjectsResourceState pins the observable state projection.
func TestB02DescribeProjectsResourceState(t *testing.T) {
	cases := []struct {
		holders int
		mode    Mode
		waiters int
		want    LockState
	}{
		{0, "", 0, LockStateFree},
		{1, ModeS, 0, LockStateShared},
		{2, ModeS, 0, LockStateShared},
		{1, ModeX, 0, LockStateExclusive},
		{2, ModeX, 0, LockStateExclusive},
		{1, ModeS, 1, LockStateWaiting},
		{1, ModeX, 1, LockStateWaiting},
		{0, "", 1, LockStateWaiting},
	}
	for _, tc := range cases {
		if got := Describe(tc.holders, tc.mode, tc.waiters); got != tc.want {
			t.Fatalf("Describe(%d, %s, %d) = %s, want %s", tc.holders, tc.mode, tc.waiters, got, tc.want)
		}
	}
}

// TestB02ComponentValidationCoversEveryKind proves the component validator accepts
// exactly the legal mode/kind pairs and refuses everything else.
func TestB02ComponentValidationCoversEveryKind(t *testing.T) {
	legal := []ResourceRef{
		{Identity: Identity{Kind: KindTableIntent, Mode: ModeIS, Space: "table", TableID: "t1"}},
		{Identity: Identity{Kind: KindTableIntent, Mode: ModeIX, Space: "table", TableID: "t1"}},
		{Identity: Identity{Kind: KindRecord, Mode: ModeS, Space: "rows", Key: []byte("k")}},
		{Identity: Identity{Kind: KindRecord, Mode: ModeX, Space: "rows", Key: []byte("k")}},
		{Identity: Identity{Kind: KindRecord, Mode: ModeInsertIntention, Space: "rows", Key: []byte("k")}},
		{Identity: Identity{Kind: KindInterval, Mode: ModeGap, Space: "rows", Lower: []byte("a"), Upper: []byte("z")}},
	}
	for _, ref := range legal {
		if err := ValidateComponent(ref); err != nil {
			t.Fatalf("%s/%s must be legal: %v", ref.Kind, ref.Mode, err)
		}
	}
	illegal := []ResourceRef{
		{Identity: Identity{Kind: KindTableIntent, Mode: ModeS, Space: "table", TableID: "t1"}},
		{Identity: Identity{Kind: KindTableIntent, Mode: ModeX, Space: "table", TableID: "t1"}},
		{Identity: Identity{Kind: KindRecord, Mode: ModeIS, Space: "rows", Key: []byte("k")}},
		{Identity: Identity{Kind: KindRecord, Mode: ModeGap, Space: "rows", Key: []byte("k")}},
		{Identity: Identity{Kind: KindInterval, Mode: ModeX, Space: "rows"}},
		{Identity: Identity{Kind: ResourceKind("PAGE"), Mode: ModeS, Space: "rows", Key: []byte("k")}},
	}
	for _, ref := range illegal {
		if err := ValidateComponent(ref); err == nil {
			t.Fatalf("%s/%s must be refused", ref.Kind, ref.Mode)
		}
	}
}
