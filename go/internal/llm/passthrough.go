package llm

// Gateway passthrough (design/02 §4.5.2, §4.5.3, §4.11.1; wave W02-06): the
// per-attempt body overlay, the two raw wire sites of the OpenAI-compatible
// gateway and the ONE chain walker every gateway path uses. No HTTP route
// lives here — the consumer is internal/oai (W02-07/W02-08), which never
// talks to a backend itself (wire pin 0, internal/chat census).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llmlog"
)

// UpstreamSpec is the client side of one gateway chat request, parsed and
// allowlist-filtered by internal/oai (W02-07). BuildUpstreamBody adds the
// row side per attempt.
//
// There is deliberately no N field: n > 1 is RC-F (E4, waves W02-12/13), so
// the upstream always produces the engine default of one choice.
type UpstreamSpec struct {
	// Client carries the PASS keys verbatim (design/02 §4.5). The CTX keys
	// (model, messages, stream, stream_options, max_tokens,
	// max_completion_tokens, n, service_tier, reasoning*) are dropped here
	// whatever the caller passed — ctx sets them, never the client.
	Client map[string]json.RawMessage
	// Messages is the message array after the §4.5.1 checks. Required.
	Messages json.RawMessage
	// Role names the model_map entry the row is read for.
	Role   string
	Stream bool
	// MaxTokens is the caller's effective output ceiling (client, alias and
	// gateway defaults and ceilings — W02-07); the row caps only lower it.
	// Required (> 0): the ceiling always comes from the caller.
	MaxTokens int
	// ReasoningOff applies the reasoning-off policy (stage 4).
	ReasoningOff bool
}

// samplingKeys are the spec-named sampling parameters a row's model_map
// params supply as DEFAULTS only (stage 1) — never as overrides of the
// client.
var samplingKeys = [...]string{"temperature", "top_p", "top_k", "min_p", "presence_penalty", "frequency_penalty"}

// reasoningOffBody is the reasoning-off object both internal openai wires
// send (chatOpenAI for think=false, buildStreamBody always).
var reasoningOffBody = mustMarshal(reasoningOption{Enabled: false, Exclude: true})

func mustMarshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// isClientCTXKey reports a key ctx sets itself (design/02 §4.5 class CTX);
// the client's value never reaches the wire.
func isClientCTXKey(k string) bool {
	switch k {
	case "model", "messages", "stream", "stream_options", "max_tokens", "max_completion_tokens", "n", "service_tier":
		return true
	}
	return strings.HasPrefix(k, "reasoning")
}

// BuildUpstreamBody renders one attempt's request body for row b. Stages in
// ascending precedence (design/02 §4.5.2, a later stage wins on a key):
//
//	1  row model_map[role].params sampling — defaults for absent client keys
//	2  client PASS keys, verbatim; spec.Messages
//	4  reasoning off (policy or row think=false), provider-independent
//	5a row model_map[role].params non-sampling, via applyModelParams
//	5  row extra_body + OpenRouter ZDR force, via applyOpenAIBodyExtras
//	3  the frame — model, stream, stream_options, max_tokens; no n
//
// The frame (stage 3) is written LAST so no row param and no extra_body can
// rewrite it: the model the alias routes to, the stream shape the relay
// parses, the max_tokens min() the output ceiling rests on (B10) and the
// absent n (E4). Its keys are disjoint from every other stage's, so the
// ZDR force is still the last word on provider. Stage 6 (band priority,
// A03-W13) does not exist yet; it goes after stage 5.
func BuildUpstreamBody(s UpstreamSpec, b *backends.Backend) ([]byte, error) {
	if s.MaxTokens <= 0 {
		return nil, fmt.Errorf("llm: passthrough spec max_tokens %d: the ceiling comes from the caller", s.MaxTokens)
	}
	if len(bytes.TrimSpace(s.Messages)) == 0 {
		return nil, errors.New("llm: passthrough spec without messages")
	}
	spec := b.ModelFor(s.Role)
	if spec.Model == "" {
		return nil, fmt.Errorf("llm: backend %q has no model for role %q", b.Name, s.Role)
	}
	resolved := *b // copy: never mutate the pool snapshot
	opts, think := applyModelParams(Options{}, spec.Params, &resolved)

	m := make(map[string]json.RawMessage, len(s.Client)+12)
	if err := putSamplingDefaults(m, spec.Params, &opts); err != nil {
		return nil, err
	}
	for k, v := range s.Client {
		if !isClientCTXKey(k) {
			m[k] = v
		}
	}
	m["messages"] = s.Messages
	if s.ReasoningOff || (think != nil && !*think) {
		m["reasoning"] = reasoningOffBody
	}
	// Stage 5a: the same flat last-write-wins merge chatOpenAI does with
	// mergeJSONFields, after the client. The sampling keys left the set in
	// stage 1 (frequency_penalty is the one applyModelParams files under
	// Extra): a default must not come back as an override.
	for k, v := range opts.Extra {
		if isSamplingKey(k) {
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("llm: marshal model_map param %s: %w", k, err)
		}
		m[k] = raw
	}
	if err := applyRowBody(m, b); err != nil {
		return nil, err
	}
	putFrame(m, spec.Model, s.Stream, effectiveMaxTokens(s.MaxTokens, b.ChatMaxTokens(), opts.NumPredict))
	return json.Marshal(m)
}

