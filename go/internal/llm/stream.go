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

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
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
	// ProviderBlocks are opaque content blocks the provider wants back
	// unchanged — on the anthropic wire the assistant turn's content as the
	// API produced it (text, thinking / redacted_thinking, tool_use, in
	// block order; StreamResult.ProviderBlocks), replayed verbatim because
	// the API rejects thinking blocks in the latest assistant turn that
	// were reordered or rebuilt. A list holding only reasoning blocks is
	// replayed ahead of the turn's text and calls instead. Carried IN
	// MEMORY across the iterations of one turn only (the harness does not
	// persist them; a later turn's history replays without them, and
	// dropping every thinking block keeps a request valid). json:"-": never
	// a field of the OpenAI wire's message object.
	ProviderBlocks []json.RawMessage `json:"-"`
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
	// CacheReadTokens / CacheCreationTokens are filled by the anthropic wire
	// only (ChatResponse's twins): already inside PromptTokens, they travel
	// for llmlog metadata. Zero on the openai wire.
	CacheReadTokens     int
	CacheCreationTokens int
	// ProviderRequestID is the provider's id for this answer, for audit: the
	// anthropic wire's message id, or on the openai wire the response id of
	// an openrouter-class backend (GET /api/v1/generation?id=…).
	ProviderRequestID string
	// CostUSD and ServedModel are the stream twins of ChatResponse's
	// provider fields (G29), filled for provider_class=openrouter only:
	// CostUSD from usage.cost of the final usage chunk, ServedModel from the
	// chunks' top-level model (models-fallback can answer with another model
	// than the one requested). No other wire reports a cost — the anthropic
	// Messages API has none on either path — so they stay nil/"" there and
	// llmlog's cost_usd stays NULL. The tenant cost budget sums exactly this
	// column (backends/quota.go).
	CostUSD     *float64
	ServedModel string
	// ProviderBlocks are the answer's content blocks in the API's order
	// (anthropic wire, only when the answer carried a reasoning block), to
	// be echoed back via ChatMsg.ProviderBlocks when the harness replays
	// this assistant turn inside the same tool loop. nil on the openai wire.
	ProviderBlocks []json.RawMessage
}

// StreamError is an error event delivered inside an HTTP-200 SSE stream.
// OpenRouter reports provider failures this way (error object in the data
// frame, finish_reason "error") — callers can errors.As on it to separate
// backend-reported failures from transport errors.
type StreamError struct {
	Code    string // provider code, number or string ("502", "server_error")
	Message string
}

// ProviderErrorType exposes the provider's error type string to the failover
// classifier (backends.Classify) without a backends→llm import: the
// anthropic wire's mid-stream error event names the same types its HTTP
// errors do (overloaded_error, rate_limit_error, …), and the classifier maps
// them onto the classes the equivalent status codes get. OpenRouter's
// numeric codes match nothing there and keep their stop-the-chain default.
func (e *StreamError) ProviderErrorType() string { return e.Code }

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
// hatch: OpenRouter provider/zdr objects, per-backend reasoning override);
// ChatStream then runs the OpenRouter ZDR force on top (applyOpenAIBodyExtras).
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

// ChatStreamBackend is the protocol dispatch of the streaming path: the
// anthropic wire (anthropic_stream.go) for ProtocolAnthropic, ChatStream
// (the OpenAI-compatible wire) for everything else — ollama rows included,
// which serve /v1/chat/completions too and were always streamed that way.
// The chat harness calls this; ChatStream stays exported as the openai
// implementation its tests drive directly.
func ChatStreamBackend(ctx context.Context, b backends.Backend, model string,
	msgs []ChatMsg, tools []ToolDef, opts Options, timeout time.Duration,
	onEvent func(StreamEvent) error) (*StreamResult, error) {
	if b.Protocol == backends.ProtocolAnthropic {
		return chatStreamAnthropic(ctx, b, model, msgs, tools, opts, timeout, onEvent)
	}
	return ChatStream(ctx, b, model, msgs, tools, opts, b.ExtraBody, timeout, onEvent)
}

