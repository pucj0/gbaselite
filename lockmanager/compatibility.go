package lockmanager

// compatibility.go is the single published compatibility matrix of B02.
//
// Every lock decision in the manager goes through this file, so there is exactly one
// place where "may these two modes coexist" is answered. Nothing here waits, allocates
// or touches a manager field: these are pure predicates over modes and component
// lists, which is what makes the whole matrix exhaustively testable.
//
// The rules come from contracts/lock-manager.md and are reproduced here in the order
// the contract states them:
//
//	Record:            (request S: S=Y X=N) (request X: S=N X=N)
//	Table intention:   (request IS: IS=Y IX=Y) (request IX: IS=Y IX=Y)
//	Interval:          decided by interval intersection, refined in US2
//
// The cross-owner table is only half the rule. A record request must also satisfy its
// table intention, and B02 has no table S/X: a table-level exclusive request is
// refused rather than approximated.

// modesCompatible reports whether a request mode may be granted while another owner
// holds held.
//
// Same-owner re-entrancy is deliberately NOT this function's business: an ancestor or
// child of one root always re-enters compatibly, so the manager never asks the matrix
// about its own owner. Keeping the two rules apart is what stops a re-entrant request
// from having to wait behind its own grant.
func modesCompatible(request, held Mode) bool {
	if !request.Valid() || !held.Valid() {
		// An unknown mode on either side must never be treated as compatible. The matrix
		// below is only defined for the published B02 mode set.
		return false
	}
	switch held {
	case ModeIS, ModeIX:
		// Table intentions are mutually compatible, and a table intention never
		// conflicts with a record lock: the intention is a declaration about the whole
		// table, the record lock is about one key.
		return true
	case ModeS:
		// A shared record lock excludes only another owner's exclusive record lock.
		return request != ModeX
	case ModeX:
		// An exclusive record lock excludes every other owner's record lock and every
		// other owner's insertion intention, because both touch the row it protects, and
		// an insert intention at this exact key is the claim on that same row. A gap and
		// a table intention are still compatible: the contract keeps the interval rule
		// independent of the record rule.
		return request == ModeGap || request.Intention()
	case ModeGap:
		// The bounds-aware rule is in componentsConflict, which decides gap-versus-insertion by
		// containment. This mode-only projection is only reachable through recordsConflict, so it
		// reports the conservative answer rather than pretending to know the bounds.
		return request != ModeInsertIntention
	case ModeInsertIntention:
		// An insertion intention is compatible with another insertion intention, with a record and
		// with a table intention; the gap and exclusive-record cases are decided with bounds in
		// componentsConflict.
		return request != ModeGap && request != ModeX
	default:
		return false
	}
}

// ValidateComponent reports why one component is not a legal B02 lock component.
//
// The kind/mode pairing is checked first, so an unsupported combination such as a table
// S or X reports ErrUnsupportedMode rather than a generic invalid-identity error. That
// distinction is what lets a caller tell "B02 does not have this lock" from "this
// component is malformed".
func ValidateComponent(ref ResourceRef) error {
	switch ref.Kind {
	case KindTableIntent:
		if !ref.Mode.Intention() {
			// A table component carrying a record mode is the shape a table S/X or SIX
			// request would take. B02 has no table S/X, no SIX and no automatic table
			// escalation, so this fails closed with a distinct error instead of being
			// approximated by an intention lock.
			return ErrUnsupportedMode
		}
	case KindRecord:
		if !ref.Mode.Record() && ref.Mode != ModeInsertIntention {
			return ErrUnsupportedMode
		}
	case KindInterval:
		if ref.Mode != ModeGap {
			return ErrUnsupportedMode
		}
	default:
		return ErrInvalidResource
	}
	return ref.Identity.Validate()
}