func isSamplingKey(k string) bool {
	for _, s := range samplingKeys {
		if s == k {
			return true
		}
	}
	return false
}

// putSamplingDefaults is stage 1: the sampling values applyModelParams
// resolves from the row's params, in the internal openai wire's form
// (chatOpenAI): a zero top_p/top_k/min_p/presence_penalty/frequency_penalty
// is the parameter's neutral value and stays off the wire exactly like that
// request's omitempty fields; temperature 0 is greedy decoding, so it follows
// the param's presence. frequency_penalty is the row's own param (Extra) or,
// failing that, repeat_penalty − 1 — chatOpenAI's mapping.
func putSamplingDefaults(m map[string]json.RawMessage, params map[string]any, opts *Options) error {
	defaults := map[string]any{}
	switch params["temperature"].(type) {
	case float64, int: // the forms toFloat reads; anything else left opts at its zero
		defaults["temperature"] = opts.Temperature
	}
	if opts.TopP != 0 {
		defaults["top_p"] = opts.TopP
	}
	if opts.TopK != 0 {
		defaults["top_k"] = opts.TopK
	}
	if opts.MinP != 0 {
		defaults["min_p"] = opts.MinP
	}
	if opts.PresencePenalty != 0 {
		defaults["presence_penalty"] = opts.PresencePenalty
	}
	switch fp, ok := opts.Extra["frequency_penalty"]; {
	case ok:
		defaults["frequency_penalty"] = fp
	case opts.RepeatPenalty > 0 && opts.RepeatPenalty-1.0 != 0:
		defaults["frequency_penalty"] = opts.RepeatPenalty - 1.0
	}
	for k, v := range defaults {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("llm: marshal model_map sampling default %s: %w", k, err)
		}
		m[k] = raw
	}
	return nil
}

// applyRowBody is stage 5 through the ONE implementation of the OpenRouter
// ZDR force (applyOpenAIBodyExtras, W02-01). The helper merges flat and then
// forces provider, so applying it to a seed holding only the current
// provider value and overlaying its output yields exactly what applying it
// to the whole body would — without decoding the messages (up to the
// request cap, vision data URIs included) into Go values and without
// rounding a client number beyond float64 precision (the client map stays
// verbatim).
func applyRowBody(m map[string]json.RawMessage, b *backends.Backend) error {
	seed := []byte("{}")
	if prov, ok := m["provider"]; ok {
		var err error
		if seed, err = json.Marshal(map[string]json.RawMessage{"provider": prov}); err != nil {
			return fmt.Errorf("llm: marshal provider: %w", err)
		}
	}
	out, err := applyOpenAIBodyExtras(seed, b.ExtraBody, b)
	if err != nil {
		return fmt.Errorf("llm: merge extra_body: %w", err)
	}
	var layer map[string]json.RawMessage
	if err := json.Unmarshal(out, &layer); err != nil {
		return fmt.Errorf("llm: merge extra_body: %w", err)
	}
	for k, v := range layer {
		m[k] = v
	}
	return nil
}

// effectiveMaxTokens is stage 3's min(): the caller's ceiling, lowered by the
// row's limits.chat_max_tokens and its model_map max_tokens/num_predict when
// those are > 0 (0 = no cap, the row default).
func effectiveMaxTokens(ceiling, rowCap, modelMapCap int) int {
	v := ceiling
	for _, c := range [...]int{rowCap, modelMapCap} {
		if c > 0 && c < v {
			v = c
		}
	}
	return v
}

