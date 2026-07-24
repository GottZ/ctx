package graphcache

import "math"

// u16 confidence fixpoint (§3.2 Nr. 2). Confidence in [0,1] is stored as x/65535
// (resolution ~1.5e-5, three orders of magnitude below the config gate
// granularity 0.75/0.8). The conversion rule is NORMATIVE and asymmetric — it
// is the mechanism that keeps cache gates AT LEAST AS STRICT as SQL, never
// laxer (fail-closed direction):
//
//	edge weights      → ConfToFix      (FLOOR)
//	comparison thresholds → ThresholdToFix (CEIL)
//
// Proof sketch that floor(weight) ≥ ceil(threshold) ⇒ weight ≥ threshold:
// floor(w·N) ≥ ceil(s·N) ⇒ w·N ≥ floor(w·N) ≥ ceil(s·N) ≥ s·N ⇒ w ≥ s. The
// converse borderline case is the one the W05.1 gate probes: a weight just
// below a threshold that shares its floor bucket (e.g. w=0.75, s=0.75 both floor
// to 49151) is REJECTED by the cache (floor 49151 < ceil 49152) — strictly
// safer than SQL, which would accept w=0.75 ≥ 0.75. Using floor on BOTH sides
// would make the cache laxer (both 49151, edge passes) — the bug this rule
// forbids.
const fixScale = 65535

// ConfToFix converts an edge weight in [0,1] to its u16 fixpoint by FLOOR.
// Out-of-range inputs clamp to the endpoints.
func ConfToFix(w float64) uint16 {
	if w <= 0 {
		return 0
	}
	if w >= 1 {
		return fixScale
	}
	return uint16(math.Floor(w * fixScale))
}

// ThresholdToFix converts a comparison threshold in [0,1] to its u16 fixpoint by
// CEIL. Out-of-range inputs clamp to the endpoints. Compare an edge's ConfToFix
// value against a threshold's ThresholdToFix value with >= to reproduce the SQL
// `confidence >= threshold` gate, never laxer.
func ThresholdToFix(s float64) uint16 {
	if s <= 0 {
		return 0
	}
	if s >= 1 {
		return fixScale
	}
	return uint16(math.Ceil(s * fixScale))
}
