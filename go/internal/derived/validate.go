package derived

import (
	"errors"
	"fmt"
)

// Target describes the block that is about to be written: the level its writer
// claims, the untrusted flag of its TYPE, and the sensitivity the write call
// will demand.
type Target struct {
	// Stratum is the level the writer claims for this block (I2). It is a
	// parameter and not derived from the type name — see the package doc on
	// why level 2 has no type name of its own.
	Stratum Stratum

	// Untrusted is retrieval.untrusted of the TARGET type (set.go:206 resolves
	// it from the type name). It is a type property, not a block property,
	// which is exactly why V11 exists.
	Untrusted bool

	// Required is call.Required — the sensitivity the write path will demand
	// on egress (§4.8.1a). V13 pins it against the floored source maximum.
	Required string
}

// SourceFacts are the per-source facts Validate needs and that only the
// DB-facing half of the contract can produce (store.ResolveSources, §4.5.4,
// wave W01-5). They are handed in rather than fetched, because derived is a
// leaf package that must not import store.
//
// EVERY field is unexported, and that is the mechanism rather than a style
// choice — it is the structural half of W01-1 review note N4.
//
// N4 says: MissingInScope and ForeignOrUnknown must not be caller CLAIMS,
// because checkFactsCoverDeclared accepts "reported unresolvable" as the legal
// way to carry no facts. A caller who declares every source missing therefore
// switches V6, V11 and the monotonicity half of V13 off, while FlooredMax stays
// correct and the first V13 clause still passes. W01-5's first version answered
// that with a text pin over `SourceFacts{` and `.FlooredMax =` — and its review
// walked straight past it: a production file that MUTATES a fact set
// (`f.MissingInScope = append(…)`, `delete(f.Strata, id)`) needs neither of
// those spellings, and both pins stayed green.
//
// So the fields are closed instead of watched. Outside this package the only
// way to obtain a populated SourceFacts is NewSourceFacts, which copies every
// map and slice it is given; the accessors hand out copies too, so a consumer
// cannot reach back into the resolve result. Inside this package the fields
// stay directly addressable — the V-clause tests build and mutate them on
// purpose, and they are the code that is under test.
//
// One producer remains the rule (store.ResolveSources), and it is pinned over
// the syntax tree at store/resolve_sources_pin_test.go. NewSourceFacts is what
// that pin now watches: one call site instead of five field spellings.
type SourceFacts struct {
	// strata is the resolved level per source id (V6).
	strata map[string]Stratum

	// untrusted is retrieval.untrusted of each source's TYPE (V11). It falls
	// out of the registry snapshot ResolveSources already reads, without a
	// second query.
	untrusted map[string]bool

	// sensitivity is the raw per-source sensitivity (V13's monotonicity half).
	sensitivity map[string]string

	// flooredMax is config.ScopeFloor.Apply(max(sensitivity(sources)), scope)
	// — computed by the caller, because ScopeFloor lives in internal/config
	// and derived may not import it. Validate re-checks that this value really
	// is at or above the raw maximum, so the floor's raise-only property is
	// verified here even though it is applied elsewhere.
	flooredMax string

	// foreignOrUnknown are declared sources that resolve in a FOREIGN scope or
	// nowhere at all. V5, fail-closed: the write dies (B7).
	foreignOrUnknown []string

	// missingInScope are declared sources that are provably archived or gone
	// in the OWN scope. Explicitly NOT a V5 case (§4.5.4): the arm drops them,
	// sources_covered falls, and the run continues. Kept in the struct so the
	// distinction is visible at the type and cannot collapse back into one
	// "missing" set — that collapse is what turned the scope check B7 into a
	// silent swallow in the first draft.
	missingInScope []string
}

// NewSourceFacts is the ONE way to build a populated SourceFacts from outside
// this package. Every map and slice is COPIED, so the resolve result the caller
// keeps and the facts the validator sees cannot drift apart afterwards — not by
// a later append, not by a delete on a shared map.
//
// It takes no responsibility for the CONTENT being a real resolution: that is
// what the one-producer pin in store is for. What it guarantees is that the
// content cannot change after the fact.
func NewSourceFacts(
	strata map[string]Stratum,
	untrusted map[string]bool,
	sensitivity map[string]string,
	flooredMax string,
	missingInScope, foreignOrUnknown []string,
) SourceFacts {
	f := SourceFacts{
		strata:           make(map[string]Stratum, len(strata)),
		untrusted:        make(map[string]bool, len(untrusted)),
		sensitivity:      make(map[string]string, len(sensitivity)),
		flooredMax:       flooredMax,
		missingInScope:   append([]string(nil), missingInScope...),
		foreignOrUnknown: append([]string(nil), foreignOrUnknown...),
	}
	for id, s := range strata {
		f.strata[id] = s
	}
	for id, u := range untrusted {
		f.untrusted[id] = u
	}
	for id, s := range sensitivity {
		f.sensitivity[id] = s
	}
	return f
}

