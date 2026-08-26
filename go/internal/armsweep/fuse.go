// Package armsweep is the offline arm-weight sweep of the ctx retrieval
// fusion (design 04 §4.6-§4.9, wave B-W5).
//
// It has three stages and they are deliberately separate programs sharing one
// library, not one long-running job:
//
//	prime  every gold query once through the admin-gated arm_ranks seam WITHOUT
//	       pins, to capture the translation/temporal results as pins and warm
//	       the embed cache. Nothing is scored.
//	dump   the same queries WITH those pins, recording the per-arm ranks, the
//	       live fusion order and the delivered ranking into one JSONL file,
//	       bracketed by a drift stamp on both sides.
//	score  a dump (or a dump PAIR) re-fused under 16 configurations, scored per
//	       slice, gated by G-NOISE and G-WIN, written as a deterministic report.
//
// The split exists because the measurement is the expensive, fragile part and
// the scoring is the part that gets iterated. A dump is an artefact: once
// written it can be re-scored under new configurations without touching the
// live instance again, and two runs of `score` over the same dump produce the
// same bytes.
//
// WHAT THIS INSTRUMENT DOES NOT MEASURE: everything after ctx_rrf. Gravity,
// cluster injection, the graph expansion, the aggregate fold and the rerank
// stage all run on the live path and are RECORDED (the delivered ranking, the
// effective post-fusion stage config in the stamp) but never re-simulated. A
// weight that wins here wins at the fusion stage; whether it survives the
// post-stages is a different question and needs a different instrument.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package armsweep

import (
	"sort"

	"github.com/GottZ/ctx/internal/rrf"
)

// LiveK is ctx_rrf's reciprocal-rank constant (migration 139:335-338, the
// `60 +`).
const LiveK = 60.0

// LiveWeights are the four channel weights the production fusion multiplies
// onto the reciprocal ranks (139:335-338): semantic 0.45, fts_de 0.20,
// fts_en 0.25, trigram 0.10.
var LiveWeights = Weights{Semantic: 0.45, FTSDe: 0.20, FTSEn: 0.25, Trigram: 0.10}

// Weights is one arm-weight vector in the ctx_rrf term order.
//
// Under Config.MergeFTS the FTSDe field carries the weight of the MERGED
// lexical arm and FTSEn must be zero — the merge is a three-arm fusion wearing
// a four-field struct, which keeps one weight vector type in the reports
// instead of two.
type Weights struct {
	Semantic float64 `json:"semantic"`
	FTSDe    float64 `json:"fts_de"`
	FTSEn    float64 `json:"fts_en"`
	Trigram  float64 `json:"trigram"`
}

// TypeNameMigration is the migration that added `type_name` to the
// ctx_rrf_arms return (M-W1). A dump written by an instance below it carries
// the empty string in every row's TypeName — not because those blocks have no
// type, but because the column did not exist yet. Every damping decision in
// this package is keyed on that name, which is why the number is a constant
// here and a refusal in Score rather than a comment.
const TypeNameMigration = 142

// Config is one fusion configuration under test (§4.6).
type Config struct {
	Name    string  `json:"name"`
	Weights Weights `json:"weights"`
	K       float64 `json:"k"`
	// Damping replaces the per-row type_factor for the types it names
	// (type_name → factor, wave M-W8). It is the one input of the fusion the
	// live registry owns rather than the dump: the factor sits in
	// blocktype/builtin.go, it multiplies into the score AFTER arm membership
	// is decided (139:335-338, and the arm CTEs never see it), and therefore a
	// dump that recorded the name of each candidate's type can be re-fused at
	// any damping value without measuring anything again.
	//
	// A row whose TypeName is not in the map — including every row of a dump
	// written before migration 142, where the name is empty — keeps the factor
	// the dump recorded. That is the reproduction path, and it is the reason
	// the empty name is never a map key: an old dump must re-fuse to exactly
	// the numbers it re-fused to before this field existed.
	Damping map[string]float64 `json:"damping,omitempty"`
	// MergeFTS collapses the two FTS arms into one whose rank is
	// min(rank_de, rank_en) — configuration V1, the only structural change in
	// the set. Everything else moves numbers.
	MergeFTS bool `json:"merge_fts,omitempty"`
	// FactorsOutside multiplies mass·type onto the SUM rather than onto each
	// term — configuration V5. Algebraically identical, not identical in
	// float64, and the difference is the measurement.
	FactorsOutside bool `json:"factors_outside,omitempty"`
	// Note records a derivation caveat (V6 fallback) so a report never carries
	// a weight vector whose provenance is implicit.
	Note string `json:"note,omitempty"`
}

