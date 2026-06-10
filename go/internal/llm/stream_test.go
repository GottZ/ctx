package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// serveFixture streams the named testdata file as an SSE response body.
func serveFixture(t *testing.T, name string) *httptest.Server {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(raw)
	}))
}

// collectEvents returns an onEvent callback appending into events.
func collectEvents(events *[]StreamEvent) func(StreamEvent) error {
	return func(ev StreamEvent) error {
		*events = append(*events, ev)
		return nil
	}
}

func argsAsMap(t *testing.T, tc ToolCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(tc.Function.Arguments, &m); err != nil {
		t.Fatalf("arguments %q not a JSON object: %v", tc.Function.Arguments, err)
	}
	return m
}

// --- fixture parser tests (live captures, herbert b9585) ---.

func TestChatStreamToolCallFixture(t *testing.T) {
	srv := serveFixture(t, "llamacpp_tool_call.sse")
	defer srv.Close()

	var events []StreamEvent
	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, collectEvents(&events))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", res.FinishReason)
	}
	if res.Content != "" {
		t.Errorf("content = %q, want empty", res.Content)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "cpY1IW3Rs5H69YRdOHKQUm75F6u9aFOs" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v, want id/function/get_weather", tc)
	}
	// Assembled from 5 string fragments: { + "city":" + Berlin + " + }.
	if got := string(tc.Function.Arguments); got != `{"city":"Berlin"}` {
		t.Errorf("arguments = %q, want {\"city\":\"Berlin\"}", got)
	}
	if want := map[string]any{"city": "Berlin"}; !reflect.DeepEqual(argsAsMap(t, tc), want) {
		t.Errorf("arguments map = %v, want %v", argsAsMap(t, tc), want)
	}
	if res.PromptTokens != 314 || res.CompletionTokens != 26 {
		t.Errorf("tokens = %d/%d, want 314/26", res.PromptTokens, res.CompletionTokens)
	}
	if res.DraftAccept != 1.0 {
		t.Errorf("draft accept = %f, want 1.0 (18/18)", res.DraftAccept)
	}

	var names, finishes, deltas []string
	for _, ev := range events {
		if ev.ToolCallName != "" {
			names = append(names, ev.ToolCallName)
		}
		if ev.FinishReason != "" {
			finishes = append(finishes, ev.FinishReason)
		}
		if ev.ContentDelta != "" {
			deltas = append(deltas, ev.ContentDelta)
		}
	}
	if len(names) != 1 || names[0] != "get_weather" {
		t.Errorf("tool_call_start events = %v, want exactly one get_weather", names)
	}
	if len(finishes) != 1 || finishes[0] != "tool_calls" {
		t.Errorf("finish events = %v, want exactly one tool_calls", finishes)
	}
	if len(deltas) != 0 {
		t.Errorf("content deltas = %v, want none", deltas)
	}
	// tool_call_start must precede the finish event.
	if events[len(events)-1].FinishReason == "" {
		t.Errorf("last event = %+v, want the finish event", events[len(events)-1])
	}
}

func TestChatStreamContentFixture(t *testing.T) {
	srv := serveFixture(t, "llamacpp_content.sse")
	defer srv.Close()

	var events []StreamEvent
	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, collectEvents(&events))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if res.Content != "streaming smoke ok" {
		t.Errorf("content = %q, want streaming smoke ok", res.Content)
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish = %q, want stop", res.FinishReason)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("tool calls = %d, want 0", len(res.ToolCalls))
	}
	if res.PromptTokens != 30 || res.CompletionTokens != 5 {
		t.Errorf("tokens = %d/%d, want 30/5", res.PromptTokens, res.CompletionTokens)
	}
	var joined strings.Builder
	for _, ev := range events {
		joined.WriteString(ev.ContentDelta)
	}
	if joined.String() != res.Content {
		t.Errorf("joined deltas = %q, want %q", joined.String(), res.Content)
	}
}

