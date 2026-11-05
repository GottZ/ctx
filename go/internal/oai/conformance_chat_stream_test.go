package oai_test

// Conformance harness, Go half, for the streamed POST /v1/chat/completions
// (wave W02-08, gates G1, G3 and G7; design/02 §4.6, §4.7, §4.10.3, §7): the
// official OpenAI Go SDK (client.Chat.Completions.NewStreaming) against the
// stream route of the harness (newChatRig over the engine streams of
// chat_stream_harness_test.go) for each of the three engine families of the
// live chat chain — SGLang, llama.cpp, OpenRouter — with and without
// stream_options.include_usage. The SDK's own HTTP transport is tapped, so
// every data payload on the wire (all of them, not only the last) is checked
// against the vendored CreateChatCompletionStreamResponse (openai-openapi @
// c2aecc2f, the same commit as the other excerpts) and the SSE framing
// against the relay contract: one id/created/model in every chunk, [DONE]
// exactly once and last, an error frame never followed by [DONE].

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// chatStreamExcerpt is the vendored excerpt of a streamed chat chunk
// (CreateChatCompletionStreamResponse and its transitive $ref closure, same
// commit as schemaExcerpt). The Python harness reads the same file.
const chatStreamExcerpt = "../../conformance/oai/testdata/openapi-c2aecc2f-create-chat-completion-stream-response.json"

// cfStreamError is the mid-stream error object an engine sends instead of
// its next chunk (G7).
const cfStreamError = `{"error":{"message":"boom","type":"server_error","code":"internal"}}`

// wireTap is the SDK's HTTP transport: it counts the requests and keeps the
// body of the last response as the client read it.
type wireTap struct {
	mu       sync.Mutex
	requests int
	last     *tapBody
}

// tapBody tees a response body into its buffer.
type tapBody struct {
	io.ReadCloser
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *tapBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.buf.Write(p[:n])
	b.mu.Unlock()
	return n, err
}

func (w *wireTap) RoundTrip(r *http.Request) (*http.Response, error) {
	w.mu.Lock()
	w.requests++
	w.mu.Unlock()
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	tb := &tapBody{ReadCloser: resp.Body}
	resp.Body = tb
	w.mu.Lock()
	w.last = tb
	w.mu.Unlock()
	return resp, nil
}

// drain reads the last body to its end (the SDK stops at [DONE] or an
// error frame; whatever the route sent after it is part of the wire) and
// returns everything the client received.
func (w *wireTap) drain() []byte {
	w.mu.Lock()
	tb := w.last
	w.mu.Unlock()
	if tb == nil {
		return nil
	}
	_, _ = io.Copy(io.Discard, tb)
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return append([]byte(nil), tb.buf.Bytes()...)
}

func (w *wireTap) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests
}

// cfStreamClient is a Go SDK client against srv over a tap.
func cfStreamClient(srv *httptest.Server, retries int) (openai.Client, *wireTap) {
	tap := &wireTap{}
	return openai.NewClient(option.WithBaseURL(srv.URL+"/v1/"), option.WithAPIKey("ctxm_conformance"),
		option.WithMaxRetries(retries), option.WithHTTPClient(&http.Client{Transport: tap})), tap
}

// cfStreamParams is the streamed request of the harness.
func cfStreamParams(include bool) openai.ChatCompletionNewParams {
	p := openai.ChatCompletionNewParams{
		Model:    "chat",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	}
	if include {
		p.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	}
	return p
}

// cfCheckID reports what is wrong with a chunk id: it must be
// chatcmpl-<server-side UUIDv7>, never the client's X-Request-ID.
func cfCheckID(id string) string {
	suffix, ok := strings.CutPrefix(id, "chatcmpl-")
	switch u, err := uuid.Parse(suffix); {
	case !ok:
		return fmt.Sprintf("id %q lacks the chatcmpl- prefix", id)
	case strings.Contains(id, cfClientRequestID):
		return fmt.Sprintf("id %q carries the client's X-Request-ID", id)
	case err != nil || u.Version() != 7:
		return fmt.Sprintf("id %q: suffix is no UUIDv7 (%v)", id, err)
	}
	return ""
}

