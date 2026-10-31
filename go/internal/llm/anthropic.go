// Native Anthropic Messages API wire (backends.ProtocolAnthropic): POST
// {base_url}/v1/messages with x-api-key + anthropic-version, the system
// prompt as a top-level cached block, and NO sampling parameters.
//
// Why a third wire instead of the openai path against Anthropic's
// OpenAI-compatible layer: that layer rejects every sampling parameter on
// the Claude 5 models (400 "temperature is deprecated for this model" — and
// chatOpenAI always sends temperature), drops response_format silently, has
// no prompt caching and no effort control, and Anthropic documents it as a
// comparison surface, not a production one. A production install ran a
// stripping sidecar in front of it for exactly those reasons; this file is
// what makes that sidecar unnecessary.
//
// Model-side facts this wire is built on (claude-api reference, 2026-10,
// Claude Opus 5.5 / Sonnet 5.5 / Fable 5.1 family):
//   - temperature / top_p / top_k → 400: none of Options' sampling fields
//     are ever marshalled; num_ctx stays a ctx-internal prompt budget.
//   - thinking cannot be disabled (Opus 5.5: {type:"disabled"} is a 400 at
//     every effort); omitting the field runs adaptive thinking. The
//     model_map `think` toggle is therefore ignored here, and depth is
//     steered with output_config.effort instead (resolveAnthropicEffort).
//   - forced tool_choice (any/tool) → 400: never sent (auto is the default,
//     matching buildStreamBody's doctrine on the openai wire). The one
//     tool_choice this wire sends is {"type":"none"} on the chat harness's
//     closing call (Options.ToolChoiceNone), which keeps the loop's tools
//     array so a replayed thinking block's prefix stays unchanged.
//   - output_config.format needs a JSON schema with additionalProperties:
//     false on every object; ctx's "json" format carries no schema and the
//     dream stages answer with arrays, so ChatJSON is a documented no-op on
//     this wire — the pipelines' local parsers stay the validator, exactly
//     the CTX_DREAM_JSON_MODE=off contract (docs/operations.md).
//   - max_tokens is REQUIRED and thinking tokens count against it: the
//     wire sends NumPredict plus a per-effort thinking reserve
//     (anthropicMaxTokens), and an answer that still ends at the limit
//     without any text is ErrOutputLimitNoText, never an empty answer.
//   - usage.input_tokens is the UNCACHED remainder; the total prompt is
//     input + cache_read + cache_creation (anthropicUsageTotals), which is
//     what PromptTokens reports so llmlog's prompt_tokens stays comparable
//     with the other wires. The two cache counters travel separately for
//     the llmlog metadata (ApplyProviderTelemetry).
//   - 529 overloaded_error is a plain non-2xx: httpx.StatusError carries it
//     into backends.Classify as ClassGateway (failover), 429 as
//     ClassRateLimit with the Retry-After cooldown, 401 as ClassAuth. The
//     same types inside a mid-stream error event classify the same way
//     (StreamError.ProviderErrorType).
//   - The wire targets the Claude 5 family (claude-opus-5-5 in production).
//     output_config.effort is sent on every call; models that predate it
//     (Sonnet 4.5, Haiku 4.5) reject the field, and on Opus 4.7/4.8 an
//     omitted thinking field means NO thinking — neither is a supported row.
//
// Raw net/http rather than the official Go SDK, like every other ctx wire:
// the backend pool, the dispatch invariant (one pinned
// http.NewRequestWithContext per wire file, chat/dispatch_invariant_test.go)
// and the failover classifier all key on ctx's own request/response path,
// and the request shape is small enough that an SDK would add a dependency
// without removing any of that.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/promptguard"
)

