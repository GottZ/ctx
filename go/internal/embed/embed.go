// Package embed handles text embedding generation via the Ollama API.
// Uses Qwen3-Embedding-8B with Matryoshka truncation to 1024 dimensions.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// httpClient is a package-level HTTP client with connection pooling for Ollama embedding requests.
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

const (
	// TargetDims is the Matryoshka-truncated output dimensionality.
	TargetDims = 1024

	// embedTimeout is the HTTP timeout for embedding requests.
	embedTimeout = 30 * time.Second

	// Quality gate thresholds.
	minNorm     = 0.99
	maxNorm     = 1.01
	minVariance = 0.0001
)

// Prefix determines the asymmetric instruction prefix for embeddings.
type Prefix int

const (
	// PrefixQuery is used when embedding a search query.
	PrefixQuery Prefix = iota
	// PrefixDocument is used when embedding a document for storage.
	PrefixDocument
)

// prefixText maps a Prefix to its Ollama instruction string.
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

// EmbedOptions holds Ollama options for embedding requests.
type EmbedOptions struct {
	NumCtx int `json:"num_ctx,omitempty"` // 0 = model default
}

// EmbedRequest is the Ollama /api/embed request body.
type EmbedRequest struct {
	Model   string        `json:"model"`
	Input   string        `json:"input"`
	Options *EmbedOptions `json:"options,omitempty"`
}

// EmbedResponse is the Ollama /api/embed response body.
type EmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// Embed generates an embedding via Ollama, truncates to 1024d, and L2-normalizes.
// The prefix parameter controls the asymmetric instruction prefix (query vs document).
// numCtx controls the context size (0 = model default).
// apiKey is optional: if non-empty, sets Authorization Bearer header (for cloud providers).
func Embed(ctx context.Context, host, apiKey, model, text string, prefix Prefix, numCtx int) ([]float32, error) {
	input := prefix.text() + text

	reqBody := EmbedRequest{
		Model: model,
		Input: input,
	}
	if numCtx > 0 {
		reqBody.Options = &EmbedOptions{NumCtx: numCtx}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("embed: unexpected status %d: %s", resp.StatusCode, string(errBody))
	}

	var result EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}

	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embed: empty embedding returned")
	}

	raw := result.Embeddings[0]

	// Matryoshka truncation: take first 1024 dimensions.
	if len(raw) < TargetDims {
		return nil, fmt.Errorf("embed: embedding too short: got %d, need >= %d", len(raw), TargetDims)
	}
	truncated := raw[:TargetDims]

	// L2 normalization.
	vec := l2Normalize(truncated)

	// Quality gate.
	if err := qualityGate(vec); err != nil {
		return nil, err
	}

	return vec, nil
}

// l2Normalize normalizes a float64 slice to unit length and converts to float32.
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

// qualityGate validates the embedding meets quality thresholds.
func qualityGate(vec []float32) error {
	if len(vec) != TargetDims {
		return fmt.Errorf("embed: quality gate: dimension mismatch: got %d, want %d", len(vec), TargetDims)
	}

	// Compute norm.
	var sumSq float64
	for _, x := range vec {
		sumSq += float64(x) * float64(x)
	}
	norm := math.Sqrt(sumSq)

	if norm < minNorm || norm > maxNorm {
		return fmt.Errorf("embed: quality gate: norm %.6f outside [%.2f, %.2f]", norm, minNorm, maxNorm)
	}

	// Compute variance to detect zero/constant vectors.
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
		return fmt.Errorf("embed: quality gate: variance %.8f below minimum %.4f (constant/zero vector)", variance, minVariance)
	}

	return nil
}
