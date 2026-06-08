package rerank

import (
	"context"
	"encoding/json"
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

	scores, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"apple", "bear", "exact"})
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
	if _, err := Score(context.Background(), srv.URL, "", "mymodel", "myquery", []string{"d0", "d1", "d2"}); err != nil {
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
	if _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b"}); err == nil {
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
	if _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b"}); err == nil {
		t.Fatal("expected error for duplicate index, got nil")
	}
}

func TestScore_RejectsCountMismatch(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		// Only one result for two docs (e.g. server truncated to top_n).
		return http.StatusOK, okResults(map[string]any{"index": 0, "relevance_score": 1.0})
	})
	if _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b"}); err == nil {
		t.Fatal("expected error for count mismatch, got nil")
	}
}

func TestScore_RejectsNon200(t *testing.T) {
	srv := rerankServer(t, func(_ rerankRequest) (int, any) {
		return http.StatusInternalServerError, map[string]any{"error": "boom"}
	})
	if _, err := Score(context.Background(), srv.URL, "", "m", "q", []string{"a", "b", "c"}); err == nil {
		t.Fatal("expected error for non-200, got nil")
	}
}

func TestScore_EmptyDocs(t *testing.T) {
	scores, err := Score(context.Background(), "http://unused.invalid", "", "m", "q", nil)
	if err != nil || scores != nil {
		t.Errorf("empty docs: got (%v, %v), want (nil, nil)", scores, err)
	}
}
