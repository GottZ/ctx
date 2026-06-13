// Streaming chat with tool-calling over the OpenAI-compatible wire
// (POST /v1/chat/completions, stream:true). This is a parallel path next to
// chatOpenAI: the existing non-streaming call sites carry system+user prompt
// pairs and stay untouched; the web-chat harness (F6) needs multi-turn
// message arrays, tool definitions and per-delta events.
//
// Wire realities this parser is hardened against (design 06-web-chat §3.4):
//   - llama.cpp b9585 + --jinja streams tool calls as index-keyed deltas with
//     the arguments as JSON-STRING fragments (live capture E3, testdata).
//   - Other builds/providers send arguments as a complete JSON object
//     (llama.cpp issue #20198) — both forms normalise to the object form.
//   - OpenRouter interleaves SSE comment frames (": OPENROUTER PROCESSING")
//     and reports provider failures as an error object INSIDE an HTTP-200
//     stream (finish_reason "error") — status-only error handling would
//     misread those as success.
//   - "data: [DONE]" terminates the stream.

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
)

// doneSentinel terminates an OpenAI-compatible SSE stream.
const doneSentinel = "[DONE]"

// ToolDef describes one tool in the OpenAI function-calling schema.
type ToolDef struct {
	Type     string          `json:"type"` // "function"
	Function ToolDefFunction `json:"function"`
}

// ToolDefFunction is the function block of a ToolDef.
type ToolDefFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
}

// ToolCall is one assembled tool invocation emitted by the model.
//
// Function.Arguments always holds the OBJECT form ({"city":"Berlin"}):
// llama.cpp b9585 delivers a JSON string (live capture E2/E3), other
// builds/providers a JSON object (llama.cpp issue #20198) — the parser
// normalises both (first non-whitespace byte '"' → unquote, '{' → as-is).
// Marshalling emits the OpenAI-canonical string form, so assembled calls can
// be echoed back verbatim in the next request's message history.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the function block of a ToolCall.
type ToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolCallWire is the OpenAI request encoding of a ToolCall: arguments as a
// JSON string containing the serialised arguments object.
type toolCallWire struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolCallFuncWire `json:"function"`
}

type toolCallFuncWire struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// MarshalJSON emits the OpenAI-canonical wire form (arguments as JSON
// string) regardless of the internal form, so a ToolCall taken from a
// StreamResult round-trips into the next request without caller conversion.
func (c ToolCall) MarshalJSON() ([]byte, error) {
	args := bytes.TrimSpace(c.Function.Arguments)
	var argStr string
	switch {
	case len(args) == 0:
		argStr = "{}"
	case args[0] == '"':
		// Already string-form — unwrap once so we never double-encode.
		if err := json.Unmarshal(args, &argStr); err != nil {
			return nil, fmt.Errorf("llm: tool call %q: invalid string-form arguments: %w", c.ID, err)
		}
	default:
		argStr = string(args)
	}
	typ := c.Type
	if typ == "" {
		typ = "function"
	}
	return json.Marshal(toolCallWire{
		ID:       c.ID,
		Type:     typ,
		Function: toolCallFuncWire{Name: c.Function.Name, Arguments: argStr},
	})
}

// UnmarshalJSON accepts both wire forms and normalises Arguments to the
// object form — symmetric to MarshalJSON, so persisting/round-tripping a
// ToolCall through JSON never flips its internal representation.
func (c *ToolCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.ID = raw.ID
	c.Type = raw.Type
	c.Function.Name = raw.Function.Name
	c.Function.Arguments = normalizeArgs(raw.Function.Arguments)
	return nil
}

// normalizeArgs converts tool-call arguments to the object form: first
// non-whitespace byte '"' → unquote (string-form carries the JSON text),
// anything else → as-is. Empty/null → "{}". Content that is no valid JSON
// after normalisation is passed through untouched — the tool executor turns
// unparseable arguments into an ok:false tool result, the wire layer must
// not fail the whole turn over them.
func normalizeArgs(raw json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return json.RawMessage("{}")
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			if strings.TrimSpace(s) == "" {
				return json.RawMessage("{}")
			}
			return json.RawMessage(s)
		}
	}
	return t
}

