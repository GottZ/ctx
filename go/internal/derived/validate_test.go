package derived

import (
	"testing"
	"time"
)

// keptClaims builds n claims over the first n declared sources.
func keptClaims(n int) []Claim {
	out := make([]Claim, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Claim{
			Claim:    "Aussage über Quelle.",
			Quote:    realQuote,
			SourceID: srcID(i),
			Kind:     KindFinding,
		})
	}
	return out
}

// wantClause runs Validate and asserts which clause fired.
func wantClause(t *testing.T, p Provenance, kept []Claim, tgt Target, src SourceFacts, clause string) {
	t.Helper()
	err := Validate(p, kept, tgt, src)
	if err == nil {
		t.Fatalf("Validate accepted the block, want %s to fire", clause)
	}
	if got := Violation(err); got != clause {
		t.Fatalf("Validate returned %s (%v), want %s", got, err, clause)
	}
}

// TestValidateAcceptsTheReferenceBlock is the baseline every clause test
// mutates away from: if this one is red, no clause test below means anything.
func TestValidateAcceptsTheReferenceBlock(t *testing.T) {
	if err := Validate(validProvenance(26), keptClaims(3), validTarget(), validFacts(26)); err != nil {
		t.Fatalf("the reference block must validate, got %v", err)
	}
}

// TestGate5_ValidateRefusesLevelOneOverLevelOne is gate 5 of §7 W01-1.
//
// I2 is what makes the cascade provably acyclic and terminating: a level-2
// regeneration may read level 1, never the other way round, and the depth is
// bounded by the highest level ever handed in. Without V6, "catalogue over
// catalogues" and "catalogue over itself" are not distinguishable, and the
// difference only shows up at target scale.
//
// Red probe: neutralise the comparison in checkStrata — the level-1-over-
// level-1 block validates and this test fails.
func TestGate5_ValidateRefusesLevelOneOverLevelOne(t *testing.T) {
	facts := validFacts(26)
	facts.strata[srcID(7)] = StratumDerived // a level-1 source under a level-1 target
	wantClause(t, validProvenance(26), keptClaims(3), validTarget(), facts, "V6")

	// Control: the same source set under a level-2 target is legal — the rule
	// is the ORDER, not a ban on derived sources. The block must SAY level 2,
	// not only be checked as level 2 (see TestReviewFix1).
	tgt := validTarget()
	tgt.Stratum = StratumSuper
	super := validProvenance(26)
	super.Stratum = StratumSuper
	if err := Validate(super, keptClaims(3), tgt, facts); err != nil {
		t.Fatalf("level 1 under level 2 must validate, got %v", err)
	}
}

// TestGate6_ValidateRefusesUntrustedSourceForTrustedTarget is gate 6 of
// §7 W01-1.
//
// untrusted is a TYPE field, not a block field, so "this one block is
// untrusted because one of its sources was" cannot be expressed in the current
// schema. The model's answer is exclusion, not framing: a source that carries
// untrusted may only feed a target type that carries it too.
//
// Red probe: make checkUntrusted return nil unconditionally — the untrusted
// source passes into a trusted target and this test fails.
func TestGate6_ValidateRefusesUntrustedSourceForTrustedTarget(t *testing.T) {
	facts := validFacts(26)
	facts.untrusted[srcID(4)] = true
	wantClause(t, validProvenance(26), keptClaims(3), validTarget(), facts, "V11")

	// Control: the same source under an untrusted target type (insight,
	// per E-12) validates — the clause is about inheritance, not about
	// untrusted sources as such.
	tgt := validTarget()
	tgt.Untrusted = true
	if err := Validate(validProvenance(26), keptClaims(3), tgt, facts); err != nil {
		t.Fatalf("untrusted source under an untrusted target must validate, got %v", err)
	}
}

