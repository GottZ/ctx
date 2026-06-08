// Package rerank scores query/document relevance via a local cross-encoder.
//
// It speaks the cohere-style /v1/rerank wire format — what a llama.cpp server
// started with --reranking --pooling rank exposes (aliases /rerank, /v1/rerank,
// /reranking, /v1/reranking are all identical). The server runs ctx's chosen
// reference model (bge-reranker-v2-m3, an XLM-RoBERTa sequence-classification
// cross-encoder) on CPU and returns one relevance score per document.
//
// Two server behaviours are load-bearing and handled here:
//   - The response "results" array is sorted DESCENDING by score and may be
//     truncated to top_n, so each entry carries its original "index". Score()
//     re-aligns STRICTLY by that index back into input order — never by array
//     position (llama.cpp #9510, server-common.cpp format_response_rerank).
//   - relevance_score is a RAW LOGIT (can be negative, e.g. -11..+6), NOT a
//     0-1 probability. Score() returns it verbatim; the caller decides whether
//     to sigmoid / normalize (the rrf layer does, see RerankCrossEncoder).
//
// Source: https://github.com/GottZ/ctx
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// Timeout caps a single rerank round-trip. A CPU cross-encoder costs roughly
// ~1s per query+document pair, so a full window (MAX_DOCS) runs tens of seconds;
// query latency is deliberately not a constraint here. This is purely a
// fail-open safety net (a genuinely stuck/oversized request degrades to the
// pre-rerank order) — set well above the worst-case full-window time.
const Timeout = 180 * time.Second

// rerankRequest is the cohere/llama.cpp request body. Sending "documents"
// (not "texts") + "query" selects the Jina/cohere response path, whose score
// field is "relevance_score". top_n is omitted so the server returns one
// result per document (still score-sorted — see Score's re-alignment).
type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type rerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

type rerankResponse struct {
	Results []rerankResult `json:"results"`
}

// Score returns one relevance score per document, in the SAME order as docs
// (scores[i] is the score for docs[i]). Scores are raw cross-encoder logits
// (higher = more relevant; may be negative).
//
// Returns an error on transport failure, non-200, malformed JSON, or any
// index the server returns that does not cleanly cover the input set — the
// caller fails open and keeps the pre-rerank order.
func Score(ctx context.Context, host, apiKey, model, query string, docs []string) ([]float64, error) {
	if len(docs) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(rerankRequest{Model: model, Query: query, Documents: docs})
	if err != nil {
		return nil, fmt.Errorf("rerank: marshal: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, host+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rerank: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("rerank: unexpected status %d: %s", resp.StatusCode, string(errBody))
	}

	var result rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("rerank: decode: %w", err)
	}

	// Re-align strictly by result.Index into input order. The server sorts
	// results by score descending (and may resize to top_n), so array position
	// is meaningless — only Index maps a score back to its document. Validate
	// the indices cover [0,len) exactly once: any gap, dup, or out-of-range
	// entry means we cannot trust the mapping, so error → caller fails open.
	scores := make([]float64, len(docs))
	seen := make([]bool, len(docs))
	got := 0
	for _, r := range result.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			return nil, fmt.Errorf("rerank: result index %d out of range [0,%d)", r.Index, len(docs))
		}
		if seen[r.Index] {
			return nil, fmt.Errorf("rerank: duplicate result index %d", r.Index)
		}
		seen[r.Index] = true
		scores[r.Index] = r.RelevanceScore
		got++
	}
	if got != len(docs) {
		return nil, fmt.Errorf("rerank: result count mismatch: got %d scores for %d documents", got, len(docs))
	}

	return scores, nil
}