// ChatMsg is the multi-turn message of the streaming chat path (deliberately
// separate from llm.Message, which carries the system+user pairs of the
// existing pipelines and cannot express tool calls or tool results).
type ChatMsg struct {
	Role       string     `json:"role"` // system|user|assistant|tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// StreamEvent is handed to the ChatStream callback as the stream progresses.
// Exactly one field group is set per event: a content fragment, the name of
// a newly started tool call (first delta that carries it — the UI's
// tool_call_start signal), or the terminal finish reason.
type StreamEvent struct {
	ContentDelta string // delta.content fragment
	ToolCallName string // set once per call, when its name first arrives
	FinishReason string // "" until the end; stop|tool_calls|length
}

// StreamResult is the fully assembled outcome of one streaming call.
type StreamResult struct {
	Content      string
	ToolCalls    []ToolCall // assembled from index-keyed deltas, index order
	FinishReason string
	// PromptTokens/CompletionTokens come from usage (stream_options
	// include_usage) or, when absent, from llama.cpp's timings block.
	PromptTokens     int
	CompletionTokens int
	DraftAccept      float64 // timings.draft_n_accepted/draft_n, 0 when n/a
}

// StreamError is an error event delivered inside an HTTP-200 SSE stream.
// OpenRouter reports provider failures this way (error object in the data
// frame, finish_reason "error") — callers can errors.As on it to separate
// backend-reported failures from transport errors.
type StreamError struct {
	Code    string // provider code, number or string ("502", "server_error")
	Message string
}

func (e *StreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("llm: stream error event: %s (code %s)", e.Message, e.Code)
	}
	return fmt.Sprintf("llm: stream error event: %s", e.Message)
}

// --- request building ---.

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIStreamRequest struct {
	Model            string           `json:"model"`
	Messages         []ChatMsg        `json:"messages"`
	Stream           bool             `json:"stream"`
	StreamOptions    streamOptions    `json:"stream_options"`
	MaxTokens        int              `json:"max_tokens,omitempty"`
	Temperature      float64          `json:"temperature"`
	FrequencyPenalty float64          `json:"frequency_penalty,omitempty"`
	Tools            []ToolDef        `json:"tools,omitempty"`
	Reasoning        *reasoningOption `json:"reasoning,omitempty"`
}

// buildStreamBody marshals the request. Deliberately absent fields:
// tool_choice ("none" emits tool syntax as plain text, "required" is not
// enforced — E4/E5; "no more tools" means omitting the tools array) and
// parallel_tool_calls (off by default; the parser handles arrays defensively
// anyway). TopP/TopK/MinP/NumCtx are not part of the openai wire — parity
// with chatOpenAI, sampling parity comes from the server start profile.
// extraBody entries are merged last and win on key collision (F3's escape
// hatch: OpenRouter provider/zdr objects, per-backend reasoning override).
func buildStreamBody(model string, msgs []ChatMsg, tools []ToolDef, opts Options, extraBody map[string]any) ([]byte, error) {
	req := openAIStreamRequest{
		Model:         model,
		Messages:      msgs,
		Stream:        true,
		StreamOptions: streamOptions{IncludeUsage: true},
		MaxTokens:     opts.NumPredict,
		Temperature:   opts.Temperature,
		Tools:         tools,
		// The chat path never wants chain-of-thought in v1 (the production
		// server runs --reasoning off, OpenRouter bills reasoning tokens at
		// completion rate) — mirror the non-streaming think=false path
		// (chatOpenAI). Per-backend override via extraBody["reasoning"].
		Reasoning: &reasoningOption{Enabled: false, Exclude: true},
	}
	if opts.RepeatPenalty > 0 {
		req.FrequencyPenalty = opts.RepeatPenalty - 1.0
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(extraBody) == 0 {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	for k, v := range extraBody {
		m[k] = v
	}
	return json.Marshal(m)
}

// ChatStream sends a streaming chat request with optional tool definitions
// over the OpenAI-compatible wire (the only streaming protocol in F6) and
// assembles the response. onEvent is invoked for content fragments, tool
// call starts and the finish reason; an error from onEvent aborts the
// stream. Cancelling ctx aborts the in-flight generation (the HTTP request
// to the backend is dropped, llama.cpp frees its slot).
//
// httpx.DoRetryOnce covers the connection setup only: a transport error
// after response bytes arrived surfaces as a read error and is NOT replayed
// — no silent double generation or double tool execution mid-stream.
func ChatStream(ctx context.Context, host, apiKey, model string,
	msgs []ChatMsg, tools []ToolDef, opts Options, extraBody map[string]any,
	timeout time.Duration, onEvent func(StreamEvent) error) (*StreamResult, error) {
	body, err := buildStreamBody(model, msgs, tools, opts, extraBody)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/v1/chat/completions", bytes.NewReader(body)) //nolint:gosec // G704: host aus dem F3-Backend-Pool (context_backends) — admin-gated + locality-validiert, kein freier User-Input. SSRF über die Egress-Locality-Trust-Dimension gegated (vor Prompt-Übertragung), nicht auf dieser Wire-Schicht.
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpx.DoRetryOnce(httpClient, req, body)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// Typed StatusError so the F3 failover classifier (backends.Classify)
		// sees the status code — the streaming path needs the same
		// errors.As-able error the non-streaming wire clients return, or a
		// 502/503/504 before the first byte could never fail over (§2.3/R4).
		// The "%w" wrap keeps the rendered string byte-identical.
		return nil, fmt.Errorf("llm: %w", httpx.NewStatusError(resp, errBody))
	}

	return parseSSEStream(resp.Body, onEvent)
}

// --- SSE parsing ---.

// streamChunk is one data frame of an OpenAI-compatible stream. Error is the
// OpenRouter mid-stream error object (HTTP 200); Timings is llama.cpp's
// final-chunk telemetry.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			Reasoning string          `json:"reasoning"`
			ToolCalls []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage   *usageBody       `json:"usage"`
	Timings *timingsBody     `json:"timings"`
	Error   *streamErrorBody `json:"error"`
}

