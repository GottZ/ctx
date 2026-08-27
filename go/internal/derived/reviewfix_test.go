package derived

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The clauses in this file close the findings of the adversarial review of
// wave W01-1. Each one binds a value that Validate CHECKS to the value the
// block WRITES — the class of hole the review found three times: the module
// verified what the caller handed it, not what would land in the database.

// TestReviewFix1_ProvenanceStratumIsBoundToTheCheckedLevel is the blocker:
// `provenance.stratum` was serialised but never read, so I2 hung on a
// parameter instead of on the block.
//
// The bruise path is concrete. store.ResolveSources reads the level back as
// COALESCE(metadata->'provenance'->>'stratum','0')::int (§4.5.4). An arm that
// raises the CHECKED level to 2 so its level-1 sources pass V6, while leaving
// the WRITTEN level at 0 from a template, persists a derivative that every
// later run counts as an original — and "catalogue over catalogues" is
// indistinguishable from "catalogue over itself" again, which is the one
// difference I2 exists for.
//
// Red probe: neutralise the p.Stratum != t.Stratum clause in checkStrata.
func TestReviewFix1_ProvenanceStratumIsBoundToTheCheckedLevel(t *testing.T) {
	t.Run("written level below the checked level", func(t *testing.T) {
		facts := validFacts(26)
		facts.strata[srcID(7)] = StratumDerived // needs a level-2 target to pass V6
		tgt := validTarget()
		tgt.Stratum = StratumSuper

		p := validProvenance(26)
		p.Stratum = StratumSource // what would land in the database
		wantClause(t, p, keptClaims(3), tgt, facts, "V6")
	})

	t.Run("written level above the checked level", func(t *testing.T) {
		p := validProvenance(26)
		p.Stratum = StratumSuper
		wantClause(t, p, keptClaims(3), validTarget(), validFacts(26), "V6")
	})

	t.Run("a derivative is never level 0, even when both agree", func(t *testing.T) {
		p := validProvenance(26)
		p.Stratum = StratumSource
		tgt := validTarget()
		tgt.Stratum = StratumSource
		wantClause(t, p, keptClaims(3), tgt, validFacts(26), "V6")
	})

	t.Run("level 2 validates when the block says so", func(t *testing.T) {
		facts := validFacts(26)
		facts.strata[srcID(7)] = StratumDerived
		tgt := validTarget()
		tgt.Stratum = StratumSuper
		p := validProvenance(26)
		p.Stratum = StratumSuper
		if err := Validate(p, keptClaims(3), tgt, facts); err != nil {
			t.Fatalf("a level-2 block over level-1 sources must validate, got %v", err)
		}
	})
}

// TestReviewFix2_FactsMustAccountForEveryDeclaredSource is major finding #2:
// V6, V11 and the monotonicity half of V13 all iterate the caller's maps, so a
// declared source that appears in none of them is touched by no clause — the
// caller decided by omission which sources were validated at all.
//
// Red probe: make checkFactsCoverDeclared return nil.
func TestReviewFix2_FactsMustAccountForEveryDeclaredSource(t *testing.T) {
	p := validProvenance(26)

	t.Run("empty facts against 26 declared sources", func(t *testing.T) {
		empty := SourceFacts{
			strata:      map[string]Stratum{},
			untrusted:   map[string]bool{},
			sensitivity: map[string]string{},
			flooredMax:  SensitivityInternal,
		}
		wantClause(t, p, keptClaims(3), validTarget(), empty, "V5")
	})

	// Absent from ONE map is still absent for the clause that reads it: a
	// source without an untrusted flag is invisible to V11 even when V6 sees
	// it. One silently missing source is the dangerous shape, not twenty-six.
	for _, c := range []struct {
		name string
		drop func(f *SourceFacts, id string)
	}{
		{"no stratum (V6 blind)", func(f *SourceFacts, id string) { delete(f.strata, id) }},
		{"no untrusted flag (V11 blind)", func(f *SourceFacts, id string) { delete(f.untrusted, id) }},
		{"no sensitivity (V13 blind)", func(f *SourceFacts, id string) { delete(f.sensitivity, id) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := validFacts(26)
			c.drop(&f, srcID(7))
			wantClause(t, p, keptClaims(3), validTarget(), f, "V5")
		})
	}

	t.Run("reported unresolvable is the legal way to carry no facts", func(t *testing.T) {
		f := validFacts(26)
		delete(f.strata, srcID(7))
		delete(f.untrusted, srcID(7))
		delete(f.sensitivity, srcID(7))
		f.missingInScope = []string{srcID(7)}
		if err := Validate(p, keptClaims(3), validTarget(), f); err != nil {
			t.Fatalf("a source reported as MissingInScope must not need facts, got %v", err)
		}
	})
}