func putFrame(m map[string]json.RawMessage, model string, stream bool, maxTokens int) {
	m["model"] = mustMarshal(model)
	m["max_tokens"] = mustMarshal(maxTokens)
	delete(m, "max_completion_tokens")
	delete(m, "n")
	if stream {
		m["stream"] = json.RawMessage("true")
		m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		return
	}
	m["stream"] = json.RawMessage("false")
	delete(m, "stream_options")
}

// ErrPassthroughProtocol refuses a row whose wire does not speak the OpenAI
// chat-completions shape (anthropic: W02-12, RC-F; rerank rows never chat).
// The chain builder (W02-07) keeps such rows off the chain; this is the
// wire's own refusal, before any byte leaves the process.
var ErrPassthroughProtocol = errors.New("llm: passthrough needs an openai-wire row")

func passthroughRow(b *backends.Backend, timeout time.Duration) error {
	switch b.Protocol {
	case backends.ProtocolOpenAI, backends.ProtocolOllama:
	default:
		return fmt.Errorf("%w (backend %q, protocol %q)", ErrPassthroughProtocol, b.Name, b.Protocol)
	}
	if timeout <= 0 {
		return fmt.Errorf("llm: passthrough to %q without an attempt timeout", b.Name)
	}
	return nil
}

// RawChatResult is one non-streamed upstream answer. Body is the response
// verbatim (internal/oai rewrites it, design/02 §4.6); the other fields are
// read from it for the lease, llmlog and the cost column.
type RawChatResult struct {
	Body             []byte
	PromptTokens     int
	CompletionTokens int
	FinishReason     string
	// CostUSD, ServedModel and ProviderRequestID are filled for
	// provider_class=openrouter only — chatOpenAI's provenance gate.
	CostUSD           *float64
	ServedModel       string
	ProviderRequestID string
}

// rawChatMeta is the part of a chat completion ChatRaw reads.
type rawChatMeta struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageBody `json:"usage"`
}

// ChatRaw is the gateway's non-stream wire site: one POST of an already
// built body (BuildUpstreamBody) to b's /v1/chat/completions. The client is
// chosen inside, by the one egress predicate — there is no client and no
// header parameter (B19, R-02-2). maxResp caps the body read (≤
// httpx.MaxResponseBytes; ≤ 0 means that cap). A non-200 is a StatusError
// for the failover classifier, exactly like chatOpenAI.
//
// Headers come ONLY from the row (setChatHeaders: extra_headers, then
// Content-Type and the row's Authorization): no parameter of either wire can
// carry an inbound header, so nothing of the client's request
// (Authorization, X-Api-Key, Cookie, Ctx-*, X-Request-ID) reaches a backend
// (design/04 §5.2 S1, R-02-2, I-F1). A server-side X-Request-ID for
// provider_class=ctx rows (K28) arrives with the federation stamp
// (F-W4/F-W5), at both sites.
func ChatRaw(ctx context.Context, b backends.Backend, body []byte, timeout time.Duration, maxResp int64) (*RawChatResult, error) {
	if err := passthroughRow(&b, timeout); err != nil {
		return nil, err
	}
	if maxResp <= 0 || maxResp > httpx.MaxResponseBytes {
		maxResp = httpx.MaxResponseBytes
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Host+"/v1/chat/completions", bytes.NewReader(body)) //nolint:gosec // G704: host from the F3 backend pool (context_backends), admin-gated; tenant rows dial through ssrfguard (httpClients.For below).
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	setChatHeaders(req, &b)
	resp, err := httpx.DoRetryOnce(httpClients.For(b.EgressGuarded()), req, body)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llm: %w", httpx.NewStatusError(resp, errBody))
	}
	raw, err := io.ReadAll(httpx.CapBody(resp.Body, maxResp))
	if err != nil {
		return nil, fmt.Errorf("llm: read response: %w", err)
	}
	var meta rawChatMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(meta.Choices) == 0 {
		return nil, errors.New("llm: empty choices in response")
	}
	out := &RawChatResult{Body: raw, FinishReason: meta.Choices[0].FinishReason}
	if meta.Usage != nil {
		out.PromptTokens = meta.Usage.PromptTokens
		out.CompletionTokens = meta.Usage.CompletionTokens
	}
	if b.ProviderClass == backends.ProviderOpenRouter {
		if meta.Usage != nil {
			out.CostUSD = meta.Usage.Cost
		}
		out.ServedModel = meta.Model
		out.ProviderRequestID = meta.ID
	}
	return out, nil
}