// toolCallDelta is one fragment of a tool call. Index keys the assembly
// (E3); ID/Type/Name arrive with the first fragment, Arguments accumulate.
type toolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type usageBody struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type timingsBody struct {
	PromptN        int `json:"prompt_n"`
	PredictedN     int `json:"predicted_n"`
	DraftN         int `json:"draft_n"`
	DraftNAccepted int `json:"draft_n_accepted"`
}

type streamErrorBody struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

func (b *streamErrorBody) toStreamError() *StreamError {
	code := strings.Trim(string(bytes.TrimSpace(b.Code)), `"`)
	if code == "null" {
		code = ""
	}
	return &StreamError{Code: code, Message: b.Message}
}

// pendingToolCall accumulates one tool call across its deltas.
type pendingToolCall struct {
	index       int
	id          string
	typ         string
	name        string
	args        bytes.Buffer
	nameEmitted bool
}

// streamState carries the assembly across chunks.
type streamState struct {
	onEvent   func(StreamEvent) error
	content   strings.Builder
	reasoning strings.Builder
	calls     []*pendingToolCall
	byIndex   map[int]*pendingToolCall
	finish    string
	usage     *usageBody
	timings   *timingsBody
	done      bool
}

func (st *streamState) emit(ev StreamEvent) error {
	if st.onEvent == nil {
		return nil
	}
	if err := st.onEvent(ev); err != nil {
		return fmt.Errorf("llm: stream event callback: %w", err)
	}
	return nil
}

// parseSSEStream reads SSE frames until [DONE] or EOF. Comment lines
// (": OPENROUTER PROCESSING") are skipped per SSE spec; multi-line data
// fields are joined with newlines before JSON decoding. Lenient endings:
// EOF after a finish_reason but before [DONE] is accepted (the result is
// complete); EOF before any finish_reason is a hard error — mid-stream
// connection death must not look like a finished turn.
func parseSSEStream(r io.Reader, onEvent func(StreamEvent) error) (*StreamResult, error) {
	st := &streamState{onEvent: onEvent, byIndex: map[int]*pendingToolCall{}}
	br := bufio.NewReader(r)
	var data []string
	for {
		line, readErr := br.ReadString('\n')
		if err := st.feedLine(strings.TrimRight(line, "\r\n"), &data); err != nil {
			return nil, err
		}
		if st.done {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				// Flush an unterminated trailing frame (server ended without
				// the final blank line), then leave the loop.
				if err := st.dispatchData(&data); err != nil {
					return nil, err
				}
				break
			}
			return nil, fmt.Errorf("llm: read stream: %w", readErr)
		}
	}
	if !st.done && st.finish == "" {
		return nil, fmt.Errorf("llm: stream ended unexpectedly (no [DONE], no finish_reason)")
	}
	return st.result(), nil
}

// feedLine handles one SSE line: blank → dispatch the buffered data frame,
// ":" prefix → comment (ignored), "data:" → buffer, anything else → other
// SSE field (event:/id:/retry: — unused by chat completion streams).
func (st *streamState) feedLine(line string, data *[]string) error {
	switch {
	case line == "":
		return st.dispatchData(data)
	case strings.HasPrefix(line, ":"):
		return nil
	case strings.HasPrefix(line, "data:"):
		v := strings.TrimPrefix(line, "data:")
		v = strings.TrimPrefix(v, " ")
		*data = append(*data, v)
		return nil
	default:
		return nil
	}
}

func (st *streamState) dispatchData(data *[]string) error {
	if len(*data) == 0 {
		return nil
	}
	payload := strings.Join(*data, "\n")
	*data = (*data)[:0]
	if strings.TrimSpace(payload) == doneSentinel {
		st.done = true
		return nil
	}
	return st.handleChunk(payload)
}

