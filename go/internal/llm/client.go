// Package llm provides an HTTP client for Ollama's /api/chat endpoint.
// Used for LLM synthesis in the context-agent query pipeline.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpClient is a package-level HTTP client with connection pooling for Ollama requests.
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

const (
	// ChatTimeout is the HTTP timeout for synthesis requests.
	ChatTimeout = 60 * time.Second
	// TranslateTimeout is the HTTP timeout for translation requests.
	TranslateTimeout = 15 * time.Second
	// RerankTimeout is the HTTP timeout for reranking requests.
	// Ollama needs time for model loading/scheduling; 15s too short for cold starts.
	RerankTimeout = 30 * time.Second
)

// Message represents a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Options holds Ollama sampling parameters.
type Options struct {
	Temperature   float64 `json:"temperature"`
	RepeatPenalty float64 `json:"repeat_penalty,omitempty"`
	NumPredict    int     `json:"num_predict,omitempty"`
}

// ChatRequest is the Ollama /api/chat request body.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Think    *bool     `json:"think,omitempty"`
	Options  Options   `json:"options"`
}

// ChatResponse is the Ollama /api/chat response body.
type ChatResponse struct {
	Message   Message `json:"message"`
	EvalCount int     `json:"eval_count"`
}

// Chat sends a non-streaming chat request to Ollama and returns the response content.
// The timeout parameter controls the HTTP client timeout.
func Chat(ctx context.Context, host, model, systemPrompt, userPrompt string, opts Options, timeout time.Duration) (*ChatResponse, error) {
	think := false
	reqBody := ChatRequest{
		Model: model,
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream:  false,
		Think:   &think,
		Options: opts,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llm: unexpected status %d: %s", resp.StatusCode, string(errBody))
	}

	var result ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}

	return &result, nil
}

// SynthesisOptions returns the default Ollama options for LLM synthesis.
func SynthesisOptions() Options {
	return Options{
		Temperature:   0.1,
		RepeatPenalty: 1.1,
		NumPredict:    500,
	}
}

// TranslateOptions returns the default Ollama options for query translation.
func TranslateOptions() Options {
	return Options{
		Temperature: 0,
		NumPredict:  100,
	}
}

// RerankOptions returns the default Ollama options for reranking.
func RerankOptions() Options {
	return Options{
		Temperature: 0,
		NumPredict:  80,
	}
}