// RawStreamResult is the outcome of one relayed upstream stream. ChatRawStream
// returns it whenever the backend answered 200 — on a later error too, with
// what was seen so far — so the caller can tell a committed failure (Frames
// > 0: the relay may have written to its client, no failover) from one
// before the first data frame, and charge partial usage.
type RawStreamResult struct {
	// Frames counts the data frames handed to onFrame.
	Frames           int
	PromptTokens     int
	CompletionTokens int
	FinishReason     string
	// openrouter-class provenance only, as on RawChatResult.
	CostUSD           *float64
	ServedModel       string
	ProviderRequestID string
}

// ChatRawStream is the gateway's stream wire site: it POSTs body (a stream
// body from BuildUpstreamBody) and hands every SSE data payload — verbatim,
// the JSON of one chunk — to onFrame, in order. Never relayed: comment lines,
// [DONE] (the relay writes its own end), a mid-stream error object (lifted
// to a StreamError, the classifier's input, design/02 §4.10.3) and a chunk
// with finish_reason "error". maxFrame caps one line and one data frame
// (≤ the harness cap 4 MiB; ≤ 0 means that cap), the whole stream keeps the
// harness's 64 MiB. Client choice and headers as ChatRaw, plus Accept.
func ChatRawStream(ctx context.Context, b backends.Backend, body []byte, timeout time.Duration, maxFrame int64,
	onFrame func(payload []byte) error) (*RawStreamResult, error) {
	if err := passthroughRow(&b, timeout); err != nil {
		return nil, err
	}
	if onFrame == nil {
		return nil, errors.New("llm: passthrough stream without a frame callback")
	}
	frameCap := maxSSEFrameBytes
	if maxFrame > 0 && maxFrame < int64(frameCap) {
		frameCap = int(maxFrame)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Host+"/v1/chat/completions", bytes.NewReader(body)) //nolint:gosec // G704: host from the F3 backend pool (context_backends), admin-gated; tenant rows dial through ssrfguard (httpClients.For below).
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	setChatHeaders(req, &b)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := httpx.DoRetryOnce(httpClients.For(b.EgressGuarded()), req, body)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llm: %w", httpx.NewStatusError(resp, errBody))
	}
	rl := &sseRelay{onFrame: onFrame, data: sseFrame{limit: frameCap}}
	err = rl.run(resp.Body, frameCap)
	res := rl.result()
	if b.ProviderClass != backends.ProviderOpenRouter {
		res.CostUSD, res.ServedModel, res.ProviderRequestID = nil, "", ""
	}
	return res, err
}

// sseRelay is ChatRawStream's frame loop over the harness's SSE building
// blocks (readSSELineMax, sseFrame, streamChunk): the same line grammar and
// lenient end as parseSSEStream, but frames go out raw instead of as events.
type sseRelay struct {
	onFrame func([]byte) error
	data    sseFrame
	frames  int
	finish  string
	done    bool
	usage   *usageBody
	timings *timingsBody
	id      string
	model   string
}

func (rl *sseRelay) run(r io.Reader, frameCap int) error {
	br := bufio.NewReader(httpx.CapBody(r, maxStreamBytes))
	for !rl.done {
		line, readErr := readSSELineMax(br, frameCap)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("llm: read stream: %w", readErr)
		}
		line = strings.TrimRight(line, "\r\n")
		var err error
		switch {
		case line == "":
			err = rl.dispatch()
		case strings.HasPrefix(line, "data:"):
			err = rl.data.add(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if err != nil {
			return err
		}
		if readErr != nil { // io.EOF: flush an unterminated trailing frame
			if err := rl.dispatch(); err != nil {
				return err
			}
			break
		}
	}
	if !rl.done && rl.finish == "" {
		return errors.New("llm: stream ended unexpectedly (no [DONE], no finish_reason)")
	}
	return nil
}

func (rl *sseRelay) dispatch() error {
	payload, ok := rl.data.take()
	if !ok {
		return nil
	}
	if strings.TrimSpace(payload) == doneSentinel {
		rl.done = true
		return nil
	}
	var chunk streamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return fmt.Errorf("llm: malformed stream chunk: %w", err)
	}
	if chunk.Error != nil {
		return chunk.Error.toStreamError()
	}
	if len(chunk.Choices) > 0 {
		switch fr := chunk.Choices[0].FinishReason; {
		case fr == "error":
			return &StreamError{Message: "stream finished with finish_reason \"error\""}
		case fr != "" && rl.finish == "":
			rl.finish = fr
		}
	}
	if chunk.ID != "" {
		rl.id = chunk.ID
	}
	if chunk.Model != "" {
		rl.model = chunk.Model
	}
	if chunk.Usage != nil {
		rl.usage = chunk.Usage
	}
	if chunk.Timings != nil {
		rl.timings = chunk.Timings
	}
	rl.frames++
	if err := rl.onFrame([]byte(payload)); err != nil {
		return fmt.Errorf("llm: stream frame callback: %w", err)
	}
	return nil
}