const (
	// AnthropicVersion is the Messages API version header, shared by the
	// wire and the backend-test probe (handler/backends_manage.go). It is not
	// the only version: 2026-10-21 exists too
	// (platform.claude.com/docs/en/api/versioning — a 400 for a document
	// block in an assistant message, for citations in a user text block, and
	// for temperature/top_p/top_k on every model). The wire stays on
	// 2023-06-01 on purpose: it sends none of what 2026-10-21 removes, so the
	// request shape is the same under both, and 2023-06-01 is the version the
	// wire was verified against live (2026-10-11). Moving is a deliberate
	// step along the migration guide (platform.claude.com/docs/en/api/migration-2026-10-21),
	// with the live smoke re-run — not a constant to bump in passing.
	AnthropicVersion = "2023-06-01"
	// anthropicDefaultMaxTokens applies when the caller sets no NumPredict:
	// the field is required on this wire, and every production Options
	// preset sets one (synthesis 500, translate 100, probe 1), so this is
	// the safety net for a bare Options{}, sized so a long answer is not
	// cut mid-thought (claude-api reference default for non-streaming).
	anthropicDefaultMaxTokens = 16000
	// anthropicMaxOutputTokens is the output ceiling of the Claude 5 models
	// this wire targets (Opus 5.5 / Sonnet 5.5 / Haiku 5.5: 128K) — the cap
	// on NumPredict + thinking reserve.
	anthropicMaxOutputTokens = 128000
	// anthropicEffortKey is the model_map params / extra_body key that is
	// LIFTED into output_config.effort instead of being merged top-level.
	anthropicEffortKey = "effort"
	// anthropicOutputConfigKey is the one carrier key merged one level deep
	// into the wire's own output_config (buildAnthropicBody).
	anthropicOutputConfigKey = "output_config"
)

// ErrOutputLimitNoText: the answer stopped at max_tokens (or the context
// window) before any text — on a thinking model the reasoning spent the
// whole budget. Surfaced as an error so the pipeline does not take an empty
// string for an answer.
var ErrOutputLimitNoText = errors.New("llm: anthropic: output limit reached before any answer text")

// ProbeOptions is the backend-test connectivity probe's request: one output
// token, exact (no thinking reserve), success on an empty max_tokens answer.
func ProbeOptions() Options {
	return Options{Temperature: 0, NumPredict: 1, ExactMaxTokens: true}
}

// --- request shape ---.

type anthropicCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// anthropicTextBlock is one text content block; CacheControl marks the
// prefix breakpoint (the system prompt here).
type anthropicTextBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicMessage is one turn. Content is either a plain string (the
// single-user-prompt pipelines) or a []any of content blocks (the chat
// harness: text, tool_use, tool_result).
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

// anthropicTool is the Messages API tool definition (input_schema, not the
// OpenAI function envelope).
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice is sent only as {"type":"none"} (the chat harness's
// closing call, Options.ToolChoiceNone): auto is the API default and the
// forced variants (any/tool) are a 400 on Claude Opus 5.5.
type anthropicToolChoice struct {
	Type string `json:"type"`
}

type anthropicRequest struct {
	Model        string                `json:"model"`
	MaxTokens    int                   `json:"max_tokens"`
	System       []anthropicTextBlock  `json:"system,omitempty"`
	Messages     []anthropicMessage    `json:"messages"`
	Stream       bool                  `json:"stream,omitempty"`
	OutputConfig anthropicOutputConfig `json:"output_config"`
	Tools        []anthropicTool       `json:"tools,omitempty"`
	ToolChoice   *anthropicToolChoice  `json:"tool_choice,omitempty"`
	// CacheControl at the top level is the API's automatic breakpoint on the
	// last cacheable block — set on the multi-turn stream path only.
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// --- response shape ---.

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// anthropicContentBlock is one response block. text carries Text;
// tool_use carries ID/Name/Input; thinking blocks are skipped (their text
// is empty under the default display anyway).
type anthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Content    []anthropicContentBlock `json:"content"`
	Usage      anthropicUsage          `json:"usage"`
}

// anthropicError is the body of every non-2xx answer and of a mid-stream
// error event: {"type":"error","error":{"type":"...","message":"..."}}.
type anthropicError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- helpers shared by the non-streaming and the streaming path ---.

// resolveAnthropicEffort picks output_config.effort: the role's model_map
// params (arriving in Options.Extra through applyModelParams' default
// branch) beat the row's extra_body, which beats the explicit default.
// Validation (backends.validateAnthropic) has already bounded both carriers
// to the accepted set; a non-string value here falls through to the next
// carrier rather than reaching the wire.
func resolveAnthropicEffort(opts Options, b *backends.Backend) string {
	if v, ok := opts.Extra[anthropicEffortKey].(string); ok && v != "" {
		return v
	}
	if v, ok := b.ExtraBody[anthropicEffortKey].(string); ok && v != "" {
		return v
	}
	return backends.AnthropicDefaultEffort
}

// anthropicThinkingReserve is added to the pipeline's visible-output budget
// on this wire. Thinking tokens count against max_tokens and Claude Opus 5.5
// cannot stop thinking, so a NumPredict sized for the ANSWER (translate 100,
// topiclabel 128, temporal 300, synthesis 500) would let the reasoning spend
// the whole budget and end the call at max_tokens with no text
// (platform.claude.com/docs/en/build-with-claude/thinking-troubleshooting,
// "The response stops with stop_reason max_tokens": "Raise max_tokens to
// leave room for both thinking and text"). max_tokens is a ceiling, not a
// charge — billing follows the tokens actually generated — so the reserve
// is sized generously and doubles per effort step (thinking depth is what
// effort steers). No figure for thinking length per effort level is
// published; these are ceilings chosen to stay out of the way of a short
// pipeline answer at every level, not measurements. An operator who sees
// ErrOutputLimitNoText rows still raises the role's model_map
// params.max_tokens (the visible budget) or lowers the effort.
var anthropicThinkingReserve = map[string]int{
	"low": 4096, "medium": 8192, "high": 16384, "xhigh": 32768, "max": 65536,
}

// anthropicMaxTokens is max_tokens for one call: the visible budget
// (NumPredict, or the fallback for a bare Options{}) plus the thinking
// reserve of the resolved effort, capped at the output ceiling.
// ExactMaxTokens sends the visible budget verbatim (the 1-token probe,
// decide).
func anthropicMaxTokens(opts Options, effort string) int {
	n := opts.NumPredict
	if n <= 0 {
		n = anthropicDefaultMaxTokens
	}
	if opts.ExactMaxTokens {
		return n
	}
	reserve, ok := anthropicThinkingReserve[effort]
	if !ok {
		reserve = anthropicThinkingReserve[backends.AnthropicDefaultEffort]
	}
	return min(n+reserve, anthropicMaxOutputTokens)
}

// outputLimitWithoutText reports the thinking-ate-the-budget case: the
// answer ended at the output limit (finish "length": max_tokens or the
// context window) with no text and no tool call. ExactMaxTokens callers
// expect exactly that (the 1-token probe) and are exempt.
func outputLimitWithoutText(opts Options, finish, text string, toolCalls int) bool {
	return !opts.ExactMaxTokens && finish == "length" && toolCalls == 0 && strings.TrimSpace(text) == ""
}

// errOutputLimitNoText wraps ErrOutputLimitNoText with what the operator
// needs to size the budget: the max_tokens sent, the effort, the output
// tokens the model reported.
func errOutputLimitNoText(maxTokens int, effort string, outputTokens int) error {
	return fmt.Errorf("%w (max_tokens=%d, effort=%s, output_tokens=%d; raise the role's model_map params.max_tokens or lower the effort)",
		ErrOutputLimitNoText, maxTokens, effort, outputTokens)
}

// promptguardRuleMarker is the opening of promptguard.Rule's sentence — the
// per-call nonce rule every pipeline appends to its system prompt.
var promptguardRuleMarker = "Everything between <" + promptguard.GuardTag + " id="

// anthropicSystem renders the system prompt as the cached top-level block.
// An empty or whitespace-only prompt yields no system field (the API rejects
// a blank text block — blankText).
//
// The cache is a byte-exact prefix match, and the pipelines end their
// system prompt with promptguard.Rule(nonce) — a fresh nonce per call. One
// block carrying both would be written to the cache on every call and read
// back never (a pure 1.25x surcharge, visible as cache_creation > 0 with
// cache_read == 0 on every row). So the rule is split off into a second,
// unmarked text block: the stable prefix before it is the breakpoint.
func anthropicSystem(systemPrompt string) []anthropicTextBlock {
	if blankText(systemPrompt) {
		return nil
	}
	if i := strings.LastIndex(systemPrompt, promptguardRuleMarker); i > 0 {
		if blankText(systemPrompt[:i]) {
			// Nothing stable ahead of the rule: no cacheable prefix, and a
			// blank block would be rejected.
			return []anthropicTextBlock{{Type: "text", Text: systemPrompt[i:]}}
		}
		return []anthropicTextBlock{
			{Type: "text", Text: systemPrompt[:i], CacheControl: &anthropicCacheControl{Type: "ephemeral"}},
			{Type: "text", Text: systemPrompt[i:]},
		}
	}
	return []anthropicTextBlock{{
		Type: "text", Text: systemPrompt,
		CacheControl: &anthropicCacheControl{Type: "ephemeral"},
	}}
}

// buildAnthropicBody marshals the request and merges the two extra carriers
// — Options.Extra (model_map params) first, b.ExtraBody last, so a backend
// row's extra_body keeps the final word exactly as on the openai wire
// (applyOpenAIBodyExtras). The effort key is consumed by
// resolveAnthropicEffort and never merged top-level (it is not a Messages
// API field). Everything else merges verbatim: that is the operator's
// escape hatch for fields this wire does not model (thinking.display,
// stop_sequences, metadata.user_id, …); the keys the Messages API cannot
// take (sampling, OpenAI/Ollama dialect, wire-owned) are a 422 at write
// time instead (backends.validateAnthropicCarrierKeys).
//
// output_config is the one key merged ONE LEVEL DEEP instead of replacing
// the top-level field: the wire's own output_config carries the resolved
// effort, and a flat merge of an operator's output_config (task_budget, …)
// would silently drop it. Params' keys first, the row's last (same
// last-word rule), the resolved effort over both — validation already
// rejects a carrier's output_config.effort and a non-object output_config
// (backends.validateAnthropicCarrierKeys); a non-object one on a psql-edited
// row is dropped here rather than allowed to replace the effort.
func buildAnthropicBody(req anthropicRequest, opts Options, b *backends.Backend) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	extra := make(map[string]any, len(opts.Extra)+len(b.ExtraBody))
	var outputConfig map[string]any
	for _, carrier := range []map[string]any{opts.Extra, b.ExtraBody} {
		for k, v := range carrier {
			switch k {
			case anthropicEffortKey:
			case anthropicOutputConfigKey:
				if m, ok := v.(map[string]any); ok {
					if outputConfig == nil {
						outputConfig = make(map[string]any, len(m)+1)
					}
					maps.Copy(outputConfig, m)
				}
			default:
				extra[k] = v
			}
		}
	}
	if outputConfig != nil {
		outputConfig[anthropicEffortKey] = req.OutputConfig.Effort
		extra[anthropicOutputConfigKey] = outputConfig
	}
	return mergeJSONFields(body, extra)
}

// SetAnthropicHeaders mirrors setChatHeaders' doctrine: ExtraHeaders first,
// the core headers LAST so a row edited past the credential-carrier
// denylist can never override the key derived from api_key_ref or the
// version pin. Exported for the backend-test probe, which must send the
// same headers the wire does.
func SetAnthropicHeaders(req *http.Request, b *backends.Backend) {
	for k, v := range b.ExtraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", AnthropicVersion)
	if b.APIKey != "" {
		req.Header.Set("x-api-key", b.APIKey)
	}
}

// mapAnthropicStopReason translates the Messages API vocabulary into the
// one every FinishReason consumer already compares against (dream's cap
// detection reads "length", the chat harness reads "tool_calls"): a raw
// pass-through — the doctrine for the ollama/openai pair, whose
// vocabularies overlap — would make an Anthropic cap hit invisible to
// evaluate.go and a tool call look like a final answer to engine.go. Unknown
// values pass through raw so a new provider state is observable in llmlog;
// an absent reason stays "" (unknown), never a fabricated "stop".
func mapAnthropicStopReason(s string) string {
	switch s {
	case "":
		return ""
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return s
	}
}

// anthropicUsageTotals folds the usage object into ctx's counters: the
// prompt total (uncached remainder + cache read + cache write) and the
// output count, plus the two cache counters for the llmlog metadata.
func anthropicUsageTotals(u anthropicUsage) (prompt, output, cacheRead, cacheCreate int) {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens
}

// anthropicStatusError wraps a non-2xx answer the way the other wires do
// (httpx.StatusError for the failover classifier, rendered string unchanged)
// and prefixes the provider's error type when the body parses, so a log
// line reads "overloaded_error: Overloaded" instead of raw JSON.
func anthropicStatusError(resp *http.Response) error {
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var ae anthropicError
	if json.Unmarshal(errBody, &ae) == nil && ae.Error.Type != "" {
		errBody = []byte(ae.Error.Type + ": " + ae.Error.Message)
	}
	return fmt.Errorf("llm: %w", httpx.NewStatusError(resp, errBody))
}

// --- non-streaming path ---.

// chatAnthropic is the ProtocolAnthropic leg of chatWithFormat. format is
// accepted for signature parity and ignored (see the package comment on
// structured outputs); think is ignored (thinking cannot be disabled).
func chatAnthropic(ctx context.Context, b backends.Backend, systemPrompt, userPrompt string, opts Options, _ string, timeout time.Duration) (*ChatResponse, error) {
	effort := resolveAnthropicEffort(opts, &b)
	req := anthropicRequest{
		Model:        b.Model,
		MaxTokens:    anthropicMaxTokens(opts, effort),
		System:       anthropicSystem(systemPrompt),
		Messages:     []anthropicMessage{{Role: "user", Content: userPrompt}},
		OutputConfig: anthropicOutputConfig{Effort: effort},
	}
	body, err := buildAnthropicBody(req, opts, &b)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Host+"/v1/messages", bytes.NewReader(body)) //nolint:gosec // G704: host from the F3 backend pool (context_backends) — admin-gated + locality-validated, no free user input.
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	SetAnthropicHeaders(httpReq, &b)

	resp, err := httpx.DoRetryOnce(httpClients.For(b.EgressGuarded()), httpReq, body)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, anthropicStatusError(resp)
	}

	var result anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}

	var text strings.Builder
	for _, blk := range result.Content {
		if blk.Type == "text" {
			text.WriteString(blk.Text)
		}
	}
	finish := mapAnthropicStopReason(result.StopReason)
	if outputLimitWithoutText(opts, finish, text.String(), 0) {
		return nil, errOutputLimitNoText(req.MaxTokens, effort, result.Usage.OutputTokens)
	}
	prompt, output, cacheRead, cacheCreate := anthropicUsageTotals(result.Usage)
	return &ChatResponse{
		Message:             Message{Role: "assistant", Content: text.String()},
		EvalCount:           output,
		PromptTokens:        prompt,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreate,
		FinishReason:        finish,
		// The message id (msg_…) is the audit handle that exists on both the
		// stream and the non-stream path (the request-id header is the one
		// support quotes; it is not read here). The model column stays
		// row-faithful — Anthropic echoes the requested id, there is no
		// models-fallback to record — and no cost is reported on this wire
		// (billing is read in the console).
		ProviderRequestID: result.ID,
	}, nil
}