// cfWireChunks checks the SSE framing of a stream the client received and
// validates every chunk payload against the stream schema: data and comment
// lines only, one id/created/model/object across the chunks. It returns the
// decoded chunks and the parsed answer.
func cfWireChunks(raw []byte, check func(v any) error, bad func(string, ...any)) ([]map[string]json.RawMessage, streamAnswer) {
	events, err := readEvents(bytes.NewReader(raw), nil)
	if err != nil {
		bad("the wire is no SSE stream: %v", err)
	}
	a := streamAnswer{events: events, raw: string(raw)}
	for i, e := range events {
		if e.other != "" {
			bad("wire event %d: line %q — the relay writes data: and comments only", i, e.other)
		}
	}
	var out []map[string]json.RawMessage
	var first map[string]json.RawMessage
	for _, c := range a.chunks() {
		var v any
		if err := json.Unmarshal([]byte(c), &v); err != nil {
			bad("wire chunk is no JSON: %v (%q)", err, c)
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(c), &m) != nil {
			bad("wire chunk is no object: %q", c)
			continue
		}
		if _, isErr := m["error"]; isErr {
			continue // the error frame is checked by its own gate (G7)
		}
		if err := check(v); err != nil {
			bad("wire chunk fails CreateChatCompletionStreamResponse: %v (%s)", err, c)
		}
		if first == nil {
			first = m
		}
		for _, k := range []string{"id", "created", "model", "object"} {
			if string(m[k]) != string(first[k]) {
				bad("wire chunk %s = %s, the first chunk had %s", k, m[k], first[k])
			}
		}
		out = append(out, m)
	}
	return out, a
}

