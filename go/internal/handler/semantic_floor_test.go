// E-M6 rule table for the post-fusion confidence gate. The handler path
// (no LLM call, response shape, log line) is pinned in
// semantic_floor_integration_test.go; here the rule itself is exercised on
// synthetic result sets, where every input the gate reads is chosen by hand.
package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/rrf"
)

// sfConfident is the query.confident_threshold default the fixtures below are
// written against — a literal, so the table keeps meaning if the default moves.
const sfConfident = 0.008

func sfCos(v float64) *float64 { return &v }

// sfSem is a native fusion hit that WAS in the semantic arm.
func sfSem(score, cos float64) rrf.SearchResult {
	return rrf.SearchResult{RRFScore: score, CosineSim: sfCos(cos)}
}

// sfLex is a native fusion hit the semantic arm never returned — ctx_rrf's
// FULL OUTER JOIN leaves cos_sim NULL for a block only FTS or trigram found.
func sfLex(score float64) rrf.SearchResult {
	return rrf.SearchResult{RRFScore: score}
}

func TestEvalSemanticFloor(t *testing.T) {
	cases := []struct {
		name       string
		results    []rrf.SearchResult
		floor      float64
		wantReject bool
		wantBest   float64
		wantLex    bool
	}{
		// --- The gate is OFF by default, and off means off. ---
		{
			name:    "floor 0 never rejects, however far the results are",
			results: []rrf.SearchResult{sfSem(0.0074, 0.01)},
			floor:   0,
			// BestCos stays -1: with the gate off nothing is measured at all,
			// so the log line can never report a number it did not compute.
			wantBest: -1,
		},
		{
			name:     "floor 0 with an empty result set",
			results:  nil,
			floor:    0,
			wantBest: -1,
		},
		{
			name: "negative floor is off too (V26 refuses it, this is the runtime half)",
			results: []rrf.SearchResult{
				sfSem(0.0074, 0.02),
			},
			floor:    -0.5,
			wantBest: -1,
		},

		// --- The case the wave exists for. ---
		{
			name: "all results far, no lexical carrier: reject",
			results: []rrf.SearchResult{
				sfSem(0.0091, 0.31), sfSem(0.0074, 0.28), sfSem(0.0033, 0.25),
			},
			floor:      0.45,
			wantReject: true,
			wantBest:   0.31,
		},
		{
			name: "best is the max, not the top-scoring row",
			results: []rrf.SearchResult{
				sfSem(0.0091, 0.31), sfSem(0.0074, 0.46),
			},
			floor:    0.45,
			wantBest: 0.46,
		},
		{
			name:       "an empty result set is not this gate's business",
			results:    nil,
			floor:      0.45,
			wantReject: false,
			wantBest:   -1,
		},

		// --- The rescue clause: exact identifiers embed to nothing. ---
		{
			name: "lexical-only hit at confident score rescues the set",
			results: []rrf.SearchResult{
				sfLex(0.0090), sfSem(0.0074, 0.30),
			},
			floor:    0.45,
			wantBest: 0.30,
			wantLex:  true,
		},
		{
			name: "lexical-only hit BELOW confident does not rescue",
			results: []rrf.SearchResult{
				sfLex(0.0041), sfSem(0.0074, 0.30),
			},
			floor:      0.45,
			wantReject: true,
			wantBest:   0.30,
		},
		{
			name: "the carrier need not be rank 1 (candidate (a), not (b))",
			results: []rrf.SearchResult{
				sfSem(0.0120, 0.30), sfSem(0.0100, 0.29), sfLex(0.0085),
			},
			floor:    0.45,
			wantBest: 0.30,
			wantLex:  true,
		},
		{
			name: "a purely lexical result set with a carrier is answerable",
			results: []rrf.SearchResult{
				sfLex(0.0090), sfLex(0.0050),
			},
			floor:    0.45,
			wantBest: -1,
			wantLex:  true,
		},
		{
			name: "a purely lexical result set WITHOUT a carrier is not",
			results: []rrf.SearchResult{
				sfLex(0.0041), sfLex(0.0016),
			},
			floor:      0.45,
			wantReject: true,
			wantBest:   -1,
		},

		// --- Injected neighbours are not lexical evidence. ---
		{
			name: "a graph-injected neighbour never carries the set",
			results: []rrf.SearchResult{
				{RRFScore: 0.0300, ViaGraph: true}, sfSem(0.0074, 0.30),
			},
			floor:      0.45,
			wantReject: true,
			wantBest:   0.30,
		},
		{
			name: "a cluster-injected neighbour never carries the set",
			results: []rrf.SearchResult{
				{RRFScore: 0.0300, ViaCluster: true, ClusterBoost: 0.5}, sfSem(0.0074, 0.30),
			},
			floor:      0.45,
			wantReject: true,
			wantBest:   0.30,
		},

		// --- The reranker rewrites RRFScore; the threshold lives on the RRF scale. ---
		{
			name: "the carrier check reads the RAW score where the reranker set one",
			results: []rrf.SearchResult{
				// Reranked to the top, but its fusion score was 0.0041 —
				// below confident, so it is not evidence of a lexical match.
				{RRFScore: 0.9000, RRFScoreOriginal: sfCos(0.0041)},
				sfSem(0.0074, 0.30),
			},
			floor:      0.45,
			wantReject: true,
			wantBest:   0.30,
		},
		{
			name: "a genuinely confident lexical hit still rescues after reranking",
			results: []rrf.SearchResult{
				{RRFScore: 0.1000, RRFScoreOriginal: sfCos(0.0090)},
				sfSem(0.0074, 0.30),
			},
			floor:    0.45,
			wantBest: 0.30,
			wantLex:  true,
		},

		// --- Above the floor: nothing to decide. ---
		{
			name: "a real semantic neighbour passes without any lexical help",
			results: []rrf.SearchResult{
				sfSem(0.0074, 0.63),
			},
			floor:    0.45,
			wantBest: 0.63,
		},
		{
			name: "exactly at the floor passes (the floor is a minimum, not a gap)",
			results: []rrf.SearchResult{
				sfSem(0.0074, 0.45),
			},
			floor:    0.45,
			wantBest: 0.45,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalSemanticFloor(tc.results, tc.floor, sfConfident)
			if got.Reject != tc.wantReject {
				t.Errorf("Reject = %v, want %v (verdict %+v)", got.Reject, tc.wantReject, got)
			}
			if got.BestCos != tc.wantBest {
				t.Errorf("BestCos = %v, want %v", got.BestCos, tc.wantBest)
			}
			if got.Lexical != tc.wantLex {
				t.Errorf("Lexical = %v, want %v", got.Lexical, tc.wantLex)
			}
		})
	}
}

// The gate must not mutate the result set it judges — it runs between the
// truncation and the source conversion, and every field it reads is also read
// by the synthesis path right after it.
func TestEvalSemanticFloorDoesNotMutateResults(t *testing.T) {
	cos := 0.30
	results := []rrf.SearchResult{
		{ID: "a", RRFScore: 0.0091, CosineSim: &cos},
		{ID: "b", RRFScore: 0.0074},
	}
	evalSemanticFloor(results, 0.45, sfConfident)
	if results[0].CosineSim == nil || *results[0].CosineSim != 0.30 || results[0].RRFScore != 0.0091 {
		t.Errorf("result 0 mutated: %+v", results[0])
	}
	if results[1].CosineSim != nil || results[1].RRFScore != 0.0074 {
		t.Errorf("result 1 mutated: %+v", results[1])
	}
}