func TestChatStreamToolRoundtripFixture(t *testing.T) {
	// The post-tool-result turn (E6): assistant tool_calls + tool message in
	// the request, final prose answer in the stream.
	srv := serveFixture(t, "llamacpp_tool_roundtrip.sse")
	defer srv.Close()

	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if res.FinishReason != "stop" || len(res.ToolCalls) != 0 {
		t.Errorf("finish = %q, tool calls = %d, want stop/0", res.FinishReason, len(res.ToolCalls))
	}
	if !strings.Contains(res.Content, "21°C") || !strings.Contains(res.Content, "sunny") {
		t.Errorf("content = %q, want the tool result woven in", res.Content)
	}
	if res.PromptTokens != 374 || res.CompletionTokens != 23 {
		t.Errorf("tokens = %d/%d, want 374/23", res.PromptTokens, res.CompletionTokens)
	}
	if want := 14.0 / 16.0; res.DraftAccept != want {
		t.Errorf("draft accept = %f, want %f", res.DraftAccept, want)
	}
}

// --- OpenRouter hardening (negative probes of the wave gate) ---.

// TestChatStreamOpenRouterCommentFrames proves that ": OPENROUTER PROCESSING"
// keep-alive comments interleaved into the stream — including mid-assembly of
// fragmented tool arguments — do not disturb parsing. A naive parser that
// JSON-decodes every line fails this fixture (red→green proven by removing
// the comment branch in feedLine).
func TestChatStreamOpenRouterCommentFrames(t *testing.T) {
	srv := serveFixture(t, "openrouter_comments.sse")
	defer srv.Close()

	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if res.FinishReason != "tool_calls" || len(res.ToolCalls) != 1 {
		t.Fatalf("finish = %q, tool calls = %d, want tool_calls/1", res.FinishReason, len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "call_demo_001" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v, want call_demo_001/get_weather", tc)
	}
	if want := map[string]any{"city": "Berlin"}; !reflect.DeepEqual(argsAsMap(t, tc), want) {
		t.Errorf("arguments map = %v, want %v", argsAsMap(t, tc), want)
	}
	if res.PromptTokens != 280 || res.CompletionTokens != 18 {
		t.Errorf("tokens = %d/%d, want 280/18", res.PromptTokens, res.CompletionTokens)
	}
	if res.DraftAccept != 0 {
		t.Errorf("draft accept = %f, want 0 (no timings)", res.DraftAccept)
	}
}

// TestChatStreamOpenRouterMidStreamError proves that an error event inside an
// HTTP-200 stream (OpenRouter provider failure) is lifted to a hard error. A
// parser without the error branch returns the partial content as success
// (red→green proven by removing the chunk.Error + finish_reason=="error"
// branches in handleChunk).
func TestChatStreamOpenRouterMidStreamError(t *testing.T) {
	srv := serveFixture(t, "openrouter_midstream_error.sse")
	defer srv.Close()

	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err == nil {
		t.Fatalf("want error, got success: %+v", res)
	}
	var se *StreamError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v (%T), want *StreamError", err, err)
	}
	if se.Code != "502" || se.Message != "Provider returned error" {
		t.Errorf("stream error = %+v, want code 502 / provider message", se)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil on mid-stream error", res)
	}
}

// TestChatStreamFinishReasonErrorWithoutObject covers the error shape where
// only finish_reason "error" arrives (no error object in the frame).
func TestChatStreamFinishReasonErrorWithoutObject(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"error\"}]}\n\n" +
		"data: [DONE]\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stream))
	}))
	defer srv.Close()

	_, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	var se *StreamError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want *StreamError on finish_reason error", err)
	}
}

// --- arguments normalisation (string form vs object form, issue #20198) ---.

// TestChatStreamArgumentsObjectForm proves that the object form yields the
// exact same map as the llama.cpp string-fragment form. A strict
// string-only parser drops the object fragment and assembles "{}" (red→green
// proven by hardening appendArgsFragment to strings only).
func TestChatStreamArgumentsObjectForm(t *testing.T) {
	objSrv := serveFixture(t, "arguments_object_form.sse")
	defer objSrv.Close()
	strSrv := serveFixture(t, "llamacpp_tool_call.sse")
	defer strSrv.Close()

	objRes, err := ChatStream(context.Background(), objSrv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("object form: %v", err)
	}
	strRes, err := ChatStream(context.Background(), strSrv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("string form: %v", err)
	}
	if len(objRes.ToolCalls) != 1 || len(strRes.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d/%d, want 1/1", len(objRes.ToolCalls), len(strRes.ToolCalls))
	}
	objMap := argsAsMap(t, objRes.ToolCalls[0])
	strMap := argsAsMap(t, strRes.ToolCalls[0])
	if !reflect.DeepEqual(objMap, strMap) {
		t.Errorf("object form %v != string form %v — normalisation broken", objMap, strMap)
	}
}