// ChatStream sends a streaming chat request with optional tool definitions
// over the OpenAI-compatible wire (the streaming protocol of F6; the
// anthropic wire joined via ChatStreamBackend) and assembles the response. onEvent is invoked for content fragments, tool
// call starts and the finish reason; an error from onEvent aborts the
// stream. Cancelling ctx aborts the in-flight generation (the HTTP request
// to the backend is dropped, llama.cpp frees its slot).
//
// httpx.DoRetryOnce covers the connection setup only: a transport error
// after response bytes arrived surfaces as a read error and is NOT replayed
// — no silent double generation or double tool execution mid-stream.
//
// b carries host, key and scope; the scope picks the client (a tenant row
// dials through ssrfguard, Backend.EgressGuarded). extraBody stays a separate
// parameter so tests can drive the wire without a full row. Body and header
// policy are chatOpenAI's (W02-01): the OpenRouter ZDR force and the row's
// extra_headers come from b, never from extraBody.
func ChatStream(ctx context.Context, b backends.Backend, model string,
	msgs []ChatMsg, tools []ToolDef, opts Options, extraBody map[string]any,
	timeout time.Duration, onEvent func(StreamEvent) error) (*StreamResult, error) {
	body, err := buildStreamBody(model, msgs, tools, opts, extraBody)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}
	// The ZDR force runs AFTER the extraBody merge, so extra_body can only
	// tighten. nil: buildStreamBody already merged; the force is b's alone.
	if body, err = applyOpenAIBodyExtras(body, nil, &b); err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Host+"/v1/chat/completions", bytes.NewReader(body)) //nolint:gosec // G704: host from the F3 backend pool (context_backends), admin-gated; tenant rows dial through ssrfguard (httpClients.For below).
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	// extra_headers first, then the hop stamp, core headers last
	// (setChatHeaders); Accept is the stream's core header and goes on after
	// them.
	hop := b.HopTarget(fedhop.WireChat)
	setChatHeaders(req, &b, hop)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := httpx.DoRetryOnce(httpClients.For(b.EgressGuarded()), req, body)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The answer headers before the status and before the first SSE byte: a
	// 508 echoing the own token must reach the observer, too.
	if err := fedhop.Observe(ctx, hop, resp.StatusCode, resp.Header); err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// Typed StatusError so the F3 failover classifier (backends.Classify)
		// sees the status code — the streaming path needs the same
		// errors.As-able error the non-streaming wire clients return, or a
		// 502/503/504 before the first byte could never fail over (§2.3/R4).
		// The "%w" wrap keeps the rendered string byte-identical.
		return nil, fmt.Errorf("llm: %w", httpx.NewStatusError(resp, errBody))
	}

	res, err := parseSSEStream(resp.Body, onEvent)
	if err != nil {
		return nil, err
	}
	// Provider provenance is an openrouter-class contract, exactly as on the
	// non-streaming wire (chatOpenAI): local /v1 servers echo an id and a
	// model string too, but only OpenRouter's model may legitimately differ
	// from the requested one and only OpenRouter reports a cost — gating
	// here keeps the web-chat row's model column and cost_usd row-faithful
	// for every other backend.
	if b.ProviderClass != backends.ProviderOpenRouter {
		res.CostUSD = nil
		res.ServedModel = ""
		res.ProviderRequestID = ""
	}
	return res, nil
}

// --- SSE parsing ---.

// streamChunk is one data frame of an OpenAI-compatible stream. Error is the
// OpenRouter mid-stream error object (HTTP 200); Timings is llama.cpp's
// final-chunk telemetry. ID and Model are the top-level response id and the
// model that answered; ChatStream keeps them for openrouter-class backends.
type streamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
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

// usageBody is the usage object of the final chunk (stream_options
// include_usage). Cost is OpenRouter's usage.cost in USD — the field the
// non-streaming wire reads too (client.go); nil when the provider sends none.
type usageBody struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	Cost             *float64 `json:"cost"`
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
	id        string // last non-empty top-level id
	model     string // last non-empty top-level model
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

