package handler

import "github.com/GottZ/ctx/internal/rrf"

// semanticFloorVerdict is what the post-fusion confidence gate (E-M6) decided
// about one fused result set, plus the two numbers the log line reports.
type semanticFloorVerdict struct {
	// Reject is true when the query must be refused WITHOUT a synthesis call.
	Reject bool
	// BestCos is the highest cosine similarity any embedding-compared result
	// carried, or -1 when NO result carried one (a purely lexical result set).
	// -1 rather than 0: 0 is a legal cosine similarity (orthogonal vectors) and
	// the log line has to distinguish "measured, and it was nothing" from "never
	// measured".
	BestCos float64
	// Lexical is true when a lexical-only hit carries the result set — the
	// rescue clause below.
	Lexical bool
}

// evalSemanticFloor is the post-fusion confidence gate: it reads the FUSED and
// boosted result set (after cluster/graph/gravity, after rerank, after the
// user-facing truncation) and answers one question — is this set worth an LLM
// call, or is the honest answer already "nothing here relates to that"?
//
// WHY IT EXISTS. ctx_rrf is rank-based; nothing in the fusion looks at how far
// away the semantic neighbours actually are (migration 134 header, same
// observation from the other side). The semantic arm returns its 75 nearest
// blocks for "what are the best restaurants in Berlin?" exactly as it does for
// "what embedding model does the store use?". Rank 1 alone contributes
// 0.45/61 = 0.0074, and one lexical graze pushes the top score past
// query.confident_threshold (0.008). The result set is then indistinguishable
// from an answerable one until the LLM has read it and refused — a ~2 s call
// whose whole output is a rejection, and whose verdict then collides with the
// RRF signal in ApplyConfidenceOverride (llm/synthesize.go): a "confident" RRF
// score turns the LLM's NO_RELEVANT_SOURCES into low_confidence instead of
// no_relevant_blocks_found, so the same off-topic query answers differently
// from one corpus state to the next.
//
// THE LEXICAL RESCUE, and why it is not optional. CosineSim is NULL exactly
// when a block never appeared in the semantic arm: ctx_rrf fuses the four arms
// with a FULL OUTER JOIN and projects s.cos_sim, so a block that only FTS or
// trigram_title found carries no similarity at all (migration 134, the rrf CTE).
// That is not a weak result — it is the signature of the queries embeddings are
// worst at: exact identifiers, port numbers, error codes, config keys (eval K01
// "workflow ID", K04 "port"). A bare identifier embeds to nothing in
// particular, so its cosine similarity is low while its lexical evidence is
// unambiguous. A floor without this clause would kill precisely the class of
// query the lexical arms exist for.
//
// THE RULE, in the order it is evaluated:
//
//	floor <= 0                      -> never rejects. This is the default, and
//	                                   with it the pipeline is byte-identical
//	                                   to the pre-E-M6 one.
//	no results at all               -> not this gate's business; llm.Synthesize's
//	                                   score filter already answers that case.
//	best CosineSim >= floor         -> a real semantic neighbour exists. Synthesize.
//	a lexical-only hit carries      -> rescue clause. Synthesize.
//	otherwise                       -> reject, no LLM call.
//
// "A lexical-only hit carries" is candidate (a) of the wave brief — ANY result
// with no CosineSim whose raw RRF score reaches confidentThreshold — and not
// candidate (b), "the rank-1 result has no CosineSim". Two reasons. Rank 1 is
// decided by the fused score, which the gravity/cluster/graph boosts and the
// reranker all rewrite; tying the rescue to the top slot would make it depend
// on stages that never looked at lexical evidence. And the threshold is what
// makes the clause safe: a lexical-only block needs real rank in the lexical
// arms to reach 0.008 (FTS-en rank 1 alone is 0.25/61 = 0.0041, trigram rank 1
// is 0.10/61 = 0.0016), so an off-topic query's incidental word overlap does not
// clear it, while an exact identifier match — which tends to top several lexical
// arms at once — does.
//
// MEASURED (live corpus, 2026-08-25, 39 of the 47 eval queries — the other 8
// had no cached query vector): the clause never fires there. The strongest
// lexical-only row over the whole fused list of any query scored 0.007031,
// below the 0.008 it needs, and no query had one in its top 10 at all. That is
// not a dead clause, it is a quiet one: the eval set's identifier questions are
// full sentences ("What port does Ollama run on?", best cosine 0.62), which the
// semantic arm handles without help. What the clause guards is the bare-token
// lookup — a workflow id, a port number pasted on its own — which that set does
// not contain and which a corpus of 10M blocks will. A seeded fixture proves
// the arithmetic can be reached: rrf/em6_lexical_cosine_integration_test.go
// measures 0.009016 for a block topping all three lexical arms.
//
// Graph- and cluster-injected neighbours are EXCLUDED from the rescue clause.
// They also carry CosineSim nil (rrf/graph.go, rrf/cluster.go clear every
// derived score field on injection, deliberately), but their nil means "never
// measured against anything", not "found by the lexical arms" — and their
// RRFScore is a fraction of the top score, so on a weak result set a boosted
// neighbour could clear confidentThreshold purely because the top score was
// already inflated. Reading them as lexical evidence would let the gate be
// talked out of a rejection by the very stages that expand a thin result set.
//
// The score read is the RAW fusion score (RRFScoreOriginal where the reranker
// set it, RRFScore otherwise) — the same derivation the retrieval-only path
// uses, because confidentThreshold is calibrated on the RRF scale and the
// reranker rewrites RRFScore onto its own.
func evalSemanticFloor(results []rrf.SearchResult, floor, confidentThreshold float64) semanticFloorVerdict {
	v := semanticFloorVerdict{BestCos: -1}
	if floor <= 0 || len(results) == 0 {
		return v
	}
	for i := range results {
		r := &results[i]
		if r.CosineSim != nil {
			if *r.CosineSim > v.BestCos {
				v.BestCos = *r.CosineSim
			}
			continue
		}
		// No cosine similarity: either a lexical-only fusion hit (the rescue
		// class) or an injected neighbour (never a carrier — see above).
		if r.ViaGraph || r.ViaCluster {
			continue
		}
		if rawRRFScore(r) >= confidentThreshold {
			v.Lexical = true
		}
	}
	v.Reject = v.BestCos < floor && !v.Lexical
	return v
}

// rawRRFScore returns the fusion score on the scale query.confident_threshold
// is calibrated for: RRFScoreOriginal is set precisely when the reranker
// replaced RRFScore with its own (Step 6b), and the retrieval-only path in
// query.go reads it the same way.
func rawRRFScore(r *rrf.SearchResult) float64 {
	if r.RRFScoreOriginal != nil {
		return *r.RRFScoreOriginal
	}
	return r.RRFScore
}