func TestChatStreamEmptyArgumentsNormalizedToObject(t *testing.T) {
	// A no-parameter tool call: name arrives, arguments never do.
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_noargs\",\"type\":\"function\",\"function\":{\"name\":\"get_time\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stream))
	}))
	defer srv.Close()

	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := string(res.ToolCalls[0].Function.Arguments); got != "{}" {
		t.Errorf("arguments = %q, want {} for argument-less call", got)
	}
}

func TestNormalizeArgs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "{}"},
		{"null literal", "null", "{}"},
		{"whitespace only", "  \n", "{}"},
		{"object passthrough", `{"city":"Berlin"}`, `{"city":"Berlin"}`},
		{"string form unquoted", `"{\"city\":\"Berlin\"}"`, `{"city":"Berlin"}`},
		{"empty string form", `""`, "{}"},
		{"leading whitespace string form", ` "{\"a\":1}"`, `{"a":1}`},
		{"array passthrough", `[1,2]`, `[1,2]`},
		{"malformed string kept raw", `"unterminated`, `"unterminated`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(normalizeArgs(json.RawMessage(tt.in)))
			if got != tt.want {
				t.Errorf("normalizeArgs(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- parallel/defensive tool-call assembly ---.

func TestChatStreamParallelToolCallsByIndex(t *testing.T) {
	// Two calls, fragments interleaved across chunks — assembly is keyed by
	// index, result ordered by index.
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"get_time\",\"arguments\":\"{\\\"tz\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Berlin\\\"}\"}},{\"index\":1,\"function\":{\"arguments\":\"\\\"UTC\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stream))
	}))
	defer srv.Close()

	var events []StreamEvent
	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, collectEvents(&events))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(res.ToolCalls))
	}
	a, b := res.ToolCalls[0], res.ToolCalls[1]
	if a.ID != "call_a" || string(a.Function.Arguments) != `{"city":"Berlin"}` {
		t.Errorf("call 0 = %s %s, want call_a {\"city\":\"Berlin\"}", a.ID, a.Function.Arguments)
	}
	if b.ID != "call_b" || string(b.Function.Arguments) != `{"tz":"UTC"}` {
		t.Errorf("call 1 = %s %s, want call_b {\"tz\":\"UTC\"}", b.ID, b.Function.Arguments)
	}
	var names []string
	for _, ev := range events {
		if ev.ToolCallName != "" {
			names = append(names, ev.ToolCallName)
		}
	}
	if !reflect.DeepEqual(names, []string{"get_weather", "get_time"}) {
		t.Errorf("tool_call_start events = %v, want one per call in order", names)
	}
}

func TestChatStreamToolCallWithoutIndexField(t *testing.T) {
	// Mistral-style providers send complete calls without an index field —
	// the array position is the fallback assembly key.
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_x\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"Berlin\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stream))
	}))
	defer srv.Close()

	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_x" {
		t.Fatalf("tool calls = %+v, want one call_x", res.ToolCalls)
	}
	if got := string(res.ToolCalls[0].Function.Arguments); got != `{"city":"Berlin"}` {
		t.Errorf("arguments = %q, want object form", got)
	}
}

func TestChatStreamIndexlessCallsAcrossChunksStayDistinct(t *testing.T) {
	// Two COMPLETE index-less calls in separate chunks: both sit at array
	// position 0 — without ID discrimination they would merge into one call
	// with concatenated arguments.
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"Berlin\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"get_time\",\"arguments\":\"{\\\"tz\\\":\\\"UTC\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stream))
	}))
	defer srv.Close()

	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2 distinct calls", res.ToolCalls)
	}
	a, b := res.ToolCalls[0], res.ToolCalls[1]
	if a.ID != "call_a" || string(a.Function.Arguments) != `{"city":"Berlin"}` {
		t.Errorf("call 0 = %s %s, want call_a city Berlin", a.ID, a.Function.Arguments)
	}
	if b.ID != "call_b" || string(b.Function.Arguments) != `{"tz":"UTC"}` {
		t.Errorf("call 1 = %s %s, want call_b tz UTC", b.ID, b.Function.Arguments)
	}
}

