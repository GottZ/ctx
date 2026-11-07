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
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/httpx"
)

// httpClients is this family's plain/guarded pool pair; every wire call picks
// its client with httpClients.For(b.EgressGuarded()) — tenant rows dial
// through ssrfguard (GHSA 2026-10-31 §3), operator rows do not.
var httpClients = httpx.NewClients()

// Timeout caps a single rerank round-trip. A CPU cross-encoder costs roughly
// ~1s per query+document pair, so a full window (MAX_DOCS) runs tens of seconds;
// query latency is deliberately not a constraint here. This is purely a
// fail-open safety net (a genuinely stuck/oversized request degrades to the
// pre-rerank order) — set well above the worst-case full-window time.
const Timeout = 180 * time.Second

// Score domains — the caller passes the backend's configured domain
// (backends.Backend.ScoreDomain()); the strings are duplicated here so
// this low-level wire package does not import the pool model.
//
//	"auto" / "":  container-coupled — "results" entries pass verbatim
//	              (raw logits), "data" entries are calibrated [0,1]
//	              probabilities and get the logit mapping.
//	"logit":      every entry passes verbatim, both containers.
//	"probability": every entry is validated to [0,1] and logit-mapped,
//	              both containers.
const (
	DomainAuto        = "auto"
	DomainLogit       = "logit"
	DomainProbability = "probability"
)

// Score returns one relevance score per document, in the SAME order as docs
// (scores[i] is the score for docs[i]). Scores are raw cross-encoder logits
// (higher = more relevant; may be negative); scoreDomain (see the Domain
// constants) decides whether wire values are already logits or calibrated
// probabilities that need the logit mapping. promptTokens is the sidecar's
// reported usage.prompt_tokens (MW5: rerank charges prompt tokens into the
// dispatch usage meter); 0 when the server reports none.
//
// Returns an error on transport failure, non-200, malformed JSON, or any
// index the server returns that does not cleanly cover the input set — the
// caller fails open and keeps the pre-rerank order.
func Score(ctx context.Context, b backends.Backend, model, query string, docs []string, scoreDomain string) ([]float64, int, error) {
	if len(docs) == 0 {
		return nil, 0, nil
	}

	body, err := wireBody(model, query, docs)
	if err != nil {
		return nil, 0, fmt.Errorf("rerank: body: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, b.Host+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("rerank: create request: %w", err)
	}
	// No row extra_headers on the rerank wire; the hop stamp, then the core.
	hop := b.HopTarget(fedhop.WireRerank)
	fedhop.Stamp(reqCtx, req.Header, hop)
	req.Header.Set("Content-Type", "application/json")
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}

	resp, err := httpx.DoRetryOnce(httpClients.For(b.EgressGuarded()), req, body)
	if err != nil {
		return nil, 0, fmt.Errorf("rerank: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := fedhop.Observe(reqCtx, hop, resp.StatusCode, resp.Header); err != nil {
		return nil, 0, fmt.Errorf("rerank: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, 0, fmt.Errorf("rerank: %w", httpx.NewStatusError(resp, errBody))
	}

	// Buffer the answer (so an unrecognized dialect can echo a snippet of
	// what the server actually sent — the original Voyage incident hid
	// behind a generic "count mismatch" for six production calls because
	// the response was decoded blind), at most answerReadCap bytes: the
	// body's length (room for a document echo), a share per document and a
	// frame — what the gateway's route books for the read (FOLGE-R).
	raw, err := readAnswer(resp.Body, resp.ContentLength, len(docs), len(body))
	if err != nil {
		return nil, 0, fmt.Errorf("rerank: read body: %w", err)
	}
	return decodeAnswer(raw, len(docs), scoreDomain)
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
