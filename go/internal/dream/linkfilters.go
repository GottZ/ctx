package dream

import (
	"log/slog"
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
	"recurrent":  true,
}

// minRawConfidence is the per-type minimum raw (unweighted) LLM confidence.
// Session 24 (2026-04-23): factual threshold lowered from 0.9 to 0.7. V5 prompt combined with
// qwen3.6:27b produces uncalibrated confidence (full_agree mean 0.87 vs wrong_type mean 0.85 —
// gap 0.02). The 0.9 factual gate was a legacy workaround for qwen3.5; on V5+3.6 it dropped
// 2 correct links and 0 wrong types (net-negative).
//
// Welle 38b (2026-05-06): 'recurrent' added with 0.8 floor — Phase-2 LLM-classification
// has more error-modes (false-positive weekly vs sessional) than topical/factual/causal,
// so the gate is one quantisation step above the others.
var minRawConfidence = map[string]float64{
	"topical":    0.7,
	"factual":    0.7,
	"causal":     0.7,
	"supersedes": 0.7,
	"recurrent":  0.8,
}

// supersedesTitleSimThreshold is the pg_trgm similarity floor for V8.
const supersedesTitleSimThreshold = 0.25

// applyLinkFloor lifts every Floored link (confidence assigned by the
// parser because the model gave none — PR #12 drift forms) to the
// operator-configured floor (dream.link_floor_confidence, default 0.9),
// keeping the per-type minRawConfidence write gate as the lower bound so a
// configured floor below a type's gate cannot turn the link into a silent
// write-path no-op. floor <= 0 (unwired routers, tests) keeps the parser's
// per-type floors — the conservative PR-#12 semantics. Links whose
// confidence the model actually emitted are never touched.
// Pure function — no I/O, no shared state.
func applyLinkFloor(links []Link, floor float64) []Link {
	if floor <= 0 {
		return links
	}
	if floor > 1 {
		floor = 1
	}
	for i := range links {
		if !links[i].Floored {
			continue
		}
		f := floor
		if mr := minRawConfidence[links[i].Relationship]; f < mr {
			f = mr
		}
		links[i].Confidence = f
	}
	return links
}

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

// V8 check: supersedes requires same category, source meaningfully NEWER than
// target (source = the authoritative replacement), and title similarity ≥
// titleSimThreshold (caller computes via pg_trgm).
//
// Welle 46 Convention-Switch (2026-05-22): direction inverted from
// "source older than target" to "source newer than target", and switched from
// updated_at to created_at. Audit (Sub-Agent 2 Semantic-Stichprobe) found ALL
// 15 supersedes-Links in production inverted relative to natural-language
// semantics ("A supersedes B" → A is the newer authoritative version).
// Migration 042 truncates context_dream_links + resets dream_checked_at; the
// Re-Dream-Pass writes the new corpus under the English convention.
// Downstream consumers (SupersedesMap, filterSuperseded, ApplySupersedes,
// PromoteToCanonical) were aligned with this filter in Welle 46.
func acceptSupersedes(srcCat, tgtCat string, srcCreated, tgtCreated time.Time, titleSim float64) (bool, string) {
	if srcCat != tgtCat {
		return false, "different category"
	}
	if !srcCreated.After(tgtCreated) {
		return false, "source not newer"
	}
	if titleSim < supersedesTitleSimThreshold {
		return false, "low title similarity"
	}
	return true, ""
}

// enforceSupersedesDirection is the post-parse hard-constraint that runs
// after parseLinks but before filterValidCandidates. For every link with
// relationship='supersedes', it requires that the source candidate's timestamp
// is strictly after the target candidate's timestamp.
//
// Welle 46 (2026-05-22): inverted links are DOWNGRADED to 'topical' instead
// of dropped. The reasoning: the LLM emitted a supersedes claim between two
// blocks A and B that ARE related — the relation label is just directionally
// wrong relative to created_at. A swap to "B supersedes A" is not possible at
// this layer because Link only carries TargetID (source is the current dream
// block, fixed by the caller). Dropping the link discards the information
// that A and B are topically connected. Downgrading to 'topical' preserves
// the edge in the graph at the conservative cost of one relationship class:
// 'topical' is the documented fallback per V5 prompt ("when in doubt: topical")
// and matches what coerceCategoryFactual does for same-category factual links.
// Audit-Trail: the returned downgrade count feeds
// entry.Metadata["supersedes_direction_downgraded"] in the caller
// (evaluate.go) so the rate of inversion stays observable. (The caller
// previously measured len(in)-len(out), which is structurally zero since the
// Welle-46 switch from drop to downgrade — the telemetry was dead.)
//
// candidates[].UpdatedAt is used as approximation for CreatedAt because the
// rrf.SearchResult struct only carries UpdatedAt. For the Dream corpus,
// UpdatedAt >= CreatedAt; a link inverted by UpdatedAt is also inverted by
// CreatedAt (exception: blocks updated after creation can flip the inequality;
// rare and conservative). The final created_at check in acceptSupersedes
// (writelinks.go) is the authoritative gate — this is defense in depth.
func enforceSupersedesDirection(links []Link, sourceCreated time.Time, candidates []BlockInfo) ([]Link, int) {
	if len(links) == 0 {
		return links, 0
	}
	candByID := make(map[string]time.Time, len(candidates))
	for _, c := range candidates {
		candByID[c.ID] = c.UpdatedAt
	}
	downgraded := 0
	out := make([]Link, 0, len(links))
	for _, l := range links {
		if l.Relationship == "supersedes" {
			tgtTS, ok := candByID[l.TargetID]
			if !ok {
				// Target not in candidates — filterValidCandidates will drop it later.
				out = append(out, l)
				continue
			}
			if !sourceCreated.After(tgtTS) {
				// Inverted direction → downgrade to topical (conservative: preserves
				// the edge, discards the directional spec-claim). Sub-Agent-2 Audit
				// 2026-05-22 found this is the dominant LLM-error pattern.
				slog.Debug("dream: supersedes downgraded to topical (inverted direction)",
					"target_id", l.TargetID,
					"source_created", sourceCreated,
					"target_updated", tgtTS,
				)
				l.Relationship = "topical"
				downgraded++
			}
		}
		out = append(out, l)
	}
	return out, downgraded
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