// ValidateBundle checks every component of one atomic bundle and the bundle-level
// prerequisites.
//
// The bundle is validated as a whole because the prerequisites are properties of the bundle,
// not of a component:
//
//   - a record X component is legal exactly when the same bundle pins an IX on the same table,
//     or a namespace binding supplies that table. A caller therefore cannot acquire a record lock
//     and only afterwards remember its table intention, which is what keeps the table intention
//     part of the same atomic grant;
//   - an Insert Intention component is legal exactly when the same bundle also takes the record
//     lock of its own key. The intention is the claim on an insertion *position* and is what a
//     covering Gap blocks; the record lock is the row claim and, per FR-005, it is what decides
//     same-key contention. Requiring both in one bundle is what makes "an insertion claim" a single
//     indivisible request: a bare intention could otherwise be granted past a row another owner
//     already holds, and a bare record lock could walk through another owner's gap.
//
// A record component with no table identity at all has no table to intend and is
// therefore exempt: an unbound raw namespace reaches this package before the adapter
// publishes a binding. MissingTableIntents reports that case as "no requirement", so
// the exemption is visible rather than inferred.
func ValidateBundle(components []ResourceRef, bindings *Bindings) error {
	if len(components) == 0 {
		return ErrInvalidResource
	}
	for _, component := range components {
		if err := ValidateComponent(component); err != nil {
			return err
		}
	}
	if len(MissingTableIntents(components, bindings)) > 0 {
		return ErrTableIntentRequired
	}
	if len(MissingRowClaims(components)) > 0 {
		return ErrInsertIntentionRequiresRecord
	}
	return nil
}

// MissingRowClaims reports the insertion keys whose record lock is absent from the bundle.
//
// It is the second bundle-level prerequisite: every Insert Intention must be accompanied by the
// exclusive record lock of the same key in the same namespace. The pairing is what keeps the two
// answers of an insertion in one atomic request — the intention answers "may I insert here at all"
// and the record lock answers "is this key already claimed" — so a caller cannot request only the
// half it finds convenient. The returned keys are the insertion keys, in bundle order, so a caller
// can name what is missing.
func MissingRowClaims(components []ResourceRef) [][]byte {
	claimed := make(map[string]bool, len(components))
	for _, component := range components {
		if component.Kind != KindRecord || component.Mode != ModeX {
			continue
		}
		claimed[component.Space+"\x00"+string(component.Key)] = true
	}
	var missing [][]byte
	seen := make(map[string]bool, len(components))
	for _, component := range components {
		if component.Kind != KindRecord || component.Mode != ModeInsertIntention {
			continue
		}
		match := component.Space + "\x00" + string(component.Key)
		if claimed[match] || seen[match] {
			continue
		}
		seen[match] = true
		missing = append(missing, component.Key)
	}
	return missing
}

// MissingTableIntents reports the table IDs whose required intention is absent from
// the bundle.
//
// The rule is the contract's: record S needs IS or IX, and record X or an insert
// intention needs IX. The contract states it explicitly -- "IX also satisfies this root's
// IS" -- so a bundle that carries an IX covers both requirements and a shared request
// never needs a second, weaker table intention it has already declared. The table
// identity of a record component is its own TableID label, falling back to the namespace
// binding when the label is empty, so a SQL write that only knows the namespace still
// meets the same table.
//
// A component with neither a label nor a binding is skipped, because there is no table
// to intend. Interval components are skipped for the same reason at the US1 stage: the
// interval rule (which insertion points a gap blocks) belongs to US2.
func MissingTableIntents(components []ResourceRef, bindings *Bindings) []string {
	pinned := make(map[string]bool, len(components))
	for _, component := range components {
		if component.Kind != KindTableIntent {
			continue
		}
		pinned[component.TableID+"\x00"+string(component.Mode)] = true
		if component.Mode == ModeIX {
			// IX also satisfies this root's need for IS.
			pinned[component.TableID+"\x00"+string(ModeIS)] = true
		}
	}
	checked := make(map[string]bool, len(components))
	var missing []string
	for _, component := range components {
		// A labelled interval component needs the same table intention a record lock needs. A range
		// lock protects keys of one table's index, and every real next-key bundle carries that
		// intention with it; an interval that names a table but does not declare the intention is a
		// malformed bundle, not a lock without a table. An *unlabelled* interval stays exempt for the
		// same reason an unlabelled record does: a raw namespace has no table to intend.
		if component.Kind != KindRecord && component.Kind != KindInterval {
			continue
		}
		tableID := component.TableID
		if tableID == "" && bindings != nil {
			if binding, ok := bindings.Lookup(component.Space); ok {
				tableID = binding.TableID
			}
		}
		if tableID == "" {
			// An unbound raw namespace has no table to intend.
			continue
		}
		need := ModeIS
		switch component.Kind {
		case KindInterval:
			// A gap locks insert positions, which is a write intention on the table: IX, exactly as
			// an insertion claim needs.
			need = ModeIX
		default:
			if component.Mode == ModeX || component.Mode == ModeInsertIntention {
				need = ModeIX
			}
		}
		key := tableID + "\x00" + string(need)
		if checked[key] {
			continue
		}
		checked[key] = true
		if !pinned[key] {
			missing = append(missing, tableID)
		}
	}
	return missing
}