func (rl *sseRelay) result() *RawStreamResult {
	res := &RawStreamResult{Frames: rl.frames, FinishReason: rl.finish, ServedModel: rl.model, ProviderRequestID: rl.id}
	switch {
	case rl.usage != nil:
		res.PromptTokens = rl.usage.PromptTokens
		res.CompletionTokens = rl.usage.CompletionTokens
		res.CostUSD = rl.usage.Cost
	case rl.timings != nil:
		res.PromptTokens = rl.timings.PromptN
		res.CompletionTokens = rl.timings.PredictedN
	}
	return res
}

// --- the gateway chain walker (design/02 §4.11.1) ---.

// ErrNoContinuation is Acquirer.Continue's answer while no continuation
// acquire exists (A03-W17, K21): a sub-block walk then stays at k = 1.
var ErrNoContinuation = errors.New("llm: continuation acquire not available")

// ErrWalkClass refuses an Acquirer whose class is not in
// WalkRules.AllowedClasses — a planner bug (I-03-1), caught before any
// acquire. It is a code-bug sentinel, deliberately not an AdmissionError:
// no lease was ever asked for (/v1 maps it to 503, K29).
var ErrWalkClass = errors.New("llm: walk class not allowed")

// Acquirer separates the ONE wait point of a gateway request from its
// follow-ups (design/02 §4.11.1, L5): First blocks (the wait budget lives in
// the dispatcher, A03-W4), Next never blocks (TryAcquire semantics — no
// second queue wait across failover), Continue serves a further sub-block of
// the same attempt (A03-W17; ErrNoContinuation until then). Class exposes
// the admission class for the walker's assert. The production
// implementation is the planner's (A03-W3/W5/W17, wired by W02-07);
// llm.Admission is deliberately none — it has no non-blocking door.
// Errors end the walk terminally (acquire-error doctrine, design/01 §4.3).
type Acquirer interface {
	Class() dispatch.Class
	First(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error)
	Next(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error)
	Continue(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error)
}

// WalkRules parameterize WalkChain per gateway path.
type WalkRules struct {
	// WindowRule enables K27 for bad_request (see WalkChain).
	WindowRule bool
	// AllowedClasses is the class assert's set; empty admits nothing.
	AllowedClasses []dispatch.Class
	// Timeout resolves one attempt's wire timeout (alias policy over the
	// row's role timeout, W02-07); nil or ≤ 0 ⇒ b.TimeoutFor(role,
	// ChatTimeout). The walker hands the SAME value to the acquire as its
	// deadline hint and to the attempt as its wire deadline (rule V1).
	Timeout func(b *backends.Backend) time.Duration
}

// AttemptFunc is one gateway attempt on row b under lease: build the body,
// make the wire call, relay. It reports the backend usage for the lease and
// whether it committed — handed a data frame on to its client — which ends
// every failover. The walker owns the lease: Release and ReportUsage are
// its, never the attempt's.
type AttemptFunc func(runCtx context.Context, b *backends.Backend, lease *dispatch.Lease, timeout time.Duration) (usage dispatch.Usage, committed bool, err error)

func (r *WalkRules) admits(c dispatch.Class) error {
	for _, a := range r.AllowedClasses {
		if a == c {
			return nil
		}
	}
	return fmt.Errorf("%w: class %s not in %v", ErrWalkClass, c, r.AllowedClasses)
}

func (r *WalkRules) timeoutFor(b *backends.Backend, role string) time.Duration {
	if r.Timeout != nil {
		if t := r.Timeout(b); t > 0 {
			return t
		}
	}
	return b.TimeoutFor(role, ChatTimeout)
}

