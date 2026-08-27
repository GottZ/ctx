// Package derived is THE gate module for derived knowledge blocks (design
// D-01 §4.5, masterplan K5). It owns four things and nothing else:
//
//  1. the provenance contract every derived writer must fill (§3.2) and its
//     fourteen fail-closed checks V1–V14 (§4.5.3),
//  2. the citation gate G0–G7 that decides, per claim line, whether the line
//     is a verified quote of an ORIGINAL source (§4.4.1),
//  3. the deterministic renderer that turns surviving claims into block
//     content whose head carries freshness, coverage limit and framing before
//     any truncation can reach it (§4.6.2),
//  4. the reserved-metadata list a derived writer never passes through (§3.2).
//
// # Leaf package
//
// derived imports only promptguard and sensitivity (both of which import only
// util). It must NOT import store, llm, blocktype or topiclabel: the DB-facing
// half of the contract (store.ResolveSources) lives in store and calls INTO
// this package, exactly the cut internal/visibility uses. The echo index in
// echo.go is therefore an independent implementation of the one in
// topiclabel/guard.go rather than an import.
//
// # No callers
//
// This package has no production caller yet, by design (wave W01-1). Precedent
// in the tree: internal/hermesstate has lain callerless since migration 135.
//
// # StratumOf and the level-2 question
//
// I2 (§1.3) says a block of level n only ever has sources of level < n.
// The truth for a concrete block is provenance.stratum — a field IN the block.
// StratumOf is the writer-side property of a TYPE NAME: the level a writer of
// that type starts from. The two derived type names insight and catalog both
// map to 1; every other type name maps to 0 (originals).
//
// There is deliberately NO type name that maps to 2. A super-catalog (§1.3,
// the RAPTOR recursion over overview/super.go) is written with type_name
// "catalog" and carries provenance.stratum = 2; Validate takes the writer's
// own level as a PARAMETER and enforces max(srcStrata) < own against it.
//
// Two reasons, both structural. First, a third type name would need a third
// registry row, a third policy set and a third classify priority for a block
// that is identical to catalog in every retrieval-relevant property. Second —
// and this decides it — deriving the level from the type name would make the
// level a property of the type registry, which is a table an operator can edit
// with SQL; §4.5.1 rejects exactly that ("ein Registry-Feld wäre ein per SQL
// editierbarer Weg, I2 zu brechen"). Keeping the level in code (this function,
// as the floor) plus in the block (provenance.stratum, as the truth) also
// leaves the recursion open-ended: level 3 needs no new type name.
package derived

// ContractVersion is the value of provenance.v this build writes and the ONLY
// value it decodes. Unknown versions are refused, not best-effort parsed
// (§3.2: "Vertrags-Version; Decode lehnt Unbekanntes ab").
const ContractVersion = 1

// GateVersion is provenance.generator.gate_version — the version of the
// citation gate below. It changes when G0–G7 change, so a stored block records
// which gate admitted its lines.
const GateVersion = 1

// The four gate constants. Constants, NOT config keys (§4.4.1): a knob here
// switches the gate off without anything turning red. At MinQuoteRunes = 3,
// "ok" would be a valid quote of every second line and the gate would report
// 100 % coverage while checking nothing; at MinKeepRatio = 0.01 an operator
// has disabled the gate with a settings write.
const (
	// MinQuoteRunes is the shortest admissible quote, in runes (G2).
	MinQuoteRunes = 32

	// MinKeepRatio is the lowest admissible claims_kept/claims_offered (V9).
	// A policy, not a derivation: "at least a third of what the model offers
	// must be provable, otherwise it is confabulating". Calibrated against
	// real yields in W01-M3 before any arm goes live.
	MinKeepRatio = 0.34

	// MinClaimsKept is the absolute floor of surviving claims (V9). A block
	// with two lines is not a catalogue.
	MinClaimsKept = 3

	// MinSourceCount is the floor of the SOURCE set (V12, §4.7.5). A derivative
	// over two blocks is not a derivative, it is a duplicate.
	MinSourceCount = 3
)

// Stratum is the level of a block in the derivation order (I2, §1.3).
type Stratum int

// The three levels the model admits. The depth of the cascade is bounded by
// the highest level ever handed to Validate, which is what makes it provably
// acyclic and terminating.
const (
	// StratumSource is an original block: written by a human or ingested, not
	// derived from other blocks in this store.
	StratumSource Stratum = 0

	// StratumDerived is a first-order derivative — insight or catalog.
	StratumDerived Stratum = 1

	// StratumSuper is a second-order derivative: a catalogue over catalogues.
	StratumSuper Stratum = 2
)

// Type names of the derived layer (masterplan K3: insight, not
// session-insight — shorter, consistent with catalog, and it keeps the title
// and classify namespace clear of "session", which auditPatterns[0] claims).
const (
	TypeInsight = "insight"
	TypeCatalog = "catalog"
)

// StratumOf reports the level a writer of typeName starts from. It is the only
// type-name-to-level mapping in the tree; see the package doc for why level 2
// has no type name of its own.
func StratumOf(typeName string) Stratum {
	switch typeName {
	case TypeInsight, TypeCatalog:
		return StratumDerived
	default:
		return StratumSource
	}
}

// IsDerivedType reports whether typeName belongs to the derived layer. Sugar
// over StratumOf, kept because the callers that need it (write lock I7,
// registry asserts) read better with a boolean.
func IsDerivedType(typeName string) bool {
	return StratumOf(typeName) > StratumSource
}

// derivedTypeNames is the ENUMERATION of what StratumOf decides one name at a
// time. StratumOf answers "is this name derived?"; a coverage report has to ask
// the opposite question — "which names are derived?" — and a switch cannot be
// iterated. Kept next to StratumOf so the two are read and edited together, and
// pinned against it by TestDerivedTypeNames_Integration: every entry here must
// have a level > 0, and both type constants must appear.
//
// Order is deliberate and stable: insight before catalog, the order design/01
// §3.3/§3.4 and migration 143 introduce them in. The status surface renders in
// this order, so a reader's eye lands on the same row between polls.
var derivedTypeNames = []string{TypeInsight, TypeCatalog}

// DerivedTypeNames returns the type names of the derived layer — the population
// a coverage figure (§4.7.4) is computed over. It returns a COPY: the slice
// decides which rows /api/status shows, and a caller that sorts or truncates it
// in place would silently shrink the operating surface it was asked to report.
func DerivedTypeNames() []string {
	return append([]string(nil), derivedTypeNames...)
}
