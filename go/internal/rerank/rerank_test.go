package rerank

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rerankServer spins up a fake cohere-style /v1/rerank endpoint. The handler
// receives the decoded request and returns (status, jsonBody).
func rerankServer(t *testing.T, handler func(req rerankRequest) (int, any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req rerankRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		status, body := handler(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okResults(rs ...map[string]any) map[string]any {
	return map[string]any{"model": "m", "object": "list", "results": rs}
}

// THE load-bearing test: the server returns results sorted by score DESCENDING
// (as llama.cpp does), i.e. NOT in input order. Score must re-map each back to
// its document by .index, never by array position.
func TestScore_ReAlignsByIndex(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		// docs in:  [0]="apple" [1]="bear" [2]="exact". Server orders 2,0,1.
		return http.StatusOK, okResults(
			map[string]any{"index": 2, "relevance_score": 5.97},
			map[string]any{"index": 0, "relevance_score": -2.0},
			map[string]any{"index": 1, "relevance_score": -8.41},
		)
	})

	scores, _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"apple", "bear", "exact"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Want INPUT order: doc0, doc1, doc2. Raw logits preserved verbatim (incl. negatives).
	want := []float64{-2.0, -8.41, 5.97}
	if len(scores) != len(want) {
		t.Fatalf("got %d scores, want %d", len(scores), len(want))
	}
	for i := range want {
		if scores[i] != want[i] {
			t.Errorf("scores[%d] = %v, want %v (index re-alignment broken)", i, scores[i], want[i])
		}
	}
}

func TestScore_SendsDocumentsAndQuery(t *testing.T) {
	var got rerankRequest
	srv := rerankServer(t, func(req rerankRequest) (int, any) {
		got = req
		return http.StatusOK, okResults(
			map[string]any{"index": 0, "relevance_score": 1.0},
			map[string]any{"index": 1, "relevance_score": 2.0},
			map[string]any{"index": 2, "relevance_score": 3.0},
		)
	})
	if _, _, err := Score(context.Background(), srv.URL, "", "mymodel", "myquery", []string{"d0", "d1", "d2"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "mymodel" || got.Query != "myquery" || len(got.Documents) != 3 || got.Documents[2] != "d2" {
		t.Errorf("request body = %+v, want model=mymodel query=myquery docs=[d0 d1 d2]", got)
	}
}

func TestScore_RejectsOutOfRangeIndex(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, okResults(
			map[string]any{"index": 0, "relevance_score": 1.0},
			map[string]any{"index": 9, "relevance_score": 2.0}, // out of range for 2 docs
		)
	})
	if _, _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b"}); err == nil {
		t.Fatal("expected error for out-of-range index, got nil")
	}
}

func TestScore_RejectsDuplicateIndex(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, okResults(
			map[string]any{"index": 0, "relevance_score": 1.0},
			map[string]any{"index": 0, "relevance_score": 2.0}, // dup → index 1 never filled
		)
	})
	if _, _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b"}); err == nil {
		t.Fatal("expected error for duplicate index, got nil")
	}
}

func TestScore_RejectsCountMismatch(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		// Only one result for two docs (e.g. server truncated to top_n).
		return http.StatusOK, okResults(map[string]any{"index": 0, "relevance_score": 1.0})
	})
	if _, _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b"}); err == nil {
		t.Fatal("expected error for count mismatch, got nil")
	}
}

func TestScore_RejectsNon200(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusInternalServerError, map[string]any{"error": "boom"}
	})
	if _, _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b", "c"}); err == nil {
		t.Fatal("expected error for non-200, got nil")
	}
}

func TestScore_EmptyDocs(t *testing.T) {
	scores, _, err := Score(context.Background(), "http://unused.invalid", "", "m", "q", nil)
	if err != nil || scores != nil {
		t.Errorf("empty docs: got (%v, %v), want (nil, nil)", scores, err)
	}
}

// Voyage AI format compatibility tests.

// logit is the test-side mirror of probabilityToLogit: the expected wire
// transform for Voyage's calibrated [0,1] scores (no clamping — tests use
// interior probabilities).
func logit(p float64) float64 {
	return math.Log(p / (1 - p))
}

