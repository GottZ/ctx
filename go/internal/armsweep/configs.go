package armsweep

import (
	"fmt"
	"math"
)

// The 16 configurations of design 04 §4.6, in the canonical report order.
//
// V0 and V0' carry IDENTICAL parameters. They are not two variants but one
// configuration scored on two independent dumps of the same corpus — the
// replicate pair whose disagreement IS the noise floor (gate G-NOISE). Naming
// the replicate as a configuration keeps the report's column set honest: the
// noise floor sits in the same table as the effects it has to dwarf.
const (
	NameV0      = "V0"
	NameV0Prime = "V0'"
	NameV1      = "V1"
	NameV6a     = "V6a"
	NameV6b     = "V6b"
)

// SoloNDCG is the per-arm nDCG@10 profile the V6 derivation consumes,
// measured on G-Q-DERIV alone (§4.6) — the half of the split that exists to be
// derived on, so the confirming half never sees the weights it will judge.
type SoloNDCG struct {
	Semantic float64 `json:"semantic"`
	FTSDe    float64 `json:"fts_de"`
	FTSEn    float64 `json:"fts_en"`
	Trigram  float64 `json:"trigram"`
}

// soloConfigs are the four single-arm fusions S1-S4. Weight 1 rather than the
// live weight: a solo arm's ranking is scale-invariant, and 1 makes the
// configuration readable as "this arm alone".
func soloConfigs() []Config {
	return []Config{
		{Name: "S1", Weights: Weights{Semantic: 1}, K: LiveK},
		{Name: "S2", Weights: Weights{FTSDe: 1}, K: LiveK},
		{Name: "S3", Weights: Weights{FTSEn: 1}, K: LiveK},
		{Name: "S4", Weights: Weights{Trigram: 1}, K: LiveK},
	}
}

// ConfigV0 is the live fusion: the baseline every variant is compared against
// and the configuration a dump's own fusion_order must reproduce (gate P2).
func ConfigV0() Config {
	return Config{Name: NameV0, Weights: LiveWeights, K: LiveK}
}

// staticConfigs are the 14 configurations whose parameters are literals. V6a
// and V6b are missing on purpose: their weights are DERIVED from a measurement
// and only exist once a dump has been scored (see DeriveV6).
func staticConfigs() []Config {
	out := []Config{
		ConfigV0(),
		{Name: NameV0Prime, Weights: LiveWeights, K: LiveK,
			Note: "replicate of V0 on the second dump — the noise floor, not a variant"},
	}
	out = append(out, soloConfigs()...)
	return append(out,
		// V1: the two FTS arms as ONE arm ranked by min(rank_de, rank_en). The
		// weight of the merged arm is the SUM of the two it replaces (0.45),
		// so the total lexical mass is unchanged and the comparison isolates
		// the merge itself rather than a reweighting hidden inside it.
		Config{Name: NameV1, Weights: Weights{Semantic: 0.45, FTSDe: 0.45, Trigram: 0.10},
			K: LiveK, MergeFTS: true},
		Config{Name: "V2", Weights: Weights{Semantic: 0.80, FTSDe: 0.08, FTSEn: 0.10, Trigram: 0.02}, K: LiveK},
		Config{Name: "V3", Weights: Weights{Semantic: 0.60, FTSDe: 0.15, FTSEn: 0.17, Trigram: 0.08}, K: LiveK},
		Config{Name: "V4", Weights: Weights{Semantic: 0.50, FTSDe: 0.22, FTSEn: 0.28, Trigram: 0.00}, K: LiveK},
		Config{Name: "V5", Weights: LiveWeights, K: LiveK, FactorsOutside: true},
		Config{Name: "V7a", Weights: LiveWeights, K: 10},
		Config{Name: "V7b", Weights: LiveWeights, K: 30},
		Config{Name: "V7c", Weights: LiveWeights, K: 120},
	)
}

// DampingStops are the ten support points of the damping curve (M-W8, design
// 05 §4.3/§6.1) — a family of its own, not four more entries in the weight
// sweep.
//
// The grid is not uniform and is not meant to be: it contains EVERY factor the
// live registry currently assigns (`toolEvidenceDamping` 0.15,
// `auditTrailDamping` 0.3, `toolOverviewDamping` 0.35, blocktype/builtin.go
// :79/:80/:32) plus the undamped 1.0, so whatever the swept type's live factor
// is, the curve contains the status quo and the report can show the measured
// optimum NEXT TO it instead of on a grid that misses it by 0.02. The rest of
// the points thicken the low end, where the multiplicative factor still changes
// the ranking; between 0.85 and 1.0 it mostly does not.
var DampingStops = []float64{0.05, 0.10, 0.15, 0.20, 0.30, 0.35, 0.50, 0.70, 0.85, 1.00}