// FlooredMax is the floored source maximum (§4.8.1a) — one value, three uses
// (the written column, provenance.sensitivity_max, ChainCall.Required).
func (f SourceFacts) FlooredMax() string { return f.flooredMax }

// MissingInScope returns a copy of the droppable set (§4.7.5 case 1).
func (f SourceFacts) MissingInScope() []string { return append([]string(nil), f.missingInScope...) }

// ForeignOrUnknown returns a copy of the fail-closed set (V5, B7).
func (f SourceFacts) ForeignOrUnknown() []string {
	return append([]string(nil), f.foreignOrUnknown...)
}

// StratumOf reports the resolved level of one source (V6's input).
func (f SourceFacts) StratumOf(id string) (Stratum, bool) {
	s, ok := f.strata[id]
	return s, ok
}

// IsUntrusted reports retrieval.untrusted of one source's TYPE (V11's input).
func (f SourceFacts) IsUntrusted(id string) (bool, bool) {
	u, ok := f.untrusted[id]
	return u, ok
}

// SensitivityOf reports the raw sensitivity of one source (V13's input).
func (f SourceFacts) SensitivityOf(id string) (string, bool) {
	s, ok := f.sensitivity[id]
	return s, ok
}

// Len is the number of sources that carry facts.
func (f SourceFacts) Len() int { return len(f.sensitivity) }

// ViolationError names the violated clause. Typed so a caller (and a test) can
// assert WHICH check fired instead of matching on message text.
type ViolationError struct {
	Clause string // "V1" … "V14"
	Detail string
}

func (e *ViolationError) Error() string {
	return "derived: " + e.Clause + " violated: " + e.Detail
}

// Violation returns the clause name of a ViolationError, or "" for any other
// error.
func Violation(err error) string {
	var v *ViolationError
	if errors.As(err, &v) {
		return v.Clause
	}
	return ""
}

func fail(clause, format string, args ...any) error {
	return &ViolationError{Clause: clause, Detail: fmt.Sprintf(format, args...)}
}

// Validate runs V1–V14 (§4.5.3) and returns the FIRST violated clause.
//
// All fourteen are fail-closed: a violation prevents the write, it does not
// degrade it. There is no partial write, no "best effort" provenance and no
// clause that only logs.
//
// kept are the claims that survived CiteGate; V14 checks them against the
// declared source list at block level, the same statement G0 makes per line.
func Validate(p Provenance, kept []Claim, t Target, src SourceFacts) error {
	checks := []func() error{
		func() error { return checkContract(p) },       // V1–V4
		func() error { return checkSources(p, src) },   // V5, V12
		func() error { return checkStrata(p, t, src) }, // V6
		func() error { return checkAnchor(p.Anchor) },  // V7
		func() error { return checkGenerator(p) },      // V8
		func() error { return checkCoverage(p, kept) }, // V9
		func() error { return checkSensitivity(p, t, src) },
		func() error { return checkUntrusted(t, src) }, // V11
		func() error { return checkClaims(p, kept) },   // V14
	}
	for _, c := range checks {
		if err := c(); err != nil {
			return err
		}
	}
	return nil
}

// checkContract covers V1–V4: the version, a non-empty source list, the count
// and the digest.
func checkContract(p Provenance) error {
	if p.V != ContractVersion {
		return fail("V1", "provenance v=%d, want %d", p.V, ContractVersion)
	}
	if len(p.SourceBlockIDs) == 0 {
		return fail("V2", "no source_block_ids — a derivative without a source is not one")
	}
	if p.SourceCount != len(p.SourceBlockIDs) {
		return fail("V3", "source_count=%d, len(source_block_ids)=%d", p.SourceCount, len(p.SourceBlockIDs))
	}
	// The field's own contract in §3.2 is "ascending, deduplicated". Checked
	// here rather than assumed: a duplicate id would make source_count count
	// something other than the number of sources, and V4 alone cannot see it
	// because SourceDigest sorts before hashing.
	for i := 1; i < len(p.SourceBlockIDs); i++ {
		if p.SourceBlockIDs[i] <= p.SourceBlockIDs[i-1] {
			return fail("V3", "source_block_ids not strictly ascending at index %d", i)
		}
	}
	if want := SourceDigest(p.SourceBlockIDs); p.SourceDigest != want {
		return fail("V4", "source_digest does not match the id list")
	}
	return nil
}