// TestValidateClauses walks the remaining clauses. Each case mutates exactly
// one thing away from the reference block, so the clause it names is the only
// one that can fire.
func TestValidateClauses(t *testing.T) {
	cases := []struct {
		name   string
		clause string
		mutate func(p *Provenance, kept *[]Claim, tgt *Target, src *SourceFacts)
	}{
		{"V1 unknown contract version", "V1", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.V = 2
		}},
		{"V2 no sources", "V2", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.SourceBlockIDs = nil
		}},
		{"V3 count disagrees with list", "V3", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.SourceCount = 25
		}},
		{"V3 list not deduplicated", "V3", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.SourceBlockIDs[5] = p.SourceBlockIDs[4]
			p.SourceDigest = SourceDigest(p.SourceBlockIDs)
		}},
		{"V3 list not sorted", "V3", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.SourceBlockIDs[0], p.SourceBlockIDs[1] = p.SourceBlockIDs[1], p.SourceBlockIDs[0]
		}},
		{"V4 digest does not match", "V4", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.SourceDigest = "sha256:" + "00"
		}},
		{"V5 foreign source", "V5", func(_ *Provenance, _ *[]Claim, _ *Target, src *SourceFacts) {
			src.foreignOrUnknown = []string{srcID(99)}
		}},
		{"V7 anchor kind unknown", "V7", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.Anchor.Kind = "topic"
		}},
		{"V7 cluster_topic without core_hash", "V7", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.Anchor.CoreHash = ""
		}},
		{"V7 two forms at once", "V7", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.Anchor.BlockID = srcID(1)
		}},
		{"V7 root_session without watermark", "V7", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.Anchor = Anchor{Kind: AnchorRootSession, RootSessionID: "abc12345"}
		}},
		{"V8 generator without model", "V8", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.Generator.Model = ""
		}},
		{"V8 generator without prompt version", "V8", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.Generator.PromptVersion = ""
		}},
		{"V9 too few kept claims", "V9", func(p *Provenance, kept *[]Claim, _ *Target, _ *SourceFacts) {
			// The claim set moves too: the floor is judged on the BOUND
			// number, so a mismatch would fire the binding clause instead.
			*kept = keptClaims(MinClaimsKept - 1)
			p.Coverage.ClaimsKept = MinClaimsKept - 1
			p.Coverage.ClaimsOffered = MinClaimsKept - 1
			p.Coverage.SourcesCovered = MinClaimsKept - 1
		}},
		{"V9 keep ratio below the floor", "V9", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.Coverage.ClaimsOffered = 100
		}},
		{"V9 kept above offered", "V9", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.Coverage.ClaimsOffered = referenceClaims - 1
		}},
		{"V10 sensitivity_max not a level", "V10", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.SensitivityMax = "secret"
		}},
		{"V12 source set below the floor", "V12", func(p *Provenance, _ *[]Claim, _ *Target, src *SourceFacts) {
			*p = validProvenance(MinSourceCount - 1)
			p.Coverage = Coverage{ClaimsOffered: 31, ClaimsKept: 24, Rejects: newRejects(), SourcesCovered: 2}
			*src = validFacts(MinSourceCount - 1)
		}},
		{"V13 call demands less than the floored maximum", "V13", func(_ *Provenance, _ *[]Claim, tgt *Target, _ *SourceFacts) {
			tgt.Required = SensitivityPublic
		}},
		{"V13 metadata disagrees with the floored maximum", "V13", func(p *Provenance, _ *[]Claim, _ *Target, _ *SourceFacts) {
			p.SensitivityMax = SensitivityPublic
		}},
		{"V13 a source is above the floored maximum", "V13", func(_ *Provenance, _ *[]Claim, _ *Target, src *SourceFacts) {
			src.sensitivity[srcID(2)] = SensitivityCredentials
		}},
		{"V14 kept claim cites an undeclared source", "V14", func(_ *Provenance, kept *[]Claim, _ *Target, _ *SourceFacts) {
			(*kept)[1].SourceID = srcID(99)
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, kept, tgt, src := validProvenance(26), keptClaims(3), validTarget(), validFacts(26)
			c.mutate(&p, &kept, &tgt, &src)
			wantClause(t, p, kept, tgt, src, c.clause)
		})
	}
}

// TestValidateAcceptsTheOtherAnchorForms — V7 must not be a cluster_topic-only
// rule; the other two forms are legal blocks, not violations.
func TestValidateAcceptsTheOtherAnchorForms(t *testing.T) {
	watermark := int64(1000)
	next := genAt.Add(2 * time.Hour)
	forms := map[string]Anchor{
		"root_session": {
			Kind: AnchorRootSession, RootSessionID: "abc12345", WatermarkFrom: &watermark,
			Attempts: 1, NextAttemptAt: &next,
		},
		"root_session without manifest": {
			Kind: AnchorRootSession, RootSessionID: "abc12345", WatermarkFrom: &watermark,
		},
		"block": {Kind: AnchorBlock, BlockID: srcID(1)},
	}
	for name, a := range forms {
		t.Run(name, func(t *testing.T) {
			p := validProvenance(26)
			p.Anchor = a
			if err := Validate(p, keptClaims(3), validTarget(), validFacts(26)); err != nil {
				t.Errorf("anchor form %s must validate, got %v", name, err)
			}
		})
	}
}

// TestMissingInScopeIsNotAV5Case pins the distinction §4.5.4 exists for: a
// source that is archived or gone in the OWN scope makes the coverage smaller,
// not the write illegal. Collapsing the two sets back into one is what turned
// the scope check into a silent swallow in the first draft.
func TestMissingInScopeIsNotAV5Case(t *testing.T) {
	facts := validFacts(26)
	facts.missingInScope = []string{srcID(9), srcID(10)}
	if err := Validate(validProvenance(26), keptClaims(3), validTarget(), facts); err != nil {
		t.Fatalf("MissingInScope must not fail the write, got %v", err)
	}
}

// TestViolationOnAForeignError — Violation must not claim a clause for an
// error that is not one.
func TestViolationOnAForeignError(t *testing.T) {
	if got := Violation(ErrNoClaims); got != "" {
		t.Errorf("Violation(ErrNoClaims) = %q, want \"\"", got)
	}
	if got := Violation(nil); got != "" {
		t.Errorf("Violation(nil) = %q, want \"\"", got)
	}
}