// --- stream endings ---.

func TestChatStreamEOFWithoutFinishIsError(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"},\"finish_reason\":null}]}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stream))
	}))
	defer srv.Close()

	_, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "ended unexpectedly") {
		t.Fatalf("err = %v, want stream-ended-unexpectedly", err)
	}
}

func TestChatStreamEOFAfterFinishWithoutDoneIsAccepted(t *testing.T) {
	// Lenient ending: finish_reason arrived, [DONE] did not (server closed
	// early). The result is complete — usage may be missing, that is the
	// documented 0-tokens convention.
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(stream))
	}))
	defer srv.Close()

	res, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if res.Content != "done" || res.FinishReason != "stop" {
		t.Errorf("result = %q/%q, want done/stop", res.Content, res.FinishReason)
	}
}

func TestChatStreamHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"loading model"}}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want unexpected status 503", err)
	}
}

func TestChatStreamCallbackErrorAborts(t *testing.T) {
	srv := serveFixture(t, "llamacpp_content.sse")
	defer srv.Close()

	wantErr := errors.New("sink gone")
	calls := 0
	_, err := ChatStream(context.Background(), srv.URL, "", "m", nil, nil, Options{}, nil, 5*time.Second, func(StreamEvent) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapped callback error", err)
	}
	if calls != 1 {
		t.Errorf("callback calls = %d, want 1 (abort on first error)", calls)
	}
}

func TestChatStreamContextCancelMidStream(t *testing.T) {
	// Server streams one delta, then holds the connection open until the
	// client gives up — cancellation must abort the read promptly.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	_, err := ChatStream(ctx, srv.URL, "", "m", nil, nil, Options{}, nil, time.Minute, func(StreamEvent) error {
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("want error after mid-stream cancel")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want context cancellation", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("cancel took %v, want prompt abort", time.Since(start))
	}
}

// --- request wire shape ---.

func captureRequestBody(t *testing.T, reply string) (*httptest.Server, *[]byte, *http.Header) {
	t.Helper()
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = io.ReadFull(r.Body, b)
		body = b
		hdr = r.Header.Clone()
		_, _ = w.Write([]byte(reply))
	}))
	return srv, &body, &hdr
}

const minimalStopStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n"