// voyageResults builds a Voyage-style response body: results under "data"
// (OpenAPI RerankingObject schema) with "total_tokens" usage.
func voyageResults(rs ...map[string]any) map[string]any {
	return map[string]any{
		"object": "list",
		"data":   rs,
		"model":  "rerank-2.5",
		"usage":  map[string]any{"total_tokens": 42},
	}
}

// TestScore_VoyageDataFallback is the load-bearing regression test: Voyage AI
// returns results under "data" instead of "results". Before the fix, Score
// decoded an empty Results slice and failed with "got 0 scores for N documents".
func TestScore_VoyageDataFallback(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, voyageResults(
			map[string]any{"index": 0, "relevance_score": 0.95},
			map[string]any{"index": 1, "relevance_score": 0.42},
			map[string]any{"index": 2, "relevance_score": 0.11},
		)
	})

	scores, ptoks, err := Score(context.Background(), srv.URL, "", "rerank-2.5", "q", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Voyage data fallback failed: %v", err)
	}
	// Voyage scores are calibrated [0,1] probabilities; Score() maps them
	// to logits at the wire boundary so its raw-logit return contract
	// holds for every backend (the caller's sigmoid reconstructs p).
	want := []float64{logit(0.95), logit(0.42), logit(0.11)}
	for i := range want {
		if scores[i] != want[i] {
			t.Errorf("scores[%d] = %v, want %v (logit of Voyage probability)", i, scores[i], want[i])
		}
	}
	// Voyage reports total_tokens; Score must surface it as promptTokens.
	if ptoks != 42 {
		t.Errorf("promptTokens = %d, want 42 (total_tokens fallback)", ptoks)
	}
}

// TestScore_VoyageDataReAlignsByIndex verifies index re-alignment works through
// the "data" path too (Voyage sorts descending by score, not input order).
func TestScore_VoyageDataReAlignsByIndex(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		// Server returns sorted DESC: doc2 (best), doc0, doc1 (worst).
		return http.StatusOK, voyageResults(
			map[string]any{"index": 2, "relevance_score": 0.99},
			map[string]any{"index": 0, "relevance_score": 0.50},
			map[string]any{"index": 1, "relevance_score": 0.05},
		)
	})

	scores, _, err := Score(context.Background(), srv.URL, "", "rerank-2.5", "q", []string{"apple", "bear", "exact"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must be re-aligned to INPUT order (as logits of the Voyage
	// probabilities): doc0=0.50, doc1=0.05, doc2=0.99.
	want := []float64{logit(0.50), logit(0.05), logit(0.99)}
	for i := range want {
		if scores[i] != want[i] {
			t.Errorf("scores[%d] = %v, want %v (index re-alignment via data broken)", i, scores[i], want[i])
		}
	}
}

// TestScore_PrefersResultsOverData: when both "results" and "data" are present
// (hypothetical server that sends both), "results" wins (cohere/llama.cpp path).
func TestScore_PrefersResultsOverData(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, map[string]any{
			"object": "list",
			"results": []map[string]any{
				{"index": 0, "relevance_score": 1.0},
				{"index": 1, "relevance_score": 2.0},
			},
			"data": []map[string]any{
				{"index": 0, "relevance_score": 9.0},
				{"index": 1, "relevance_score": 8.0},
			},
			"usage": map[string]any{"prompt_tokens": 10},
		}
	})

	scores, ptoks, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must use "results" values (1.0, 2.0), NOT "data" values (9.0, 8.0).
	if scores[0] != 1.0 || scores[1] != 2.0 {
		t.Errorf("scores = %v, want [1.0 2.0] (results must take precedence over data)", scores)
	}
	if ptoks != 10 {
		t.Errorf("promptTokens = %d, want 10 (prompt_tokens preferred over total_tokens)", ptoks)
	}
}

// TestScore_VoyageTotalTokensFallback: when prompt_tokens is absent but
// total_tokens is present (Voyage), Score returns total_tokens as the usage.
func TestScore_VoyageTotalTokensFallback(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, map[string]any{
			"object": "list",
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.5},
			},
			"usage": map[string]any{"total_tokens": 77},
		}
	})

	_, ptoks, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ptoks != 77 {
		t.Errorf("promptTokens = %d, want 77 (total_tokens fallback)", ptoks)
	}
}