// windowBound reports whether a bad_request is subject to the K27 window
// rule: a 4xx other than 404, or a provider error event without a status
// (request-specific by its type). 404 (the row does not know the model) and
// a 3xx (a cross-origin redirect the pool client refused, F-W1) are row
// configuration faults, never the client's — they move on unconditionally,
// and so do the status-less twins of a 404: the anthropic not_found_error
// and the gateway codes not_found/model_not_found (404 in oai.Codes).
func windowBound(err error) bool {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return se.Code >= 400 && se.Code < 500 && se.Code != http.StatusNotFound
	}
	var pe interface{ ProviderErrorType() string }
	if errors.As(err, &pe) {
		switch pe.ProviderErrorType() {
		case "not_found_error", "not_found", "model_not_found":
			return false
		}
	}
	return true
}

// windowAbove reports whether a row of window cand may still serve a prompt
// refused at window floor (the largest KNOWN window that answered
// bad_request): an unknown window (0, NULL) always may, a known one only
// when it is larger.
func windowAbove(cand, floor int) bool {
	return cand <= 0 || cand > floor
}

// WalkChain walks a gateway chain — the ONE walker of every gateway path,
// with ChatChainVia's rules (design/02 §4.11.1) generic over the attempt:
//
//   - class assert before every acquire (I-03-1): ErrWalkClass, 0 acquires;
//   - one lease per attempt, acquired right before the wire call — First for
//     the walk's first acquire, Next for every later one — with `defer
//     Release` (panic-safe) and the attempt's usage reported to it;
//   - an acquire error is terminal (no attempt, no Classify, no report),
//     always an AdmissionError, so no llmlog line is written for it;
//   - a dispatcher abort (preempt/reap) is terminal without a report;
//   - otherwise Classify + report, then STOP on a parent cancel, on a
//     committed attempt (no failover after the first data frame) and on the
//     STOP classes (server_fault, timeout);
//   - window rule K27 (rules.WindowRule) for bad_request: a 404 or 3xx moves
//     on; any other bad_request on a row with a known NumCtx moves on only
//     to a later row whose NumCtx is larger or unknown (0) — smaller/equal
//     rows are skipped without an attempt, and with no such row left the
//     walk stops with the backend's error (the prompt, not the row, is at
//     fault). A refusal by a row of unknown window sets no floor.
//
// attempts carries wire attempts only, one per lease: a row without a model
// for role and a window-skipped row leave no entry (a pseudo-attempt would
// defeat the len(attempts) == 0 admission check, A03-W1 B1). The walker
// writes no llmlog line — the caller does (W02-07).
func WalkChain(ctx context.Context, chain []backends.Backend, role string, acq Acquirer, report ReportFunc,
	rules WalkRules, attempt AttemptFunc,
) (served *backends.Backend, attempts []ChainAttempt, err error) {
	if acq == nil || attempt == nil {
		return nil, nil, errors.New("llm: gateway walk without acquirer or attempt")
	}
	if err := rules.admits(acq.Class()); err != nil {
		slog.Error("llm: gateway walk refused before any acquire", "role", role, "error", err)
		return nil, nil, err
	}
	if len(chain) == 0 {
		return nil, nil, fmt.Errorf("llm: chain for role %q is empty", role)
	}
	var lastErr error
	acquired, floored, floor := false, false, 0
	for i := range chain {
		b := &chain[i]
		if b.ModelFor(role).Model == "" {
			slog.Error("llm: gateway chain backend has no model for role — skipping", "backend", b.Name, "role", role)
			continue
		}
		if floored && !windowAbove(b.NumCtx, floor) {
			slog.Info("llm: gateway window rule skips backend", "backend", b.Name, "role", role, "num_ctx", b.NumCtx, "failed_num_ctx", floor)
			continue
		}
		if err := rules.admits(acq.Class()); err != nil {
			slog.Error("llm: gateway walk refused before acquire", "role", role, "backend", b.Name, "error", err)
			return nil, attempts, err
		}
		timeout := rules.timeoutFor(b, role)
		lease, runCtx, aerr := acquireFor(ctx, acq, &rules, acquired, b, role, timeout)
		if aerr != nil {
			return nil, attempts, aerr
		}
		acquired = true
		waitMs := lease.WaitDur().Milliseconds()

		start := time.Now()
		committed, err := runAttempt(runCtx, b, lease, timeout, attempt)
		elapsed := time.Since(start)
		if err == nil {
			attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: "ok", Ms: elapsed.Milliseconds(), WaitMs: waitMs})
			if report != nil {
				report(b.ID, backends.ClassOK, 0)
			}
			return b, attempts, nil
		}
		if abort := llmlog.AbortClass(runCtx); abort != "" {
			attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: abort, Ms: elapsed.Milliseconds(), WaitMs: waitMs})
			slog.Warn("llm: gateway attempt aborted by dispatcher", "role", role, "backend", b.Name, "abort", abort, "error", err)
			return nil, attempts, err
		}
		class := backends.Classify(err, b.ProviderClass)
		attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: class.String(), Ms: elapsed.Milliseconds(), WaitMs: waitMs})
		lastErr = err
		if report != nil && class != backends.ClassCanceled {
			report(b.ID, class, retryAfterOf(err))
		}
		slog.Warn("llm: gateway attempt failed", "role", role, "backend", b.Name,
			"attempt", len(attempts), "class", class.String(), "committed", committed, "error", err)
		if ctx.Err() != nil || committed || !class.Next() {
			return nil, attempts, lastErr
		}
		// Only a KNOWN window raises the floor: an unknown row's refusal says
		// nothing about how large a known window would have to be (the
		// briefing's "NumCtx > the failed row's" with 0 admits every known
		// window). b passed the floor, so its window is never lower.
		if class == backends.ClassBadRequest && rules.WindowRule && windowBound(err) && b.NumCtx > 0 {
			floored, floor = true, b.NumCtx
			if !laterWindowAbove(chain[i+1:], role, floor) {
				return nil, attempts, lastErr
			}
		}
	}
	if lastErr == nil {
		return nil, attempts, fmt.Errorf("llm: no chain backend serves role %q", role)
	}
	return nil, attempts, fmt.Errorf("llm: chain exhausted for role %q: %w", role, lastErr)
}

