// Package llm provides an HTTP client for LLM inference.
// Supports two wire protocols via the "protocol" parameter:
//   - "ollama": Ollama-native /api/chat (supports think, num_ctx)
//   - "openai": OpenAI-compatible /v1/chat/completions (works with any provider)
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

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

const (
	ChatTimeout      = 60 * time.Second
	TranslateTimeout = 15 * time.Second
	RerankTimeout    = 30 * time.Second
)

// Message represents a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Options holds sampling parameters.
type Options struct {
	Temperature   float64 `json:"temperature"`
	TopP          float64 `json:"top_p,omitempty"`
	TopK          int     `json:"top_k,omitempty"`
	MinP          float64 `json:"min_p,omitempty"`
	RepeatPenalty float64 `json:"repeat_penalty,omitempty"`
	NumPredict    int     `json:"num_predict,omitempty"`
	NumCtx        int     `json:"num_ctx,omitempty"`
}

// ChatResponse is the unified response from any provider.
// EvalCount is the completion (output) token count.
// PromptTokens is the input token count — 0 when the provider does not report it
// (older Ollama, some OpenAI-compatibles), or when a cached prefill returns 0.
type ChatResponse struct {
	Message      Message
	EvalCount    int
	PromptTokens int
}

// --- Ollama wire format ---.

type ollamaChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Think    *bool     `json:"think,omitempty"`
	Format   string    `json:"format,omitempty"`
	Options  Options   `json:"options"`
}

type ollamaChatResponse struct {
	Message         Message `json:"message"`
	EvalCount       int     `json:"eval_count"`
	PromptEvalCount int     `json:"prompt_eval_count"`
}

// --- OpenAI wire format ---.

type openAIChatRequest struct {
	Model            string             `json:"model"`
	Messages         []Message          `json:"messages"`
	Stream           bool               `json:"stream"`
	MaxTokens        int                `json:"max_tokens,omitempty"`
	Temperature      float64            `json:"temperature"`
	FrequencyPenalty float64            `json:"frequency_penalty,omitempty"`
	ResponseFormat   *respFormat        `json:"response_format,omitempty"`
	Reasoning        *reasoningOption   `json:"reasoning,omitempty"`
}

// reasoningOption — OpenRouter-extension to disable thinking/reasoning models
// from emitting chain-of-thought traces. When think=false: enabled=false,
// exclude=true → content-only output, fast, no reasoning tokens billed.
type reasoningOption struct {
	Enabled bool `json:"enabled"`
	Exclude bool `json:"exclude"`
}

type respFormat struct {
	Type string `json:"type"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
		PromptTokens     int `json:"prompt_tokens"`
	} `json:"usage"`
}

// Chat sends a non-streaming chat request.
func Chat(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts Options, timeout time.Duration) (*ChatResponse, error) {
	return chatDispatch(ctx, host, apiKey, model, think, systemPrompt, userPrompt, opts, "", timeout)
}

// ChatJSON sends a non-streaming chat request with JSON-mode enabled.
func ChatJSON(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts Options, timeout time.Duration) (*ChatResponse, error) {
	return chatDispatch(ctx, host, apiKey, model, think, systemPrompt, userPrompt, opts, "json", timeout)
}

// Protocol is resolved from the host URL by the caller (config layer).
// The protocol parameter is passed through the apiKey field as "protocol:key"
// or detected from package-level config. For simplicity, we use a package var.

// DefaultProtocol is the wire protocol. Set by main() from config.
// Per-pipeline protocol is passed via the ChatWithProtocol/EmbedWithProtocol functions.
var DefaultProtocol = "ollama"

// ChatWithProtocol sends a chat request using the specified protocol.
func ChatWithProtocol(ctx context.Context, protocol, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts Options, timeout time.Duration) (*ChatResponse, error) {
	return chatWithFormat(ctx, protocol, host, apiKey, model, think, systemPrompt, userPrompt, opts, "", timeout)
}

// ChatJSONWithProtocol sends a JSON-mode chat request using the specified protocol.
func ChatJSONWithProtocol(ctx context.Context, protocol, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts Options, timeout time.Duration) (*ChatResponse, error) {
	return chatWithFormat(ctx, protocol, host, apiKey, model, think, systemPrompt, userPrompt, opts, "json", timeout)
}

func chatDispatch(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts Options, format string, timeout time.Duration) (*ChatResponse, error) {
	return chatWithFormat(ctx, DefaultProtocol, host, apiKey, model, think, systemPrompt, userPrompt, opts, format, timeout)
}

func chatWithFormat(ctx context.Context, protocol, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts Options, format string, timeout time.Duration) (*ChatResponse, error) {
	if protocol == "openai" {
		return chatOpenAI(ctx, host, apiKey, model, think, systemPrompt, userPrompt, opts, format, timeout)
	}
	return chatOllama(ctx, host, apiKey, model, think, systemPrompt, userPrompt, opts, format, timeout)
}

func chatOllama(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts Options, format string, timeout time.Duration) (*ChatResponse, error) {
	reqBody := ollamaChatRequest{
		Model: model,
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream:  false,
		Think:   think,
		Format:  format,
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
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llm: unexpected status %d: %s", resp.StatusCode, string(errBody))
	}

	var result ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}

	return &ChatResponse{
		Message:      result.Message,
		EvalCount:    result.EvalCount,
		PromptTokens: result.PromptEvalCount,
	}, nil
}

func chatOpenAI(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts Options, format string, timeout time.Duration) (*ChatResponse, error) {
	reqBody := openAIChatRequest{
		Model: model,
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream:      false,
		MaxTokens:   opts.NumPredict,
		Temperature: opts.Temperature,
	}
	if opts.RepeatPenalty > 0 {
		reqBody.FrequencyPenalty = opts.RepeatPenalty - 1.0
	}
	if format == "json" {
		reqBody.ResponseFormat = &respFormat{Type: "json_object"}
	}
	// OpenRouter no-think: when think=false, disable reasoning + exclude reasoning
	// tokens from response. Saves cost (reasoning tokens billed at completion rate)
	// and avoids JSON-parse-errors when the model emits CoT in the reasoning field
	// while leaving content empty/truncated.
	if think != nil && !*think {
		reqBody.Reasoning = &reasoningOption{Enabled: false, Exclude: true}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llm: unexpected status %d: %s", resp.StatusCode, string(errBody))
	}

	var result openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("llm: empty choices in response")
	}

	choice := result.Choices[0].Message
	content := choice.Content
	if content == "" && choice.Reasoning != "" {
		content = choice.Reasoning
	}

	return &ChatResponse{
		Message:      Message{Role: choice.Role, Content: content},
		EvalCount:    result.Usage.CompletionTokens,
		PromptTokens: result.Usage.PromptTokens,
	}, nil
}

// SynthesisOptions returns default options for LLM synthesis.
func SynthesisOptions() Options {
	return Options{
		Temperature:   0.1,
		RepeatPenalty: 1.1,
		NumPredict:    500,
	}
}

// TranslateOptions returns default options for query translation.
func TranslateOptions() Options {
	return Options{
		Temperature: 0,
		NumPredict:  100,
	}
}

// RerankOptions returns default options for reranking.
func RerankOptions() Options {
	return Options{
		Temperature: 0,
		NumPredict:  80,
	}
}