// TestReviewFix3_CoverageIsBoundToTheGatedClaims is major finding #3: V9 is
// the one gate that decides whether the block is written at all (§4.4.1), and
// it computed on the arm's self-report while `kept` sat in the same call.
// §4.4.2 rule 2 makes the binding well-defined — CiteGate runs exactly once
// over ALL claims of the block.
//
// Red probe: neutralise the two binding clauses in checkCoverage.
func TestReviewFix3_CoverageIsBoundToTheGatedClaims(t *testing.T) {
	t.Run("claims_kept disagrees with the gate", func(t *testing.T) {
		p := validProvenance(26)
		// 24/31 clears MinClaimsKept and MinKeepRatio, so the unbound V9
		// accepted this block although the gate kept three claims.
		p.Coverage.ClaimsKept = 24
		p.Coverage.ClaimsOffered = 31
		p.Coverage.SourcesCovered = 21
		wantClause(t, p, keptClaims(3), validTarget(), validFacts(26), "V9")
	})

	t.Run("sources_covered disagrees with the gate", func(t *testing.T) {
		// Alone this is enough to matter: RenderBlock writes the coverage gap
		// into the head from it, so an unchecked value produces exactly the
		// false completeness I6 exists to prevent — in the one line that
		// truncation never reaches.
		p := validProvenance(26)
		p.Coverage.SourcesCovered = 21
		wantClause(t, p, keptClaims(3), validTarget(), validFacts(26), "V9")
	})

	t.Run("distinct sources, not claims, is the measure", func(t *testing.T) {
		kept := keptClaims(3)
		kept[2].SourceID = kept[0].SourceID // three claims, two distinct sources
		p := validProvenance(26)
		p.Coverage.SourcesCovered = 2
		if err := Validate(p, kept, validTarget(), validFacts(26)); err != nil {
			t.Fatalf("two claims over one source cover one source: %v", err)
		}
		p.Coverage.SourcesCovered = 3
		wantClause(t, p, kept, validTarget(), validFacts(26), "V9")
	})
}

// TestReviewFix4_RejectsMustCarryTheEightBuckets is minor finding #4: the
// package promises its consumers that "a missing key and a zero must not be
// distinguishable" and §3.2 defines rejects with eight keys — but a nil map
// validated and serialised as JSON null, so the promise held for the Verdict
// and never for the Provenance that is actually stored.
//
// Red probe: make checkRejects return nil.
func TestReviewFix4_RejectsMustCarryTheEightBuckets(t *testing.T) {
	cases := map[string]map[string]int{
		"absent":     nil,
		"one bucket": {"g0": 1},
		"eight buckets, but g6 named g8": {
			"g0": 0, "g1": 0, "g2": 0, "g3": 0, "g4": 0, "g5": 0, "g7": 0, "g8": 0,
		},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			p := validProvenance(26)
			p.Coverage.Rejects = r
			wantClause(t, p, keptClaims(3), validTarget(), validFacts(26), "V9")
		})
	}

	t.Run("a validated block serialises eight buckets", func(t *testing.T) {
		b, err := json.Marshal(validProvenance(26))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var wire struct {
			Coverage struct {
				Rejects map[string]int `json:"rejects"`
			} `json:"coverage"`
		}
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if len(wire.Coverage.Rejects) != len(GateKeys) {
			t.Errorf("coverage.rejects on the wire has %d buckets, want %d",
				len(wire.Coverage.Rejects), len(GateKeys))
		}
	})
}

// TestReviewFix5_SubjectIsCappedInsideTheHeadBudget is minor finding #5:
// MaxHeadRunes was a test assertion and nothing in the code held it. The head
// budget has exactly two unbounded inputs — the display subject and the length
// of a single claim. The subject is the one that can be bounded without losing
// substance (it is display only; the identity lives in the TITLE column,
// §4.7.1), and the review measured the break at roughly 510 subject runes.
// Claim length stays uncapped on purpose and is a W01-M3 measurement.
//
// Red probe: drop the clampRunes call in headLine.
func TestReviewFix5_SubjectIsCappedInsideTheHeadBudget(t *testing.T) {
	h, kept, p := realisticBlock()
	h.Subject = padRunes("Ein Cluster-Label, das jedes gemessene Maß sprengt", 900)

	out := RenderBlock(h, kept, p)
	if got := HeadRunes(out); got > MaxHeadRunes {
		t.Errorf("head with a 900-rune subject is %d runes, budget is %d", got, MaxHeadRunes)
	}

	line1 := strings.SplitN(out, "\n", 2)[0]
	if n := utf8.RuneCountInString(line1); n > MaxSubjectRunes+utf8.RuneCountInString("Katalog — ") {
		t.Errorf("line 1 is %d runes, the subject cap is %d", n, MaxSubjectRunes)
	}
	if !strings.HasSuffix(line1, "…") {
		t.Error("the cut must be marked with an ellipsis, not silent")
	}

	t.Run("a subject inside the cap survives verbatim", func(t *testing.T) {
		h.Subject = "Retrieval-Infrastruktur und Modellarchitektur"
		if !strings.Contains(RenderBlock(h, kept, p),
			"Katalog — Retrieval-Infrastruktur und Modellarchitektur\n") {
			t.Error("a subject below MaxSubjectRunes must not be touched")
		}
	})
}
