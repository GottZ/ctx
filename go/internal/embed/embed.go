// Package embed handles text embedding generation.
// Supports two wire protocols:
//   - "ollama": /api/embed (supports num_ctx via options)
//   - "openai": /v1/embeddings (standard, works with any provider)
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

const (
	TargetDims   = 1024
	minNorm      = 0.99
	maxNorm      = 1.01
	minVariance  = 0.0001
)

// Prefix determines the asymmetric instruction prefix for embeddings.
type Prefix int

const (
	PrefixQuery    Prefix = iota
	PrefixDocument
)

func (p Prefix) text() string {
	switch p {
	case PrefixQuery:
		return "Instruct: Represent the query for retrieving relevant documents\nContent: "
	case PrefixDocument:
		return "Instruct: Represent the document for retrieval\nContent: "
	default:
		return ""
	}
}

// --- Ollama wire format ---.

type ollamaEmbedRequest struct {
	Model   string       `json:"model"`
	Input   string       `json:"input"`
	Options *ollamaOpts  `json:"options,omitempty"`
}

type ollamaOpts struct {
	NumCtx int `json:"num_ctx,omitempty"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
	// PromptEvalCount is ollama's token count of the embedded input — the
	// MW22/C1 charge dimension of an embed wire call (prompt side only).
	// Absent field ⇒ 0 ⇒ the call site charges nothing (uncharged, never an
	// estimate).
	PromptEvalCount int `json:"prompt_eval_count"`
}

// --- OpenAI wire format ---.

type openAIEmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	// Usage carries the standard OpenAI-wire token accounting (same charge
	// role as ollama's prompt_eval_count above). Voyage AI reports
	// total_tokens instead of prompt_tokens on the embed endpoint; the
	// fallback in embedOpenAI handles both wire formats.
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// Embed generates an embedding via the backend tuple, truncates to 1024d,
// and L2-normalizes. Host and Protocol arrive as one backends.Backend value
// (F1-W5) — the wire path (/api/embed vs /v1/embeddings) can never diverge
// from the host it was configured with. Any protocol other than "openai"
// takes the ollama path (pre-W5 dispatch semantics, incl. empty).
// promptTokens is the backend-reported input token count (MW5: embeds charge
// prompt tokens into the dispatch usage meter); 0 when the backend reports
// none — the call site then charges nothing instead of estimating (C1).
func Embed(ctx context.Context, b backends.Backend, text string, prefix Prefix) (vec []float32, promptTokens int, err error) {
	input := prefix.text() + text

	var raw []float64

	if b.Protocol == backends.ProtocolOpenAI {
		raw, promptTokens, err = embedOpenAI(ctx, b.Host, b.APIKey, b.Model, input)
	} else {
		raw, promptTokens, err = embedOllama(ctx, b.Host, b.APIKey, b.Model, input, b.NumCtx)
	}
	if err != nil {
		return nil, 0, err
	}

	if len(raw) < TargetDims {
		return nil, 0, fmt.Errorf("embed: embedding too short: got %d, need >= %d", len(raw), TargetDims)
	}

	vec = l2Normalize(raw[:TargetDims])

	if err := qualityGate(vec); err != nil {
		return nil, 0, err
	}

	return vec, promptTokens, nil
}

func embedOllama(ctx context.Context, host, apiKey, model, input string, numCtx int) ([]float64, int, error) {
	reqBody := ollamaEmbedRequest{Model: model, Input: input}
	if numCtx > 0 {
		reqBody.Options = &ollamaOpts{NumCtx: numCtx}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: marshal: %w", err)
	}

	// No artificial deadline: embed-call latency is data-dependent (chunk-size,
	// queue-depth on shared GPU at high parallelism). Parent ctx provides the
	// real timeout (cycle-timeout for backfill, request-timeout for query path).
	// Was: WithTimeout(ctx, 30s) — fired under parallel=16 pressure (Welle-49).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("embed: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpx.DoRetryOnce(httpClient, req, body)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, 0, fmt.Errorf("embed: %w", httpx.NewStatusError(resp, errBody))
	}

	var result ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("embed: decode: %w", err)
	}

	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return nil, 0, fmt.Errorf("embed: empty embedding returned")
	}

	return result.Embeddings[0], result.PromptEvalCount, nil
}

func embedOpenAI(ctx context.Context, host, apiKey, model, input string) ([]float64, int, error) {
	reqBody := openAIEmbedRequest{Model: model, Input: input}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: marshal: %w", err)
	}

	// No artificial deadline (see Ollama-path note above).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("embed: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpx.DoRetryOnce(httpClient, req, body)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, 0, fmt.Errorf("embed: %w", httpx.NewStatusError(resp, errBody))
	}

	var result openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("embed: decode: %w", err)
	}

	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, 0, fmt.Errorf("embed: empty embedding returned")
	}

	// Voyage AI reports total_tokens; OpenAI-compatible servers report
	// prompt_tokens. Prefer prompt_tokens; fall back to total_tokens.
	promptTokens := result.Usage.PromptTokens
	if promptTokens == 0 && result.Usage.TotalTokens > 0 {
		promptTokens = result.Usage.TotalTokens
		// This flips the MW22 meter semantics for total_tokens-only
		// backends from "uncharged" to a real measured charge — same
		// one-time visibility as the rerank pendant (4970856): embeds
		// run per query and per backfill batch, per-call INFO would
		// be noise.
		embedTotalTokensFallbackOnce.Do(func() {
			slog.Info("embed: backend reports usage.total_tokens only; charging it as prompt tokens (previously uncharged)")
		})
	}

	return result.Data[0].Embedding, promptTokens, nil
}

var embedTotalTokensFallbackOnce sync.Once

func l2Normalize(v []float64) []float32 {
	var sumSq float64
	for _, x := range v {
		sumSq += x * x
	}
	norm := math.Sqrt(sumSq)
	result := make([]float32, len(v))
	if norm > 0 {
		for i, x := range v {
			result[i] = float32(x / norm)
		}
	}
	return result
}

func qualityGate(vec []float32) error {
	if len(vec) != TargetDims {
		return fmt.Errorf("embed: quality gate: dimension mismatch: got %d, want %d", len(vec), TargetDims)
	}
	var sumSq float64
	for _, x := range vec {
		sumSq += float64(x) * float64(x)
	}
	norm := math.Sqrt(sumSq)
	if norm < minNorm || norm > maxNorm {
		return fmt.Errorf("embed: quality gate: norm %.6f outside [%.2f, %.2f]", norm, minNorm, maxNorm)
	}
	var sum float64
	for _, x := range vec {
		sum += float64(x)
	}
	mean := sum / float64(len(vec))
	var varSum float64
	for _, x := range vec {
		d := float64(x) - mean
		varSum += d * d
	}
	variance := varSum / float64(len(vec))
	if variance < minVariance {
		return fmt.Errorf("embed: quality gate: variance %.8f below minimum %.4f", variance, minVariance)
	}
	return nil
}