// Fused is a scored candidate.
type Fused struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// Fuse recomputes a fusion from the per-arm ranks and sorts it the way Gen 17
// does: score descending, id ascending (migration 139:364 `ORDER BY r.score
// DESC, cb.id`). The id comparison is the tiebreak that makes two runs of one
// query answer the same, and a Go string comparison over the canonical UUID
// text reproduces Postgres' uuid ordering — the canonical form is the lowercase
// hex of the 16 bytes in order, and uuid comparison is a memcmp of those bytes.
//
// Truncation is NOT applied: ctx_rrf_arms ignores p_limit and hands out every
// candidate the four arms produced, which is what an offline sweep needs. A
// caller comparing against a LIVE fusion_order therefore compares prefixes.
func Fuse(rows []rrf.ArmRow, cfg Config) []Fused {
	out := make([]Fused, 0, len(rows))
	for _, r := range rows {
		out = append(out, Fused{ID: r.ID, Score: scoreRow(r, cfg)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// FusedIDs projects a fusion to its ranking.
func FusedIDs(f []Fused) []string {
	out := make([]string, len(f))
	for i := range f {
		out[i] = f[i].ID
	}
	return out
}

// scoreRow is the fusion arithmetic of ONE candidate.
//
// The term order and the placement of the mass/type factors deliberately copy
// the SQL expression (`w * mass * type * recip`, summed semantic → fts_de →
// fts_en → trigram, 139:335-338) instead of the algebraically equivalent
// `mass * type * Σ w·recip`. Both are the same real number; only one is the
// same float64, and the B-W1 parity gate measured max |Δscore| = 0 against SQL
// for exactly this form (arms_parity_integration_test.go). V5 is the
// configuration that asks what the OTHER form is worth, which is why the
// alternative lives behind a flag instead of being cleaned up into the general
// case.
func scoreRow(r rrf.ArmRow, cfg Config) float64 {
	w := cfg.Weights
	tf := typeFactor(r, cfg)
	deRank, enRank, enWeight := r.RankFTSDe, r.RankFTSEn, w.FTSEn
	if cfg.MergeFTS {
		deRank, enRank, enWeight = minRank(r.RankFTSDe, r.RankFTSEn), nil, 0
	}
	if cfg.FactorsOutside {
		return r.MassFactor * tf *
			(w.Semantic*recip(r.RankSemantic, cfg.K) +
				w.FTSDe*recip(deRank, cfg.K) +
				enWeight*recip(enRank, cfg.K) +
				w.Trigram*recip(r.RankTrigram, cfg.K))
	}
	mt := r.MassFactor * tf
	return w.Semantic*mt*recip(r.RankSemantic, cfg.K) +
		w.FTSDe*mt*recip(deRank, cfg.K) +
		enWeight*mt*recip(enRank, cfg.K) +
		w.Trigram*mt*recip(r.RankTrigram, cfg.K)
}

// typeFactor is the one place the fusion decides whether a candidate's type
// damping comes from the dump or from the configuration.
//
// The empty TypeName is checked BEFORE the map, not after: without that line a
// pre-142 dump would agree with `Damping{"": x}` on every row at once, which is
// the single substitution that turns a damping sweep into a silent global
// rescale of the whole dump. A map that names no type at all (the 14 static
// configurations) never reaches the lookup, so their arithmetic is the same
// expression it was before this field existed.
func typeFactor(r rrf.ArmRow, cfg Config) float64 {
	if len(cfg.Damping) == 0 || r.TypeName == "" {
		return r.TypeFactor
	}
	if f, ok := cfg.Damping[r.TypeName]; ok {
		return f
	}
	return r.TypeFactor
}

// recip mirrors `COALESCE(1.0 / (k + rank), 0)`: a missing arm contributes
// nothing rather than removing the candidate.
func recip(rank *int, k float64) float64 {
	if rank == nil {
		return 0
	}
	return 1.0 / (k + float64(*rank))
}

// minRank is the V1 lexical merge: the better of the two FTS ranks, or nil when
// neither arm found the candidate. nil is NOT rank 0 — rank 0 does not exist,
// 1 is the best rank, and turning a miss into a perfect hit is the one mistake
// this function exists to prevent.
func minRank(a, b *int) *int {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b < *a:
		return b
	default:
		return a
	}
}