// DampingName is the report label of one support point. The factor is IN the
// name because a damping table read without its configuration column is the
// one table in this report where the reader would otherwise have to guess.
func DampingName(factor float64) string { return fmt.Sprintf("D%.2f", factor) }

// DampingConfigs is the damping curve for ONE type: the live weight vector at
// every support point, differing in nothing but the factor.
//
// Weights and k stay at V0 on purpose. The curve answers "what is this type's
// damping worth", and a family that moved two things at once would answer
// neither question. That is also why these configurations are reported in
// their own section and never enter the variant-vs-V0 table: G-WIN reads its
// Bonferroni level off SecondaryComparisons, a fixed 13, and ten more rows in
// that table would silently loosen every level in it.
func DampingConfigs(typeName string) []Config {
	out := make([]Config, 0, len(DampingStops))
	for _, f := range DampingStops {
		out = append(out, Config{
			Name: DampingName(f), Weights: LiveWeights, K: LiveK,
			Damping: map[string]float64{typeName: f},
			Note: fmt.Sprintf("damping curve: type_factor of %q forced to %.2f; live weights, k=%g",
				typeName, f, LiveK),
		})
	}
	return out
}

// ConfigNames is the canonical report column order, V6a/V6b included at their
// derived position. A report that reordered its columns between runs would
// break the byte-identity gate, so the order lives here and nowhere else.
func ConfigNames() []string {
	return []string{NameV0, NameV0Prime, "S1", "S2", "S3", "S4", NameV1, "V2", "V3", "V4", "V5",
		NameV6a, NameV6b, "V7a", "V7b", "V7c"}
}

// ConfigByName returns a STATIC configuration. V6a/V6b are not static and are
// reported as absent — the caller must derive them from a solo profile.
func ConfigByName(name string) (Config, bool) {
	for _, c := range staticConfigs() {
		if c.Name == name {
			return c, true
		}
	}
	return Config{}, false
}

// DeriveV6 builds w ∝ nDCG_solo^β, normalised to the live weight sum of 1
// (§4.6). β=1 is proportional, β=2 sharpens the profile — the pair asks
// whether "weight the arms by how well they do alone" is a rule at all, and
// how hard it should be applied.
//
// A degenerate profile (every arm at nDCG 0 — a slice no arm can answer) has
// no proportional answer. Rather than divide by zero, invent a uniform vector
// or drop the configuration silently, it falls back to the live weights and
// SAYS SO in Note: the report then shows V6 tying V0 exactly, which reads as
// "not derivable here" instead of as a null result.
func DeriveV6(name string, solo SoloNDCG, beta float64) Config {
	raw := [4]float64{
		math.Pow(solo.Semantic, beta),
		math.Pow(solo.FTSDe, beta),
		math.Pow(solo.FTSEn, beta),
		math.Pow(solo.Trigram, beta),
	}
	sum := 0.0
	for _, v := range raw {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		sum += v
	}
	if sum <= 0 {
		return Config{Name: name, Weights: LiveWeights, K: LiveK,
			Note: fmt.Sprintf("solo nDCG profile degenerate (sum %.g) — fell back to the live weights", sum)}
	}
	return Config{
		Name: name,
		Weights: Weights{
			Semantic: raw[0] / sum, FTSDe: raw[1] / sum,
			FTSEn: raw[2] / sum, Trigram: raw[3] / sum,
		},
		K:    LiveK,
		Note: fmt.Sprintf("derived on G-Q-DERIV: w ∝ nDCG_solo^%g", beta),
	}
}

// AllConfigs is the full 16-entry set in report order, with V6a/V6b derived
// from the supplied solo profile.
func AllConfigs(solo SoloNDCG) []Config {
	byName := map[string]Config{}
	for _, c := range staticConfigs() {
		byName[c.Name] = c
	}
	byName[NameV6a] = DeriveV6(NameV6a, solo, 1)
	byName[NameV6b] = DeriveV6(NameV6b, solo, 2)

	names := ConfigNames()
	out := make([]Config, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out
}