// Stream caps (GHSA 2026-10-31 §3). A stream is consumed incrementally, so
// its memory risk is one unterminated line or one data frame that never ends
// — ReadString would buffer either without bound. maxSSEFrameBytes bounds a
// line and a buffered multi-line data frame: a legitimate frame is one delta
// (a few tokens, or one tool call's arguments — kilobytes), so 4 MiB is three
// orders of magnitude of headroom; the frame is measured as it will be
// dispatched, joining newlines included, so empty data: lines count (§D).
// maxStreamBytes bounds the whole answer: a 128000-token output at one token
// per frame and ≤ 300 bytes of framing per chunk (OpenRouter's metadata-rich
// chunks) is ≈ 38 MiB, so 64 MiB keeps the longest legitimate stream while
// ending a backend that streams forever.
const (
	maxSSEFrameBytes       = 4 << 20
	maxStreamBytes   int64 = 64 << 20
)

// readSSELine is bufio.Reader.ReadString('\n') with the frame cap: the line
// (terminator included) is returned with the reader's error, exactly like
// ReadString, unless it grows past maxSSEFrameBytes.
func readSSELine(br *bufio.Reader) (string, error) {
	return readSSELineMax(br, maxSSEFrameBytes)
}

// readSSELineMax is readSSELine with a caller-chosen cap (the gateway relay's
// gateway frame cap, ChatRawStream); limit is at most maxSSEFrameBytes there.
func readSSELineMax(br *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			return "", fmt.Errorf("%w: SSE line over %d bytes", httpx.ErrResponseTooLarge, limit)
		}
		line = append(line, chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return string(line), err
		}
	}
}

// sseFrame is the data of one not-yet-dispatched SSE frame (a server can
// send data: lines without ever sending the blank line). Lines are joined as
// they arrive — each one after the first behind the newline the SSE spec puts
// between them — so the buffer is always the payload dispatchData hands on:
// the frame cap is one comparison per line (§D: re-measuring the buffered
// lines on every line was quadratic in their number), an empty data: line
// weighs its newline, and the buffered frame never holds more than
// maxSSEFrameBytes, however its lines are cut.
//
// Two ways in (FOLGE-S): the chat harness adds strings (add, take), the
// gateway relay adds slices of its line buffer (addBytes, takeBytes) — a
// frame of one line stays that slice (one, no copy) until it is taken or a
// second line arrives; only then are the lines joined into buf, which is
// reused frame after frame and grows like a line buffer (growLine).
type sseFrame struct {
	buf   []byte
	one   []byte // the frame's only line, held by reference (addBytes)
	held  bool   // one is the frame: the caller's line buffer is lent to it
	lines int
	// limit is the frame cap; 0 = maxSSEFrameBytes (every chat-harness
	// parser). The gateway relay sets its own (ChatRawStream's maxFrame).
	limit int
	// grow books the buffer a growth of buf discards (the relay's growth
	// booking; nil: none).
	grow func(int) error
}

// room checks that a line of n bytes fits the frame and returns the bytes
// of the separator in front of it.
func (f *sseFrame) room(n int) (sep int, limit int, err error) {
	limit = f.limit
	if limit <= 0 {
		limit = maxSSEFrameBytes
	}
	if f.lines > 0 {
		sep = 1
	}
	size := len(f.buf)
	if f.held {
		size = len(f.one)
	}
	if size+sep+n > limit {
		return 0, 0, fmt.Errorf("llm: %w: SSE frame over %d bytes", httpx.ErrResponseTooLarge, limit)
	}
	return sep, limit, nil
}

func (f *sseFrame) add(v string) error {
	sep, limit, err := f.room(len(v))
	if err != nil {
		return err
	}
	if f.lines == 0 {
		f.buf = f.buf[:0]
	}
	f.buf, _ = growLine(f.buf, sep+len(v), limit, nil)
	if sep > 0 {
		f.buf = append(f.buf, '\n')
	}
	f.buf = append(f.buf, v...)
	f.lines++
	return nil
}

// take returns the frame's payload and empties it; ok is false when no data
// line arrived since the last dispatch (a blank line between frames, or EOF
// right after one).
func (f *sseFrame) take() (payload string, ok bool) {
	if f.lines == 0 {
		return "", false
	}
	payload = string(f.buf)
	f.buf = f.buf[:0]
	f.lines = 0
	return payload, true
}