func (st *streamState) handleChunk(payload string) error {
	var chunk streamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return fmt.Errorf("llm: malformed stream chunk: %w", err)
	}
	// Mid-stream error event (OpenRouter, HTTP 200) — lift to a hard error
	// before touching any deltas of this frame.
	if chunk.Error != nil {
		return chunk.Error.toStreamError()
	}
	if chunk.Usage != nil {
		st.usage = chunk.Usage
	}
	if chunk.Timings != nil {
		st.timings = chunk.Timings
	}
	if len(chunk.Choices) == 0 {
		return nil // usage-only final chunk
	}
	// n>1 is never requested on this path; only the first choice carries the
	// conversation.
	choice := chunk.Choices[0]
	if choice.FinishReason == "error" {
		return &StreamError{Message: "stream finished with finish_reason \"error\""}
	}
	if choice.Delta.Content != "" {
		st.content.WriteString(choice.Delta.Content)
		if err := st.emit(StreamEvent{ContentDelta: choice.Delta.Content}); err != nil {
			return err
		}
	}
	// Reasoning deltas are accumulated but never emitted: v1 requests
	// reasoning.exclude and only falls back to them for providers that
	// ignore the flag (see result()).
	if choice.Delta.Reasoning != "" {
		st.reasoning.WriteString(choice.Delta.Reasoning)
	}
	for i, d := range choice.Delta.ToolCalls {
		if err := st.applyToolCallDelta(i, d); err != nil {
			return err
		}
	}
	if choice.FinishReason != "" && st.finish == "" {
		st.finish = choice.FinishReason
		if err := st.emit(StreamEvent{FinishReason: choice.FinishReason}); err != nil {
			return err
		}
	}
	return nil
}

// applyToolCallDelta merges one fragment into the per-index assembly. pos is
// the fragment's position inside the delta array — the fallback key for
// providers that send complete calls without an index field.
func (st *streamState) applyToolCallDelta(pos int, d toolCallDelta) error {
	key := pos
	if d.Index != nil {
		key = *d.Index
	}
	pc := st.byIndex[key]
	// Providers that omit the index field send each call complete in one
	// delta — across chunks they would all land on array position 0 and
	// silently merge (ID/name overwritten, arguments concatenated). A
	// different non-empty ID on an index-less delta starts a NEW call.
	if pc != nil && d.Index == nil && d.ID != "" && pc.id != "" && pc.id != d.ID {
		key = len(st.calls)
		for st.byIndex[key] != nil {
			key++
		}
		pc = nil
	}
	if pc == nil {
		pc = &pendingToolCall{index: key}
		st.byIndex[key] = pc
		st.calls = append(st.calls, pc)
	}
	if d.ID != "" {
		pc.id = d.ID
	}
	if d.Type != "" {
		pc.typ = d.Type
	}
	if d.Function.Name != "" {
		pc.name = d.Function.Name
	}
	appendArgsFragment(&pc.args, d.Function.Arguments)
	if !pc.nameEmitted && pc.name != "" {
		pc.nameEmitted = true
		return st.emit(StreamEvent{ToolCallName: pc.name})
	}
	return nil
}

// appendArgsFragment normalises one arguments fragment: a JSON string
// fragment (llama.cpp E3 — each delta is valid JSON, so escapes never split
// across fragments) is unquoted and its text appended; a JSON object
// (issue #20198 — arrives complete in one delta) is appended as-is.
func appendArgsFragment(buf *bytes.Buffer, frag json.RawMessage) {
	t := bytes.TrimSpace(frag)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			buf.WriteString(s)
			return
		}
	}
	buf.Write(t)
}

func (st *streamState) result() *StreamResult {
	res := &StreamResult{
		Content:      st.content.String(),
		FinishReason: st.finish,
	}
	// Mirror the non-streaming quirk fix (chatOpenAI): a provider that
	// ignores reasoning.exclude and streams only reasoning deltas would
	// otherwise yield an empty answer.
	if res.Content == "" && st.reasoning.Len() > 0 {
		res.Content = st.reasoning.String()
	}
	sort.Slice(st.calls, func(i, j int) bool { return st.calls[i].index < st.calls[j].index })
	for _, pc := range st.calls {
		typ := pc.typ
		if typ == "" {
			typ = "function"
		}
		// Fragments were normalised on append — only the no-arguments case
		// is mapped here, so Arguments is always non-empty JSON.
		args := bytes.TrimSpace(pc.args.Bytes())
		if len(args) == 0 {
			args = []byte("{}")
		}
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:       pc.id,
			Type:     typ,
			Function: ToolCallFunction{Name: pc.name, Arguments: args},
		})
	}
	switch {
	case st.usage != nil:
		res.PromptTokens = st.usage.PromptTokens
		res.CompletionTokens = st.usage.CompletionTokens
	case st.timings != nil:
		res.PromptTokens = st.timings.PromptN
		res.CompletionTokens = st.timings.PredictedN
	}
	if st.timings != nil && st.timings.DraftN > 0 {
		res.DraftAccept = float64(st.timings.DraftNAccepted) / float64(st.timings.DraftN)
	}
	return res
}
