package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// Reasoning servers on the OpenAI wire (vLLM, and LiteLLM in front of it)
// deliver the chain-of-thought in reasoning_content, not in the OpenRouter
// reasoning field. These tests pin the three behaviours around it: the trace
// is never the answer, a cap hit spent entirely on thinking is a typed error
// instead of an empty answer, and model_map params reach the stream wire so
// thinking can be disabled per role in the first place.

// --- stream path: model_map params on the wire ---.

// TestStreamBodyCarriesModelMapParams pins the web-chat wire contract:
// model_map params without a dedicated Options field (chat_template_kwargs)
// reach the stream body through ResolveModelParams → Options.Extra, with the
// same precedence as the non-stream path (Extra before the backend's
// extra_body, which wins on collision), and a CapLocked cap is not widened by
// a role-wide max_tokens.
func TestStreamBodyCarriesModelMapParams(t *testing.T) {
	srv, body, _ := captureRequestBody(t, minimalStopStream)
	defer srv.Close()

	b := backends.Backend{}
	opts := ResolveModelParams(Options{Temperature: 0.7, NumPredict: 256, CapLocked: true}, map[string]any{
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"seed":                 float64(7),
		"temperature":          0.3,
		"max_tokens":           float64(32000),
	}, &b)
	extraBody := map[string]any{"seed": float64(99)} // backend row wins on collision

	if _, err := ChatStream(context.Background(), srv.URL, "", "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, opts, extraBody, 5*time.Second, nil); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(*body, &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	ctk, ok := req["chat_template_kwargs"].(map[string]any)
	if !ok || ctk["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs = %v, want {enable_thinking:false} on the stream wire", req["chat_template_kwargs"])
	}
	if req["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want the model_map 0.3", req["temperature"])
	}
	if req["max_tokens"] != float64(256) {
		t.Errorf("max_tokens = %v, want the CapLocked 256 (model_map 32000 must not widen it)", req["max_tokens"])
	}
	if req["seed"] != float64(99) {
		t.Errorf("seed = %v, want backend extra_body 99 to win over model_map 7", req["seed"])
	}
	if req["stream"] != true {
		t.Error("core field stream lost in merge")
	}
}

// --- stream path: reasoning_content ---.

func sseFrames(frames ...string) string {
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("data: " + f + "\n\n")
	}
	sb.WriteString("data: [DONE]\n")
	return sb.String()
}

func TestChatStreamReasoningExhaustedBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
	}{
		{"vLLM reasoning_content", "reasoning_content"},
		{"OpenRouter reasoning", "reasoning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := sseFrames(
				fmt.Sprintf(`{"choices":[{"index":0,"delta":{%q:"Let me think about"}}]}`, tc.field),
				fmt.Sprintf(`{"choices":[{"index":0,"delta":{%q:" this carefully"},"finish_reason":"length"}]}`, tc.field),
			)
			srv, _, _ := captureRequestBody(t, stream)
			defer srv.Close()
			var events []StreamEvent
			res, err := ChatStream(context.Background(), srv.URL, "", "m",
				[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 2}, nil, 5*time.Second, collectEvents(&events))
			if !errors.Is(err, ErrReasoningExhaustedBudget) {
				t.Fatalf("err = %v (res %+v), want ErrReasoningExhaustedBudget", err, res)
			}
			for _, ev := range events {
				if ev.ContentDelta != "" {
					t.Errorf("reasoning leaked as a content delta: %q", ev.ContentDelta)
				}
			}
		})
	}
}

// reasoning_content next to a real answer: the answer is returned, the trace
// is neither emitted nor appended.
func TestChatStreamReasoningContentNeverBecomesAnswer(t *testing.T) {
	stream := sseFrames(
		`{"choices":[{"index":0,"delta":{"reasoning_content":"secret thoughts"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"The answer."},"finish_reason":"stop"}]}`,
	)
	srv, _, _ := captureRequestBody(t, stream)
	defer srv.Close()
	res, err := ChatStream(context.Background(), srv.URL, "", "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if res.Content != "The answer." {
		t.Errorf("content = %q, want only the answer", res.Content)
	}

	// Natural stop with ONLY a vLLM trace: empty answer, never the trace.
	stream = sseFrames(`{"choices":[{"index":0,"delta":{"reasoning_content":"secret thoughts"},"finish_reason":"stop"}]}`)
	srv2, _, _ := captureRequestBody(t, stream)
	defer srv2.Close()
	res, err = ChatStream(context.Background(), srv2.URL, "", "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if res.Content != "" {
		t.Errorf("content = %q, want empty — reasoning_content is not an answer", res.Content)
	}
}

// A cap hit after a complete tool call is still a usable turn.
func TestChatStreamReasoningCapHitWithToolCallIsNotAnError(t *testing.T) {
	stream := sseFrames(
		`{"choices":[{"index":0,"delta":{"reasoning_content":"need a tool"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"length"}]}`,
	)
	srv, _, _ := captureRequestBody(t, stream)
	defer srv.Close()
	res, err := ChatStream(context.Background(), srv.URL, "", "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{}, nil, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Errorf("tool calls = %d, want 1", len(res.ToolCalls))
	}
}

// --- non-stream path ---.

func openAIStub(t *testing.T, reply string) backends.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return backends.Backend{Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m"}
}

func TestChatOpenAIReasoningExhaustedBudget(t *testing.T) {
	b := openAIStub(t, `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"Let me think"},"finish_reason":"length"}],"usage":{"completion_tokens":128,"prompt_tokens":10}}`)
	resp, err := Chat(context.Background(), b, "sys", "user", Options{NumPredict: 128}, 5*time.Second)
	if !errors.Is(err, ErrReasoningExhaustedBudget) {
		t.Fatalf("err = %v (resp %+v), want ErrReasoningExhaustedBudget", err, resp)
	}
	if !strings.Contains(err.Error(), "max_tokens=128") {
		t.Errorf("error %q should name the exhausted cap", err)
	}
}

func TestChatOpenAIReasoningFields(t *testing.T) {
	for _, tc := range []struct {
		name, reply, want string
	}{
		{
			"vLLM trace next to an answer is ignored",
			`{"choices":[{"message":{"content":"answer","reasoning_content":"thoughts"},"finish_reason":"stop"}]}`,
			"answer",
		},
		{
			"vLLM trace alone is never the answer",
			`{"choices":[{"message":{"content":"","reasoning_content":"thoughts"},"finish_reason":"stop"}]}`,
			"",
		},
		{
			// Pre-existing quirk fix, unchanged: providers that ignore
			// reasoning.exclude and answer in the OpenRouter field.
			"OpenRouter reasoning fallback kept",
			`{"choices":[{"message":{"content":"","reasoning":"answer via reasoning"},"finish_reason":"stop"}]}`,
			"answer via reasoning",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := Chat(context.Background(), openAIStub(t, tc.reply), "sys", "user", Options{}, 5*time.Second)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if resp.Message.Content != tc.want {
				t.Errorf("content = %q, want %q", resp.Message.Content, tc.want)
			}
		})
	}
}
