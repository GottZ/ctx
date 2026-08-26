package derived

import (
	"fmt"
	"time"
)

// srcID returns a deterministic, ascending block id. Ascending matters: the
// provenance contract says source_block_ids is sorted, and V3 checks it.
func srcID(n int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-000000000000", 0x7c3e1f00+n)
}

// srcIDs returns n ascending block ids.
func srcIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, srcID(i))
	}
	return out
}

// padRunes grows s with filler until it is exactly n runes long, then cuts it
// to n. Used to build claims and quotes of a stated length so a rune budget
// test measures a stated input instead of an accidental one.
func padRunes(s string, n int) string {
	r := []rune(s)
	filler := []rune(" und ein weiteres nachgestelltes Detail dazu")
	for len(r) < n {
		r = append(r, filler...)
	}
	return string(r[:n])
}

// sourceWith wraps quote in a longer original text, so a quote that must be
// found by G3 is a genuine substring and not the whole content.
func sourceWith(quote string) string {
	return "Vorspann des Quellblocks. " + quote + " Nachspann des Quellblocks."
}

// genAt is the fixed generation timestamp of the fixtures.
var genAt = time.Date(2026, 8, 26, 18, 0, 0, 0, time.UTC)

// validProvenance builds a provenance that passes all of V1–V14 against
// validFacts/validTarget AND against keptClaims(referenceClaims). Tests mutate
// one field to aim at one clause.
//
// The coverage block is derived from the reference claim set, not invented: the
// first version of this fixture reported claims_kept=24 while the reference
// call handed Validate three claims, and declared that combination valid. That
// made every clause test below it mutate away from an inconsistent baseline and
// hid the fact that V9 never looked at the claims at all (review finding #3).
const (
	// referenceClaims is how many claims the reference block keeps.
	referenceClaims = 3

	// referenceOffered is what the model offered for them. 3/8 = 0.375 sits
	// above MinKeepRatio (0.34) with room, so a ratio test has to move the
	// numbers on purpose to go red.
	referenceOffered = 8
)

func validProvenance(sourceCount int) Provenance {
	ids := srcIDs(sourceCount)
	return Provenance{
		V:              ContractVersion,
		Stratum:        StratumDerived,
		Arm:            "catalog",
		SourceBlockIDs: ids,
		SourceCount:    len(ids),
		SourceDigest:   SourceDigest(ids),
		Anchor: Anchor{
			Kind:     AnchorClusterTopic,
			TopicID:  "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
			CoreHash: "b1946ac92492d2347c6235b4d2611184",
		},
		GeneratedAt: genAt,
		Generator: Generator{
			Model:         "qwen38-27b",
			PromptVersion: "catalog-v1",
			GateVersion:   GateVersion,
		},
		Coverage: Coverage{
			ClaimsOffered:  referenceOffered,
			ClaimsKept:     referenceClaims,
			Rejects:        newRejects(),
			SourcesCovered: referenceClaims, // keptClaims(n) cites n distinct sources
		},
		UntrustedSources: 0,
		SensitivityMax:   SensitivityInternal,
	}
}

// validTarget is the write target the fixtures validate against.
func validTarget() Target {
	return Target{
		Stratum:   StratumDerived,
		Untrusted: false,
		Required:  SensitivityInternal,
	}
}

// validFacts builds source facts matching validProvenance(sourceCount).
func validFacts(sourceCount int) SourceFacts {
	f := SourceFacts{
		Strata:      map[string]Stratum{},
		Untrusted:   map[string]bool{},
		Sensitivity: map[string]string{},
		FlooredMax:  SensitivityInternal,
	}
	for _, id := range srcIDs(sourceCount) {
		f.Strata[id] = StratumSource
		f.Untrusted[id] = false
		f.Sensitivity[id] = SensitivityInternal
	}
	return f
}
