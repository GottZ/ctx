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
	"math"
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

const (
	TargetDims   = 1024
	embedTimeout = 30 * time.Second
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

// DefaultProtocol is the wire protocol for embeddings. Set from config.
var DefaultProtocol = "ollama"

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
}

// Embed generates an embedding, truncates to 1024d, and L2-normalizes.
func Embed(ctx context.Context, host, apiKey, model, text string, prefix Prefix, numCtx int) ([]float32, error) {
	return EmbedWithProtocol(ctx, DefaultProtocol, host, apiKey, model, text, prefix, numCtx)
}

// EmbedWithProtocol generates an embedding using the specified protocol.
func EmbedWithProtocol(ctx context.Context, protocol, host, apiKey, model, text string, prefix Prefix, numCtx int) ([]float32, error) {
	input := prefix.text() + text

	var raw []float64
	var err error

	if protocol == "openai" {
		raw, err = embedOpenAI(ctx, host, apiKey, model, input)
	} else {
		raw, err = embedOllama(ctx, host, apiKey, model, input, numCtx)
	}
	if err != nil {
		return nil, err
	}

	if len(raw) < TargetDims {
		return nil, fmt.Errorf("embed: embedding too short: got %d, need >= %d", len(raw), TargetDims)
	}

	vec := l2Normalize(raw[:TargetDims])

	if err := qualityGate(vec); err != nil {
		return nil, err
	}

	return vec, nil
}

func embedOllama(ctx context.Context, host, apiKey, model, input string, numCtx int) ([]float64, error) {
	reqBody := ollamaEmbedRequest{Model: model, Input: input}
	if numCtx > 0 {
		reqBody.Options = &ollamaOpts{NumCtx: numCtx}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
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

	var result ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}

	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embed: empty embedding returned")
	}

	return result.Embeddings[0], nil
}

func embedOpenAI(ctx context.Context, host, apiKey, model, input string) ([]float64, error) {
	reqBody := openAIEmbedRequest{Model: model, Input: input}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/v1/embeddings", bytes.NewReader(body))
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

	var result openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}

	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embed: empty embedding returned")
	}

	return result.Data[0].Embedding, nil
}

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
