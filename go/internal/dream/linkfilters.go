package dream

import (
	"math"
	"regexp"
	"sort"
	"time"
)

// uuidPattern validates UUID format for target_id fields.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// validRelationships defines the allowed relationship types.
var validRelationships = map[string]bool{
	"topical":    true,
	"factual":    true,
	"causal":     true,
	"supersedes": true,
}

// minRawConfidence is the per-type minimum raw (unweighted) LLM confidence.
// Session 24 (2026-04-23): factual threshold lowered from 0.9 to 0.7. V5 prompt combined with
// qwen3.6:27b produces uncalibrated confidence (full_agree mean 0.87 vs wrong_type mean 0.85 —
// gap 0.02). The 0.9 factual gate was a legacy workaround for qwen3.5; on V5+3.6 it dropped
// 2 correct links and 0 wrong types (net-negative).
var minRawConfidence = map[string]float64{
	"topical":    0.7,
	"factual":    0.7,
	"causal":     0.7,
	"supersedes": 0.7,
}

// supersedesTitleSimThreshold is the pg_trgm similarity floor for V8.
const supersedesTitleSimThreshold = 0.25

// filterValidCandidates rejects LLM links that fail any of:
//   - target_id not in UUID format (LLM emitted free-text)
//   - target_id not present in the candidate set (LLM hallucinated an ID)
//   - relationship not in validRelationships
//   - confidence > 1.0 or NaN or ±Inf (defensive — JSON unmarshalling never
//     yields NaN/Inf, but malformed Link slices from non-LLM sources are
//     still rejected)
//   - confidence below the per-type minRawConfidence floor
//
// Pure function — no I/O, no shared state.
func filterValidCandidates(links []Link, candidateIDs map[string]bool) []Link {
	var valid []Link
	for _, l := range links {
		if !uuidPattern.MatchString(l.TargetID) {
			continue
		}
		if !candidateIDs[l.TargetID] {
			continue
		}
		if !validRelationships[l.Relationship] {
			continue
		}
		if l.Confidence > 1.0 || math.IsNaN(l.Confidence) || math.IsInf(l.Confidence, 0) {
			continue
		}
		if l.Confidence < minRawConfidence[l.Relationship] {
			continue
		}
		valid = append(valid, l)
	}
	return valid
}

// applyHardCap caps a link slice to max entries with tier-local
// type-diversity tie-break. Returns the (possibly trimmed) slice and the
// number of dropped entries. Output is confidence-DESC sorted.
//
// Algorithm:
//  1. Sort stable by Confidence DESC.
//  2. Walk equal-confidence tiers. Within each tier, prefer types not yet
//     present in the output, then duplicates of already-picked types. The
//     "seen" set is rebuilt PER TIER from the output — diversity preference
//     applies only at the tie-break boundary; a higher-confidence link is
//     never displaced by a lower-confidence link from a different type.
//  3. Stop when cap is reached.
//
// This addresses the V3 topical-monoculture (86% live) at quantisation tiers
// (qwen3.6 emits {0.7, 0.75, 0.8, 0.85, 0.9, 0.95}) without sacrificing
// confidence ordering. Below cap: input returned untouched.
func applyHardCap(links []Link, max int) ([]Link, int) {
	if len(links) <= max {
		return links, 0
	}
	sort.SliceStable(links, func(i, j int) bool {
		return links[i].Confidence > links[j].Confidence
	})
	out := make([]Link, 0, max)
	i := 0
	for i < len(links) && len(out) < max {
		tierEnd := i + 1
		for tierEnd < len(links) && links[tierEnd].Confidence == links[i].Confidence {
			tierEnd++
		}
		seen := make(map[string]bool, len(out))
		for _, l := range out {
			seen[l.Relationship] = true
		}
		var novel, dup []Link
		for _, l := range links[i:tierEnd] {
			if !seen[l.Relationship] {
				novel = append(novel, l)
				seen[l.Relationship] = true
			} else {
				dup = append(dup, l)
			}
		}
		for _, l := range novel {
			if len(out) >= max {
				break
			}
			out = append(out, l)
		}
		for _, l := range dup {
			if len(out) >= max {
				break
			}
			out = append(out, l)
		}
		i = tierEnd
	}
	return out, len(links) - len(out)
}

// V5/V6 check: drop links to archived targets or different-scope targets.
// Returns (accepted, reason) — reason populated only when not accepted.
func acceptScopeAndArchived(targetScope, sourceScope string, targetArchived bool) (bool, string) {
	if targetArchived {
		return false, "target archived"
	}
	if targetScope != sourceScope {
		return false, "cross-scope"
	}
	return true, ""
}

// V10 coerce: factual edges between same-category blocks are nearly always
// topical ties (Bulk-test 2026-04-21, n=100, 10 iters: +10pp accuracy when
// coerced). factual semantics require SPEC→IMPL direction across categories
// (spec in decisions/reference, impl in projects).
// Returns the (possibly mutated) link relationship.
func coerceCategoryFactual(rel, srcCat, tgtCat string) string {
	if rel == "factual" && srcCat == tgtCat {
		return "topical"
	}
	return rel
}

// V8 check: supersedes requires same category, source meaningfully older than
// target, and title similarity ≥ titleSimThreshold (caller computes via pg_trgm).
// Pre-Reset-Audit 2026-04-20 motivation: 9B model can't distinguish
// "complementary" from "replaces", deterministic pre-filter.
func acceptSupersedes(srcCat, tgtCat string, srcUpdated, tgtUpdated time.Time, titleSim float64) (bool, string) {
	if srcCat != tgtCat {
		return false, "different category"
	}
	if !srcUpdated.Before(tgtUpdated) {
		return false, "source not older"
	}
	if titleSim < supersedesTitleSimThreshold {
		return false, "low title similarity"
	}
	return true, ""
}

// V9 check: causal requires source predates target by created_at.
// Pre-Reset-Audit 2026-04-20: LLM invents wrong-direction causal links
// (28% causal-reciprocity in Graph-Topology). Live post-V9: 0% reciprocity,
// 7% wrong-direction (down from 34%).
func acceptCausal(srcCreated, tgtCreated time.Time) (bool, string) {
	if !srcCreated.Before(tgtCreated) {
		return false, "source not older"
	}
	return true, ""
}
