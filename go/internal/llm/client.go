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
	Temperature     float64 `json:"temperature"`
	TopP            float64 `json:"top_p,omitempty"`
	TopK            int     `json:"top_k,omitempty"`
	MinP            float64 `json:"min_p,omitempty"`
	RepeatPenalty   float64 `json:"repeat_penalty,omitempty"`
	PresencePenalty float64 `json:"presence_penalty,omitempty"`
	NumPredict      int     `json:"num_predict,omitempty"`
	NumCtx          int     `json:"num_ctx,omitempty"`
	// NumPredictScale multiplies NumPredict once the chain has RESOLVED it —
	// after a model_map num_predict/max_tokens override, inside
	// applyModelParams, which is the only point where the effective cap of an
	// attempt is known. A caller-side doubling would be a no-op on precisely
	// the rows that override the cap. Values <= 1 (the zero value included)
	// change nothing, so every existing call site is untouched.
	//
	// json:"-" is load-bearing: this whole struct is marshalled verbatim as
	// the Ollama request's "options" object (see ollamaChatRequest), and a
	// scale factor is a ctx-internal instruction, not a sampling parameter any
	// backend knows.
	NumPredictScale float64 `json:"-"`
}

// ChatResponse is the unified response from any provider.
// EvalCount is the completion (output) token count.
// PromptTokens is the input token count — 0 when the provider does not report it
// (older Ollama, some OpenAI-compatibles), or when a cached prefill returns 0.
//
// The three provider fields are filled for provider_class=openrouter only
// (G29): CostUSD from usage.cost (response BODY, not headers — inventory A8),
// ServedModel from the top-level model (the model that ACTUALLY answered;
// OpenRouter's models-fallback can differ from the request), and
// ProviderRequestID from the response id (async audit via
// GET /api/v1/generation?id=…). Local backends leave all three zero —
// llmlog's cost_usd stays NULL by construction.
//
// FinishReason is the non-stream twin of StreamResult.FinishReason
// (stream.go): the provider's own stop reason for this completion, decoded
// from done_reason (Ollama) resp. choices[0].finish_reason (OpenAI). Observed
// values are stop | length | tool_calls | content_filter; "" when the provider
// reports none — several OpenAI-compatible servers omit the field entirely, so
// an empty value is "unknown", never "stop". The value stays RAW: the two
// vocabularies are not normalized or translated into one another here, because
// a consumer that wants to distinguish providers can only do so on what the
// provider actually said. length is the output-cap signal the dream pipeline
// reads to tell a truncated answer from a malformed one.
type ChatResponse struct {
	Message           Message
	EvalCount         int
	PromptTokens      int
	FinishReason      string
	CostUSD           *float64
	ServedModel       string
	ProviderRequestID string
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
	// DoneReason is Ollama's stop reason on the final (non-streaming) object:
	// "stop" on a natural end, "length" when num_predict cut the generation.
	// Absent on older Ollama builds and on /api/chat lookalikes — the zero
	// value then travels as the documented "unknown".
	DoneReason string `json:"done_reason"`
}

// --- OpenAI wire format ---.

// openAIChatRequest: top_p/top_k/min_p/presence_penalty pass through since
// F3-P2 — locally the llama.cpp server start compensated for the silent
// drop, externally NOTHING does, and the wire carries them (OpenRouter A3).
type openAIChatRequest struct {
	Model            string             `json:"model"`
	Messages         []Message          `json:"messages"`
	Stream           bool               `json:"stream"`
	MaxTokens        int                `json:"max_tokens,omitempty"`
	Temperature      float64            `json:"temperature"`
	TopP             float64            `json:"top_p,omitempty"`
	TopK             int                `json:"top_k,omitempty"`
	MinP             float64            `json:"min_p,omitempty"`
	PresencePenalty  float64            `json:"presence_penalty,omitempty"`
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
	// ID and Model feed the OpenRouter provenance fields of ChatResponse;
	// Usage.Cost is OpenRouter's USD charge (absent on local backends).
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"`
		} `json:"message"`
		// FinishReason is the OpenAI stop reason of this choice — the same
		// field the SSE path reads per delta (stream.go). Only choice 0 is
		// consumed, mirroring the Message above.
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens int      `json:"completion_tokens"`
		PromptTokens     int      `json:"prompt_tokens"`
		Cost             *float64 `json:"cost"`
	} `json:"usage"`
}