// checkSources covers V5 (foreign or unresolvable sources kill the write, and
// the declared set must be fully accounted for) and V12 (the source-set floor).
func checkSources(p Provenance, src SourceFacts) error {
	if n := len(src.foreignOrUnknown); n > 0 {
		return fail("V5", "%d source(s) resolve in a foreign scope or nowhere", n)
	}
	if p.SourceCount < MinSourceCount {
		return fail("V12", "source_count=%d below MinSourceCount=%d", p.SourceCount, MinSourceCount)
	}
	return checkFactsCoverDeclared(p, src)
}

// checkFactsCoverDeclared closes the vacuum that V6, V11 and the monotonicity
// half of V13 would otherwise have: all three iterate the maps in SourceFacts,
// so a declared source that appears in NONE of them is touched by no clause at
// all, and the caller decides by omission which sources get validated.
//
// That is not an exotic case. Sources listed in MissingInScope legitimately
// carry no facts (§4.5.4), so "the facts do not cover the declaration" is
// normal operation and not the exception — which is exactly why the difference
// between "reported missing" and "silently absent" has to be stated rather than
// inferred. A declared id is therefore either accounted for as unresolvable
// (MissingInScope or ForeignOrUnknown) or it carries a stratum, an untrusted
// flag AND a sensitivity — one per clause that reads them. Anything else fails
// closed, in the V5 class: the write dies rather than validating a source set
// nobody looked at.
func checkFactsCoverDeclared(p Provenance, src SourceFacts) error {
	unresolvable := make(map[string]struct{}, len(src.missingInScope)+len(src.foreignOrUnknown))
	for _, id := range src.missingInScope {
		unresolvable[id] = struct{}{}
	}
	for _, id := range src.foreignOrUnknown {
		unresolvable[id] = struct{}{}
	}
	for _, id := range p.SourceBlockIDs {
		if _, gone := unresolvable[id]; gone {
			continue
		}
		if _, ok := src.strata[id]; !ok {
			return fail("V5", "declared source %s carries no stratum and is not reported unresolvable", id)
		}
		if _, ok := src.untrusted[id]; !ok {
			return fail("V5", "declared source %s carries no untrusted flag and is not reported unresolvable", id)
		}
		if _, ok := src.sensitivity[id]; !ok {
			return fail("V5", "declared source %s carries no sensitivity and is not reported unresolvable", id)
		}
	}
	return nil
}

// checkStrata is V6, the level rule of I2: every source is strictly below the
// block's own level. This is what makes the cascade provably acyclic and
// terminating — a level-2 regeneration may read level 1, never the other way
// round, and the depth is bounded by the highest level ever handed in.
//
// The first two clauses bind the level that is CHECKED to the level that is
// WRITTEN, and they are the reason this package can put the stratum in the
// block instead of in the type registry at all. Target.Stratum is a parameter;
// provenance.stratum is the value that lands in the database and that
// store.ResolveSources reads back as
// COALESCE(metadata->'provenance'->>'stratum','0')::int for the NEXT level's
// V6 run (§4.5.4). Without the binding, an arm that raises Target.Stratum to 2
// so its level-1 sources pass, while leaving provenance.stratum at 0 or 1 from
// a template, persists a derivative that every later run counts as an original.
// "Catalogue over catalogues" and "catalogue over itself" would be
// indistinguishable again — the exact difference I2 exists for (§1.3), and one
// that §1.3 says only shows up at target scale.
//
// The second clause states the other half: a block that goes through this
// contract IS a derivative, so its level is 1 or 2. Level 0 is what sources
// have.
func checkStrata(p Provenance, t Target, src SourceFacts) error {
	if p.Stratum != t.Stratum {
		return fail("V6", "provenance.stratum=%d but the write claims stratum %d", p.Stratum, t.Stratum)
	}
	if p.Stratum < StratumDerived || p.Stratum > StratumSuper {
		return fail("V6", "provenance.stratum=%d is not a derived level (%d or %d)",
			p.Stratum, StratumDerived, StratumSuper)
	}
	for id, s := range src.strata {
		if s >= t.Stratum {
			return fail("V6", "source %s has stratum %d, target stratum is %d", id, s, t.Stratum)
		}
	}
	return nil
}