func TestChatStreamRequestWire(t *testing.T) {
	srv, body, hdr := captureRequestBody(t, minimalStopStream)
	defer srv.Close()

	tool := ToolDef{Type: "function", Function: ToolDefFunction{
		Name:        "get_weather",
		Description: "Get the current weather for a city",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}
	msgs := []ChatMsg{
		{Role: "system", Content: "You are a weather demo assistant."},
		{Role: "user", Content: "Weather in Berlin? Use get_weather."},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
			ID: "call_demo_001", Type: "function",
			Function: ToolCallFunction{Name: "get_weather", Arguments: json.RawMessage(`{"city":"Berlin"}`)},
		}}},
		{Role: "tool", Content: `{"temperature_c":21}`, ToolCallID: "call_demo_001"},
	}
	opts := Options{Temperature: 0.7, RepeatPenalty: 1.1, NumPredict: 256}
	_, err := ChatStream(context.Background(), srv.URL, "test-key", "demo-model", msgs, []ToolDef{tool}, opts, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if got := hdr.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", got)
	}

	var req map[string]any
	if err := json.Unmarshal(*body, &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if req["stream"] != true {
		t.Error("stream != true")
	}
	if so, ok := req["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage true", req["stream_options"])
	}
	if req["max_tokens"] != float64(256) {
		t.Errorf("max_tokens = %v, want 256", req["max_tokens"])
	}
	if fp, _ := req["frequency_penalty"].(float64); fp < 0.099 || fp > 0.101 {
		t.Errorf("frequency_penalty = %v, want 0.1 (RepeatPenalty-1)", req["frequency_penalty"])
	}
	// E4/E5 traps: these fields must never be sent.
	if _, has := req["tool_choice"]; has {
		t.Error("tool_choice present — must be omitted (default auto)")
	}
	if _, has := req["parallel_tool_calls"]; has {
		t.Error("parallel_tool_calls present — must be omitted in v1")
	}
	if rs, ok := req["reasoning"].(map[string]any); !ok || rs["enabled"] != false || rs["exclude"] != true {
		t.Errorf("reasoning = %v, want {enabled:false,exclude:true}", req["reasoning"])
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 entry", req["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool name = %v, want get_weather", fn["name"])
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("tool parameters = %T, want JSON object passthrough", fn["parameters"])
	}

	messages := req["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(messages))
	}
	asst := messages[2].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	tcFn := tcs[0].(map[string]any)["function"].(map[string]any)
	// Roundtrip canon: arguments must be the OpenAI STRING form on the wire.
	argStr, ok := tcFn["arguments"].(string)
	if !ok {
		t.Fatalf("assistant tool_calls arguments = %T, want JSON string", tcFn["arguments"])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argStr), &m); err != nil || m["city"] != "Berlin" {
		t.Errorf("arguments string = %q, want JSON text {\"city\":\"Berlin\"}", argStr)
	}
	toolMsg := messages[3].(map[string]any)
	if toolMsg["tool_call_id"] != "call_demo_001" || toolMsg["role"] != "tool" {
		t.Errorf("tool message = %v, want role tool + tool_call_id", toolMsg)
	}
}

func TestChatStreamNoToolsOmitsToolsField(t *testing.T) {
	// The tools-exhausted closing call relies on OMITTING the array (E4:
	// tool_choice "none" makes the model emit tool syntax as plain text).
	srv, body, _ := captureRequestBody(t, minimalStopStream)
	defer srv.Close()

	_, err := ChatStream(context.Background(), srv.URL, "", "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(*body, &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if _, has := req["tools"]; has {
		t.Error("tools field present, want omitted when nil")
	}
}

func TestChatStreamExtraBodyMergeAndOverride(t *testing.T) {
	srv, body, _ := captureRequestBody(t, minimalStopStream)
	defer srv.Close()

	extra := map[string]any{
		"provider":  map[string]any{"zdr": true},     // additive (OpenRouter)
		"reasoning": map[string]any{"enabled": true}, // override of a core field
	}
	_, err := ChatStream(context.Background(), srv.URL, "", "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{}, extra, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(*body, &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if p, ok := req["provider"].(map[string]any); !ok || p["zdr"] != true {
		t.Errorf("provider = %v, want zdr:true merged in", req["provider"])
	}
	if rs, _ := req["reasoning"].(map[string]any); rs["enabled"] != true {
		t.Errorf("reasoning = %v, want extraBody override to win", req["reasoning"])
	}
	if req["stream"] != true {
		t.Error("core field stream lost in merge")
	}
}

// --- ToolCall marshal/unmarshal symmetry ---.

func TestToolCallMarshalEmitsStringForm(t *testing.T) {
	tc := ToolCall{ID: "call_1", Type: "function",
		Function: ToolCallFunction{Name: "f", Arguments: json.RawMessage(`{"a":1}`)}}
	b, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Function struct {
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("arguments not a string on the wire: %v (%s)", err, b)
	}
	if wire.Function.Arguments != `{"a":1}` {
		t.Errorf("arguments = %q, want {\"a\":1}", wire.Function.Arguments)
	}

	// Round-trip back: unmarshal normalises to object form again.
	var back ToolCall
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(back.Function.Arguments) != `{"a":1}` {
		t.Errorf("round-tripped arguments = %q, want object form", back.Function.Arguments)
	}
}