// Chat sends a non-streaming chat request to the given backend. The wire path
// (/api/chat vs /v1/chat/completions) follows b.Protocol — host and protocol
// travel as one tuple, never as loose parameters (F1-W3).
func Chat(ctx context.Context, b backends.Backend, systemPrompt, userPrompt string, opts Options, timeout time.Duration) (*ChatResponse, error) {
	return chatWithFormat(ctx, b, systemPrompt, userPrompt, opts, "", timeout)
}

// ChatJSON sends a non-streaming chat request with JSON-mode enabled to the
// given backend.
func ChatJSON(ctx context.Context, b backends.Backend, systemPrompt, userPrompt string, opts Options, timeout time.Duration) (*ChatResponse, error) {
	return chatWithFormat(ctx, b, systemPrompt, userPrompt, opts, "json", timeout)
}

// chatWithFallback died in F3-P2: its single consumer (the synthesize step)
// walks the pool chain via ChatChain, which generalizes the hardwired
// two-leg semantics (transport failure ⇒ next backend, HTTP 500 ⇒ stop).

// chatWithFormat is the protocol dispatch shared by Chat and ChatJSON. Since
// G29 the wire paths take the full Backend — ProviderClass, ExtraHeaders and
// ExtraBody are wire-relevant (OpenRouter forced zdr/deny, per-backend
// attribution headers), so the loose-parameter form would silently drop them.
func chatWithFormat(ctx context.Context, b backends.Backend, systemPrompt, userPrompt string, opts Options, format string, timeout time.Duration) (*ChatResponse, error) {
	if b.Protocol == backends.ProtocolOpenAI {
		return chatOpenAI(ctx, b, systemPrompt, userPrompt, opts, format, timeout)
	}
	return chatOllama(ctx, b, systemPrompt, userPrompt, opts, format, timeout)
}

func chatOllama(ctx context.Context, b backends.Backend, systemPrompt, userPrompt string, opts Options, format string, timeout time.Duration) (*ChatResponse, error) {
	reqBody := ollamaChatRequest{
		Model: b.Model,
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream:  false,
		Think:   b.Think.Ptr(),
		Format:  format,
		Options: opts,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	setChatHeaders(req, &b)

	resp, err := httpx.DoRetryOnce(httpClient, req, body)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// Typed wrap (F3-P1): errors.As reaches the status code for the
		// failover classifier; the rendered string stays byte-identical.
		return nil, fmt.Errorf("llm: %w", httpx.NewStatusError(resp, errBody))
	}

	var result ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}

	return &ChatResponse{
		Message:      result.Message,
		EvalCount:    result.EvalCount,
		PromptTokens: result.PromptEvalCount,
		FinishReason: result.DoneReason,
	}, nil
}

