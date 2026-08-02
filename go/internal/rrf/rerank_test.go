package rrf

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/llm"
)

func sigmoid(x float64) float64 { return 1.0 / (1.0 + math.Exp(-x)) }

// ids extracts the result ids in order for sequence assertions.
func ids(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// applyRerankScores is the shared blend+sort machinery (judge + cross-encoder).

// At weight 1.0 the rerank score is the sole arbiter: a reranker that inverts
// the RRF order wins. Uses REAL ctx-scale RRF scores (~0.008), NOT 1.0 — the
// Wave-1 bugs hid behind score=1.0 fixtures (W10).
func TestApplyRerankScores_PureFollowsReranker(t *testing.T) {
	results := []SearchResult{res("A", 0.008), res("B", 0.004)}
	// Reranker says B (0.9) >> A (0.2), the opposite of RRF order.
	out := applyRerankScores(results, []float64{0.2, 0.9}, 2, 1.0)
	if got := ids(out); got[0] != "B" || got[1] != "A" {
		t.Errorf("weight=1.0: order = %v, want [B A] (reranker wins)", got)
	}
}

// At weight 0.0 the reranker is ignored: the RRF order survives even though the
// reranker strongly disagrees.
func TestApplyRerankScores_ZeroIgnoresReranker(t *testing.T) {
	results := []SearchResult{res("A", 0.008), res("B", 0.004)}
	out := applyRerankScores(results, []float64{0.2, 0.9}, 2, 0.0)
	if got := ids(out); got[0] != "A" || got[1] != "B" {
		t.Errorf("weight=0.0: order = %v, want [A B] (RRF wins)", got)
	}
}

// Reranked docs (normalized within their window) must sort ABOVE un-reranked
// tail docs that keep their raw ~0.008 RRF scores — i.e. no scale-mix bug
// between the [0,1] blended scores and the tiny absolute RRF tail.
func TestApplyRerankScores_RerankedBeatTail(t *testing.T) {
	results := []SearchResult{
		res("A", 0.008), res("B", 0.006), // reranked (docCount=2)
		res("C", 0.004), res("D", 0.002), // tail
	}
	// Reranker scores A and B LOW in absolute terms (0.01) but they must still
	// beat the tail because they're normalized within the reranked window.
	out := applyRerankScores(results, []float64{0.01, 0.01}, 2, 1.0)
	got := ids(out)
	if top2 := map[string]bool{got[0]: true, got[1]: true}; !top2["A"] || !top2["B"] {
		t.Errorf("order = %v, want reranked A,B above tail C,D", got)
	}
}

// Regression for the Wave-2 cross-scale bug: at blendWeight=1.0 an un-reranked
// tail doc (raw RRF ~0.008) must NEVER outrank a doc the cross-encoder scored —
// even one with a near-zero sigmoid. Pre-fix the tail kept its raw RRF while the
// weak reranked docs normalized to sub-1e-4 and sank below it.
func TestApplyRerankScores_TailNeverBeatsReranked(t *testing.T) {
	results := []SearchResult{
		res("R0", 0.012), res("R1", 0.011), res("R2", 0.010), // reranked window
		res("T0", 0.008), res("T1", 0.006), // un-reranked tail
	}
	// R1, R2 scored very low by the cross-encoder (sigmoid(-11) ~ 1.7e-5).
	rerankScores := []float64{sigmoid(-0.2), sigmoid(-11), sigmoid(-11)}
	out := applyRerankScores(results, rerankScores, 3, 1.0)
	rank := map[string]int{}
	for i, r := range out {
		rank[r.ID] = i
	}
	for _, rr := range []string{"R0", "R1", "R2"} {
		for _, tt := range []string{"T0", "T1"} {
			if rank[rr] > rank[tt] {
				t.Errorf("reranked %s (rank %d) fell below un-reranked tail %s (rank %d): %v",
					rr, rank[rr], tt, rank[tt], ids(out))
			}
		}
	}
}

// Regression for the Wave-2 rounding-collapse bug: two docs with distinct but
// tiny sigmoid scores must keep cross-encoder order, not round to a 4dp tie and
// fall back to RRF order. Input (RRF) order deliberately contradicts CE order.
func TestApplyRerankScores_NoRoundingCollapse(t *testing.T) {
	// Input order B,A,C by RRF. Cross-encoder: C best, then A(-10.5) > B(-11.5).
	results := []SearchResult{res("B", 0.012), res("A", 0.010), res("C", 0.011)}
	rerankScores := []float64{sigmoid(-11.5), sigmoid(-10.5), sigmoid(2.0)}
	out := applyRerankScores(results, rerankScores, 3, 1.0)
	rank := map[string]int{}
	for i, r := range out {
		rank[r.ID] = i
	}
	if rank["C"] != 0 {
		t.Errorf("C (highest CE score) should rank first, got %v", ids(out))
	}
	if rank["A"] > rank["B"] {
		t.Errorf("rounding collapse: A(-10.5) should precede B(-11.5) by CE score, got %v", ids(out))
	}
}

// RerankCrossEncoder end-to-end through a fake sidecar.

// crossEncoderServer maps each incoming document to a logit by substring match
// (default 0), and returns cohere-style results. status != 200 → error body.
func crossEncoderServer(t *testing.T, status int, logits map[string]float64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "boom"})
			return
		}
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		results := make([]map[string]any, len(req.Documents))
		for i, d := range req.Documents {
			score := 0.0
			for key, l := range logits {
				if strings.Contains(d, key) {
					score = l
				}
			}
			results[i] = map[string]any{"index": i, "relevance_score": score}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func rc(id string, score float64, content string) SearchResult {
	return SearchResult{ID: id, Title: id, Content: content, RRFScore: score}
}

// A high-logit doc must rise to the top, a strongly-negative one sink, at
// blendWeight 1.0 (pure cross-encoder). Verifies the full sigmoid+blend+sort
// path AND that raw negative logits do not invert ordering.
func TestRerankCrossEncoder_ReordersByRelevance(t *testing.T) {
	results := []SearchResult{
		rc("A", 0.008, "about apples"),
		rc("B", 0.006, "about bears"),
		rc("C", 0.004, "the EXACT answer"),
	}
	url := crossEncoderServer(t, http.StatusOK, map[string]float64{
		"apples": -2.0, "bears": -6.0, "EXACT": 6.0,
	})
	out, tel, err := RerankCrossEncoder(context.Background(), url, "", "m", "", 50, 1.0, "q", results, rerankTestAdmission(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tel.Wired || tel.WaitMs < 0 || tel.WireDur <= 0 {
		t.Errorf("telemetry = %+v, want wired with wait-free wire span (MW11)", tel)
	}
	if got := ids(out); got[0] != "C" {
		t.Errorf("order = %v, want C first (highest logit)", got)
	}
	for _, r := range out {
		if r.RerankScore == nil {
			t.Errorf("result %s missing RerankScore", r.ID)
		} else if *r.RerankScore <= 0 || *r.RerankScore > 1.0 {
			t.Errorf("result %s RerankScore = %v, want (0,1]", r.ID, *r.RerankScore)
		}
	}
}

// Fail-open: a sidecar error returns the input order UNCHANGED plus an error.
func TestRerankCrossEncoder_FailOpen(t *testing.T) {
	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
	url := crossEncoderServer(t, http.StatusInternalServerError, nil)
	out, _, err := RerankCrossEncoder(context.Background(), url, "", "m", "", 50, 1.0, "q", results, rerankTestAdmission(t))
	if err == nil {
		t.Fatal("expected error from failing sidecar, got nil")
	}
	if got := ids(out); got[0] != "A" || got[1] != "B" || got[2] != "C" {
		t.Errorf("fail-open order = %v, want input order [A B C]", got)
	}
}

// Below RerankMinResults the stage is a no-op (no sidecar call — host is bogus).
func TestRerankCrossEncoder_BelowMin(t *testing.T) {
	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b")}
	out, tel, err := RerankCrossEncoder(context.Background(), "http://unused.invalid", "", "m", "", 50, 1.0, "q", results, llm.Admission{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tel.Wired {
		t.Errorf("telemetry on early-out = %+v, want zero (no lease, no wire)", tel)
	}
	if len(out) != 2 || out[0].ID != "A" {
		t.Errorf("below-min should pass through unchanged, got %v", ids(out))
	}
}