func TestToolCallMarshalDoesNotDoubleEncodeStringForm(t *testing.T) {
	tc := ToolCall{ID: "call_1",
		Function: ToolCallFunction{Name: "f", Arguments: json.RawMessage(`"{\"a\":1}"`)}}
	b, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Type     string `json:"type"`
		Function struct {
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if wire.Function.Arguments != `{"a":1}` {
		t.Errorf("arguments = %q, want single-encoded {\"a\":1}", wire.Function.Arguments)
	}
	if wire.Type != "function" {
		t.Errorf("type = %q, want default function", wire.Type)
	}
}

// --- live smoke (gated; the wave's herbert roundtrip gate) ---.

// TestChatStreamLiveSmoke runs one plain turn and one full tool roundtrip
// against a real llama.cpp server. Gated behind CTX_STREAM_SMOKE_HOST (e.g.
// http://10.13.37.11:8089) and skipped in -short — the server serves the
// live system, keep invocations rare and small.
func TestChatStreamLiveSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("live smoke skipped in -short")
	}
	host := os.Getenv("CTX_STREAM_SMOKE_HOST")
	if host == "" {
		t.Skip("CTX_STREAM_SMOKE_HOST not set")
	}
	model := os.Getenv("CTX_STREAM_SMOKE_MODEL") // llama.cpp ignores it; OpenRouter needs it
	opts := Options{Temperature: 0.2, NumPredict: 256}

	t.Run("plain", func(t *testing.T) {
		var deltas int
		res, err := ChatStream(context.Background(), host, "", model, []ChatMsg{
			{Role: "system", Content: "You are a demo assistant."},
			{Role: "user", Content: "Reply with exactly: streaming smoke ok"},
		}, nil, opts, nil, 2*time.Minute, func(ev StreamEvent) error {
			if ev.ContentDelta != "" {
				deltas++
			}
			return nil
		})
		if err != nil {
			t.Fatalf("plain turn: %v", err)
		}
		if res.FinishReason != "stop" || res.Content == "" || deltas == 0 {
			t.Errorf("plain turn = finish %q, %d deltas, content %q", res.FinishReason, deltas, res.Content)
		}
		t.Logf("plain: %q (tokens %d/%d, draft %.2f)", res.Content, res.PromptTokens, res.CompletionTokens, res.DraftAccept)
	})

	t.Run("tool roundtrip", func(t *testing.T) {
		tool := ToolDef{Type: "function", Function: ToolDefFunction{
			Name:        "get_weather",
			Description: "Get the current weather for a city",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}}
		msgs := []ChatMsg{
			{Role: "system", Content: "You are a weather demo assistant."},
			{Role: "user", Content: "What is the weather in Berlin right now? Use the get_weather tool."},
		}
		res, err := ChatStream(context.Background(), host, "", model, msgs, []ToolDef{tool}, opts, nil, 2*time.Minute, nil)
		if err != nil {
			t.Fatalf("tool turn: %v", err)
		}
		if res.FinishReason != "tool_calls" || len(res.ToolCalls) != 1 {
			t.Fatalf("tool turn = finish %q, %d calls, content %q", res.FinishReason, len(res.ToolCalls), res.Content)
		}
		tc := res.ToolCalls[0]
		args := argsAsMap(t, tc)
		t.Logf("tool call: %s(%v) id=%s draft=%.2f", tc.Function.Name, args, tc.ID, res.DraftAccept)
		if tc.Function.Name != "get_weather" || args["city"] == "" {
			t.Fatalf("tool call = %s(%v), want get_weather with city", tc.Function.Name, args)
		}

		// Echo the assembled call back (MarshalJSON string-form canon) plus a
		// synthetic tool result; the model must weave it into a final answer.
		msgs = append(msgs,
			ChatMsg{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls},
			ChatMsg{Role: "tool", Content: `{"temperature_c":21,"condition":"sunny"}`, ToolCallID: tc.ID},
		)
		final, err := ChatStream(context.Background(), host, "", model, msgs, []ToolDef{tool}, opts, nil, 2*time.Minute, nil)
		if err != nil {
			t.Fatalf("roundtrip turn: %v", err)
		}
		if final.FinishReason != "stop" || len(final.ToolCalls) != 0 || !strings.Contains(final.Content, "21") {
			t.Errorf("roundtrip = finish %q, %d calls, content %q — want stop/0/answer with 21", final.FinishReason, len(final.ToolCalls), final.Content)
		}
		t.Logf("roundtrip: %q (tokens %d/%d)", final.Content, final.PromptTokens, final.CompletionTokens)
	})
}