// recordsConflict reports whether a request mode conflicts with a held mode on the same resource,
// for a different owner, using the Record/table matrix.
//
// It is modesCompatible with the arguments in the direction the contract's table is written
// (request against held), so a call site never has to remember the direction. It must not be used
// for an Interval component: the interval rule depends on the actual bounds and is decided by
// componentsConflict below.
func recordsConflict(request, held Mode) bool { return !modesCompatible(request, held) }

// componentsConflict reports whether a request conflicts with something another owner holds on the
// same resource.
//
// The two interval modes need the actual bounds, so this is not a mode matrix:
//
//   - A Gap protects the insert positions inside it. It blocks an Insert Intention only when the
//     intention insertion point is contained in the gap. Two transactions inserting at different
//     positions of one gap therefore proceed, which a mode-only rule could not express.
//   - An Insert Intention is the claim on one exact key. It blocks a Gap only when the gap would
//     have to contain that key.
//   - Gap against Gap is compatible: two transactions may both intend "no insert here".
//   - Everything else, including the Record S/X matrix and the record-versus-gap rule, is decided
//     by recordsConflict, because the contract keeps the interval rule independent of the record
//     rule rather than deriving one from the other.
//
// The bounds come from requestRef and heldRef. A caller that cannot supply the bounds of an
// interval component gets the conservative answer from gapBlocksInsertion, never a permissive one.
func componentsConflict(requestRef, heldRef ResourceRef) bool {
	request, held := requestRef.Mode, heldRef.Mode
	switch {
	case request == ModeGap && held == ModeInsertIntention:
		return gapBlocksInsertion(requestRef, heldRef.Key)
	case request == ModeInsertIntention && held == ModeGap:
		return gapBlocksInsertion(heldRef, requestRef.Key)
	case request == ModeGap && held == ModeGap:
		return false
	}
	return recordsConflict(request, held)
}

// effectiveMode reduces a set of modes to the strongest one.
//
// A resource held by any X is exclusively held; the projection is used to report a
// resource's state and to detect whether an upgrade is still needed.
func effectiveMode(modes ...Mode) Mode {
	strongest := Mode("")
	for _, mode := range modes {
		if modeStrength(mode) > modeStrength(strongest) {
			strongest = mode
		}
	}
	return strongest
}

// modeStrength orders modes by how much they exclude, for a resource's reported state
// and for upgrade detection. It is deliberately NOT the compatibility matrix: two modes
// of different strength can still be compatible (IS and IX), so strength must never be
// used to decide a conflict.
//
// An unknown mode has no strength. Callers that must distinguish "no strength" from "the
// weakest known mode" use modeStrengthKnown, because reporting the same zero for an
// unknown mode and for the empty string would let an unrecognized mode slip through an
// upgrade check.
func modeStrength(mode Mode) int {
	strength, _ := modeStrengthKnown(mode)
	return strength
}

// modeStrengthKnown reports a mode's strength and whether it is a known mode.
func modeStrengthKnown(mode Mode) (int, bool) {
	switch mode {
	case ModeIS:
		return 1, true
	case ModeGap:
		return 2, true
	case ModeInsertIntention:
		return 3, true
	case ModeS:
		return 4, true
	case ModeIX:
		return 5, true
	case ModeX:
		return 6, true
	default:
		return 0, false
	}
}

// upgradeNeeded reports whether already holding held still requires a conversion for a
// request of request.
//
// An upgrade is a request whose strength exceeds what the owner holds on that resource
// (the S to X case). Holding the same or a stronger mode is a no-op re-entrant
// acquisition, which is what makes a repeated lock free instead of a self-conflict.
//
// An unknown mode on either side reports "no upgrade": the request is refused earlier by
// component validation, so answering conservatively here cannot admit anything.
func upgradeNeeded(request, held Mode) bool {
	if held == "" {
		return false
	}
	requestStrength, requestKnown := modeStrengthKnown(request)
	heldStrength, heldKnown := modeStrengthKnown(held)
	if !requestKnown || !heldKnown {
		return false
	}
	return requestStrength > heldStrength
}