// TestScore_VoyageDataRejectsCountMismatch: the "data" path must enforce the
// same count-mismatch validation as "results" (no silent partial scoring).
func TestScore_VoyageDataRejectsCountMismatch(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		// Only 1 result in "data" for 3 docs.
		return http.StatusOK, voyageResults(
			map[string]any{"index": 0, "relevance_score": 0.9},
		)
	})

	if _, _, err := Score(context.Background(), srv.URL, "", "rerank-2.5", "q", []string{"a", "b", "c"}); err == nil {
		t.Fatal("expected count mismatch error via data path, got nil")
	}
}

// TestScore_VoyageDataRejectsDuplicateIndex: duplicate index validation applies
// through the "data" fallback path too.
func TestScore_VoyageDataRejectsDuplicateIndex(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, voyageResults(
			map[string]any{"index": 0, "relevance_score": 0.9},
			map[string]any{"index": 0, "relevance_score": 0.8}, // dup
		)
	})

	if _, _, err := Score(context.Background(), srv.URL, "", "rerank-2.5", "q", []string{"a", "b"}); err == nil {
		t.Fatal("expected duplicate index error via data path, got nil")
	}
}

// TestScore_NeitherResultsNorData: a response with neither "results" nor "data"
// must fail with count mismatch (got 0), not silently succeed.
func TestScore_NeitherResultsNorData(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, map[string]any{"object": "list", "model": "m"}
	})

	if _, _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a"}); err == nil {
		t.Fatal("expected error when neither results nor data present, got nil")
	}
}

// TestScore_VoyageLogitRoundTrip pins the cross-backend score contract: the
// caller's sigmoid (rrf.RerankCrossEncoder) applied to what Score() returns
// for a Voyage "data" response must reconstruct the original probability.
// This is the regression fence against double-sigmoiding calibrated scores,
// which compresses the rerank signal into [0.5,0.73] and lets RRF outvote
// the reranker at blend_weight < 1.
func TestScore_VoyageLogitRoundTrip(t *testing.T) {
	probs := []float64{0.05, 0.10, 0.20, 0.98}
	rs := make([]map[string]any, len(probs))
	for i, p := range probs {
		rs[i] = map[string]any{"index": i, "relevance_score": p}
	}
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, voyageResults(rs...)
	})

	scores, _, err := Score(context.Background(), srv.URL, "", "rerank-2.5", "q", []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, p := range probs {
		got := 1.0 / (1.0 + math.Exp(-scores[i]))
		if math.Abs(got-p) > 1e-9 {
			t.Errorf("sigmoid(scores[%d]) = %v, want %v (logit round-trip broken)", i, got, p)
		}
	}
}

// TestScore_VoyageEndpointScoresStayFinite: exact 0 and 1 are legitimately
// producible by a quantized probability; they must clamp to large finite
// logits, never ±Inf (which would poison the downstream blend arithmetic).
func TestScore_VoyageEndpointScoresStayFinite(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, voyageResults(
			map[string]any{"index": 0, "relevance_score": 0.0},
			map[string]any{"index": 1, "relevance_score": 1.0},
		)
	})

	scores, _, err := Score(context.Background(), srv.URL, "", "rerank-2.5", "q", []string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.IsInf(scores[0], 0) || math.IsInf(scores[1], 0) || math.IsNaN(scores[0]) || math.IsNaN(scores[1]) {
		t.Errorf("endpoint probabilities must clamp to finite logits, got %v", scores)
	}
	if scores[0] >= 0 || scores[1] <= 0 {
		t.Errorf("clamped endpoint logits lost their sign: got %v, want [negative, positive]", scores)
	}
}

// TestScore_VoyageDataRejectsOutOfRangeScore: a "data" entry outside [0,1]
// breaks the documented Voyage schema — Score must error (caller fails open)
// rather than feed a bogus value through the probability→logit transform.
func TestScore_VoyageDataRejectsOutOfRangeScore(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusOK, voyageResults(
			map[string]any{"index": 0, "relevance_score": 4.2},
		)
	})

	if _, _, err := Score(context.Background(), srv.URL, "", "rerank-2.5", "q", []string{"a"}); err == nil {
		t.Fatal("expected error for data score outside [0,1], got nil")
	}
}