// checkAnchor is V7: exactly one anchor form, kind and fields in agreement.
//
// core_hash is required for cluster_topic (§4.7.3, the drift anchor of the
// catalogue arm) and merely permitted for the other kinds — it is a drift
// signal, not a form discriminator, so its presence elsewhere does not make
// the form ambiguous.
func checkAnchor(a Anchor) error {
	switch a.Kind {
	case AnchorClusterTopic:
		if a.TopicID == "" || a.CoreHash == "" {
			return fail("V7", "anchor kind=cluster_topic needs topic_id and core_hash")
		}
		if a.BlockID != "" || a.RootSessionID != "" || a.ManifestID != "" || a.WatermarkFrom != nil {
			return fail("V7", "anchor kind=cluster_topic carries fields of another form")
		}
	case AnchorRootSession:
		if a.RootSessionID == "" || a.WatermarkFrom == nil {
			return fail("V7", "anchor kind=root_session needs root_session_id and watermark_from")
		}
		if a.TopicID != "" || a.BlockID != "" {
			return fail("V7", "anchor kind=root_session carries fields of another form")
		}
	case AnchorBlock:
		if a.BlockID == "" {
			return fail("V7", "anchor kind=block needs block_id")
		}
		if a.TopicID != "" || a.RootSessionID != "" || a.ManifestID != "" || a.WatermarkFrom != nil {
			return fail("V7", "anchor kind=block carries fields of another form")
		}
	default:
		return fail("V7", "anchor kind %q is not one of cluster_topic|root_session|block", a.Kind)
	}
	return nil
}

// checkGenerator is V8: I1 (regenerable) is only a verifiable statement if the
// block records which model and which prompt version produced it.
func checkGenerator(p Provenance) error {
	if p.Generator.Model == "" || p.Generator.PromptVersion == "" {
		return fail("V8", "generator.model and generator.prompt_version are both required")
	}
	return nil
}

// checkCoverage is V9: the block-level yield floor. A block below it is not
// written; the run counts it as a failure.
//
// The order is deliberate — the reported numbers are BOUND to the gated claims
// first, and only then judged. V9 is the one gate that decides whether the
// block gets written at all (§4.4.1), so a V9 that computes on
// p.Coverage alone judges the arm's self-report and not the gate's outcome:
// an arm could report claims_kept=24 over three surviving claims and pass.
// §4.4.2 rule 2 makes the binding well-defined — CiteGate runs exactly once
// over ALL claims of the block, so len(kept) IS claims_kept, and the distinct
// sources among them ARE sources_covered.
//
// sources_covered is bound for a second reason: render.go writes
// "N Quellen ohne belegbare Aussage" into the head from it. An unchecked value
// there produces exactly the false completeness I6 exists to prevent, in the
// one line of the block that survives every truncation.
//
// coverage.rejects must carry the eight gate buckets, zeros included. The
// package makes that promise to its own consumers at citegate.go ("a missing
// key and a zero must not be distinguishable") and §3.2 defines the field with
// eight keys; without a clause here a nil map serialises as JSON null and the
// promise holds only for the Verdict, never for the Provenance that is
// actually stored.
func checkCoverage(p Provenance, kept []Claim) error {
	c := p.Coverage
	if err := checkRejects(c.Rejects); err != nil {
		return err
	}
	if c.ClaimsKept != len(kept) {
		return fail("V9", "claims_kept=%d but the gate kept %d claims", c.ClaimsKept, len(kept))
	}
	if got := distinctSources(kept); c.SourcesCovered != got {
		return fail("V9", "sources_covered=%d but the kept claims cover %d sources", c.SourcesCovered, got)
	}
	if c.ClaimsKept < MinClaimsKept {
		return fail("V9", "claims_kept=%d below MinClaimsKept=%d", c.ClaimsKept, MinClaimsKept)
	}
	if c.ClaimsKept > c.ClaimsOffered {
		return fail("V9", "claims_kept=%d exceeds claims_offered=%d", c.ClaimsKept, c.ClaimsOffered)
	}
	if float64(c.ClaimsKept)/float64(c.ClaimsOffered) < MinKeepRatio {
		return fail("V9", "keep ratio %d/%d below MinKeepRatio=%.2f", c.ClaimsKept, c.ClaimsOffered, MinKeepRatio)
	}
	return nil
}