// addBytes adds the data line v, a slice of the caller's line buffer. The
// first line of a frame is kept by reference: until the frame is taken or
// a second line joins it, the caller reads its next line into another
// buffer (holds). A second line joins the first into buf (both are intact
// then: the first in the lent buffer, the second in the other).
func (f *sseFrame) addBytes(v []byte) error {
	sep, limit, err := f.room(len(v))
	if err != nil {
		return err
	}
	switch {
	case f.lines == 0:
		f.one, f.held = v, true
	case f.held:
		if f.buf, err = growLine(f.buf[:0], len(f.one)+1+len(v), limit, f.grow); err != nil {
			return err
		}
		f.buf = append(append(append(f.buf, f.one...), '\n'), v...)
		f.one, f.held = nil, false
	default:
		if f.buf, err = growLine(f.buf, sep+len(v), limit, f.grow); err != nil {
			return err
		}
		f.buf = append(append(f.buf, '\n'), v...)
	}
	f.lines++
	return nil
}

// holds reports whether the frame holds a line of the caller's buffer by
// reference: the next line must be read into another one.
func (f *sseFrame) holds() bool { return f.held }

// takeBytes returns the frame's payload and empties it, as take. The
// payload is a slice of the caller's line buffer (a frame of one line) or of
// the frame's own buffer: valid until the caller's next read or the next
// addBytes.
func (f *sseFrame) takeBytes() (payload []byte, ok bool) {
	if f.lines == 0 {
		return nil, false
	}
	payload = f.buf
	if f.held {
		payload = f.one
	}
	f.buf, f.one, f.held, f.lines = f.buf[:0], nil, false, 0
	return payload, true
}

// parseSSEStream reads SSE frames until [DONE] or EOF. Comment lines
// (": OPENROUTER PROCESSING") are skipped per SSE spec; multi-line data
// fields are joined with newlines before JSON decoding. Lenient endings:
// EOF after a finish_reason but before [DONE] is accepted (the result is
// complete); EOF before any finish_reason is a hard error — mid-stream
// connection death must not look like a finished turn.
func parseSSEStream(r io.Reader, onEvent func(StreamEvent) error) (*StreamResult, error) {
	st := &streamState{onEvent: onEvent, byIndex: map[int]*pendingToolCall{}}
	br := bufio.NewReader(httpx.CapBody(r, maxStreamBytes))
	var data sseFrame
	for {
		line, readErr := readSSELine(br)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			// The error wins (§D): the line it cut short is no line, and
			// its empty stand-in must not dispatch the buffered frame as if
			// the blank separator had arrived.
			return nil, fmt.Errorf("llm: read stream: %w", readErr)
		}
		if err := st.feedLine(strings.TrimRight(line, "\r\n"), &data); err != nil {
			return nil, err
		}
		if st.done {
			break
		}
		if readErr != nil { // io.EOF — any other error returned above
			// Flush an unterminated trailing frame (server ended without
			// the final blank line), then leave the loop.
			if err := st.dispatchData(&data); err != nil {
				return nil, err
			}
			break
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
func (st *streamState) feedLine(line string, data *sseFrame) error {
	switch {
	case line == "":
		return st.dispatchData(data)
	case strings.HasPrefix(line, ":"):
		return nil
	case strings.HasPrefix(line, "data:"):
		v := strings.TrimPrefix(line, "data:")
		v = strings.TrimPrefix(v, " ")
		return data.add(v)
	default:
		return nil
	}
}

func (st *streamState) dispatchData(data *sseFrame) error {
	payload, ok := data.take()
	if !ok {
		return nil
	}
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
	if chunk.ID != "" {
		st.id = chunk.ID
	}
	if chunk.Model != "" {
		st.model = chunk.Model
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
	// Provider provenance travels raw here; ChatStream keeps it for
	// openrouter-class backends only (the gate chatOpenAI applies).
	res.ProviderRequestID = st.id
	res.ServedModel = st.model
	switch {
	case st.usage != nil:
		res.PromptTokens = st.usage.PromptTokens
		res.CompletionTokens = st.usage.CompletionTokens
		res.CostUSD = st.usage.Cost
	case st.timings != nil:
		res.PromptTokens = st.timings.PromptN
		res.CompletionTokens = st.timings.PredictedN
	}
	if st.timings != nil && st.timings.DraftN > 0 {
		res.DraftAccept = float64(st.timings.DraftNAccepted) / float64(st.timings.DraftN)
	}
	return res
}