func chatOpenAI(ctx context.Context, b backends.Backend, systemPrompt, userPrompt string, opts Options, format string, timeout time.Duration) (*ChatResponse, error) {
	think := b.Think.Ptr()
	reqBody := openAIChatRequest{
		Model: b.Model,
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream:          false,
		MaxTokens:       opts.NumPredict,
		Temperature:     opts.Temperature,
		TopP:            opts.TopP,
		TopK:            opts.TopK,
		MinP:            opts.MinP,
		PresencePenalty: opts.PresencePenalty,
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
	body, err = applyOpenAIBodyExtras(body, &b)
	if err != nil {
		return nil, fmt.Errorf("llm: merge extra_body: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Host+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	setChatHeaders(req, &b)

	resp, err := httpx.DoRetryOnce(httpClient, req, body)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llm: %w", httpx.NewStatusError(resp, errBody))
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

	out := &ChatResponse{
		Message:      Message{Role: choice.Role, Content: content},
		EvalCount:    result.Usage.CompletionTokens,
		PromptTokens: result.Usage.PromptTokens,
		FinishReason: result.Choices[0].FinishReason,
	}
	// Provider provenance is an openrouter-class contract (design 03 §2.7.4):
	// local /v1 servers also echo a model string, but only OpenRouter's may
	// legitimately differ from the requested one (models-fallback) — gating
	// here keeps llmlog's model column row-faithful for local backends.
	if b.ProviderClass == backends.ProviderOpenRouter {
		out.CostUSD = result.Usage.Cost
		out.ServedModel = result.Model
		out.ProviderRequestID = result.ID
	}
	return out, nil
}

// applyOpenAIBodyExtras merges b.ExtraBody into the marshaled request (last
// write wins on key collision — the stream path's escape-hatch semantics,
// buildStreamBody) and then FORCES provider.zdr=true +
// provider.data_collection="deny" for provider_class=openrouter,
// trust-INDEPENDENT (design 03 §3.3): trust decides WHICH content may flow,
// the provider class decides whether the provider may store it. extra_body
// can only tighten, never loosen — the force runs after the merge. The only
// way out is the explicit metadata.allow_data_collection=true escape,
// confirm-gated at backend-create/update.
func applyOpenAIBodyExtras(body []byte, b *backends.Backend) ([]byte, error) {
	forceZDR := b.ProviderClass == backends.ProviderOpenRouter && !allowsDataCollection(b)
	if len(b.ExtraBody) == 0 && !forceZDR {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	for k, v := range b.ExtraBody {
		m[k] = v
	}
	if forceZDR {
		prov, _ := m["provider"].(map[string]any)
		if prov == nil {
			prov = map[string]any{}
		}
		prov["zdr"] = true
		prov["data_collection"] = "deny"
		m["provider"] = prov
	}
	return json.Marshal(m)
}

// allowsDataCollection reads the explicit non-ZDR escape. Anything but a
// literal bool true (string "true", 1, absent) keeps the enforcement on.
func allowsDataCollection(b *backends.Backend) bool {
	v, ok := b.Metadata["allow_data_collection"].(bool)
	return ok && v
}

// setChatHeaders applies the standard headers, then ExtraHeaders (OpenRouter
// attribution: HTTP-Referer/X-Title). Core headers are set LAST so a row
// edited past the credential-carrier denylist can not override the
// Authorization derived from api_key_ref.
func setChatHeaders(req *http.Request, b *backends.Backend) {
	for k, v := range b.ExtraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
}

// SynthesisOptions returns default options for LLM synthesis.
// numCtx sets the model context window (0 = omit → model default). All chat-model
// call sites must pass the SAME ChatNumCtx so Ollama loads a single 27B runner.
func SynthesisOptions(numCtx int) Options {
	opts := Options{
		Temperature:   0.1,
		RepeatPenalty: 1.1,
		NumPredict:    500,
	}
	if numCtx > 0 {
		opts.NumCtx = numCtx
	}
	return opts
}

// TranslateOptions returns default options for query translation.
// numCtx sets the model context window (0 = omit → model default).
func TranslateOptions(numCtx int) Options {
	opts := Options{
		Temperature: 0,
		NumPredict:  100,
	}
	if numCtx > 0 {
		opts.NumCtx = numCtx
	}
	return opts
}

// RerankOptions returns default options for reranking.
// numCtx sets the model context window (0 = omit → model default).
func RerankOptions(numCtx int) Options {
	opts := Options{
		Temperature: 0,
		NumPredict:  80,
	}
	if numCtx > 0 {
		opts.NumCtx = numCtx
	}
	return opts
}