// acquireFor leases one attempt: First for the walk's first acquire, Next
// for every later one. Any failure is terminal and comes back as an
// AdmissionError (an Acquirer's bare sentinel gets wrapped, so the
// no-llmlog-line check holds whatever the implementation returns); an
// Acquirer that answers without a lease or run context is refused as one,
// and a lease it returned next to an error is released.
func acquireFor(ctx context.Context, acq Acquirer, rules *WalkRules, acquired bool, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	acquire := acq.First
	if acquired {
		acquire = acq.Next
	}
	lease, runCtx, err := acquire(ctx, b, role, timeout)
	if err == nil && (lease == nil || runCtx == nil) {
		err = errors.New("llm: acquirer returned no lease")
	}
	if err != nil {
		if lease != nil {
			lease.Release() // a contract breach must not leak the slot
		}
		if !IsAdmissionError(err) {
			err = &AdmissionError{Err: err, Backend: b.Name, Host: b.Host}
		}
		return nil, nil, err
	}
	// The class assert also covers what was ADMITTED: a lease in another
	// class than the Acquirer declared (a B8 downgrade, a planner bug) is
	// handed back before any wire contact — a code-bug sentinel, as above.
	if cerr := rules.admits(lease.Class()); cerr != nil {
		lease.Release()
		slog.Error("llm: gateway lease admitted outside the walk's classes", "role", role, "backend", b.Name, "declared", acq.Class(), "error", cerr)
		return nil, nil, cerr
	}
	return lease, runCtx, nil
}

// laterWindowAbove reports whether a later row may still serve the prompt.
func laterWindowAbove(rest []backends.Backend, role string, floor int) bool {
	for i := range rest {
		if rest[i].ModelFor(role).Model != "" && windowAbove(rest[i].NumCtx, floor) {
			return true
		}
	}
	return false
}

// runAttempt runs one attempt under its lease; defer is the only allowed
// release form (B1: panic-safe). Usage goes to the lease when the attempt
// succeeded or reported any (a partial stream).
func runAttempt(runCtx context.Context, b *backends.Backend, lease *dispatch.Lease, timeout time.Duration, attempt AttemptFunc) (bool, error) {
	defer lease.Release()
	usage, committed, err := attempt(runCtx, b, lease, timeout)
	if err == nil || usage != (dispatch.Usage{}) {
		lease.ReportUsage(usage)
	}
	return committed, err
}
