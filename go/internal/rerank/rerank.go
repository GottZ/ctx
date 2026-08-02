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
// Voyage AI (rerank-2.5/-lite) is supported as an alternative backend. Its
// wire format differs in two coupled ways: results live under "data" (OpenAPI
// RerankingObject) and relevance_score is a calibrated [0,1] relevance — not
// a logit. Score() maps those probabilities through the logit function at the
// wire boundary so the return contract above (raw logits) holds for every
// backend and the caller's sigmoid reconstructs the original probability.
//
// Source: https://github.com/GottZ/ctx
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
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
	// Voyage AI returns results under "data" instead of "results"
	// (OpenAPI RerankingObject schema). If Results is empty after
	// decode, fall back to Data.
	Data []rerankResult `json:"data"`
	// Usage is llama.cpp's Jina-style token accounting on the rerank
	// response. prompt_tokens covers ALL query+document pairs of the request
	// — the MW22/C1 charge dimension of the sidecar call. Absent field ⇒ 0 ⇒
	// the call site charges nothing (uncharged, never an estimate).
	// Voyage reports total_tokens instead of prompt_tokens.
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// Score returns one relevance score per document, in the SAME order as docs
// (scores[i] is the score for docs[i]). Scores are raw cross-encoder logits
// (higher = more relevant; may be negative). promptTokens is the sidecar's
// reported usage.prompt_tokens (MW5: rerank charges prompt tokens into the
// dispatch usage meter); 0 when the server reports none.
//
// Returns an error on transport failure, non-200, malformed JSON, or any
// index the server returns that does not cleanly cover the input set — the
// caller fails open and keeps the pre-rerank order.
func Score(ctx context.Context, host, apiKey, model, query string, docs []string) ([]float64, int, error) {
	if len(docs) == 0 {
		return nil, 0, nil
	}

	body, err := json.Marshal(rerankRequest{Model: model, Query: query, Documents: docs})
	if err != nil {
		return nil, 0, fmt.Errorf("rerank: marshal: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, host+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("rerank: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpx.DoRetryOnce(httpClient, req, body)
	if err != nil {
		return nil, 0, fmt.Errorf("rerank: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, 0, fmt.Errorf("rerank: %w", httpx.NewStatusError(resp, errBody))
	}

	var result rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("rerank: decode: %w", err)
	}

	// Voyage AI returns results under "data" (OpenAPI RerankingObject),
	// while cohere/llama.cpp uses "results". Prefer "results"; fall back
	// to "data" when "results" is absent. The container name is coupled to
	// the score domain: "data" backends (Voyage) report calibrated [0,1]
	// relevance, "results" backends (cohere/llama.cpp) report raw logits.
	entries := result.Results
	fromData := false
	if len(entries) == 0 && len(result.Data) > 0 {
		entries = result.Data
		fromData = true
	}

	// Re-align strictly by result.Index into input order. The server sorts
	// results by score descending (and may resize to top_n), so array position
	// is meaningless — only Index maps a score back to its document. Validate
	// the indices cover [0,len) exactly once: any gap, dup, or out-of-range
	// entry means we cannot trust the mapping, so error → caller fails open.
	scores := make([]float64, len(docs))
	seen := make([]bool, len(docs))
	got := 0
	for _, r := range entries {
		if r.Index < 0 || r.Index >= len(docs) {
			return nil, 0, fmt.Errorf("rerank: result index %d out of range [0,%d)", r.Index, len(docs))
		}
		if seen[r.Index] {
			return nil, 0, fmt.Errorf("rerank: duplicate result index %d", r.Index)
		}
		seen[r.Index] = true
		score := r.RelevanceScore
		if fromData {
			// Voyage's calibrated [0,1] relevance would be sigmoided a
			// second time by the caller (rrf treats every score as a
			// logit), compressing the signal into [0.5,0.73] and letting
			// RRF outvote the reranker at blend_weight < 1. Map the
			// probability to its logit here so the downstream sigmoid
			// reconstructs it exactly. A score outside [0,1] breaks the
			// documented Voyage schema — error → caller fails open.
			if score < 0 || score > 1 {
				return nil, 0, fmt.Errorf("rerank: data score %g at index %d outside [0,1] (voyage schema violation)", score, r.Index)
			}
			score = probabilityToLogit(score)
		}
		scores[r.Index] = score
		got++
	}
	if got != len(docs) {
		return nil, 0, fmt.Errorf("rerank: result count mismatch: got %d scores for %d documents", got, len(docs))
	}

	// Voyage reports total_tokens; llama.cpp reports prompt_tokens.
	promptTokens := result.Usage.PromptTokens
	if promptTokens == 0 && result.Usage.TotalTokens > 0 {
		promptTokens = result.Usage.TotalTokens
	}

	return scores, promptTokens, nil
}

// probabilityToLogit maps a calibrated probability to its logit,
// logit(p) = ln(p/(1-p)), the exact inverse of the caller's sigmoid.
// p is clamped away from the exact endpoints so 0 and 1 (legitimately
// producible by a quantized probability) yield large finite logits
// instead of ±Inf, which would poison the downstream blend arithmetic.
func probabilityToLogit(p float64) float64 {
	const eps = 1e-7
	if p < eps {
		p = eps
	}
	if p > 1-eps {
		p = 1 - eps
	}
	return math.Log(p / (1 - p))
}