// checkRejects enforces the eight-bucket shape of coverage.rejects.
func checkRejects(r map[string]int) error {
	if r == nil {
		return fail("V9", "coverage.rejects is absent; §3.2 requires the eight gate buckets")
	}
	if len(r) != len(GateKeys) {
		return fail("V9", "coverage.rejects carries %d buckets, want the %d gate keys", len(r), len(GateKeys))
	}
	for _, k := range GateKeys {
		if _, ok := r[k]; !ok {
			return fail("V9", "coverage.rejects is missing bucket %q", k)
		}
	}
	return nil
}

// checkSensitivity covers V10 and V13.
//
// V13 is the EGRESS half of the sensitivity fold (§4.8.1a). The doc writes it
// as call.Required != ScopeFloor.Apply(max(sensitivity(sources)), scope).
// ScopeFloor lives in internal/config, which this leaf package may not import,
// so the applied value arrives as SourceFacts.FlooredMax and the clause splits
// into three statements that together say the same thing:
//
//	(a) the value the write call will demand equals the floored maximum,
//	(b) the value STORED in the block equals it too — §3.2 defines
//	    sensitivity_max as exactly the floored maximum, so a block whose
//	    metadata disagrees with its own write call is not honest about I5,
//	(c) the floored maximum is at or above the raw source maximum, which is
//	    the raise-only property of the floor, re-checked where it is used.
//
// WHAT THIS DOES NOT DO, stated as an obligation on the DB-facing wave (W01-5)
// rather than as a closed clause: none of the three verifies that
// ScopeFloor.Apply was applied AT ALL. Because the floor only ever raises, a
// caller that skips it and sets FlooredMax = max(raw sensitivity) satisfies
// (a), (b) and (c). The egress promise of §4.8.1a is therefore carried by the
// caller in this wave, not by this clause — W01-5 owns the single place where
// FlooredMax is produced and must be the only producer of it.
//
// The doc's "empty source set ⇒ credentials" branch is unreachable behind V2
// and is not re-encoded here.
func checkSensitivity(p Provenance, t Target, src SourceFacts) error {
	if _, ok := sensitivityRank[p.SensitivityMax]; !ok {
		return fail("V10", "sensitivity_max %q is not one of credentials|personal|internal|public", p.SensitivityMax)
	}
	if t.Required != src.flooredMax {
		return fail("V13", "call.Required=%q but floored source maximum is %q", t.Required, src.flooredMax)
	}
	if p.SensitivityMax != src.flooredMax {
		return fail("V13", "sensitivity_max=%q but floored source maximum is %q", p.SensitivityMax, src.flooredMax)
	}
	floored, ok := sensitivityRank[src.flooredMax]
	if !ok {
		return fail("V13", "floored source maximum %q is not a sensitivity level", src.flooredMax)
	}
	for id, s := range src.sensitivity {
		r, ok := sensitivityRank[s]
		if !ok {
			return fail("V13", "source %s carries sensitivity %q", id, s)
		}
		if r > floored {
			return fail("V13", "source %s is %q, above the floored maximum %q", id, s, src.flooredMax)
		}
	}
	return nil
}

// checkUntrusted is V11, the untrusted inheritance (§4.8.3).
//
// untrusted is a TYPE field (retrieval.untrusted), not a block field, so "this
// one block is untrusted because one of its 26 sources was" is not expressible
// in the current schema. The model's answer is exclusion rather than framing:
// a source that carries untrusted may only feed a target type that carries it
// too. For insight the target type is itself untrusted (E-12), so the clause
// does not bite there; the registry values themselves are not part of this
// wave.
func checkUntrusted(t Target, src SourceFacts) error {
	if t.Untrusted {
		return nil
	}
	for id, u := range src.untrusted {
		if u {
			return fail("V11", "source %s is untrusted but the target type is not", id)
		}
	}
	return nil
}

// checkClaims is V14: the block-level statement of G0. Every surviving claim
// names a source the provenance declares, so every line in the rendered block
// is traceable through the metadata a reader can actually fetch.
func checkClaims(p Provenance, kept []Claim) error {
	declared := make(map[string]struct{}, len(p.SourceBlockIDs))
	for _, id := range p.SourceBlockIDs {
		declared[id] = struct{}{}
	}
	for _, c := range kept {
		if _, ok := declared[c.SourceID]; !ok {
			return fail("V14", "a kept claim cites a source that provenance does not declare")
		}
	}
	return nil
}