// cfStreamViolations streams once through the Go SDK over a rig whose only
// row streams in shape sh and returns what a client would trip over — empty
// means conformant.
func cfStreamViolations(t *testing.T, sh streamShape, include bool) []string {
	t.Helper()
	schema := loadSchema(t, chatStreamExcerpt, "CreateChatCompletionStreamResponse")
	up := newUpstream(t, "")
	up.setRespond(sseRespond(sh.steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{up.row("conf", backends.TrustFull, 1)}, 4, 1, withStreamSettings)
	srv := serveHTTP(t, rig.router)
	client, tap := cfStreamClient(srv, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	stream := client.Chat.Completions.NewStreaming(ctx, cfStreamParams(include), option.WithHeader("X-Request-ID", cfClientRequestID))
	var chunks []openai.ChatCompletionChunk
	for stream.Next() {
		chunks = append(chunks, stream.Current())
	}
	if err := stream.Err(); err != nil {
		bad("stream.Err() = %v, want nil", err)
	}
	raw := tap.drain()
	_ = stream.Close()
	if n := up.hits.Load(); n != 1 {
		bad("the upstream saw %d request(s), want 1", n)
	}
	if len(chunks) == 0 {
		return append(out, fmt.Sprintf("the SDK read no chunk; wire %q", raw))
	}

	// The SDK's typed view.
	var content strings.Builder
	usageChunks := 0
	for i, c := range chunks {
		if c.ID != chunks[0].ID || c.Created != chunks[0].Created || c.Model != chunks[0].Model {
			bad("chunk %d: id/created/model %q/%d/%q differ from the first chunk %q/%d/%q", i, c.ID, c.Created, c.Model,
				chunks[0].ID, chunks[0].Created, chunks[0].Model)
		}
		if c.Object != "chat.completion.chunk" {
			bad("chunk %d: object %q, want chat.completion.chunk", i, c.Object)
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
		}
		if c.JSON.Usage.Valid() {
			usageChunks++
			switch {
			case !include:
				bad("chunk %d carries usage without include_usage: %s", i, c.JSON.Usage.Raw())
			case len(c.Choices) != 0 || i != len(chunks)-1:
				bad("usage chunk %d of %d has %d choices, want 0 and the last chunk", i, len(chunks), len(c.Choices))
			case c.Usage.PromptTokens != 11 || c.Usage.CompletionTokens != 3 || c.Usage.TotalTokens != 14:
				bad("usage %+v, want 11/3/14", c.Usage)
			}
		} else if len(c.Choices) == 0 {
			bad("chunk %d has no choices and no usage — client code reading choices[0] breaks", i)
		}
	}
	if include && usageChunks != 1 {
		bad("%d usage chunks with include_usage, want 1", usageChunks)
	}
	if msg := cfCheckID(chunks[0].ID); msg != "" {
		bad("%s", msg)
	}
	if chunks[0].Model != "chat" {
		bad("model %q, want the alias name %q", chunks[0].Model, "chat")
	}
	if got := content.String(); got != streamText {
		bad("content %q, want %q", got, streamText)
	}

	// The wire.
	wire, a := cfWireChunks(raw, schema.Validate, bad)
	if n := a.dones(); n != 1 {
		bad("[DONE] %d times on the wire, want exactly once", n)
	}
	if len(a.events) == 0 || a.events[len(a.events)-1].data != "[DONE]" {
		bad("the wire does not end with [DONE]: %q", raw)
	}
	if len(wire) != len(chunks) {
		bad("%d chunks on the wire, the SDK read %d", len(wire), len(chunks))
	}
	for i, m := range wire {
		if _, has := m["usage"]; has && !include {
			bad("wire chunk %d carries a usage key without include_usage", i)
		}
		if i < len(chunks) && string(m["id"]) != strconv.Quote(chunks[0].ID) {
			bad("wire chunk %d id %s, the SDK read %q", i, m["id"], chunks[0].ID)
		}
	}
	if strings.Contains(string(raw), "reasoning_content") || strings.Contains(string(raw), "THINK-CANARY") {
		bad("the wire carries a reasoning trace")
	}
	return out
}

// TestConformanceGoSDKChatStream is G1 (Go half): every engine shape, with
// and without include_usage, reaches the SDK as a conformant chunk stream —
// stream.Err() nil, the engine's text, the usage chunk 11/3 only for
// include_usage, every wire chunk valid against
// CreateChatCompletionStreamResponse with one id/created/model, [DONE] once.
// Red (base f6c6c189): the route answers stream:true with 400
// unsupported_value.
func TestConformanceGoSDKChatStream(t *testing.T) {
	for _, sh := range streamShapes {
		for _, include := range []bool{true, false} {
			t.Run(sh.name+map[bool]string{true: "/include_usage", false: "/no_usage"}[include], func(t *testing.T) {
				for _, v := range cfStreamViolations(t, sh, include) {
					t.Error(v)
				}
			})
		}
	}
}

// TestConformanceChatStreamSchemaHasTeeth is G1's control: the vendored
// stream schema refuses what is no chunk — the route's own error envelope
// and a chunk without the required id — and accepts a minimal chunk (so the
// refusals come from the schema, not from a broken load).
func TestConformanceChatStreamSchemaHasTeeth(t *testing.T) {
	schema := loadSchema(t, chatStreamExcerpt, "CreateChatCompletionStreamResponse")
	w := httptest.NewRecorder()
	oai.WriteInternal(w)
	const noID = `{"object":"chat.completion.chunk","created":1,"model":"chat","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`
	const valid = `{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"chat","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`
	for name, raw := range map[string][]byte{"error envelope": w.Body.Bytes(), "no id": []byte(noID), "valid": []byte(valid)} {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		err := schema.Validate(v)
		switch {
		case name == "valid" && err != nil:
			t.Errorf("a minimal chunk fails CreateChatCompletionStreamResponse: %v", err)
		case name != "valid" && err == nil:
			t.Errorf("%s passes CreateChatCompletionStreamResponse — the schema check has no teeth", name)
		}
	}
}

// TestConformanceGoSDKChatStreamErrorFrame is G7 (Go side): the engine
// breaks off after its first chunk with an error object — the client has
// the first chunk, then stream.Err() is the SDK's in-band StreamError (not
// an HTTP status: the stream was committed) carrying the route's
// upstream_… code; the wire ends with the error frame (a valid
// ErrorResponse), no [DONE]. Red (base): 400 unsupported_value.
func TestConformanceGoSDKChatStreamErrorFrame(t *testing.T) {
	errSchema := loadErrorSchema(t)
	for _, sh := range streamShapes {
		t.Run(sh.name, func(t *testing.T) {
			up := newUpstream(t, "")
			up.setRespond(sseRespond([]sseStep{{data: sh.chunk("hello", "")}, {data: cfStreamError}}, nil, nil))
			rig := newChatRig(t, []backends.Backend{up.row("conf", backends.TrustFull, 1)}, 4, 1, withStreamSettings)
			srv := serveHTTP(t, rig.router)
			client, tap := cfStreamClient(srv, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stream := client.Chat.Completions.NewStreaming(ctx, cfStreamParams(false))
			var content strings.Builder
			n := 0
			for stream.Next() {
				n++
				for _, ch := range stream.Current().Choices {
					content.WriteString(ch.Delta.Content)
				}
			}
			err := stream.Err()
			raw := tap.drain()
			_ = stream.Close()
			if n == 0 || content.String() != "hello" {
				t.Errorf("the SDK read %d chunk(s) with %q before the error, want the engine's first chunk", n, content.String())
			}
			var se *ssestream.StreamError
			if err == nil || !errors.As(err, &se) {
				t.Fatalf("stream.Err() = %v (%T), want the SDK's in-band StreamError; wire %q", err, err, raw)
			}
			events, rerr := readEvents(bytes.NewReader(raw), nil)
			if rerr != nil {
				t.Errorf("the wire is no SSE stream: %v", rerr)
			}
			a := streamAnswer{events: events, raw: string(raw)}
			code, last := a.errorFrame(t)
			if !strings.HasPrefix(code, "upstream_") || !last {
				t.Errorf("error frame code %q (last data event %v), want an upstream_… code as the last frame; wire %q", code, last, raw)
			}
			if !strings.Contains(err.Error(), `"`+code+`"`) {
				t.Errorf("stream.Err() %q lacks the route's code %q", err, code)
			}
			if n := a.dones(); n != 0 {
				t.Errorf("[DONE] %d times after an error frame, want none", n)
			}
			for _, e := range a.events {
				if e.data == "" || !strings.Contains(e.data, `"error"`) {
					continue
				}
				var v any
				if json.Unmarshal([]byte(e.data), &v) != nil || errSchema.Validate(v) != nil {
					t.Errorf("error frame %s is no valid ErrorResponse", e.data)
				}
			}
		})
	}
}

// TestConformanceGoSDKChatStreamBusy is G3 (Go side, B24): the external
// partition full (slots 2, reserved 1, the one external slot held by another
// consumer) and a short wait budget — a streamed request is refused with a
// real HTTP 429 the SDK exposes as *openai.Error (server_busy, Retry-After
// and retry-after-ms on the response), never a 200 with an error frame; no
// wire call. With one retry allowed the SDK retries it (two requests).
// Red (base): 400 unsupported_value.
func TestConformanceGoSDKChatStreamBusy(t *testing.T) {
	up := newUpstream(t, "")
	up.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{up.row("conf", backends.TrustFull, 1)}, 2, 1, withStreamSettings, withWait(100*time.Millisecond))
	defer occupyExternal(t, rig.disp, up.srv.URL)()
	srv := serveHTTP(t, rig.router)
	for _, retries := range []int{0, 1} {
		client, tap := cfStreamClient(srv, retries)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stream := client.Chat.Completions.NewStreaming(ctx, cfStreamParams(true))
		if stream.Next() {
			t.Errorf("retries %d: the SDK read a chunk %+v of a refused stream", retries, stream.Current())
		}
		err := stream.Err()
		raw := tap.drain()
		_ = stream.Close()
		cancel()
		var apiErr *openai.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("retries %d: stream.Err() = %v (%T), want *openai.Error; wire %q", retries, err, err, raw)
		}
		if apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != oai.CodeServerBusy {
			t.Errorf("retries %d: %d %q, want 429 %s; wire %q", retries, apiErr.StatusCode, apiErr.Code, oai.CodeServerBusy, raw)
		}
		h := apiErr.Response.Header
		if ra, err := strconv.Atoi(h.Get("Retry-After")); err != nil || ra < 1 {
			t.Errorf("retries %d: Retry-After %q, want whole seconds ≥ 1", retries, h.Get("Retry-After"))
		}
		if ms, err := strconv.Atoi(h.Get("retry-after-ms")); err != nil || ms < 1 {
			t.Errorf("retries %d: retry-after-ms %q, want milliseconds ≥ 1", retries, h.Get("retry-after-ms"))
		}
		if ct := h.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("retries %d: a refusal with Content-Type %q — a byte left before the admission", retries, ct)
		}
		if n := tap.count(); n != retries+1 {
			t.Errorf("retries %d: the SDK sent %d request(s), want %d (429 server_busy is retried)", retries, n, retries+1)
		}
	}
	if n := up.hits.Load(); n != 0 {
		t.Errorf("wire hits = %d, want 0 (refused in the queue)", n)
	}
}
